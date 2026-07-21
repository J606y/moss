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
	NetOn         bool   `json:"netOn"`
	NetThreshold  int    `json:"netThreshold"` // MB/s（1MB=1024KB，与面板展示同口径），上/下行任一方向
	NetSeconds    int    `json:"netSeconds"`   // 持续超阈值多少秒才告警
	ExpireOn      bool   `json:"expireOn"`
	ExpireDays    int    `json:"expireDays"` // 到期前几天提醒，1~7
}

func loadNotifyConfig(db *sql.DB) notifyConfig {
	return notifyConfig{
		TgToken:       getSetting(db, keyNotifyTgToken, ""),
		TgChat:        getSetting(db, keyNotifyTgChat, ""),
		OfflineOn:     getSetting(db, keyNotifyOffline, "0") == "1",
		OfflineDelay:  getSettingInt(db, keyNotifyOfflineDelay, 60),
		LoadOn:        getSetting(db, keyNotifyLoad, "0") == "1",
		CPUThreshold:  getSettingInt(db, keyNotifyCPU, 90),
		MemThreshold:  getSettingInt(db, keyNotifyMem, 90),
		DiskThreshold: getSettingInt(db, keyNotifyDisk, 95),
		LoadMinutes:   getSettingInt(db, keyNotifyLoadMin, 5),
		NetOn:         getSetting(db, keyNotifyNet, "0") == "1",
		NetThreshold:  getSettingInt(db, keyNotifyNetMB, 50),
		NetSeconds:    getSettingInt(db, keyNotifyNetSec, 60),
		ExpireOn:      getSetting(db, keyNotifyExpire, "0") == "1",
		ExpireDays:    getSettingInt(db, keyNotifyExpireDays, 3),
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

	// isOnline 从 Hub 取节点实时在线状态（main.go 注入），GCP 自动开机
	// 靠它而非 WS 断连事件判离线，面板重启后仍能守护已离线节点。
	isOnline func(id string) bool

	mu     sync.Mutex
	cfg    notifyConfig
	states map[string]*alertState

	gcpCfg   gcpConfig
	gcp      map[string]*gcpState
	gcpCli   *gcpClient // 懒建缓存，凭证变更（Reload/内容比对）后重建
	gcpSARaw string
}

func newNotifier(db *sql.DB) *Notifier {
	return &Notifier{
		db:       db,
		isOnline: func(string) bool { return false },
		cfg:      loadNotifyConfig(db),
		states:   make(map[string]*alertState),
		gcpCfg:   loadGCPConfig(db),
		gcp:      make(map[string]*gcpState),
	}
}

// Reload 设置变更后刷新配置缓存。
func (n *Notifier) Reload() {
	cfg := loadNotifyConfig(n.db)
	gcpCfg := loadGCPConfig(n.db)
	n.mu.Lock()
	n.cfg = cfg
	n.gcpCfg = gcpCfg
	n.gcpCli = nil
	n.gcpSARaw = ""
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
	delete(n.gcp, id) // 上线即重置自动开机计数/冷却/放弃标记
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

// OnReport 负载/网速告警：超阈值持续指定时长才告警，回落 10%（迟滞）后发恢复。
// netUp/netDown 单位 B/s。
func (n *Notifier) OnReport(id string, cpu, mem, disk, netUp, netDown float64) {
	n.mu.Lock()
	cfg := n.cfg
	if !cfg.LoadOn && !cfg.NetOn {
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
		} else if val < th*0.9 { // 按比例迟滞带（恢复回差），低阈值也可达，避免在阈值附近反复告警
			if st.highAlerted[metric] {
				st.highAlerted[metric] = false
				fires = append(fires, fire{fmt.Sprintf("✅ 负载恢复\n%%NAME%% %s 已回落至 %.1f%%", metric, val)})
			}
			delete(st.highSince, metric)
		}
	}
	if cfg.LoadOn {
		check("CPU", cpu, cfg.CPUThreshold)
		check("内存", mem, cfg.MemThreshold)
		check("硬盘", disk, cfg.DiskThreshold)
	}

	// 网速：上/下行任一方向超阈值即计时，独立的持续时长（秒），复用同一状态机与迟滞逻辑
	if cfg.NetOn && cfg.NetThreshold > 0 {
		const mb = 1024 * 1024 // 与面板展示同口径（1MB = 1024KB）
		th := float64(cfg.NetThreshold) * mb
		speed := netUp
		if netDown > speed {
			speed = netDown
		}
		if speed >= th {
			if st.highSince["net"].IsZero() {
				st.highSince["net"] = now
			}
			if !st.highAlerted["net"] && now.Sub(st.highSince["net"]) >= time.Duration(cfg.NetSeconds)*time.Second {
				st.highAlerted["net"] = true
				fires = append(fires, fire{fmt.Sprintf("⚠️ 网速告警\n%%NAME%% 上行 %.1f MB/s / 下行 %.1f MB/s，已持续 %d 秒（阈值 %d MB/s）",
					netUp/mb, netDown/mb, cfg.NetSeconds, cfg.NetThreshold)})
			}
		} else if speed < th*0.9 {
			if st.highAlerted["net"] {
				st.highAlerted["net"] = false
				fires = append(fires, fire{fmt.Sprintf("✅ 网速恢复\n%%NAME%% 网速已回落至 ↑ %.1f / ↓ %.1f MB/s", netUp/mb, netDown/mb)})
			}
			delete(st.highSince, "net")
		}
	}
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
	delete(n.gcp, id)
	n.mu.Unlock()
}

// Run 离线检查循环：离线超过 OfflineDelay 且未告警 → 推送。
func (n *Notifier) Run() {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for range t.C {
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

		if cfg.ExpireOn {
			n.checkExpiry(cfg)
		}

		n.checkGCPStart()
	}
}

// checkExpiry 到期提醒：到期日在 ExpireDays 天内且尚未就该日期提醒过的服务器推送通知。
// 已提醒的到期日持久化在 servers.expire_notified（防重启重发）；改到期时间后按新日期重新提醒。
func (n *Notifier) checkExpiry(cfg notifyConfig) {
	rows, err := n.db.Query(
		`SELECT id, name, expire_at FROM servers WHERE expire_at != '' AND expire_at != expire_notified`)
	if err != nil {
		log.Printf("checkExpiry query: %v", err)
		return
	}
	defer rows.Close()
	type due struct {
		id, name, expireAt string
		daysLeft           int
	}
	var dues []due
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.id, &d.name, &d.expireAt); err != nil {
			continue
		}
		exp, err := time.ParseInLocation("2006-01-02", d.expireAt, time.Local)
		if err != nil {
			continue // 非 YYYY-MM-DD 格式无法判定，跳过
		}
		d.daysLeft = int(exp.Sub(today).Hours() / 24)
		if d.daysLeft < 0 || d.daysLeft > cfg.ExpireDays {
			continue
		}
		dues = append(dues, d)
	}
	for _, d := range dues {
		// 先落标记再推送：宁可推送失败漏一条（有日志），不因发送阻塞/失败反复轰炸
		if _, err := n.db.Exec(`UPDATE servers SET expire_notified = ? WHERE id = ?`, d.expireAt, d.id); err != nil {
			log.Printf("checkExpiry mark (id=%s): %v", d.id, err)
			continue
		}
		left := fmt.Sprintf("剩余 %d 天", d.daysLeft)
		if d.daysLeft == 0 {
			left = "今天到期"
		}
		n.send(cfg, fmt.Sprintf("📅 到期提醒\n%s 将于 %s 到期（%s）", d.name, d.expireAt, left))
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
	setSetting(s.db, keyNotifyTgToken, strings.TrimSpace(v.TgToken))
	setSetting(s.db, keyNotifyTgChat, strings.TrimSpace(v.TgChat))
	setSetting(s.db, keyNotifyOffline, b2s(v.OfflineOn))
	setSetting(s.db, keyNotifyOfflineDelay, strconv.Itoa(clampInt(v.OfflineDelay, 30, 3600, 60)))
	setSetting(s.db, keyNotifyLoad, b2s(v.LoadOn))
	setSetting(s.db, keyNotifyCPU, strconv.Itoa(clampInt(v.CPUThreshold, 1, 100, 90)))
	setSetting(s.db, keyNotifyMem, strconv.Itoa(clampInt(v.MemThreshold, 1, 100, 90)))
	setSetting(s.db, keyNotifyDisk, strconv.Itoa(clampInt(v.DiskThreshold, 1, 100, 95)))
	setSetting(s.db, keyNotifyLoadMin, strconv.Itoa(clampInt(v.LoadMinutes, 1, 120, 5)))
	setSetting(s.db, keyNotifyNet, b2s(v.NetOn))
	setSetting(s.db, keyNotifyNetMB, strconv.Itoa(clampInt(v.NetThreshold, 1, 100000, 50)))
	setSetting(s.db, keyNotifyNetSec, strconv.Itoa(clampInt(v.NetSeconds, 10, 3600, 60)))
	setSetting(s.db, keyNotifyExpire, b2s(v.ExpireOn))
	setSetting(s.db, keyNotifyExpireDays, strconv.Itoa(clampInt(v.ExpireDays, 1, 7, 3)))
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
