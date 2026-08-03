package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// webhookSink 是一个假的接收端，用来断言 moss 真的把事件推了出去。
type webhookSink struct {
	srv  *httptest.Server
	recv chan alertEvent
	auth chan string
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	s := &webhookSink{
		recv: make(chan alertEvent, 8),
		auth: make(chan string, 8),
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev alertEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("webhook 载荷不是合法 JSON: %v", err)
		}
		s.auth <- r.Header.Get("Authorization")
		s.recv <- ev
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// wait 等待一条事件。推送是异步的（不阻塞告警主流程），必须等而不能立即断言。
func (s *webhookSink) wait(t *testing.T) alertEvent {
	t.Helper()
	select {
	case ev := <-s.recv:
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("等待 webhook 推送超时")
		return alertEvent{}
	}
}

func webhookTestNotifier(t *testing.T, url, secret string, on bool) *Notifier {
	t.Helper()
	db := testDB(t)
	setSetting(db, keyWebhookURL, url)
	setSetting(db, keyWebhookSecret, secret)
	if on {
		setSetting(db, keyWebhookOn, "1")
	}
	return newNotifier(db)
}

func TestWebhookDeliversEvent(t *testing.T) {
	sink := newWebhookSink(t)
	n := webhookTestNotifier(t, sink.srv.URL, "", true)

	n.fire(notifyConfig{}, alertEvent{
		Type:       evtServerOffline,
		ServerID:   "srv1",
		ServerName: "hk-01",
		Text:       "🔴 服务器离线",
	})

	ev := sink.wait(t)
	if ev.Type != evtServerOffline {
		t.Errorf("事件类型应为 %q，实际 %q", evtServerOffline, ev.Type)
	}
	if ev.ServerID != "srv1" || ev.ServerName != "hk-01" {
		t.Errorf("机器标识未正确传递: %+v", ev)
	}
	// 时间戳由 fire 自动补齐，接收端据此判断事件新鲜度
	if ev.Timestamp == 0 {
		t.Error("时间戳应被自动填充")
	}
}

// TestWebhookCarriesStructuredMetrics 验证负载告警带上指标与阈值。
// 接收端应当读结构化字段而非解析中文文案——文案一改对端就崩。
func TestWebhookCarriesStructuredMetrics(t *testing.T) {
	sink := newWebhookSink(t)
	n := webhookTestNotifier(t, sink.srv.URL, "", true)

	n.fire(notifyConfig{}, alertEvent{
		Type:       evtLoadAlert,
		ServerID:   "srv1",
		ServerName: "hk-01",
		Text:       "⚠️ 负载告警\nhk-01 CPU 使用率 95.0%",
		Metric:     "CPU",
		Value:      95,
		Threshold:  80,
	})

	ev := sink.wait(t)
	if ev.Metric != "CPU" || ev.Value != 95 || ev.Threshold != 80 {
		t.Errorf("指标信息未结构化传递: metric=%q value=%v threshold=%v", ev.Metric, ev.Value, ev.Threshold)
	}
}

func TestWebhookSendsBearerSecret(t *testing.T) {
	sink := newWebhookSink(t)
	n := webhookTestNotifier(t, sink.srv.URL, "s3cret", true)

	n.fire(notifyConfig{}, alertEvent{Type: evtServerOnline, Text: "上线"})
	sink.wait(t)

	select {
	case got := <-sink.auth:
		if got != "Bearer s3cret" {
			t.Errorf("鉴权头应为 %q，实际 %q", "Bearer s3cret", got)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到鉴权头")
	}
}

func TestWebhookSkippedWhenDisabled(t *testing.T) {
	sink := newWebhookSink(t)
	n := webhookTestNotifier(t, sink.srv.URL, "", false) // 开关关闭

	n.fire(notifyConfig{}, alertEvent{Type: evtServerOffline, Text: "离线"})

	select {
	case ev := <-sink.recv:
		t.Fatalf("开关关闭时不应推送，却收到 %+v", ev)
	case <-time.After(600 * time.Millisecond):
		// 预期：什么都没发生
	}
}

func TestWebhookSkippedWhenURLEmpty(t *testing.T) {
	n := webhookTestNotifier(t, "", "", true)
	// 只要不 panic 即通过：地址为空时应静默跳过，而不是构造出一个非法请求
	n.fire(notifyConfig{}, alertEvent{Type: evtServerOffline, Text: "离线"})
	time.Sleep(300 * time.Millisecond)
}

// TestWebhookFailureDoesNotBlock 验证对端不可用不会拖垮告警主流程。
// 监控系统的告警通道故障绝不能反过来影响监控本身。
func TestWebhookFailureDoesNotBlock(t *testing.T) {
	// 指向一个必定连不通的地址
	n := webhookTestNotifier(t, "http://127.0.0.1:1/nope", "", true)

	done := make(chan struct{})
	go func() {
		n.fire(notifyConfig{}, alertEvent{Type: evtServerOffline, Text: "离线"})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook 投递失败时阻塞了告警主流程")
	}
}

func TestWebhookReloadPicksUpConfig(t *testing.T) {
	sink := newWebhookSink(t)
	db := testDB(t)
	n := newNotifier(db) // 初始未配置

	setSetting(db, keyWebhookURL, sink.srv.URL)
	setSetting(db, keyWebhookOn, "1")
	n.Reload()

	n.fire(notifyConfig{}, alertEvent{Type: evtServerOnline, Text: "上线"})
	if ev := sink.wait(t); ev.Type != evtServerOnline {
		t.Errorf("Reload 后应使用新配置，实际收到 %+v", ev)
	}
}
