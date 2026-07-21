package main

import (
	"database/sql"
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
	interval := getSettingInt(s.db, keyReportInterval, 2)
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
		"name": getSetting(s.db, keySiteName, "Moss"),
		"desc": getSetting(s.db, keySiteDesc, "轻量服务器监控"),
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

// bucketMs 把所选时段切成约 targetPoints 个桶，返回每桶毫秒宽度，
// 供 SQL 内 GROUP BY time/桶宽 聚合降采样：无论时段多长，每条曲线点数都被压到 ~targetPoints，
// 避免前端 recharts 渲染上万点而卡顿。桶宽小于实际采样间隔时每桶至多一行，等价于不降采样，
// 短时段保持原始精度。
func bucketMs(hours int) int64 {
	const targetPoints = 600
	b := int64(hours) * 3600 * 1000 / targetPoints
	if b < 1 {
		b = 1
	}
	return b
}

func (s *App) handleRecent(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.hub.Recent(r.PathValue("id")))
}

func (s *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	hours := parseHours(r)
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	bucket := bucketMs(hours)
	// 按时间桶聚合：每桶取各指标均值，把点数压到 ~600，整数列四舍五入回整数。
	rows, err := s.db.Query(
		`SELECT CAST(AVG(time) AS INTEGER), AVG(cpu), AVG(mem), AVG(swap), AVG(disk), AVG(load1),
		        AVG(net_up), AVG(net_down),
		        CAST(ROUND(AVG(tcp)) AS INTEGER), CAST(ROUND(AVG(processes)) AS INTEGER)
		 FROM history WHERE server_id = ? AND time >= ?
		 GROUP BY time / ? ORDER BY 1`, id, since, bucket)
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
	hours := parseHours(r)
	since := time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	bucket := bucketMs(hours)

	taskRows, err := s.db.Query(`SELECT id, name, server_id FROM ping_tasks ORDER BY sort, id`)
	if err != nil {
		log.Printf("handlePing tasks (server=%s): %v", id, err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer taskRows.Close()
	tasks := []pingTaskMeta{}
	for taskRows.Next() {
		var t pingTaskMeta
		var sid string
		if err := taskRows.Scan(&t.ID, &t.Name, &sid); err == nil && taskAppliesTo(sid, id) {
			tasks = append(tasks, t)
		}
	}

	series := map[string][]pingPt{}
	for _, t := range tasks {
		key := fmt.Sprint(t.ID)
		series[key] = []pingPt{}
		// 按时间桶聚合：每桶取成功探测(ms>=0)的均值；整桶全丢包则均值为 NULL → 该点记为丢包。
		rows, err := s.db.Query(
			`SELECT CAST(AVG(time) AS INTEGER),
			        CAST(ROUND(AVG(CASE WHEN ms >= 0 THEN ms END)) AS INTEGER)
			 FROM ping_results WHERE server_id = ? AND task_id = ? AND time >= ?
			 GROUP BY time / ? ORDER BY 1`,
			id, t.ID, since, bucket)
		if err != nil {
			continue
		}
		for rows.Next() {
			var tm int64
			var ms sql.NullInt64
			if err := rows.Scan(&tm, &ms); err != nil {
				continue
			}
			p := pingPt{Time: tm}
			if ms.Valid {
				v := int(ms.Int64)
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
