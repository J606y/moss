package main

import (
	"database/sql"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

// clientIP 取真实来源 IP。仅当 --trust-proxy 开启时才信任反代转发头
// (X-Real-IP/X-Forwarded-For)，否则一律取 r.RemoteAddr 的 host，防止伪造。
func (s *App) clientIP(r *http.Request) string {
	if s.trustProxy {
		if v := r.Header.Get("X-Real-IP"); v != "" {
			return v
		}
		if v := r.Header.Get("X-Forwarded-For"); v != "" {
			return strings.TrimSpace(strings.Split(v, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tasksForServer 查询应用到某服务器的启用探测任务。
func tasksForServer(db *sql.DB, serverID string) []protocol.PingTask {
	rows, err := db.Query(
		`SELECT id, type, target, interval FROM ping_tasks WHERE enabled = 1 AND (server_id = '' OR server_id = ?)`,
		serverID,
	)
	if err != nil {
		log.Printf("查询探测任务失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []protocol.PingTask
	for rows.Next() {
		var t protocol.PingTask
		if err := rows.Scan(&t.ID, &t.Type, &t.Target, &t.Interval); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// pushConfig 向某台服务器的 agent 下发最新配置。
func (s *App) pushConfig(serverID string) {
	c := s.hub.AgentConn(serverID)
	if c == nil {
		return
	}
	msg := protocol.ServerMsg{
		Type:     "config",
		Interval: getSettingInt(s.db, "report_interval", 2),
		Tasks:    tasksForServer(s.db, serverID),
	}
	if err := c.send(msg); err != nil {
		log.Printf("下发配置到 %s 失败: %v", serverID, err)
	}
}

// pushConfigAll 配置变更后通知所有在线 agent。
func (s *App) pushConfigAll() {
	for _, id := range s.hub.AgentIDs() {
		s.pushConfig(id)
	}
}

func (s *App) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	var serverID string
	if err := s.db.QueryRow(`SELECT id FROM servers WHERE token = ?`, token).Scan(&serverID); err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ac := &agentConn{conn: conn}
	ip := s.clientIP(r)
	log.Printf("agent 已连接: %s (%s)", serverID, ip)

	s.hub.RegisterAgent(serverID, ac)
	defer func() {
		conn.Close()
		s.hub.UnregisterAgent(serverID, ac)
		log.Printf("agent 已断开: %s", serverID)
	}()

	// 心跳：30s ping，60s 未读到任何数据判定断开
	conn.SetReadLimit(64 << 10)
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				ac.mu.Lock()
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteMessage(websocket.PingMessage, nil)
				ac.mu.Unlock()
				if err != nil {
					return
				}
			case <-stopPing:
				return
			}
		}
	}()

	// 采样间隔不再于连接建立时固定读取；HandleReport 每次上报时按需读取（带缓存），
	// 后台改“采样间隔”后近实时生效。

	for {
		var msg protocol.AgentMsg
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		switch msg.Type {
		case "register":
			if msg.Info == nil {
				continue
			}
			info := msg.Info
			// 优先用 agent 自测的公网 IP；拿不到才回退到连接来源 IP
			// （Docker/反代下来源 IP 往往是网桥网关，如 172.17.0.1）。
			realIP := info.IP
			if realIP == "" {
				realIP = ip
			}
			if _, err := s.db.Exec(
				`UPDATE servers SET os=?, arch=?, virt=?, cpu_model=?, cpu_cores=?, mem_total=?, swap_total=?,
				 disk_total=?, agent_version=?, ip=?, last_seen=? WHERE id=?`,
				info.OS, info.Arch, info.Virtualization, info.CPUModel, info.CPUCores,
				info.MemTotal, info.SwapTotal, info.DiskTotal, info.AgentVersion, realIP, time.Now().Unix(), serverID,
			); err != nil {
				log.Printf("更新主机信息失败: %v", err)
			}
			// 自动国旗：仅在 agent 解析出国家码时更新，避免覆盖；手动设置的 flag 始终优先。
			if info.CountryCode != "" {
				if _, err := s.db.Exec(`UPDATE servers SET auto_flag=? WHERE id=?`, info.CountryCode, serverID); err != nil {
					log.Printf("更新自动国旗失败: %v", err)
				}
			}
			s.hub.SetTotals(serverID, info.MemTotal, info.SwapTotal, info.DiskTotal)
			s.hub.BroadcastMeta()
			s.pushConfig(serverID)

		case "report":
			if msg.Stats == nil {
				continue
			}
			// 第四参数已废弃（HandleReport 内部按需读取采样间隔），传 0 占位以维持签名稳定。
			s.hub.HandleReport(serverID, msg.Stats, msg.UptimeSec, 0)

		case "ping":
			if _, err := s.db.Exec(
				`INSERT INTO ping_results(task_id, server_id, time, ms) VALUES(?, ?, ?, ?)`,
				msg.TaskID, serverID, time.Now().UnixMilli(), msg.Ms,
			); err != nil {
				log.Printf("写入探测结果失败: %v", err)
			}
		}
	}
}
