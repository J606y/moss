//go:build windows

package main

import "errors"

// Windows 的自升级路径与 Unix 完全不同，本版（v2.0.0-beta.2）不实现，
// 目标机仍走安装脚本手动升级。差异在于：
//
//   - 运行中的 .exe 有文件锁，不能像 Linux 那样直接 rename 覆盖（inode 语义不存在），
//     必须先停计划任务、把旧文件改名、再放入新文件；
//   - 没有 systemd，重启走 Stop-ScheduledTask / Start-ScheduledTask，
//     而计划任务的停止是异步的，需要轮询确认进程真的退出；
//   - 没有 setsid，脱离进程树要用 CREATE_NEW_PROCESS_GROUP + DETACHED_PROCESS。
//
// 更关键的是 agent 在真 Windows Server 上的执行路径至今零真机验证，
// 把两件都没验证过的事叠在一起风险不可控，故拆到后续版本单独做。

var errUpgradeUnsupported = errors.New("Windows 暂不支持一键升级，请用安装脚本手动升级")

func spawnRollbackGuard(backup, target string, grace int) error {
	return errUpgradeUnsupported
}

// 与 Unix 侧一致用变量，便于测试替换（见 upgrade_unix.go 的说明）。
var restartAgentService = func() error {
	return errUpgradeUnsupported
}

func upgradeSupported() error {
	return errUpgradeUnsupported
}
