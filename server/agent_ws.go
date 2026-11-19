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

// taskAppliesTo 判断探测任务是否应用于指定服务器。
// taskServerID 为空字符串表示「全部服务器」，否则为逗号分隔的服务器 ID 列表。
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

// tasksForServer 查询应用到某服务器的启用探测任务。
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

// pushConfig 向某台服务器的 agent 下发最新配置。
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

// pushConfigAll 配置变更后通知所有在线 agent。
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
		// 只有这条连接仍是活跃连接时才收敛在途执行。
		//
		// OnAgentGone 按 serverID 扫全表，不区分任务是由哪条连接下发的。
		// agent 抖动重连后，旧连接的 goroutine 才从 ReadJSON 醒来跑到这里，
		// 此刻新连接早已在册、新的 exec 也可能刚下发——无条件调用会把它打成
		// 「执行期间 agent 掉线」，而那条命令实际正在目标机上正常执行。
		if s.hub.UnregisterAgent(serverID, ac) {
			s.exec.OnAgentGone(serverID)
		}
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
			// 优先用 agent 自测的公网 IP；某一族拿不到才用连接来源 IP 按地址族兜底
			// （Docker/反代下来源 IP 往往是网桥网关，如 172.17.0.1）。
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
			// 自动国旗：仅在 agent 解析出国家码时更新，避免覆盖；手动设置的 flag 始终优先。
			if info.CountryCode != "" {
				if _, err := s.db.Exec(`UPDATE servers SET auto_flag=? WHERE id=?`, info.CountryCode, serverID); err != nil {
					log.Printf("更新自动国旗失败: %v", err)
				}
			}
			s.hub.SetTotals(serverID, info.MemTotal, info.SwapTotal, info.DiskTotal)
			s.hub.BroadcastMeta()
			s.pushConfig(serverID)
			// 升级的成功判据就在这里：agent 重连并上报版本号。
			// 版本等于目标即成功；仍是旧版本说明 agent 侧守护已经回滚。
			s.upgrade.OnRegister(serverID, info.AgentVersion)

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
