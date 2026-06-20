package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// API 数据均为实时，禁止 CDN / 反代 / 浏览器缓存，否则增删后列表会拿到
	// 旧响应、要等缓存 TTL 过期才更新（表现为“列表好几分钟才刷新”）。
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// publicServer 与前端 ServerMeta + stats 对齐。
type publicServer struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Region         string          `json:"region"`
	Flag           string          `json:"flag"`
	OS             string          `json:"os"`
	Arch           string          `json:"arch"`
	Virtualization string          `json:"virtualization"`
	CPUModel       string          `json:"cpuModel"`
	CPUCores       int             `json:"cpuCores"`
	MemTotal       uint64          `json:"memTotal"`
	SwapTotal      uint64          `json:"swapTotal"`
	DiskTotal      uint64          `json:"diskTotal"`
	AgentVersion   string          `json:"agentVersion"`
	IntervalSec    int             `json:"intervalSec"`
	Online         bool            `json:"online"`
	UptimeSec      uint64          `json:"uptimeSec"`
	Group          string          `json:"group"`
	ExpireAt       string          `json:"expireAt,omitempty"`
	// Note 备注属内部信息，刻意不在公开接口返回（仅 /api/admin/servers 可见）
	Stats          *protocol.Stats `json:"stats"`
}

func (s *App) listPublicServers() ([]publicServer, error) {
	// 必须在 Query 之前取 setting：rows 未关闭时占用连接，期间再发查询会耗尽连接池
	interval := getSettingInt(s.db, "report_interval", 2)
	rows, err := s.db.Query(
		`SELECT id, name, grp, region, flag, auto_flag, expire_at, os, arch, virt, cpu_model, cpu_cores,
		 mem_total, swap_total, disk_total, agent_version FROM servers ORDER BY sort, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []publicServer{}
	for rows.Next() {
		var p publicServer
		var autoFlag string
		if err := rows.Scan(&p.ID, &p.Name, &p.Group, &p.Region, &p.Flag, &autoFlag, &p.ExpireAt,
			&p.OS, &p.Arch, &p.Virtualization, &p.CPUModel, &p.CPUCores,
			&p.MemTotal, &p.SwapTotal, &p.DiskTotal, &p.AgentVersion); err != nil {
			return nil, err
		}
		if p.Flag == "" {
			p.Flag = autoFlag // 手动国旗优先，未设则用自动识别
		}
		p.IntervalSec = interval
		stats, uptime, online := s.hub.Snapshot(p.ID)
		p.Stats = &stats
		p.UptimeSec = uptime
		p.Online = online
		out = append(out, p)
	}
	return out, nil
}

func (s *App) handleServers(w http.ResponseWriter, r *http.Request) {
	list, err := s.listPublicServers()
	if err != nil {
		log.Printf("handleServers: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, list)
}

func (s *App) handleSite(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{
		"name": getSetting(s.db, "site_name", "Moss"),
		"desc": getSetting(s.db, "site_desc", "轻量服务器监控"),
	})
}

func parseHours(r *http.Request) int {
	h, err := strconv.Atoi(r.URL.Query().Get("hours"))
	if err != nil || h < 1 {
		return 1
	}
	if h > 24*30 {
		h = 24 * 30
	}
	return h
}

func (s *App) handleRecent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.hub.Recent(r.PathValue("id")))
}

func (s *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since := time.Now().Add(-time.Duration(parseHours(r)) * time.Hour).UnixMilli()
	rows, err := s.db.Query(
		`SELECT time, cpu, mem, swap, disk, load1, net_up, net_down, tcp, processes
		 FROM history WHERE server_id = ? AND time >= ? ORDER BY time`, id, since)
	if err != nil {
		log.Printf("handleHistory query (server=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer rows.Close()
	out := []LivePoint{}
	for rows.Next() {
		var p LivePoint
		if err := rows.Scan(&p.Time, &p.CPU, &p.Mem, &p.Swap, &p.Disk, &p.Load1,
			&p.NetUp, &p.NetDown, &p.TCP, &p.Processes); err != nil {
			log.Printf("handleHistory scan (server=%s): %v", id, err)
			writeErr(w, 500, "内部错误")
			return
		}
		out = append(out, p)
	}
	writeJSON(w, 200, out)
}

type pingTaskMeta struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type pingPt struct {
	Time int64 `json:"time"`
	Ms   *int  `json:"ms"` // null = 丢包
}

func (s *App) handlePing(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since := time.Now().Add(-time.Duration(parseHours(r)) * time.Hour).UnixMilli()

	taskRows, err := s.db.Query(
		`SELECT id, name FROM ping_tasks WHERE server_id = '' OR server_id = ? ORDER BY id`, id)
	if err != nil {
		log.Printf("handlePing tasks (server=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer taskRows.Close()
	tasks := []pingTaskMeta{}
	for taskRows.Next() {
		var t pingTaskMeta
		if err := taskRows.Scan(&t.ID, &t.Name); err == nil {
			tasks = append(tasks, t)
		}
	}

	series := map[string][]pingPt{}
	for _, t := range tasks {
		key := fmt.Sprint(t.ID)
		series[key] = []pingPt{}
		rows, err := s.db.Query(
			`SELECT time, ms FROM ping_results WHERE server_id = ? AND task_id = ? AND time >= ? ORDER BY time`,
			id, t.ID, since)
		if err != nil {
			continue
		}
		for rows.Next() {
			var tm int64
			var ms int
			if err := rows.Scan(&tm, &ms); err != nil {
				continue
			}
			p := pingPt{Time: tm}
			if ms >= 0 {
				v := ms
				p.Ms = &v
			}
			series[key] = append(series[key], p)
		}
		rows.Close()
	}
	writeJSON(w, 200, map[string]any{"tasks": tasks, "series": series})
}

// handleBrowserWS 浏览器实时订阅。
func (s *App) handleBrowserWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ch := make(chan []byte, 64)
	s.hub.mu.Lock()
	s.hub.browsers[ch] = struct{}{}
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		delete(s.hub.browsers, ch)
		s.hub.mu.Unlock()
		conn.Close()
	}()

	// 读循环只用于感知断开
	go func() {
		conn.SetReadLimit(1024)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				conn.Close()
				return
			}
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case data, ok := <-ch:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
