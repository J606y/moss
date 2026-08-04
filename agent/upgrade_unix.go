//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// spawnRollbackGuard 用旧二进制拉起回滚守护，并把它放进独立的 systemd transient unit。
//
// **必须用 systemd-run，Setsid 不够。** 守护接下来要 systemctl restart moss-agent，
// 而 systemd 停止一个 service 时默认 KillMode=control-group——它杀的是该 unit
// cgroup 内的**所有**进程。Setsid 换的是 session 不是 cgroup，守护仍留在
// moss-agent.service 的 cgroup 里，会被连同旧 agent 一起杀掉：
// 二进制已换新、服务没起来、也没有任何东西执行回滚，机器直接失联。
// 这个失效只会在真机升级时暴露，而那正是最不该失效的时刻。
//
// 不指定 --unit 名：让 systemd 自动生成，避免上一次残留的同名单元导致启动失败。
// --collect 让单元退出后自我清理，不在 systemctl 列表里留下 failed 记录。
func spawnRollbackGuard(backup, target string, grace int) error {
	systemdRun, err := exec.LookPath("systemd-run")
	if err != nil {
		return errors.New("未找到 systemd-run，无法让回滚守护脱离 agent 的 cgroup")
	}
	cmd := exec.Command(systemdRun,
		"--collect",
		"--description", "moss-agent 升级回滚守护",
		backup,
		"--rollback-guard",
		"--guard-target", target,
		"--guard-grace", strconv.Itoa(grace),
	)
	// systemd-run 本身秒退（它只是把单元交给 PID 1），可以安全等待它的退出码：
	// 拿不到这个退出码就无法区分「守护已托管」与「压根没起来」。
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run 启动守护失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
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
	// 回滚守护靠它脱离 cgroup。缺了它整条链路的安全网就是空的，
	// 与其替换完才发现没人能回滚，不如在下载之前就拒绝。
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("未找到 systemd-run，回滚守护无法脱离 agent 的 cgroup")
	}
	if os.Geteuid() != 0 {
		return errors.New("自升级需要 root 权限")
	}
	return nil
}
