package main

import (
	"testing"
	"time"
)

// offlineTestNotifier 造一个只开了离线告警的 Notifier，OfflineDelay 为 60 秒。
func offlineTestNotifier(t *testing.T, ids ...string) *Notifier {
	t.Helper()
	db := testDB(t)
	setSetting(db, keyNotifyOffline, "1")
	setSetting(db, keyNotifyOfflineDelay, "60")
	for _, id := range ids {
		if _, err := db.Exec(`INSERT INTO servers(id, name, token, created_at) VALUES(?, ?, ?, ?)`,
			id, "机器"+id, "tok-"+id, time.Now().Unix()); err != nil {
			t.Fatalf("插入测试服务器失败: %v", err)
		}
	}
	n := newNotifier(db)
	n.isOnline = func(string) bool { return false }
	return n
}

// dueOffline 复刻 Run() 的判定，返回本轮应当告警的机器。
// 不直接跑 Run()：它是 15 秒一拍的常驻循环，测试等不起。
func dueOffline(n *Notifier) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	cfg := n.cfg
	var due []string
	if !cfg.OfflineOn {
		return nil
	}
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
		due = append(due, id)
	}
	return due
}

// TestSeedOfflineStatesAlertsAfterRestart 是这条修复的核心用例。
//
// 此前 states 的条目只能由 WS 连接事件创建，面板重启后一台已经死掉的机器
// 从未在新进程里注册过，Run() 遍历不到它 ——「🔴 离线」永远不会发，
// 它日后恢复上线时「🟢 恢复」也不会发。一台机器就这样彻底静默。
func TestSeedOfflineStatesAlertsAfterRestart(t *testing.T) {
	n := offlineTestNotifier(t, "s1", "s2")

	// 面板刚起来：库里有两台机器，都还没连上来
	started := time.Now()
	n.SeedOfflineStates(started)

	if len(n.states) != 2 {
		t.Fatalf("应为库里两台机器都播种状态，实际 %d 条", len(n.states))
	}
	// 刚启动，还在重连窗口内，不该告警
	if due := dueOffline(n); len(due) != 0 {
		t.Fatalf("启动后的重连窗口内不该告警，却要发: %v", due)
	}

	// 把计时起点拨到 OfflineDelay 之前，模拟窗口已过、机器仍未连上
	n.mu.Lock()
	for _, st := range n.states {
		st.offlineSince = started.Add(-90 * time.Second)
	}
	n.mu.Unlock()

	due := dueOffline(n)
	if len(due) != 2 {
		t.Fatalf("重连窗口过后仍未上线的机器必须告警，实际只有 %d 台", len(due))
	}
	// 只告警一次，不重复刷
	if again := dueOffline(n); len(again) != 0 {
		t.Fatalf("同一次离线不应重复告警，又发了: %v", again)
	}
}

// TestSeedOfflineStatesGivesReconnectWindow 面板停机远长于 OfflineDelay 也不该刷屏。
//
// 计时起点取进程启动时间而非 servers.last_seen 就是为了这个：用 last_seen 的话，
// 面板自己停机一周后重启，所有机器一上来就越过 OfflineDelay，一口气补发一堆告警。
func TestSeedOfflineStatesGivesReconnectWindow(t *testing.T) {
	n := offlineTestNotifier(t, "s1", "s2", "s3")

	// 面板停机很久后才重启，但计时从此刻开始
	n.SeedOfflineStates(time.Now())

	if due := dueOffline(n); len(due) != 0 {
		t.Fatalf("面板停机多久都不该在启动瞬间补发告警，却要发 %d 条: %v", len(due), due)
	}

	// 窗口内连上来的机器，计时被清掉，永远不会告警
	n.OnOnline("s1")
	n.mu.Lock()
	st1 := n.states["s1"]
	zeroed := st1.offlineSince.IsZero()
	n.mu.Unlock()
	if !zeroed {
		t.Fatal("重连后应清掉离线计时")
	}
}

// TestSeedOfflineStatesDoesNotClobberLiveState 播种不能覆盖已经连上来的机器。
//
// 播种发生在启动流程里，理论上此刻还没有 agent 连上；但真要有连接抢在前面，
// 覆盖它的 offlineSince 会让一台正在线的机器凭空收到离线告警。
func TestSeedOfflineStatesDoesNotClobberLiveState(t *testing.T) {
	n := offlineTestNotifier(t, "s1")

	n.OnOnline("s1") // 抢在播种之前连上来
	n.SeedOfflineStates(time.Now())

	n.mu.Lock()
	st := n.states["s1"]
	since := st.offlineSince
	n.mu.Unlock()
	if !since.IsZero() {
		t.Fatalf("已在线的机器不应被播种成离线，offlineSince=%v", since)
	}
}

// TestOfflineAlertDefersToHubState 陈旧的 offlineSince 不该变成假告警。
//
// offlineSince 是事件驱动写上去的，可能因连接事件乱序而失真（旧连接的
// OnOffline 跑在新连接的 OnOnline 之后）。只认 offlineSince 就会推一条假的
// 「🔴 离线」，并一直挂着，直到下次重连再补一条同样假的「🟢 恢复」。
func TestOfflineAlertDefersToHubState(t *testing.T) {
	n := offlineTestNotifier(t, "s1")
	n.isOnline = func(string) bool { return true } // hub 说这台机器好好的

	n.SeedOfflineStates(time.Now().Add(-90 * time.Second))

	if due := dueOffline(n); len(due) != 0 {
		t.Fatalf("hub 说在线时不该发离线告警，却要发: %v", due)
	}
	n.mu.Lock()
	since := n.states["s1"].offlineSince
	n.mu.Unlock()
	if !since.IsZero() {
		t.Fatal("确认在线后应清掉陈旧的离线计时，否则它会一直卡在那里")
	}
}

// TestReloadClearsStaleAlertState 改配置必须清掉受影响指标的告警态。
//
// 不清的话会留下一个 stale 的 highAlerted：网速告警已触发 → 用户关掉网速告警
// → 过一阵再打开，此时 highAlerted["net"] 仍是 true，**新的超阈值不会再告警**；
// 而等网速跌破回差线并满 RecoverSec，又会凭空发出一条「✅ 网速恢复」
// ——对应的告警用户从未收到过。改阈值同理：留下的告警态是按旧阈值算的。
func TestReloadClearsStaleAlertState(t *testing.T) {
	db := testDB(t)
	setSetting(db, keyNotifyNet, "1")
	setSetting(db, keyNotifyNetMB, "50")
	setSetting(db, keyNotifyLoad, "1")
	setSetting(db, keyNotifyCPU, "90")
	n := newNotifier(db)

	// 造出「已经告警过」的状态
	n.mu.Lock()
	st := n.state("s1")
	st.highAlerted["net"] = true
	st.highAlerted["CPU"] = true
	n.mu.Unlock()

	// 关掉网速告警再重载
	setSetting(db, keyNotifyNet, "0")
	n.Reload()

	n.mu.Lock()
	netStale := n.states["s1"].highAlerted["net"]
	cpuKept := n.states["s1"].highAlerted["CPU"]
	n.mu.Unlock()
	if netStale {
		t.Error("关掉网速告警后应清掉它的告警态，否则重新打开时新的超阈值不会告警")
	}
	if !cpuKept {
		t.Error("没被改动的指标不应被牵连清掉，否则一次无关的设置保存会重发告警")
	}

	// 改阈值同样要清
	setSetting(db, keyNotifyCPU, "70")
	n.Reload()
	n.mu.Lock()
	cpuStale := n.states["s1"].highAlerted["CPU"]
	n.mu.Unlock()
	if cpuStale {
		t.Error("阈值改动后应清掉告警态：旧状态是按旧阈值算出来的")
	}
}
