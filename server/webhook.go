// 通用 webhook 告警通道：把告警事件以结构化 JSON 推给外部 AI 网关。
//
// 这是「人不在场」链路的起点。MCP 是客户端拉取模型，服务端无法主动叫醒 AI——
// 靠 OpenClaw 定时轮询 get_metrics 不可行（延迟高，且绝大多数轮询什么也没发生，纯烧 token）。
// 所以链路必须双向：出事时 moss 用 webhook 推，AI 醒来后再用 MCP 拉数据、下命令。
//
// 做成通用 webhook 而非「OpenClaw 专用推送」：载荷是标准 JSON，任何网关都能接。
// 绑死单一产品会在换工具时全部推倒重来。
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
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
)

// alertEvent 是推送给外部的告警载荷，同时也是 Telegram 文案的载体。
//
// Text 与结构化字段并存：人看 Text，AI 看结构化字段。让 AI 去解析中文告警文案
// 是脆弱的——文案一改，接收端就崩。
type alertEvent struct {
	Type       string  `json:"type"`
	ServerID   string  `json:"serverId,omitempty"`
	ServerName string  `json:"serverName,omitempty"`
	Text       string  `json:"text"`
	Metric     string  `json:"metric,omitempty"`    // CPU / 内存 / 硬盘 / net
	Value      float64 `json:"value,omitempty"`     // 触发时的实测值
	Threshold  float64 `json:"threshold,omitempty"` // 配置的阈值
	Timestamp  int64   `json:"timestamp"`           // 秒级
}

// webhookConfig 独立于 notifyConfig：webhook 与 Telegram 是两条可各自开关的通道。
type webhookConfig struct {
	URL string `json:"url"`
	// Secret 若非空，以 `Authorization: Bearer <secret>` 发送。
	// 多数 AI 网关（含 OpenClaw）用这种方式鉴权。
	Secret string `json:"secret"`
	On     bool   `json:"on"`
}

func loadWebhookConfig(db *sql.DB) webhookConfig {
	return webhookConfig{
		URL:    getSetting(db, keyWebhookURL, ""),
		Secret: getSetting(db, keyWebhookSecret, ""),
		On:     getSetting(db, keyWebhookOn, "0") == "1",
	}
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
	body, err := json.Marshal(ev)
	if err != nil {
		log.Printf("webhook 载荷序列化失败: %v", err)
		return
	}
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
		if cfg.Secret != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Secret)
		}
		resp, err := webhookHTTP.Do(req)
		if err != nil {
			log.Printf("webhook 推送失败: %v", err)
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
		"url":       cfg.URL,
		"on":        cfg.On,
		"secretSet": cfg.Secret != "",
	})
}

func (s *App) handlePutWebhook(w http.ResponseWriter, r *http.Request) {
	var f struct {
		URL    string `json:"url"`
		Secret string `json:"secret"`
		On     bool   `json:"on"`
		// ClearSecret 为 true 时清空已存的密钥（留空 Secret 表示「不修改」）
		ClearSecret bool `json:"clearSecret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	url := strings.TrimSpace(f.URL)
	if f.On && url == "" {
		writeErr(w, 400, "启用 webhook 需要填写地址")
		return
	}
	if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		writeErr(w, 400, "地址需以 http:// 或 https:// 开头")
		return
	}
	if err := setSetting(s.db, keyWebhookURL, url); err != nil {
		log.Printf("保存 webhook 地址失败: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	// 留空表示不改动已有密钥，避免前端因不回传明文而在保存时意外清空
	if f.ClearSecret {
		setSetting(s.db, keyWebhookSecret, "")
	} else if strings.TrimSpace(f.Secret) != "" {
		setSetting(s.db, keyWebhookSecret, strings.TrimSpace(f.Secret))
	}
	on := "0"
	if f.On {
		on = "1"
	}
	if err := setSetting(s.db, keyWebhookOn, on); err != nil {
		log.Printf("保存 webhook 开关失败: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	s.notifier.Reload()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleTestWebhook 发一条测试事件，验证对端能收到。
func (s *App) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	cfg := loadWebhookConfig(s.db)
	if strings.TrimSpace(cfg.URL) == "" {
		writeErr(w, 400, "请先填写并保存 webhook 地址")
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
