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
	evtGCPStarting    = "gcp.autostart"
	evtGCPFailed      = "gcp.autostart_failed"
	evtGCPGaveUp      = "gcp.autostart_gaveup"
	evtGCPRunningNC   = "gcp.running_not_connected"
)

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
		safe = u.Scheme + "://" + u.Host + "/…"
	}
	inner := "未知错误"
	if ue.Err != nil {
		inner = ue.Err.Error()
	}
	return fmt.Sprintf("%s %s: %s", ue.Op, safe, inner)
}

type alertEvent struct {
	Type       string  `json:"type"`
	ServerID   string  `json:"serverId,omitempty"`
	ServerName string  `json:"serverName,omitempty"`
	Text       string  `json:"text"`
	Metric     string  `json:"metric,omitempty"`
	Value      float64 `json:"value,omitempty"`
	Threshold  float64 `json:"threshold,omitempty"`
	Timestamp  int64   `json:"timestamp"`

	textKey string
	params  map[string]string
}

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
	if f.ClearSecret {
		setSetting(s.db, keyWebhookSecret, "")
	} else if sec := strings.TrimSpace(f.Secret); sec != "" {
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

func (s *App) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := loadWebhookConfig(s.db)
	if strings.TrimSpace(cfg.URL) == "" {
		writeErr(w, errWebhookNotConfigured)
		return
	}
	cfg.On = true
	s.notifier.sendWebhook(cfg, alertEvent{
		Type:      "test",
		Text:      "Moss webhook 测试消息，配置正常。",
		Timestamp: time.Now().Unix(),
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}
