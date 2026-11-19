//go:build !windows

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"moss/internal/protocol"
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

var (
	systemdRunOnce sync.Once
	systemdRunOK   bool
)

// systemdRunUsable 探测 systemd-run --scope 是否真的能用，只探一次。
//
// **默认关闭，需 MOSS_EXEC_CGROUP=1 显式开启。**
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

// scopeUnit 由任务 ID 生成流式执行的 systemd scope 单元名。
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
		cmd = exec.Command("systemd-run", "--scope", "--quiet", "--collect",
			"--unit="+scopeUnit(id), "/bin/sh", "-c", command)
	} else {
		cmd = exec.Command("/bin/sh", "-c", command)
	}
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
	k := &unixKiller{pgid: cmd.Process.Pid}
	if systemdRunUsable() {
		k.scope = scopeUnit(id) + ".scope"
	}
	return k, nil
}

func (k *unixKiller) KillTree() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	if k.scope != "" {
		out, err := exec.Command("systemctl", "kill", "--kill-whom=all",
			"--signal=SIGKILL", k.scope).CombinedOutput()
		if err != nil && !strings.Contains(string(out), "not loaded") {
			log.Printf("终止 scope %s 失败: %v (%s)", k.scope, err, strings.TrimSpace(string(out)))
		}
	}
	if err := syscall.Kill(-k.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("终止进程组 %d 失败: %v", k.pgid, err)
	}
}

func (k *unixKiller) Close() {
	k.mu.Lock()
	k.closed = true
	k.mu.Unlock()
}

/* ---------- detach 后台托管（systemd-run --unit=moss-job-<id>） ---------- */

// buildDetachSystemdArgs 构造 systemd-run 启动后台托管服务的参数列表。
//
// 关键设计：
//   - 不加 --scope：以 transient service 单元交由 PID 1 systemd 托管，命令立即返回且彻底脱离 moss-agent；
//   - RuntimeMaxSec 控制最长运行时间（最高 24 小时），超时由 systemd 自动终止整个 cgroup；
//   - ExecStopPost 作为兜底：无论正常退出、RuntimeMaxSec 超时、OOM 还是 systemctl stop，
//     都在单元回收前把 SERVICE_RESULT 与 EXIT_STATUS 原子写入状态文件。
func buildDetachSystemdArgs(unit, dir, runScript, postScript string, timeout int) []string {
	args := []string{
		"--unit=" + unit,
		"--quiet",
		"--collect",
		"-p", "RuntimeMaxSec=" + strconv.Itoa(timeout),
		"-p", "ExecStopPost=/bin/sh " + postScript,
	}
	if dir != "" {
		args = append(args, "--working-directory="+dir)
	}
	args = append(args, "/bin/sh", runScript)
	return args
}

// pruneDetachedJobDirs 顺带清理已结束且超过保留期的磁盘任务目录。
func (r *execRunner) pruneDetachedJobDirs() {
	base := r.jobDir
	if base == "" {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	now := time.Now()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		fi, err := os.Stat(filepath.Join(dir, "status"))
		if err != nil {
			continue // 尚无 status 文件说明可能仍在运行，绝不清理
		}
		if now.Sub(fi.ModTime()) > detachKeepAge {
			_ = os.RemoveAll(dir)
		}
	}
}

// startDetachedJob 用 systemd-run --unit=moss-job-<id> 拉起后台托管任务并立即返回。
func (r *execRunner) startDetachedJob(task protocol.ExecTask, timeout int) (string, error) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return "", errors.New("本机未找到 systemd-run，无法启动 detach 后台托管任务")
	}
	if task.Dir != "" {
		fi, err := os.Stat(task.Dir)
		if err != nil || !fi.IsDir() {
			return "", fmt.Errorf("工作目录不存在或不可用: %s", task.Dir)
		}
	}

	r.pruneDetachedJobDirs()

	dir := r.jobPath(task.ID)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建任务目录失败: %w", err)
	}

	logPath := filepath.Join(dir, "output.log")
	cmdScript := filepath.Join(dir, "cmd.sh")
	runScript := filepath.Join(dir, "run.sh")
	postScript := filepath.Join(dir, "post.sh")
	statusPath := filepath.Join(dir, "status")
	statusTmp := filepath.Join(dir, "status.tmp")

	// 预先创建空日志文件，使任务刚启动时的首次 get_result 即可正常打开读取。
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("创建日志文件失败: %w", err)
	}
	// 用户命令单独落脚本文件，彻底规避多层引号、换行、heredoc 与 exit 提前退出外层包装的问题。
	if err := os.WriteFile(cmdScript, []byte(task.Cmd+"\n"), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("写入命令脚本失败: %w", err)
	}

	runBody := fmt.Sprintf(`#!/bin/sh
/bin/sh %q >> %q 2>&1
code=$?
if [ ! -f %q ]; then
	printf 'exited:%%d\n' "$code" > %q && mv %q %q
fi
exit "$code"
`, cmdScript, logPath, statusPath, statusTmp, statusTmp, statusPath)
	if err := os.WriteFile(runScript, []byte(runBody), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("写入启动包装脚本失败: %w", err)
	}

	postBody := fmt.Sprintf(`#!/bin/sh
if [ ! -f %q ]; then
	printf '%%s:%%s\n' "${SERVICE_RESULT:-unknown}" "${EXIT_STATUS:-1}" > %q && mv %q %q
fi
`, statusPath, statusTmp, statusTmp, statusPath)
	if err := os.WriteFile(postScript, []byte(postBody), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("写入收尾脚本失败: %w", err)
	}

	unit := jobUnit(task.ID)
	args := buildDetachSystemdArgs(unit, task.Dir, runScript, postScript, timeout)
	out, err := exec.Command("systemd-run", args...).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return "", fmt.Errorf("systemd-run 启动失败: %v (%s)", err, detail)
		}
		return "", fmt.Errorf("systemd-run 启动失败: %v", err)
	}
	return unit + ".service", nil
}

// inspectDetachedState 读取指定后台托管任务当前的运行状态与退出码。
func (r *execRunner) inspectDetachedState(dir, unit string) (running, canceled bool, exitCode int, errMsg string) {
	statusPath := filepath.Join(dir, "status")
	if b, err := os.ReadFile(statusPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		canceled, exitCode, errMsg = parseDetachStatusFile(string(b))
		return false, canceled, exitCode, errMsg
	}

	// 状态文件尚未生成时，向 systemd 确认单元是否仍在运行。
	if err := exec.Command("systemctl", "is-active", "--quiet", unit+".service").Run(); err == nil {
		return true, false, 0, ""
	}

	// 单元已不活跃：给 ExecStopPost 留出极短落盘窗口后再读一次 status 文件，
	// 避免恰好在进程退出与 ExecStopPost 写盘之间读到空状态。
	time.Sleep(50 * time.Millisecond)
	if b, err := os.ReadFile(statusPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		canceled, exitCode, errMsg = parseDetachStatusFile(string(b))
		return false, canceled, exitCode, errMsg
	}

	// 单元已消失且无状态文件（例如机器重启过），落盘标记以免后续每次都调 systemctl。
	_ = writeDetachStatusAtomic(dir, "lost:-1")
	return false, false, -1, "后台任务单元已停止，未捕获到退出码"
}

// queryDetachedJob 读取后台托管任务的状态与指定区间（offset 或 tail）的磁盘日志。
func (r *execRunner) queryDetachedJob(q protocol.JobQuery) protocol.JobStatus {
	dir := r.jobPath(q.JobID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return protocol.JobStatus{
			ReqID: q.ReqID,
			JobID: q.JobID,
			Found: false,
			Error: "目标机上找不到该后台任务的磁盘记录",
		}
	}
	unit := jobUnit(q.JobID)
	running, canceled, exitCode, errMsg := r.inspectDetachedState(dir, unit)
	logPath := filepath.Join(dir, "output.log")
	data, actualOffset, nextOffset, totalBytes := readLogSlice(logPath, q.Offset, q.Tail, q.Limit)
	return protocol.JobStatus{
		ReqID:      q.ReqID,
		JobID:      q.JobID,
		Unit:       unit + ".service",
		Found:      true,
		Running:    running,
		Canceled:   canceled,
		ExitCode:   exitCode,
		Error:      errMsg,
		Data:       data,
		Offset:     actualOffset,
		NextOffset: nextOffset,
		TotalBytes: totalBytes,
	}
}

// cancelDetachedJob 终止后台托管任务的 systemd 单元并落盘取消状态。
func (r *execRunner) cancelDetachedJob(req protocol.JobCancel) protocol.JobStatus {
	dir := r.jobPath(req.JobID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return protocol.JobStatus{
			ReqID: req.ReqID,
			JobID: req.JobID,
			Found: false,
			Error: "目标机上找不到该后台任务的磁盘记录",
		}
	}
	unit := jobUnit(req.JobID)
	statusPath := filepath.Join(dir, "status")
	if b, err := os.ReadFile(statusPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		canceled, exitCode, errMsg := parseDetachStatusFile(string(b))
		_, _, _, totalBytes := readLogSlice(filepath.Join(dir, "output.log"), 0, 0, -1)
		return protocol.JobStatus{
			ReqID:      req.ReqID,
			JobID:      req.JobID,
			Unit:       unit + ".service",
			Found:      true,
			Running:    false,
			Canceled:   canceled,
			ExitCode:   exitCode,
			Error:      errMsg,
			Offset:     totalBytes,
			NextOffset: totalBytes,
			TotalBytes: totalBytes,
		}
	}

	// 先写状态文件再停单元，防止 ExecStopPost 抢先把状态写成 signal:TERM。
	_ = writeDetachStatusAtomic(dir, "canceled:130")
	_ = exec.Command("systemctl", "stop", unit+".service").Run()
	_ = exec.Command("systemctl", "kill", "--kill-whom=all", "--signal=SIGKILL", unit+".service").Run()

	_, _, _, totalBytes := readLogSlice(filepath.Join(dir, "output.log"), 0, 0, -1)
	return protocol.JobStatus{
		ReqID:      req.ReqID,
		JobID:      req.JobID,
		Unit:       unit + ".service",
		Found:      true,
		Running:    false,
		Canceled:   true,
		ExitCode:   -1,
		Error:      "任务已取消",
		Offset:     totalBytes,
		NextOffset: totalBytes,
		TotalBytes: totalBytes,
	}
}
