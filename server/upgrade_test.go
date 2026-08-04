package main

import (
	"strings"
	"testing"
	"time"

	"moss/internal/protocol"
)

// 低于 2.0.0 的 agent 其 WS 分发 switch 没有 default 分支，upgrade 消息会被静默丢弃，
// 症状是干等到超时。判错这一条，用户点了按钮就是白等一场。
func TestAgentSupportsUpgrade(t *testing.T) {
	cases := map[string]bool{
		"2.0.0":        true,
		"v2.0.0":       true,
		"2.0.0-beta.2": true,
		"10.1.0":       true,
		"dev":          true, // 源码构建，必然含最新协议；本地端到端验证要靠它
		"1.4.0":        false,
		"v1.4.0":       false,
		"0.9.0":        false,
		"":             false, // 从未上报过版本，保守视为不支持
		"garbage":      false,
	}
	for v, want := range cases {
		if got := agentSupportsUpgrade(v); got != want {
			t.Errorf("agentSupportsUpgrade(%q) = %v, 期望 %v", v, got, want)
		}
	}
}

func TestUpgradeAvailability(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.2"

	// 旧 agent：必须拦在下发之前，并说清楚要手动升一次。
	ok, hint := upgradeAvailability("1.4.0", true)
	if ok {
		t.Error("v1.4.0 不认识升级指令，不该允许一键升级")
	}
	if !strings.Contains(hint, "手动") {
		t.Errorf("提示应说明需手动升级一次，实际: %q", hint)
	}

	// 已是目标版本：不可升级，且不该有提示——没什么好提示的。
	if ok, hint := upgradeAvailability("2.0.0-beta.2", true); ok || hint != "" {
		t.Errorf("已是最新时应无提示，实际 ok=%v hint=%q", ok, hint)
	}

	// 离线机器下发不出去。
	if ok, hint := upgradeAvailability("2.0.0-beta.1", false); ok || !strings.Contains(hint, "离线") {
		t.Errorf("离线应被拦下，实际 ok=%v hint=%q", ok, hint)
	}

	// 正常可升级。
	if ok, hint := upgradeAvailability("2.0.0-beta.1", true); !ok || hint != "" {
		t.Errorf("应允许升级，实际 ok=%v hint=%q", ok, hint)
	}

	// 开发态 server 没有对应的 release，钉上去只会 404。
	serverVersion = "dev"
	if ok, hint := upgradeAvailability("2.0.0-beta.1", true); ok || !strings.Contains(hint, "开发版本") {
		t.Errorf("开发态应拒绝，实际 ok=%v hint=%q", ok, hint)
	}
}

// Start 在校验不通过时绝不能下发。
func TestUpgradeStartRejectsOldAgent(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.2"

	app := mcpTestApp(t)
	err := app.upgrade.Start(app.hub, "srv", "1.4.0")
	if err == nil {
		t.Fatal("旧 agent 必须被拦下")
	}
	if !strings.Contains(err.Error(), "手动") {
		t.Errorf("错误信息应说明需手动升级，实际: %v", err)
	}
	// 被拦下的任务不该在状态表里留下痕迹，否则界面会显示一个从未发生的升级。
	if stage, _ := app.upgrade.Status("srv"); stage != "" {
		t.Errorf("未下发的任务不该有状态，实际: %q", stage)
	}
}

// 升级成功的唯一可靠判据：agent 重连并上报目标版本。
func TestUpgradeSuccessOnRegister(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage: protocol.UpgradeStageDownloading, Started: time.Now(), graceSec: 60,
	}

	// 替换之前的重连只是网络抖动，此时版本本来就还是旧的，不能据此判失败。
	m.OnRegister("srv", "2.0.0-beta.1")
	if stage, _ := m.Status("srv"); stage != protocol.UpgradeStageDownloading {
		t.Fatalf("替换前的重连不应改变状态，实际: %q", stage)
	}

	m.OnResult("srv", &protocol.UpgradeResult{ID: "j1", Stage: protocol.UpgradeStageReplaced})
	m.OnRegister("srv", "2.0.0-beta.2")
	if stage, errMsg := m.Status("srv"); stage != upgradeStageSuccess || errMsg != "" {
		t.Errorf("上报目标版本应判成功，实际 stage=%q err=%q", stage, errMsg)
	}
}

// 回滚后 agent 会带着旧版本号重新连上来——这时必须判失败，而不是等超时。
func TestUpgradeRollbackDetected(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage: protocol.UpgradeStageDownloading, Started: time.Now(), graceSec: 60,
	}
	m.OnResult("srv", &protocol.UpgradeResult{ID: "j1", Stage: protocol.UpgradeStageReplaced})
	m.OnRegister("srv", "2.0.0-beta.1")

	stage, errMsg := m.Status("srv")
	if stage != upgradeStageFailed {
		t.Fatalf("回滚应判失败，实际: %q", stage)
	}
	if !strings.Contains(errMsg, "回滚") {
		t.Errorf("失败原因应说明已回滚，实际: %q", errMsg)
	}
}

// agent 在替换前报错时，二进制未动、机器仍正常，应立刻判失败而不是干等超时。
func TestUpgradeFailsFastOnAgentError(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage: protocol.UpgradeStageDownloading, Started: time.Now(), graceSec: 60,
	}
	m.OnResult("srv", &protocol.UpgradeResult{ID: "j1", Stage: protocol.UpgradeStageDownloading, Error: "校验和不匹配，拒绝安装"})

	stage, errMsg := m.Status("srv")
	if stage != upgradeStageFailed || !strings.Contains(errMsg, "校验和") {
		t.Errorf("应立即判失败并带上原因，实际 stage=%q err=%q", stage, errMsg)
	}
}

// 升级过程中掉线是预期行为——替换后必然要重启。
// 把正常的重启窗口误报成失败，会让每次成功的升级都先闪一下红。
func TestUpgradeAgentGoneIsNotFailure(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage: protocol.UpgradeStageReplaced, Started: time.Now(),
		restartedAt: time.Now(), graceSec: 60,
	}
	m.OnAgentGone("srv")
	if stage, _ := m.Status("srv"); stage != protocol.UpgradeStageReplaced {
		t.Errorf("掉线不该改变升级状态，实际: %q", stage)
	}
}

// 机器彻底没连回来时要有兜底，否则状态永远停在「重启中」。
func TestUpgradeTimesOut(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage:       protocol.UpgradeStageRestarting,
		Started:     time.Now().Add(-30 * time.Minute),
		restartedAt: time.Now().Add(-30 * time.Minute),
		graceSec:    60,
	}
	stage, errMsg := m.Status("srv")
	if stage != upgradeStageFailed {
		t.Fatalf("超时应判失败，实际: %q", stage)
	}
	// 无法确认目标机实际状态时，措辞不能断言，只能如实说明。
	if !strings.Contains(errMsg, "确认") {
		t.Errorf("超时提示应引导人确认机器状态，实际: %q", errMsg)
	}
}

// 同一台机器同时只允许一个升级任务在途。
func TestUpgradeRejectsConcurrent(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.2"

	app := mcpTestApp(t)
	app.upgrade.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.2",
		Stage: protocol.UpgradeStageDownloading, Started: time.Now(), graceSec: 60,
	}
	err := app.upgrade.Start(app.hub, "srv", "2.0.0-beta.1")
	if err == nil || !strings.Contains(err.Error(), "已有升级任务") {
		t.Errorf("并发升级应被拒绝，实际: %v", err)
	}
}
