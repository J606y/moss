// 通用 webhook 告警通道：把告警事件以结构化 JSON 推给外部 AI 网关，原生附带 HMAC-SHA256 签名与防抖控制。
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// 告警事件类型。命名用 `<对象>.<事件>`，便于接收端按前缀路由。
const (
	evtServerOnline   = "server.online"
	evtServerOffline  = "server.offline"
	evtLoadAlert      = "server.load_alert"
	evtLoadRecovered  = "server.load_recovered"
	evtNetAlert       = "server.net_alert"
	evtNetRecovered   = "server.net_recovered"
	evtServerExpiring = "server.expiring"
	evtCommandBlocked = "exec.blocked"
	// GCP 守护。这几条此前直接调 send 走 Telegram、绕过了 fire，
	// 于是只配了 webhook 的用户完全收不到——而「Spot 被抢占后开机失败」
	// 恰恰是这条链路最该覆盖的场景。
	evtGCPStarting  = "gcp.autostart"
	evtGCPFailed    = "gcp.autostart_failed"
	evtGCPGaveUp    = "gcp.autostart_gaveup"
	evtGCPRunningNC = "gcp.running_not_connected"
)

// 指标码。与事件类型同为对外契约，一并定在这里。
//
// 必须是常量，因为这个字符串身兼三职：告警状态机的 key（st.highSince[metric]）、
// webhook 载荷的 metric 字段、以及文案里 {metric} 的查表依据。写字面量时
// 改一个字，状态机就与 clearMetricState 对不上——而编译器完全不管。
//
// 取值与 MCP 的 get_history 一致（historyMetricList），对端只需认一套词。
const (
	metricCPU  = "cpu"
	metricMem  = "mem"
	metricDisk = "disk"
	metricNet  = "net"
)

// defaultWebhookDebounceSec 默认告警防抖时间窗（秒）：同一节点、同一事件类型与指标在窗口内不重复轰炸。
const defaultWebhookDebounceSec = 30

func redactURLErr(err error) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err.Error()
	}
	safe := "(地址已隐去)"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		// 只留 scheme://host，path 与 query 一概不要——凭证可能在两者中任意一处
		safe = u.Scheme + "://" + u.Host + "/…"
	}
	inner := "未知错误"
	if ue.Err != nil {
		inner = ue.Err.Error()
	}
	return fmt.Sprintf("%s %s: %s", ue.Op, safe, inner)
}

// alertEvent 是推送给外部的告警载荷，同时也是 Telegram 文案的载体。
//
// Text 与结构化字段并存：人看 Text，AI 看结构化字段。让 AI 去解析中文告警文案
// 是脆弱的——文案一改，接收端就崩。
//
// Text 由 fire 在出口按站点语言渲染，告警产生处只填 textKey 与 params，
// 不拼人话（见 alert_text.go）。
type alertEvent struct {
	Type       string  `json:"type"`
	ServerID   string  `json:"serverId,omitempty"`
	ServerName string  `json:"serverName,omitempty"`
	Text       string  `json:"text"`
	Metric     string  `json:"metric,omitempty"`    // metricCPU / metricMem / metricDisk / metricNet
	Value      float64 `json:"value,omitempty"`     // 触发时的实测值
	Threshold  float64 `json:"threshold,omitempty"` // 配置的阈值
	Timestamp  int64   `json:"timestamp"`           // 秒级

	// textKey 选用哪条文案。为空即取 Type；同一 Type 下需要分措辞时才填
	// （见 alert_text.go 的 txt* 常量）。
	//
	// textKey 与 params 都不导出，因此不进 JSON——接收端该读的是上面那些
	// 结构化字段，模板参数是渲染的内部细节，暴露出去只会变成新的隐式契约。
	textKey string
	// params 渲染 Text 用的参数，语言中立：机器名、数字、时长、错误原文。
	params map[string]string
}

// webhookConfig 独立于 notifyConfig：webhook 与 Telegram 是两条可各自开关的通道。
type webhookConfig struct {
	URL         string `json:"url"`
	Secret      string `json:"secret"`
	On          bool   `json:"on"`
	DebounceSec int    `json:"debounceSec"` // 同一告警事件的推送防抖窗口（秒），默认 30
}

func loadWebhookConfig(db *sql.DB) webhookConfig {
	deb := defaultWebhookDebounceSec
	if raw := strings.TrimSpace(getSetting(db, keyWebhookDebounce, "")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			deb = n
		}
	}
	return webhookConfig{
		URL:         getSetting(db, keyWebhookURL, ""),
		Secret:      decryptSecret(getSetting(db, keyWebhookSecret, "")),
		On:          getSetting(db, keyWebhookOn, "0") == "1",
		DebounceSec: deb,
	}
}

// signWebhookHMAC 计算带时间戳防重放的 HMAC-SHA256 签名：HMAC_SHA256(secret, "<timestamp>.<body>")。
func signWebhookHMAC(secret string, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// signWebhookBodyHMAC 计算纯请求体的 HMAC-SHA256 签名：HMAC_SHA256(secret, body)。
func signWebhookBodyHMAC(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// verifyWebhookHMAC 校验 X-Moss-Signature（"sha256=<hex>"）或 X-Moss-Signature-256。
func verifyWebhookHMAC(secret, sigHeader string, timestamp int64, body []byte) bool {
	if secret == "" || !strings.HasPrefix(sigHeader, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(sigHeader, "sha256="))
	if err != nil {
		return false
	}
	wantTS, _ := hex.DecodeString(signWebhookHMAC(secret, timestamp, body))
	if hmac.Equal(got, wantTS) {
		return true
	}
	wantBody, _ := hex.DecodeString(signWebhookBodyHMAC(secret, body))
	return hmac.Equal(got, wantBody)
}

var webhookHTTP = &http.Client{Timeout: 10 * time.Second}

// sendWebhook 异步推送。失败只记日志：告警通道不可用不应拖垮监控本身。
//
// 不做重试队列。告警是时效性信息——一条 5 分钟前投递失败的「CPU 过高」
// 重发到现在已无意义，而重试队列会在对端长时间不可用时无限堆积。
// 真正需要不丢的记录在审计表里，不在这里。
func (n *Notifier) sendWebhook(cfg webhookConfig, ev alertEvent) {
	if !cfg.On || strings.TrimSpace(cfg.URL) == "" {
		return
	}
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().Unix()
	}
	body, err := json.Marshal(ev)
	if err != nil {
		log.Printf("webhook 载荷序列化失败: %v", err)
		return
	}
	ts := ev.Timestamp
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
		if err != nil {
			log.Printf("构造 webhook 请求失败: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "moss/"+serverVersion)
		req.Header.Set("X-Moss-Event", ev.Type)
		req.Header.Set("X-Moss-Timestamp", strconv.FormatInt(ts, 10))
		if cfg.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Secret)
			req.Header.Set("X-Moss-Signature", "sha256="+signWebhookHMAC(cfg.Secret, ts, body))
			req.Header.Set("X-Moss-Signature-256", "sha256="+signWebhookBodyHMAC(cfg.Secret, body))
		}
		resp, err := webhookHTTP.Do(req)
		if err != nil {
			log.Printf("webhook 推送失败: %s", redactURLErr(err))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("webhook 返回异常状态: %d", resp.StatusCode)
		}
	}()
}

/* ---------- 管理接口 ---------- */

func (s *App) handleGetWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := loadWebhookConfig(s.db)
	// Secret 不回传明文，只告知是否已配置——与 GCP 凭证的处理方式一致。
	writeJSON(w, 200, map[string]any{
		"url":         cfg.URL,
		"on":          cfg.On,
		"secretSet":   cfg.Secret != "",
		"debounceSec": cfg.DebounceSec,
	})
}

func (s *App) handlePutWebhook(w http.ResponseWriter, r *http.Request) {
	var f struct {
		URL         string `json:"url"`
		Secret      string `json:"secret"`
		On          bool   `json:"on"`
		ClearSecret bool   `json:"clearSecret"`
		DebounceSec *int   `json:"debounceSec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, errBadParams)
		return
	}
	u := strings.TrimSpace(f.URL)
	if f.On && u == "" {
		writeErr(w, errWebhookURLRequired)
		return
	}
	if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		writeErr(w, errWebhookBadScheme)
		return
	}
	if err := setSetting(s.db, keyWebhookURL, u); err != nil {
		log.Printf("保存 webhook 地址失败: %v", err)
		writeErr(w, errInternal)
		return
	}
	// 留空表示不改动已有密钥，避免前端因不回传明文而在保存时意外清空
	if f.ClearSecret {
		setSetting(s.db, keyWebhookSecret, "")
	} else if sec := strings.TrimSpace(f.Secret); sec != "" {
		// 加密落库：这把密钥是对端 webhook 的 Bearer 凭证，明文存等于随库泄漏。
		enc, err := encryptSecret(sec)
		if err != nil {
			log.Printf("加密 webhook 密钥失败: %v", err)
			writeErr(w, errWebhookEncrypt)
			return
		}
		setSetting(s.db, keyWebhookSecret, enc)
	}
	if f.DebounceSec != nil {
		deb := *f.DebounceSec
		if deb < 0 {
			deb = 0
		}
		if deb > 3600 {
			deb = 3600
		}
		setSetting(s.db, keyWebhookDebounce, strconv.Itoa(deb))
	}
	on := "0"
	if f.On {
		on = "1"
	}
	if err := setSetting(s.db, keyWebhookOn, on); err != nil {
		log.Printf("保存 webhook 开关失败: %v", err)
		writeErr(w, errInternal)
		return
	}
	s.notifier.Reload()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleTestWebhook 发一条测试事件，验证对端能收到。
func (s *App) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := loadWebhookConfig(s.db)
	if strings.TrimSpace(cfg.URL) == "" {
		writeErr(w, errWebhookNotConfigured)
		return
	}
	// 测试推送忽略开关：用户点「测试」时就是想验证连通性，
	// 若因未启用而静默不发，会误以为对端有问题。
	cfg.On = true
	s.notifier.sendWebhook(cfg, alertEvent{
		Type:      "test",
		Text:      "Moss webhook 测试消息，配置正常。",
		Timestamp: time.Now().Unix(),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
