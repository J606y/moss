package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// notifyConfig 通知告警配置，存于 settings 表。
type notifyConfig struct {
	TgToken       string `json:"tgToken"`
	TgChat        string `json:"tgChat"`
	OfflineOn     bool   `json:"offlineOn"`
	OfflineDelay  int    `json:"offlineDelay"` // 秒，离线超过该时长才告警（防抖）
	LoadOn        bool   `json:"loadOn"`
	CPUThreshold  int    `json:"cpuThreshold"`  // %
	MemThreshold  int    `json:"memThreshold"`  // %
	DiskThreshold int    `json:"diskThreshold"` // %
	LoadMinutes   int    `json:"loadMinutes"`   // 持续超阈值多少分钟才告警
}

func loadNotifyConfig(db *sql.DB) notifyConfig {
	return notifyConfig{
		TgToken:       getSetting(db, "notify_tg_token", ""),
		TgChat:        getSetting(db, "notify_tg_chat", ""),
		OfflineOn:     getSetting(db, "notify_offline", "0") == "1",
		OfflineDelay:  getSettingInt(db, "notify_offline_delay", 60),
		LoadOn:        getSetting(db, "notify_load", "0") == "1",
		CPUThreshold:  getSettingInt(db, "notify_cpu", 90),
		MemThreshold:  getSettingInt(db, "notify_mem", 90),
		DiskThreshold: getSettingInt(db, "notify_disk", 95),
		LoadMinutes:   getSettingInt(db, "notify_load_min", 5),
	}
}

// alertState 单台服务器的告警状态机。
type alertState struct {
	offlineSince   time.Time // 零值 = 在线
	offlineAlerted bool
	highSince      map[string]time.Time // 指标 → 开始超阈值时间
	highAlerted    map[string]bool
}

// Notifier 离线/负载告警引擎，事件由 Hub 驱动，离线判定走独立检查循环。
type Notifier struct {
	db *sql.DB

	mu     sync.Mutex
	cfg    notifyConfig
	states map[string]*alertState
}

func newNotifier(db *sql.DB) *Notifier {
	return &Notifier{
		db:     db,
		cfg:    loadNotifyConfig(db),
		states: make(map[string]*alertState),
	}
}

// Reload 设置变更后刷新配置缓存。
func (n *Notifier) Reload() {
	cfg := loadNotifyConfig(n.db)
	n.mu.Lock()
	n.cfg = cfg
	n.mu.Unlock()
}

func (n *Notifier) state(id string) *alertState {
	st, ok := n.states[id]
	if !ok {
		st = &alertState{
			highSince:   make(map[string]time.Time),
			highAlerted: make(map[string]bool),
		}
		n.states[id] = st
	}
	return st
}

func (n *Notifier) serverName(id string) string {
	var name string
	if err := n.db.QueryRow(`SELECT name FROM servers WHERE id = ?`, id).Scan(&name); err != nil {
		return id
	}
	return name
}

/* ---------- Hub 事件入口 ---------- */

func (n *Notifier) OnOnline(id string) {
	n.mu.Lock()
	st := n.state(id)
	wasAlerted := st.offlineAlerted
	downSince := st.offlineSince
	st.offlineSince = time.Time{}
	st.offlineAlerted = false
	cfg := n.cfg
	n.mu.Unlock()

	if wasAlerted && cfg.OfflineOn {
		dur := time.Since(downSince).Round(time.Second)
		n.send(cfg, fmt.Sprintf("🟢 服务器恢复\n%s 已重新上线（离线 %s）", n.serverName(id), dur))
	}
}

func (n *Notifier) OnOffline(id string) {
	n.mu.Lock()
	st := n.state(id)
	if st.offlineSince.IsZero() {
		st.offlineSince = time.Now()
	}
	n.mu.Unlock()
}

// OnReport 负载告警：超阈值持续 LoadMinutes 分钟才告警，回落 5%（迟滞）后发恢复。
func (n *Notifier) OnReport(id string, cpu, mem, disk float64) {
	n.mu.Lock()
	cfg := n.cfg
	if !cfg.LoadOn {
		n.mu.Unlock()
		return
	}
	st := n.state(id)
	type fire struct{ msg string }
	var fires []fire
	now := time.Now()
	hold := time.Duration(cfg.LoadMinutes) * time.Minute

	check := func(metric string, val float64, threshold int) {
		th := float64(threshold)
		if th <= 0 {
			return
		}
		if val >= th {
			if st.highSince[metric].IsZero() {
				st.highSince[metric] = now
			}
			if !st.highAlerted[metric] && now.Sub(st.highSince[metric]) >= hold {
				st.highAlerted[metric] = true
				fires = append(fires, fire{fmt.Sprintf("⚠️ 负载告警\n%%NAME%% %s 使用率 %.1f%%，已持续 %d 分钟（阈值 %d%%）",
					metric, val, cfg.LoadMinutes, threshold)})
			}
		} else if val < th-5 { // 迟滞带，避免在阈值附近反复告警
			if st.highAlerted[metric] {
				st.highAlerted[metric] = false
				fires = append(fires, fire{fmt.Sprintf("✅ 负载恢复\n%%NAME%% %s 已回落至 %.1f%%", metric, val)})
			}
			delete(st.highSince, metric)
		}
	}
	check("CPU", cpu, cfg.CPUThreshold)
	check("内存", mem, cfg.MemThreshold)
	check("硬盘", disk, cfg.DiskThreshold)
	n.mu.Unlock()

	if len(fires) > 0 {
		name := n.serverName(id)
		for _, f := range fires {
			n.send(cfg, strings.ReplaceAll(f.msg, "%NAME%", name))
		}
	}
}

// Forget 服务器删除时清理状态。
func (n *Notifier) Forget(id string) {
	n.mu.Lock()
	delete(n.states, id)
	n.mu.Unlock()
}

// Run 离线检查循环：离线超过 OfflineDelay 且未告警 → 推送。
func (n *Notifier) Run() {
	for range time.Tick(15 * time.Second) {
		n.mu.Lock()
		cfg := n.cfg
		type pending struct{ id string }
		var due []pending
		if cfg.OfflineOn {
			delay := time.Duration(cfg.OfflineDelay) * time.Second
			for id, st := range n.states {
				if !st.offlineSince.IsZero() && !st.offlineAlerted && time.Since(st.offlineSince) >= delay {
					st.offlineAlerted = true
					due = append(due, pending{id})
				}
			}
		}
		n.mu.Unlock()

		for _, p := range due {
			n.send(cfg, fmt.Sprintf("🔴 服务器离线\n%s 已离线超过 %d 秒", n.serverName(p.id), cfg.OfflineDelay))
		}
	}
}

/* ---------- Telegram 推送 ---------- */

var notifyHTTP = &http.Client{Timeout: 15 * time.Second}

// send 异步推送，失败仅记日志，不阻塞调用方。
func (n *Notifier) send(cfg notifyConfig, text string) {
	if cfg.TgToken == "" || cfg.TgChat == "" {
		return
	}
	go func() {
		if err := sendTelegram(cfg.TgToken, cfg.TgChat, text); err != nil {
			log.Printf("Telegram 推送失败: %v", err)
		}
	}()
}

func sendTelegram(token, chat, text string) error {
	api := "https://api.telegram.org/bot" + token + "/sendMessage"
	resp, err := notifyHTTP.PostForm(api, url.Values{
		"chat_id": {chat},
		"text":    {text},
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var e struct {
			Description string `json:"description"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Description)
	}
	return nil
}

/* ---------- 管理接口 ---------- */

func (s *App) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, loadNotifyConfig(s.db))
}

func (s *App) handlePutNotify(w http.ResponseWriter, r *http.Request) {
	var v notifyConfig
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	b2s := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	setSetting(s.db, "notify_tg_token", strings.TrimSpace(v.TgToken))
	setSetting(s.db, "notify_tg_chat", strings.TrimSpace(v.TgChat))
	setSetting(s.db, "notify_offline", b2s(v.OfflineOn))
	setSetting(s.db, "notify_offline_delay", strconv.Itoa(clampInt(v.OfflineDelay, 30, 3600, 60)))
	setSetting(s.db, "notify_load", b2s(v.LoadOn))
	setSetting(s.db, "notify_cpu", strconv.Itoa(clampInt(v.CPUThreshold, 1, 100, 90)))
	setSetting(s.db, "notify_mem", strconv.Itoa(clampInt(v.MemThreshold, 1, 100, 90)))
	setSetting(s.db, "notify_disk", strconv.Itoa(clampInt(v.DiskThreshold, 1, 100, 95)))
	setSetting(s.db, "notify_load_min", strconv.Itoa(clampInt(v.LoadMinutes, 1, 120, 5)))
	s.notifier.Reload()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	cfg := loadNotifyConfig(s.db)
	if cfg.TgToken == "" || cfg.TgChat == "" {
		writeErr(w, 400, "请先填写并保存 Bot Token 与 Chat ID")
		return
	}
	if err := sendTelegram(cfg.TgToken, cfg.TgChat, "✅ Moss 测试消息\n通知配置正常。"); err != nil {
		writeErr(w, 502, "发送失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
