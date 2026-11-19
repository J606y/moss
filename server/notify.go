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

type notifyConfig struct {
	TgToken       string `json:"tgToken"`
	TgChat        string `json:"tgChat"`
	OfflineOn     bool   `json:"offlineOn"`
	OfflineDelay  int    `json:"offlineDelay"`
	LoadOn        bool   `json:"loadOn"`
	CPUThreshold  int    `json:"cpuThreshold"`
	MemThreshold  int    `json:"memThreshold"`
	DiskThreshold int    `json:"diskThreshold"`
	LoadMinutes   int    `json:"loadMinutes"`
	NetOn         bool   `json:"netOn"`
	NetThreshold  int    `json:"netThreshold"`
	NetSeconds    int    `json:"netSeconds"`
	RecoverSec    int    `json:"recoverSec"`
	ExpireOn      bool   `json:"expireOn"`
	ExpireDays    int    `json:"expireDays"`
}

func loadNotifyConfig(db *sql.DB) notifyConfig {
	return notifyConfig{
		TgToken:       decryptSecret(getSetting(db, keyNotifyTgToken, "")),
		TgChat:        getSetting(db, keyNotifyTgChat, ""),
		OfflineOn:     getSetting(db, keyNotifyOffline, "0") == "1",
		OfflineDelay:  getSettingInt(db, keyNotifyOfflineDelay, 60),
		LoadOn:        getSetting(db, keyNotifyLoad, "0") == "1",
		CPUThreshold:  getSettingInt(db, keyNotifyCPU, 90),
		MemThreshold:  getSettingInt(db, keyNotifyMem, 90),
		DiskThreshold: getSettingInt(db, keyNotifyDisk, 95),
		LoadMinutes:   getSettingInt(db, keyNotifyLoadMin, 5),
		RecoverSec:    getSettingInt(db, keyNotifyRecoverSec, 60),
		NetOn:         getSetting(db, keyNotifyNet, "0") == "1",
		NetThreshold:  getSettingInt(db, keyNotifyNetMB, 50),
		NetSeconds:    getSettingInt(db, keyNotifyNetSec, 60),
		ExpireOn:      getSetting(db, keyNotifyExpire, "0") == "1",
		ExpireDays:    getSettingInt(db, keyNotifyExpireDays, 3),
	}
}

type alertState struct {
	offlineSince   time.Time
	offlineAlerted bool
	highSince      map[string]time.Time
	highAlerted    map[string]bool
	lowSince       map[string]time.Time
}

type Notifier struct {
	db       *sql.DB
	isOnline func(id string) bool

	mu     sync.Mutex
	cfg    notifyConfig
	states map[string]*alertState

	gcpCfg     gcpConfig
	gcp        map[string]*gcpState
	gcpClients map[string]*gcpCachedClient

	webhook   webhookConfig
	lastFired map[string]time.Time // 告警推送防抖表：eventKey → 最近一次推送时刻
}

type gcpCachedClient struct {
	raw string
	cli *gcpClient
}

// alertDedupKey 生成单条告警事件的防抖键（按事件类型 + 节点 + 指标区分）。
func alertDedupKey(ev alertEvent) string {
	target := ev.ServerID
	if target == "" {
		target = ev.ServerName
	}
	return ev.Type + "|" + target + "|" + ev.Metric
}

// shouldDebounce 判断该告警事件是否落在防抖冷却窗口内；若是恢复事件，顺带清除对应告警事件的冷却记录。
func (n *Notifier) shouldDebounce(ev alertEvent) (webhookConfig, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	wh := n.webhook
	if n.lastFired == nil {
		n.lastFired = make(map[string]time.Time)
	}
	target := ev.ServerID
	if target == "" {
		target = ev.ServerName
	}
	// 恢复事件到达时清除对应故障告警的防抖记录，确保下一次真实故障能立即告警。
	switch ev.Type {
	case evtServerOnline:
		delete(n.lastFired, evtServerOffline+"|"+target+"|")
	case evtLoadRecovered:
		delete(n.lastFired, evtLoadAlert+"|"+target+"|"+ev.Metric)
	case evtNetRecovered:
		delete(n.lastFired, evtNetAlert+"|"+target+"|"+ev.Metric)
	}
	if ev.Type == "test" || wh.DebounceSec <= 0 {
		return wh, false
	}
	key := alertDedupKey(ev)
	now := time.Now()
	window := time.Duration(wh.DebounceSec) * time.Second
	if last, ok := n.lastFired[key]; ok && now.Sub(last) < window {
		return wh, true
	}
	n.lastFired[key] = now
	return wh, false
}

// fire 是所有告警的唯一出口：一份事件同时走 Telegram 与 webhook，并在出口做原生防抖抑制。
func (n *Notifier) fire(cfg notifyConfig, ev alertEvent) {
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().Unix()
	}
	wh, suppressed := n.shouldDebounce(ev)
	if suppressed {
		return
	}

	ev.Text = renderAlert(ev, n.alertLang())
	n.send(cfg, ev.Text)
	n.sendWebhook(wh, ev)
}

func (n *Notifier) alertLang() string {
	if normLang(getSetting(n.db, keyLang, langAuto)) == langEN {
		return langEN
	}
	return langZH
}

func newNotifier(db *sql.DB) *Notifier {
	return &Notifier{
		db:         db,
		isOnline:   func(string) bool { return false },
		cfg:        loadNotifyConfig(db),
		states:     make(map[string]*alertState),
		gcpCfg:     loadGCPConfig(db),
		gcp:        make(map[string]*gcpState),
		gcpClients: make(map[string]*gcpCachedClient),
		webhook:    loadWebhookConfig(db),
		lastFired:  make(map[string]time.Time),
	}
}

func (n *Notifier) Reload() {
	cfg := loadNotifyConfig(n.db)
	gcpCfg := loadGCPConfig(n.db)
	wh := loadWebhookConfig(n.db)
	n.mu.Lock()
	old := n.cfg
	n.cfg = cfg
	n.gcpCfg = gcpCfg
	n.webhook = wh
	n.gcpClients = make(map[string]*gcpCachedClient)

	if !cfg.LoadOn || cfg.CPUThreshold != old.CPUThreshold {
		n.clearMetricState(metricCPU)
	}
	if !cfg.LoadOn || cfg.MemThreshold != old.MemThreshold {
		n.clearMetricState(metricMem)
	}
	if !cfg.LoadOn || cfg.DiskThreshold != old.DiskThreshold {
		n.clearMetricState(metricDisk)
	}
	if !cfg.NetOn || cfg.NetThreshold != old.NetThreshold {
		n.clearMetricState(metricNet)
	}
	n.mu.Unlock()
}

func (n *Notifier) clearMetricState(metric string) {
	for _, st := range n.states {
		delete(st.highSince, metric)
		delete(st.highAlerted, metric)
		delete(st.lowSince, metric)
	}
}

func (n *Notifier) state(id string) *alertState {
	st, ok := n.states[id]
	if !ok {
		st = &alertState{
			highSince:   make(map[string]time.Time),
			highAlerted: make(map[string]bool),
			lowSince:    make(map[string]time.Time),
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
	delete(n.gcp, id)
	cfg := n.cfg
	n.mu.Unlock()

	if wasAlerted && cfg.OfflineOn {
		dur := time.Since(downSince).Round(time.Second)
		name := n.serverName(id)
		n.fire(cfg, alertEvent{
			Type:       evtServerOnline,
			ServerID:   id,
			ServerName: name,
			params:     map[string]string{"name": name, "dur": dur.String()},
		})
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

func (n *Notifier) OnReport(id string, cpu, mem, disk, netUp, netDown float64) {
	n.mu.Lock()
	cfg := n.cfg
	if !cfg.LoadOn && !cfg.NetOn {
		n.mu.Unlock()
		return
	}
	st := n.state(id)
	type fire struct {
		typ    string
		met    string
		val    float64
		thres  float64
		params map[string]string
	}
	var fires []fire
	now := time.Now()
	hold := time.Duration(cfg.LoadMinutes) * time.Minute
	recoverHold := time.Duration(cfg.RecoverSec) * time.Second

	check := func(metric string, val float64, threshold int) {
		th := float64(threshold)
		if th <= 0 {
			return
		}
		switch {
		case val >= th:
			delete(st.lowSince, metric)
			if st.highSince[metric].IsZero() {
				st.highSince[metric] = now
			}
			if !st.highAlerted[metric] && now.Sub(st.highSince[metric]) >= hold {
				st.highAlerted[metric] = true
				fires = append(fires, fire{
					typ: evtLoadAlert, met: metric, val: val, thres: th,
					params: map[string]string{
						"val": fmt.Sprintf("%.1f", val),
						"min": strconv.Itoa(cfg.LoadMinutes),
						"th":  strconv.Itoa(threshold),
					},
				})
			}
		case val < th*0.9:
			delete(st.highSince, metric)
			if st.lowSince[metric].IsZero() {
				st.lowSince[metric] = now
			}
			if st.highAlerted[metric] && now.Sub(st.lowSince[metric]) >= recoverHold {
				st.highAlerted[metric] = false
				delete(st.lowSince, metric)
				fires = append(fires, fire{
					typ: evtLoadRecovered, met: metric, val: val, thres: th,
					params: map[string]string{
						"val": fmt.Sprintf("%.1f", val),
						"sec": strconv.Itoa(cfg.RecoverSec),
					},
				})
			}
		}
	}
	if cfg.LoadOn {
		check(metricCPU, cpu, cfg.CPUThreshold)
		check(metricMem, mem, cfg.MemThreshold)
		check(metricDisk, disk, cfg.DiskThreshold)
	}

	if cfg.NetOn && cfg.NetThreshold > 0 {
		const mb = 1024 * 1024
		th := float64(cfg.NetThreshold) * mb
		speed := netUp
		if netDown > speed {
			speed = netDown
		}
		up := fmt.Sprintf("%.1f", netUp/mb)
		down := fmt.Sprintf("%.1f", netDown/mb)
		switch {
		case speed >= th:
			delete(st.lowSince, metricNet)
			if st.highSince[metricNet].IsZero() {
				st.highSince[metricNet] = now
			}
			if !st.highAlerted[metricNet] && now.Sub(st.highSince[metricNet]) >= time.Duration(cfg.NetSeconds)*time.Second {
				st.highAlerted[metricNet] = true
				fires = append(fires, fire{
					typ: evtNetAlert, met: metricNet, val: speed, thres: th,
					params: map[string]string{
						"up": up, "down": down,
						"sec": strconv.Itoa(cfg.NetSeconds),
						"th":  strconv.Itoa(cfg.NetThreshold),
					},
				})
			}
		case speed < th*0.9:
			delete(st.highSince, metricNet)
			if st.lowSince[metricNet].IsZero() {
				st.lowSince[metricNet] = now
			}
			if st.highAlerted[metricNet] && now.Sub(st.lowSince[metricNet]) >= recoverHold {
				st.highAlerted[metricNet] = false
				delete(st.lowSince, metricNet)
				fires = append(fires, fire{
					typ: evtNetRecovered, met: metricNet, val: speed, thres: th,
					params: map[string]string{
						"up": up, "down": down,
						"sec": strconv.Itoa(cfg.RecoverSec),
					},
				})
			}
		}
	}
	n.mu.Unlock()

	if len(fires) > 0 {
		name := n.serverName(id)
		for _, f := range fires {
			f.params["name"] = name
			n.fire(cfg, alertEvent{
				Type:       f.typ,
				ServerID:   id,
				ServerName: name,
				Metric:     f.met,
				Value:      f.val,
				Threshold:  f.thres,
				params:     f.params,
			})
		}
	}
}

func (n *Notifier) Forget(id string) {
	n.mu.Lock()
	delete(n.states, id)
	delete(n.gcp, id)
	n.mu.Unlock()
}

func (n *Notifier) SeedOfflineStates(startedAt time.Time) {
	rows, err := n.db.Query(`SELECT id FROM servers`)
	if err != nil {
		log.Printf("播种离线告警状态失败，本次重启后已离线的机器将不会告警: %v", err)
		return
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		log.Printf("播种离线告警状态时读取中断: %v", err)
	}

	n.mu.Lock()
	for _, id := range ids {
		if _, exists := n.states[id]; exists {
			continue
		}
		n.state(id).offlineSince = startedAt
	}
	n.mu.Unlock()
}

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
				if st.offlineSince.IsZero() || st.offlineAlerted || time.Since(st.offlineSince) < delay {
					continue
				}
				if n.isOnline(id) {
					st.offlineSince = time.Time{}
					continue
				}
				st.offlineAlerted = true
				due = append(due, pending{id})
			}
		}
		n.mu.Unlock()

		for _, p := range due {
			name := n.serverName(p.id)
			n.fire(cfg, alertEvent{
				Type:       evtServerOffline,
				ServerID:   p.id,
				ServerName: name,
				params:     map[string]string{"name": name, "sec": strconv.Itoa(cfg.OfflineDelay)},
			})
		}

		if cfg.ExpireOn {
			n.checkExpiry(cfg)
		}

		n.checkGCPStart()
	}
}

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
			continue
		}
		d.daysLeft = int(exp.Sub(today).Hours() / 24)
		if d.daysLeft < 0 || d.daysLeft > cfg.ExpireDays {
			continue
		}
		dues = append(dues, d)
	}
	for _, d := range dues {
		if _, err := n.db.Exec(`UPDATE servers SET expire_notified = ? WHERE id = ?`, d.expireAt, d.id); err != nil {
			log.Printf("checkExpiry mark (id=%s): %v", d.id, err)
			continue
		}
		key := ""
		if d.daysLeft == 0 {
			key = txtExpiringToday
		}
		n.fire(cfg, alertEvent{
			Type:       evtServerExpiring,
			ServerID:   d.id,
			ServerName: d.name,
			textKey:    key,
			params: map[string]string{
				"name": d.name,
				"date": d.expireAt,
				"days": strconv.Itoa(d.daysLeft),
			},
		})
	}
}

func (n *Notifier) NotifyBlocked(serverName, caller, cmd, reason string) {
	n.mu.Lock()
	cfg := n.cfg
	n.mu.Unlock()

	if len([]rune(cmd)) > 300 {
		cmd = string([]rune(cmd)[:300]) + "…"
	}
	n.fire(cfg, alertEvent{
		Type:       evtCommandBlocked,
		ServerName: serverName,
		params: map[string]string{
			"name":   serverName,
			"caller": caller,
			"reason": reason,
			"cmd":    cmd,
		},
	})
}

/* ---------- Telegram 推送 ---------- */

var notifyHTTP = &http.Client{Timeout: 15 * time.Second}

func (n *Notifier) send(cfg notifyConfig, text string) {
	if cfg.TgToken == "" || cfg.TgChat == "" {
		return
	}
	go func() {
		if err := sendTelegram(cfg.TgToken, cfg.TgChat, text); err != nil {
			log.Printf("Telegram 推送失败: %s", redactURLErr(err))
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
		writeErr(w, errBadJSON)
		return
	}
	b2s := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	tgToken, err := encryptSecret(strings.TrimSpace(v.TgToken))
	if err != nil {
		writeErr(w, errNotifyEncrypt)
		return
	}
	setSetting(s.db, keyNotifyTgToken, tgToken)
	setSetting(s.db, keyNotifyTgChat, strings.TrimSpace(v.TgChat))
	setSetting(s.db, keyNotifyOffline, b2s(v.OfflineOn))
	setSetting(s.db, keyNotifyOfflineDelay, strconv.Itoa(clampInt(v.OfflineDelay, 30, 3600, 60)))
	setSetting(s.db, keyNotifyLoad, b2s(v.LoadOn))
	setSetting(s.db, keyNotifyCPU, strconv.Itoa(clampInt(v.CPUThreshold, 1, 100, 90)))
	setSetting(s.db, keyNotifyMem, strconv.Itoa(clampInt(v.MemThreshold, 1, 100, 90)))
	setSetting(s.db, keyNotifyDisk, strconv.Itoa(clampInt(v.DiskThreshold, 1, 100, 95)))
	setSetting(s.db, keyNotifyLoadMin, strconv.Itoa(clampInt(v.LoadMinutes, 1, 120, 5)))
	setSetting(s.db, keyNotifyRecoverSec, strconv.Itoa(clampInt(v.RecoverSec, 10, 3600, 60)))
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
		writeErr(w, errNotifyNotConfigured)
		return
	}
	if err := sendTelegram(cfg.TgToken, cfg.TgChat, "✅ Moss 测试消息\n通知配置正常。"); err != nil {
		writeErr(w, errNotifySendFailed.with(redactURLErr(err)))
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
