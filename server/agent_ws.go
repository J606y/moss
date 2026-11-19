package main

import (
	"database/sql"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

func taskAppliesTo(taskServerID, serverID string) bool {
	if taskServerID == "" {
		return true
	}
	for _, id := range strings.Split(taskServerID, ",") {
		if id == serverID {
			return true
		}
	}
	return false
}

func tasksForServer(db *sql.DB, serverID string) []protocol.PingTask {
	rows, err := db.Query(`SELECT id, type, target, interval, server_id FROM ping_tasks WHERE enabled = 1`)
	if err != nil {
		log.Printf("查询探测任务失败: %v", err)
		return nil
	}
	defer rows.Close()
	var out []protocol.PingTask
	for rows.Next() {
		var t protocol.PingTask
		var sid string
		if err := rows.Scan(&t.ID, &t.Type, &t.Target, &t.Interval, &sid); err == nil && taskAppliesTo(sid, serverID) {
			out = append(out, t)
		}
	}
	return out
}

func (s *App) pushConfig(serverID string) {
	c := s.hub.AgentConn(serverID)
	if c == nil {
		return
	}
	msg := protocol.ServerMsg{
		Type:     "config",
		Interval: getSettingInt(s.db, keyReportInterval, 2),
		Tasks:    tasksForServer(s.db, serverID),
	}
	if err := c.send(msg); err != nil {
		log.Printf("下发配置到 %s 失败: %v", serverID, err)
	}
}

func (s *App) pushConfigAll() {
	for _, id := range s.hub.AgentIDs() {
		s.pushConfig(id)
	}
}

// rebindByUUID 若新注册上来的节点携带了固定的硬件/持久化 UUID，
// 且库中已有一条离线旧节点记录持有相同 UUID（典型场景：管理员重新添加节点拿了新 token 重装），
// 自动将新 token 合并回旧 server_id 并移除临时新建的空壳行，保证重注册后 server_id 永不改变。
func (s *App) rebindByUUID(currentID, token, uuid string) string {
	uuid = strings.ToLower(strings.TrimSpace(uuid))
	if uuid == "" {
		return currentID
	}
	var oldID string
	err := s.db.QueryRow(`SELECT id FROM servers WHERE uuid = ? AND id != ? ORDER BY created_at ASC LIMIT 1`,
		uuid, currentID).Scan(&oldID)
	if err != nil || oldID == "" {
		return currentID
	}
	// 仅在旧节点当前离线时才自动合并接管，防止克隆镜像未清理 machine-id 时两台在线机器互相顶号。
	if s.hub.AgentConn(oldID) != nil {
		return currentID
	}
	tx, err := s.db.Begin()
	if err != nil {
		return currentID
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM servers WHERE id = ?`, currentID); err != nil {
		return currentID
	}
	if _, err := tx.Exec(`UPDATE servers SET token = ?, uuid = ? WHERE id = ?`, token, uuid, oldID); err != nil {
		return currentID
	}
	if err := tx.Commit(); err != nil {
		return currentID
	}
	log.Printf("检测到相同节点 UUID (%s) 重注册，已将新连接自动绑定回原 server_id=%s（释放临时 id=%s）", uuid, oldID, currentID)
	return oldID
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
	ip := realIP(r, s.trustProxy, s.trustedProxies)
	log.Printf("agent 已连接: %s (%s)", serverID, ip)

	s.hub.RegisterAgent(serverID, ac)
	defer func() {
		conn.Close()
		if s.hub.UnregisterAgent(serverID, ac) {
			s.exec.OnAgentGone(serverID)
		}
		log.Printf("agent 已断开: %s", serverID)
	}()

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
			nodeUUID := strings.ToLower(strings.TrimSpace(info.UUID))
			if nodeUUID != "" {
				if reboundID := s.rebindByUUID(serverID, token, nodeUUID); reboundID != serverID {
					s.hub.UnregisterAgent(serverID, ac)
					s.hub.Drop(serverID)
					serverID = reboundID
					s.hub.RegisterAgent(serverID, ac)
				}
			}
			ipv4, ipv6 := info.IP, info.IPv6
			if strings.Contains(ip, ":") {
				if ipv6 == "" {
					ipv6 = ip
				}
			} else if ipv4 == "" {
				ipv4 = ip
			}
			if _, err := s.db.Exec(
				`UPDATE servers SET uuid=COALESCE(NULLIF(?, ''), uuid), os=?, arch=?, virt=?, cpu_model=?, cpu_cores=?,
				 mem_total=?, swap_total=?, disk_total=?, agent_version=?, ip=?, ipv6=?, last_seen=? WHERE id=?`,
				nodeUUID, info.OS, info.Arch, info.Virtualization, info.CPUModel, info.CPUCores,
				info.MemTotal, info.SwapTotal, info.DiskTotal, info.AgentVersion, ipv4, ipv6, time.Now().Unix(), serverID,
			); err != nil {
				log.Printf("更新主机信息失败: %v", err)
			}
			if info.CountryCode != "" {
				if _, err := s.db.Exec(`UPDATE servers SET auto_flag=? WHERE id=?`, info.CountryCode, serverID); err != nil {
					log.Printf("更新自动国旗失败: %v", err)
				}
			}
			s.hub.SetTotals(serverID, info.MemTotal, info.SwapTotal, info.DiskTotal)
			s.hub.BroadcastMeta()
			s.pushConfig(serverID)
			s.upgrade.OnRegister(serverID, info.AgentVersion)

		case "report":
			if msg.Stats == nil {
				continue
			}
			s.hub.HandleReport(serverID, msg.Stats, msg.UptimeSec, 0)

		case "ping":
			if _, err := s.db.Exec(
				`INSERT INTO ping_results(task_id, server_id, time, ms) VALUES(?, ?, ?, ?)`,
				msg.TaskID, serverID, time.Now().UnixMilli(), msg.Ms,
			); err != nil {
				log.Printf("写入探测结果失败: %v", err)
			}

		case "exec_result":
			s.exec.OnResult(serverID, msg.Exec)

		case "job_status":
			s.exec.OnJobStatus(serverID, msg.JobStatus)

		case "read_result":
			s.exec.OnReadResult(serverID, msg.Read)

		case "upgrade_result":
			s.upgrade.OnResult(serverID, msg.Upgrade)
		}
	}
}
