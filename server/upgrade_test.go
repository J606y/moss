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
		"2.0.0-rc.1":   true, // rc > beta
		"2.0.1":        true,
		"10.1.0":       true,
		"dev":          true, // 源码构建，必然含最新协议；本地端到端验证要靠它
		// beta.1 的主版本也是 2，只比主版本会把它误判为支持。
		// upgrade 消息是 beta.2 才加的，beta.1 收到同样静默丢弃。
		"2.0.0-beta.1":  false,
		"2.0.0-alpha.9": false,
		"1.4.0":         false,
		"v1.4.0":        false,
		"0.9.0":         false,
		"":              false, // 从未上报过版本，保守视为不支持
		"garbage":       false,
	}
	for v, want := range cases {
		if got := agentSupportsUpgrade(v); got != want {
			t.Errorf("agentSupportsUpgrade(%q) = %v, 期望 %v", v, got, want)
		}
	}
}

// semver 的排序规则里最容易写错的就是预发布段，而按钮能不能点全靠它。
func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.0.0", "2.0.0", 0},
		{"2.0.1", "2.0.0", 1},
		{"2.1.0", "2.0.9", 1},
		{"3.0.0", "2.9.9", 1},
		{"1.4.0", "2.0.0", -1},
		// 预发布版排在同号正式版之前
		{"2.0.0-beta.1", "2.0.0", -1},
		{"2.0.0", "2.0.0-beta.1", 1},
		// 预发布版之间按段比较，数字段按数值而非字典序
		{"2.0.0-beta.1", "2.0.0-beta.2", -1},
		{"2.0.0-beta.9", "2.0.0-beta.10", -1}, // 字典序会判反
		{"2.0.0-alpha.1", "2.0.0-beta.1", -1},
		{"2.0.0-rc.1", "2.0.0-beta.9", 1},
		{"2.0.0-beta", "2.0.0-beta.1", -1}, // 段数少的在前
		// 构建元数据不参与比较
		{"2.0.0+build1", "2.0.0", 0},
	}
	for _, c := range cases {
		if got := compareSemver(c.a, c.b); got != c.want {
			t.Errorf("compareSemver(%q, %q) = %d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpgradeAvailability(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.3"

	const linux = "Debian 13"

	// 旧 agent：必须拦在下发之前，并说清楚要手动升一次。
	ok, hint := upgradeAvailability("1.4.0", linux, true)
	if ok {
		t.Error("v1.4.0 不认识升级指令，不该允许一键升级")
	}
	if !strings.Contains(hint, "手动") {
		t.Errorf("提示应说明需手动升级一次，实际: %q", hint)
	}

	// beta.1 的主版本也是 2，但 upgrade 消息是 beta.2 才加的，它同样收不到。
	// 这条曾经判反过：按钮显示为可点，点下去只会静默丢弃、干等超时。
	if ok, hint := upgradeAvailability("2.0.0-beta.1", linux, true); ok || !strings.Contains(hint, "手动") {
		t.Errorf("beta.1 不认识升级指令，应提示手动升级，实际 ok=%v hint=%q", ok, hint)
	}

	// 已是目标版本：不可升级，且不该有提示——没什么好提示的。
	if ok, hint := upgradeAvailability("2.0.0-beta.3", linux, true); ok || hint != "" {
		t.Errorf("已是最新时应无提示，实际 ok=%v hint=%q", ok, hint)
	}

	// 已是最新的 Windows / macOS 机器同样不该有提示：平台判定排在版本判定之后，
	// 否则一台早已跟上版本的机器会常年挂着「不支持自动升级」的提示。
	for _, osName := range []string{"Microsoft Windows Server 2022 Datacenter", "Darwin 15"} {
		if ok, hint := upgradeAvailability("2.0.0-beta.3", osName, true); ok || hint != "" {
			t.Errorf("%q 已是最新时不该有提示，实际 ok=%v hint=%q", osName, ok, hint)
		}
	}

	// 离线机器下发不出去。
	if ok, hint := upgradeAvailability("2.0.0-beta.2", linux, false); ok || !strings.Contains(hint, "离线") {
		t.Errorf("离线应被拦下，实际 ok=%v hint=%q", ok, hint)
	}

	// 正常可升级：agent 认识升级指令、在线、且版本落后。
	if ok, hint := upgradeAvailability("2.0.0-beta.2", linux, true); !ok || hint != "" {
		t.Errorf("应允许升级，实际 ok=%v hint=%q", ok, hint)
	}

	// 系统名从未上报（旧 agent / 刚建档）：不该替用户下结论说不支持，
	// 交给版本与在线判定，此处应正常放行。
	if ok, hint := upgradeAvailability("2.0.0-beta.2", "", true); !ok || hint != "" {
		t.Errorf("系统名未知不该拦，实际 ok=%v hint=%q", ok, hint)
	}

	// 开发态 server 没有对应的 release，钉上去只会 404。
	serverVersion = "dev"
	if ok, hint := upgradeAvailability("2.0.0-beta.2", linux, true); ok || !strings.Contains(hint, "开发版本") {
		t.Errorf("开发态应拒绝，实际 ok=%v hint=%q", ok, hint)
	}
}

// Windows 与 macOS 的 agent 都没有自升级能力：
// Windows 侧 upgradeSupported() 恒返回错误（agent/upgrade_windows.go）；
// macOS 走的是 upgrade_unix.go（构建约束 !windows 包含 darwin），但它要求
// systemctl 与 systemd-run，而 macOS 没有 systemd，必然失败。
// 两者下发过去都只会失败，拦在 server 侧是让用户不必点一次才知道。
func TestUpgradeAvailabilityUnsupportedOS(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.3"

	// 库里存的是 gopsutil 上报的人类可读名，没有固定格式，故用子串匹配。
	// macOS 取 kern.ostype 小写再拼主版本号，实际形如 "Darwin 15"，不是 "macOS"——
	// 这条判错会让所有 Mac 都以为能一键升级。
	for _, osName := range []string{
		"Microsoft Windows Server 2022 Datacenter 21H2",
		"Windows 11 Pro",
		"windows", // 大小写不敏感
		"Darwin 15",
		"Darwin 24.0",
		"macOS 15.5", // 上报格式若变化也要认
	} {
		ok, hint := upgradeAvailability("2.0.0-beta.2", osName, true)
		if ok {
			t.Errorf("%q 应被判为不可自动升级", osName)
		}
		if hint != upgradeOSUnsupportedHint {
			t.Errorf("%q 的提示应为统一文案，实际: %q", osName, hint)
		}
	}

	// 平台判定必须排在在线判定之前：这类机器等它上线也没用，
	// 显示「机器离线」会让用户白等一场，再点一次仍然失败。
	for _, osName := range []string{"Windows 11 Pro", "Darwin 15"} {
		if _, hint := upgradeAvailability("2.0.0-beta.2", osName, false); hint != upgradeOSUnsupportedHint {
			t.Errorf("离线的 %q 应先报平台不支持，实际: %q", osName, hint)
		}
	}

	// Linux 发行版名不能被误伤——它们是唯一真正支持自升级的平台。
	for _, osName := range []string{"Debian 13", "Ubuntu 24.04", "Alpine Linux v3.20", "Rocky Linux 9", ""} {
		if upgradeOSUnsupported(osName) {
			t.Errorf("%q 不该被判为不支持自动升级", osName)
		}
	}
}

// Start 在校验不通过时绝不能下发。
func TestUpgradeStartRejectsOldAgent(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.2"

	app := mcpTestApp(t)
	err := app.upgrade.Start(app.hub, "srv", "1.4.0", "Debian 13")
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

// Windows / macOS 机器即便版本够新也必须拦在下发之前：任务发过去 agent 也只会报错，
// 白白留下一条失败记录。
func TestUpgradeStartRejectsUnsupportedOS(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.3"

	for _, osName := range []string{"Microsoft Windows Server 2022 Datacenter", "Darwin 15"} {
		app := mcpTestApp(t)
		err := app.upgrade.Start(app.hub, "srv", "2.0.0-beta.2", osName)
		if err == nil {
			t.Fatalf("%q 必须被拦下", osName)
		}
		if err.Error() != upgradeOSUnsupportedHint {
			t.Errorf("%q 的错误信息应为统一文案，实际: %v", osName, err)
		}
		if stage, _ := app.upgrade.Status("srv"); stage != "" {
			t.Errorf("%q 未下发的任务不该有状态，实际: %q", osName, stage)
		}
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

// 升级失败后人工把版本修上去了，红色的失败标记必须自行消失，
// 否则界面会一直挂着一个已经不存在的问题。
func TestUpgradeFailureClearedAfterManualFix(t *testing.T) {
	m := newUpgradeManager()
	m.jobs["srv"] = &upgradeJob{
		ID: "j1", ServerID: "srv", Target: "v2.0.0-beta.3",
		Stage: upgradeStageFailed, Err: "新版本未能连回",
		Started: time.Now(), restartedAt: time.Now(), graceSec: 60,
	}

	// 版本仍是旧的：问题还在，标记必须保留。
	m.OnRegister("srv", "2.0.0-beta.2")
	if stage, _ := m.Status("srv"); stage != upgradeStageFailed {
		t.Fatalf("问题未解决时应保留失败标记，实际: %q", stage)
	}

	// 人工升上去了：标记应当消失。
	m.OnRegister("srv", "2.0.0-beta.3")
	if stage, errMsg := m.Status("srv"); stage != "" || errMsg != "" {
		t.Errorf("人工修复后失败标记应清除，实际 stage=%q err=%q", stage, errMsg)
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
	err := app.upgrade.Start(app.hub, "srv", "2.0.0-beta.1", "Debian 13")
	if err == nil || !strings.Contains(err.Error(), "已有升级任务") {
		t.Errorf("并发升级应被拒绝，实际: %v", err)
	}
}
