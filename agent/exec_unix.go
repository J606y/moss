//go:build !windows

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// ---- 为什么需要 cgroup，而不只是进程组 ----
//
// 进程组信号收不走 setsid 出去的子孙：它们换了新的会话与进程组，
// `kill(-pgid)` 够不到。这类进程会一直攥着命令的 stdout 管道写端不放，
// 让 agent 读不到 EOF——排空宽限（见 exec.go 的 execDrainGrace）能保证 agent
// 自己不被拖死，但那些进程仍然在目标机上跑着，超时终止形同虚设。
//
// cgroup 是内核里唯一「进程无法自行退出」的归属关系。把命令放进一条 transient
// scope，杀 cgroup 就能连 setsid 的子孙一起收走。
//
// 代价与回退：systemd-run 每条命令多一个进程和约几十毫秒开销，且它并非处处可用
// （容器里没有 systemd、非 root 且无 polkit 授权都会失败）。所以先探测再决定，
// 探不通就回退到原来的进程组隔离——宁可少杀，也不能让 exec 整体不可用。

var (
	systemdRunOnce sync.Once
	systemdRunOK   bool
)

// systemdRunUsable 探测 systemd-run --scope 是否真的能用，只探一次。
//
// **默认关闭，需 MOSS_EXEC_CGROUP=1 显式开启。**
//
// 关着不是因为它不对，而是因为它验证不了：探测在非 root 或无 polkit 授权时
// 必然失败，而 CI runner 正是这种环境——也就是说这条路径在能跑测试的任何地方
// 都不会被执行到，只在真实的 root + systemd 生产机上激活。
// 而它一旦出错，表现是**所有机器的命令执行全部失效**，exec 是这个产品的核心能力。
//
// 排空宽限（exec.go 的 execDrainGrace）已经堵掉了那个真正致命的后果
// ——agent 被逃逸进程拖到并发槽耗尽、彻底无法执行命令。cgroup 解决的是
// 「逃逸进程本身还活着」，重要但不紧急。所以：先在一台机器上开、验证、
// 再全量铺开，而不是拿全部机器赌一条没人跑过的路径。
//
// 光看 LookPath 不够：装了 systemd-run 不等于跑得起来。容器里通常没有
// systemd（连不上 D-Bus），非 root 又没有 polkit 授权时也会被拒。
// 所以真跑一条 true 试试——这个代价只付一次。
func systemdRunUsable() bool {
	systemdRunOnce.Do(func() {
		if os.Getenv("MOSS_EXEC_CGROUP") != "1" {
			return
		}
		if _, err := exec.LookPath("systemd-run"); err != nil {
			log.Printf("未找到 systemd-run，命令执行使用进程组隔离：" +
				"超时终止收不走 setsid 出去的子孙进程")
			return
		}
		if err := exec.Command("systemd-run", "--scope", "--quiet", "--collect", "true").Run(); err != nil {
			log.Printf("systemd-run 不可用（%v），命令执行回退到进程组隔离："+
				"超时终止收不走 setsid 出去的子孙进程", err)
			return
		}
		systemdRunOK = true
		log.Printf("命令执行使用 systemd scope 隔离，超时可终止整棵进程树")
	})
	return systemdRunOK
}

// scopeUnit 由任务 ID 生成 systemd 单元名。
//
// 只保留字母数字：单元名的合法字符集有限，而 jobID 的字母表将来可能变。
// 与其信任上游，不如在这里过滤掉——一个非法单元名会让 systemd-run 直接失败，
// 那就等于这条命令根本执行不了。
func scopeUnit(id string) string {
	var b strings.Builder
	b.WriteString("moss-exec-")
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// buildShellCmd 用 /bin/sh 执行命令，并让子进程自成进程组，
// 以便超时时能向整组发信号，连子孙进程一并终止。
// systemd 可用时再套一层 transient scope，把 setsid 逃逸也堵上。
func buildShellCmd(id, command string) *exec.Cmd {
	var cmd *exec.Cmd
	if systemdRunUsable() {
		// --collect 让 scope 在退出后自动回收，不留下失败态单元堆积。
		// --scope 是同步执行且透传退出码与 stdio，因此不影响输出采集与 exitCode。
		cmd = exec.Command("systemd-run", "--scope", "--quiet", "--collect",
			"--unit="+scopeUnit(id), "/bin/sh", "-c", command)
	} else {
		cmd = exec.Command("/bin/sh", "-c", command)
	}
	// 进程组照设：scope 模式下它是收不走逃逸子孙时的兜底，
	// 回退模式下它是唯一的手段。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

type unixKiller struct {
	mu     sync.Mutex
	pgid   int
	scope  string // 非空表示这条命令跑在 systemd scope 里
	closed bool
}

func newProcessKiller(id string, cmd *exec.Cmd) (processKiller, error) {
	// Setpgid 保证子进程自成进程组且 pgid == pid，故直接取 pid。
	// 不调用 Getpgid：命令秒退时它会返回 ESRCH，导致本可正常收尾的执行被误判为失败。
	k := &unixKiller{pgid: cmd.Process.Pid}
	if systemdRunUsable() {
		k.scope = scopeUnit(id) + ".scope"
	}
	return k, nil
}

func (k *unixKiller) KillTree() {
	k.mu.Lock()
	defer k.mu.Unlock()
	// Close 之后一律不再发信号：此时进程已被回收，PID 可能已被系统复用，
	// 继续发信号会误杀一个无关进程。
	if k.closed {
		return
	}
	// 先杀 cgroup：这是唯一能收走 setsid 逃逸子孙的手段。
	// 失败不算致命——下面的进程组信号仍会收走绝大多数情况。
	if k.scope != "" {
		out, err := exec.Command("systemctl", "kill", "--kill-whom=all",
			"--signal=SIGKILL", k.scope).CombinedOutput()
		// 单元已不存在说明命令早已自然结束，属预期情况。
		if err != nil && !strings.Contains(string(out), "not loaded") {
			log.Printf("终止 scope %s 失败: %v (%s)", k.scope, err, strings.TrimSpace(string(out)))
		}
	}
	// 负号表示向整个进程组发信号。ESRCH 意味着进程组已不存在
	// （命令在超时回调抢到锁之前自然结束），属预期情况，不算失败。
	if err := syscall.Kill(-k.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("终止进程组 %d 失败: %v", k.pgid, err)
	}
}

func (k *unixKiller) Close() {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
}
