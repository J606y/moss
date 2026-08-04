//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// spawnRollbackGuard 用旧二进制拉起回滚守护，并让它脱离本进程的会话与进程组。
//
// Setsid 是这整套设计成立的前提：守护接下来要 systemctl restart moss-agent，
// 而重启会终止 agent 所在的整个进程组。守护若仍在组内，会连同被杀在半路——
// 二进制已经换成新的、服务却没起来、也没有任何东西执行回滚，机器直接失联。
//
// 用 syscall.SysProcAttr 而不是调用 setsid(1)：后者来自 util-linux，
// 精简镜像里未必存在，而升级链路不能依赖一个可能缺失的外部命令。
func spawnRollbackGuard(backup, target string, grace int) error {
	cmd := exec.Command(backup,
		"--rollback-guard",
		"--guard-target", target,
		"--guard-grace", strconv.Itoa(grace),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// 守护的输出走它自己的日志文件（logGuard），这里断开继承来的标准流，
	// 避免它持有 agent 的管道导致重启时产生僵持。
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return err
	}
	// 不 Wait：守护要活得比本进程久。释放 Process 句柄即可，
	// 本进程马上会被 systemd 重启掉，不存在僵尸进程堆积问题。
	return cmd.Process.Release()
}

// restartAgentService 重启 agent 服务。
//
// 只支持 systemd：安装脚本在 Linux 上固定注册 moss-agent.service，
// 而自升级按钮本版也只对 Linux 开放。非 systemd 环境如实报错，
// 让升级在替换前就失败并恢复备份，而不是替换完才发现重启不了。
// 用变量而非函数：回滚守护的判断逻辑必须能在开发机上测到，
// 而测试不可能真去重启一个 systemd 服务。
var restartAgentService = func() error {
	systemctl, err := exec.LookPath("systemctl")
	if err != nil {
		return errors.New("未找到 systemctl，自升级目前只支持 systemd 环境")
	}
	out, err := exec.Command(systemctl, "restart", "moss-agent").CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart moss-agent: %w (%s)", err, string(out))
	}
	return nil
}

// upgradeSupported 平台是否支持自升级。
func upgradeSupported() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("未找到 systemctl，自升级目前只支持 systemd 环境")
	}
	if os.Geteuid() != 0 {
		return errors.New("自升级需要 root 权限")
	}
	return nil
}
