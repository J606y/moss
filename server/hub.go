package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

// LivePoint 与前端 LivePoint 对齐（百分比 + 原始速率）。
type LivePoint struct {
	Time      int64   `json:"time"`
	CPU       float64 `json:"cpu"`
	Mem       float64 `json:"mem"`
	Disk      float64 `json:"disk"`
	Swap      float64 `json:"swap"`
	Load1     float64 `json:"load1"`
	NetUp     float64 `json:"netUp"`
	NetDown   float64 `json:"netDown"`
	TCP       int     `json:"tcp"`
	Processes int     `json:"processes"`
}

// totals 计算百分比所需的容量信息，注册时更新。
type totals struct {
	mem, swap, disk uint64
}

type liveState struct {
	stats   protocol.Stats
	uptime  uint64
	online  bool
	buf     []LivePoint
	tot     totals
	// 历史采样累加器
	accN                                            int
	accCPU, accMem, accSwap, accDisk, accLoad1      float64
	accUp, accDown                                  float64
	accTCP, accProc                                 int
	lastSample                                      time.Time
}

type agentConn struct {
	conn *websocket.Conn
	mu   sync.Mutex // 串行化写
}

func (a *agentConn) send(v any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return a.conn.WriteJSON(v)
}

// Hub 维护 agent 连接、浏览器订阅与每台服务器的实时状态。
type Hub struct {
	db       *sql.DB
	notifier *Notifier // 启动时注入，上线/离线/上报事件转发给告警引擎

	mu       sync.RWMutex
	agents   map[string]*agentConn // serverID → 连接
	live     map[string]*liveState
	browsers map[chan []byte]struct{}

	// sample_interval 进程内缓存：后台改设置后近实时生效，避免每次上报都查库。
	siMu       sync.Mutex
	siCached   time.Duration
	siReadAt   time.Time
}

// sampleInterval 读取“采样间隔”设置，5 秒内复用缓存值。
// 返回 time.Duration（设置单位为秒，决定历史落库的最细粒度）。
func (h *Hub) sampleInterval() time.Duration {
	h.siMu.Lock()
	defer h.siMu.Unlock()
	if h.siCached > 0 && time.Since(h.siReadAt) < 5*time.Second {
		return h.siCached
	}
	d := time.Duration(getSettingInt(h.db, "sample_interval", 10)) * time.Second
	h.siCached = d
	h.siReadAt = time.Now()
	return d
}

func newHub(db *sql.DB) *Hub {
	return &Hub{
		db:       db,
		agents:   make(map[string]*agentConn),
		live:     make(map[string]*liveState),
		browsers: make(map[chan []byte]struct{}),
	}
}

func (h *Hub) state(id string) *liveState {
	st, ok := h.live[id]
	if !ok {
		st = &liveState{lastSample: time.Now()}
		h.live[id] = st
	}
	return st
}

// SetTotals 注册时缓存容量信息，用于折算百分比。
func (h *Hub) SetTotals(id string, mem, swap, disk uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state(id).tot = totals{mem, swap, disk}
}

// RegisterAgent 记录连接并踢掉同 ID 旧连接。
func (h *Hub) RegisterAgent(id string, c *agentConn) {
	h.mu.Lock()
	old := h.agents[id]
	h.agents[id] = c
	h.state(id).online = true
	h.mu.Unlock()
	if old != nil {
		old.conn.Close()
	}
	h.notifier.OnOnline(id)
	h.broadcast(map[string]any{"type": "online", "id": id})
}

// UnregisterAgent 仅当当前连接仍是该 ID 的活跃连接时标记离线。
func (h *Hub) UnregisterAgent(id string, c *agentConn) {
	h.mu.Lock()
	if h.agents[id] != c {
		h.mu.Unlock()
		return
	}
	delete(h.agents, id)
	st := h.state(id)
	st.online = false
	st.stats = protocol.Stats{}
	h.mu.Unlock()
	h.db.Exec(`UPDATE servers SET last_seen = ? WHERE id = ?`, time.Now().Unix(), id)
	h.notifier.OnOffline(id)
	h.broadcast(map[string]any{"type": "offline", "id": id})
}

// AgentConn 返回某服务器的活跃 agent 连接（可能为 nil）。
func (h *Hub) AgentConn(id string) *agentConn {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agents[id]
}

// AgentIDs 返回所有在线 agent 的服务器 ID。
func (h *Hub) AgentIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.agents))
	for id := range h.agents {
		ids = append(ids, id)
	}
	return ids
}

func pct(used uint64, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// HandleReport 处理一次实时上报：更新状态、滚动缓冲、采样累加、广播。
// 采样间隔在每次上报时按需读取（带 5s 进程内缓存），后台改设置后近实时生效；
// 形参 _ 保留以维持对外签名稳定（api.go 等依赖此签名）。
func (h *Hub) HandleReport(id string, s *protocol.Stats, uptime uint64, _ time.Duration) {
	now := time.Now()
	sampleInterval := h.sampleInterval()
	h.mu.Lock()
	st := h.state(id)
	st.stats = *s
	st.uptime = uptime
	st.online = true

	point := LivePoint{
		Time:      now.UnixMilli(),
		CPU:       s.CPU,
		Mem:       pct(s.MemUsed, st.tot.mem),
		Disk:      pct(s.DiskUsed, st.tot.disk),
		Swap:      pct(s.SwapUsed, st.tot.swap),
		Load1:     s.Load1,
		NetUp:     s.NetUp,
		NetDown:   s.NetDown,
		TCP:       s.TCP,
		Processes: s.Processes,
	}
	st.buf = append(st.buf, point)
	if len(st.buf) > 90 {
		st.buf = st.buf[1:]
	}

	st.accN++
	st.accCPU += point.CPU
	st.accMem += point.Mem
	st.accSwap += point.Swap
	st.accDisk += point.Disk
	st.accLoad1 += point.Load1
	st.accUp += point.NetUp
	st.accDown += point.NetDown
	st.accTCP += point.TCP
	st.accProc += point.Processes

	var sample *LivePoint
	if now.Sub(st.lastSample) >= sampleInterval && st.accN > 0 {
		n := float64(st.accN)
		sample = &LivePoint{
			Time:      now.UnixMilli(),
			CPU:       st.accCPU / n,
			Mem:       st.accMem / n,
			Swap:      st.accSwap / n,
			Disk:      st.accDisk / n,
			Load1:     st.accLoad1 / n,
			NetUp:     st.accUp / n,
			NetDown:   st.accDown / n,
			TCP:       st.accTCP / st.accN,
			Processes: st.accProc / st.accN,
		}
		st.accN, st.accCPU, st.accMem, st.accSwap, st.accDisk = 0, 0, 0, 0, 0
		st.accLoad1, st.accUp, st.accDown, st.accTCP, st.accProc = 0, 0, 0, 0, 0
		st.lastSample = now
	}
	h.mu.Unlock()

	if sample != nil {
		if _, err := h.db.Exec(
			`INSERT INTO history(server_id, time, cpu, mem, swap, disk, load1, net_up, net_down, tcp, processes)
			 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, sample.Time, sample.CPU, sample.Mem, sample.Swap, sample.Disk,
			sample.Load1, sample.NetUp, sample.NetDown, sample.TCP, sample.Processes,
		); err != nil {
			log.Printf("写入 history 失败: %v", err)
		}
	}

	h.notifier.OnReport(id, point.CPU, point.Mem, point.Disk)
	// time 用服务端落点时刻：与 /recent、历史接口同源，浏览器据此打点，
	// 避免「实时点用浏览器时钟、回填点用服务端时钟」导致两段曲线间出现时钟偏差造成的空窗。
	h.broadcast(map[string]any{"type": "stats", "id": id, "stats": s, "uptimeSec": uptime, "time": point.Time})
}

// Snapshot 返回某服务器当前实时状态。
func (h *Hub) Snapshot(id string) (protocol.Stats, uint64, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	st, ok := h.live[id]
	if !ok {
		return protocol.Stats{}, 0, false
	}
	return st.stats, st.uptime, st.online
}

// Recent 返回最近 90 个实时点（拷贝）。
func (h *Hub) Recent(id string) []LivePoint {
	h.mu.RLock()
	defer h.mu.RUnlock()
	st, ok := h.live[id]
	if !ok {
		return []LivePoint{}
	}
	out := make([]LivePoint, len(st.buf))
	copy(out, st.buf)
	return out
}

// Drop 删除服务器时清理实时状态。
func (h *Hub) Drop(id string) {
	h.mu.Lock()
	conn := h.agents[id]
	delete(h.agents, id)
	delete(h.live, id)
	h.mu.Unlock()
	h.notifier.Forget(id)
	if conn != nil {
		conn.conn.Close()
	}
}

/* ---------- 浏览器订阅 ---------- */

func (h *Hub) broadcast(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.browsers {
		select {
		case ch <- data:
		default: // 客户端写入阻塞则丢弃，避免拖垮全局
		}
	}
}

// BroadcastMeta 通知浏览器端服务器列表已变更。
func (h *Hub) BroadcastMeta() {
	h.broadcast(map[string]any{"type": "meta"})
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true },
}
