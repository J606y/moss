package main

import (
	"testing"
	"time"
)

// recoverTestNotifier 造一个开了负载告警、接了假 webhook 的 Notifier。
// 阈值 90、告警需持续 5 分钟、恢复需持续 60 秒。
func recoverTestNotifier(t *testing.T, sink *webhookSink) *Notifier {
	t.Helper()
	db := testDB(t)
	setSetting(db, keyWebhookURL, sink.srv.URL)
	setSetting(db, keyWebhookOn, "1")
	setSetting(db, keyNotifyLoad, "1")
	setSetting(db, keyNotifyCPU, "90")
	setSetting(db, keyNotifyLoadMin, "5")
	setSetting(db, keyNotifyRecoverSec, "60")
	n := newNotifier(db)
	n.isOnline = func(string) bool { return true }
	return n
}

// backdate 把某个指标的计时起点往前拨，模拟「已经持续了这么久」。
// OnReport 内部用 time.Now()，不这样做就只能真的等下去。
func backdate(n *Notifier, id, metric string, highAgo, lowAgo time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st := n.state(id)
	if highAgo > 0 {
		st.highSince[metric] = time.Now().Add(-highAgo)
	}
	if lowAgo > 0 {
		st.lowSince[metric] = time.Now().Add(-lowAgo)
	}
}

func alerted(n *Notifier, id, metric string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state(id).highAlerted[metric]
}

// 恢复必须等满确认时长。只靠回差带挡不住大幅波动——
// 阈值 90 时 95↔75 来回摆，75 已低于回差线 81，没有时间确认就会立刻发恢复，
// 于是「告警→恢复→告警」交替刷屏。
func TestLoadRecoverRequiresSustainedLow(t *testing.T) {
	sink := newWebhookSink(t)
	n := recoverTestNotifier(t, sink)

	// 先制造一次告警：超阈值并已持续满 5 分钟
	n.OnReport("srv1", 95, 0, 0, 0, 0)
	backdate(n, "srv1", "CPU", 6*time.Minute, 0)
	n.OnReport("srv1", 95, 0, 0, 0, 0)
	if ev := sink.wait(t); ev.Type != evtLoadAlert {
		t.Fatalf("应先触发负载告警，实际 %q", ev.Type)
	}

	// 掉到回差线以下，但只掉了一瞬间——不该发恢复
	n.OnReport("srv1", 75, 0, 0, 0, 0)
	select {
	case ev := <-sink.recv:
		t.Fatalf("低于回差线不足确认时长就发了恢复: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}
	if !alerted(n, "srv1", "CPU") {
		t.Error("确认时长未满，告警状态不该被清除")
	}

	// 又冲回高位：低位计时必须清零，否则下次一掉下来就会立刻恢复
	n.OnReport("srv1", 95, 0, 0, 0, 0)
	n.mu.Lock()
	_, stillLow := n.state("srv1").lowSince["CPU"]
	n.mu.Unlock()
	if stillLow {
		t.Error("重回高位后低位计时应清零")
	}

	// 这次踏踏实实低了 60 秒以上
	n.OnReport("srv1", 75, 0, 0, 0, 0)
	backdate(n, "srv1", "CPU", 0, 90*time.Second)
	n.OnReport("srv1", 75, 0, 0, 0, 0)

	ev := sink.wait(t)
	if ev.Type != evtLoadRecovered {
		t.Fatalf("持续低于回差线后应发恢复，实际 %q", ev.Type)
	}
	if alerted(n, "srv1", "CPU") {
		t.Error("恢复后告警状态应被清除")
	}
}

// 回差带内（81 ~ 90）既不算高也不算低，两侧计时都不该被动。
// 若在带内清掉 highSince，指标在阈值附近游走时告警计时会被反复重置，永远告不出来。
func TestHysteresisBandFreezesBothTimers(t *testing.T) {
	sink := newWebhookSink(t)
	n := recoverTestNotifier(t, sink)

	n.OnReport("srv1", 95, 0, 0, 0, 0) // 进入高位，highSince 起算
	n.mu.Lock()
	highBefore := n.state("srv1").highSince["CPU"]
	n.mu.Unlock()

	n.OnReport("srv1", 85, 0, 0, 0, 0) // 落进回差带

	n.mu.Lock()
	st := n.state("srv1")
	highAfter := st.highSince["CPU"]
	_, hasLow := st.lowSince["CPU"]
	n.mu.Unlock()

	if !highAfter.Equal(highBefore) {
		t.Error("回差带内不应重置高位计时")
	}
	if hasLow {
		t.Error("回差带内不应开始低位计时——那还不算「已回落」")
	}
}

// 恢复确认对网速告警同样生效，两者共用一个设置。
func TestNetRecoverRequiresSustainedLow(t *testing.T) {
	sink := newWebhookSink(t)
	db := testDB(t)
	setSetting(db, keyWebhookURL, sink.srv.URL)
	setSetting(db, keyWebhookOn, "1")
	setSetting(db, keyNotifyNet, "1")
	setSetting(db, keyNotifyNetMB, "50")
	setSetting(db, keyNotifyNetSec, "60")
	setSetting(db, keyNotifyRecoverSec, "60")
	n := newNotifier(db)
	n.isOnline = func(string) bool { return true }

	const mb = 1024 * 1024
	n.OnReport("srv1", 0, 0, 0, 80*mb, 0)
	backdate(n, "srv1", "net", 90*time.Second, 0)
	n.OnReport("srv1", 0, 0, 0, 80*mb, 0)
	if ev := sink.wait(t); ev.Type != evtNetAlert {
		t.Fatalf("应先触发网速告警，实际 %q", ev.Type)
	}

	// 掉下来一瞬间不算
	n.OnReport("srv1", 0, 0, 0, 10*mb, 0)
	select {
	case ev := <-sink.recv:
		t.Fatalf("网速刚掉下来就发了恢复: %+v", ev)
	case <-time.After(300 * time.Millisecond):
	}

	backdate(n, "srv1", "net", 0, 90*time.Second)
	n.OnReport("srv1", 0, 0, 0, 10*mb, 0)
	if ev := sink.wait(t); ev.Type != evtNetRecovered {
		t.Fatalf("持续低速后应发恢复，实际 %q", ev.Type)
	}
}
