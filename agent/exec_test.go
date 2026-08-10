package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"moss/internal/protocol"
)

// fakeSender 收集回传消息，替代真实 WebSocket 连接。
type fakeSender struct {
	mu     sync.Mutex
	msgs   []protocol.AgentMsg
	fail   bool // 模拟连接已断
	doneCh chan protocol.ExecResult
}

func newFakeSender() *fakeSender {
	return &fakeSender{doneCh: make(chan protocol.ExecResult, 4)}
}

func (f *fakeSender) send(v any) error {
	m, ok := v.(protocol.AgentMsg)
	if !ok {
		return fmt.Errorf("非预期的消息类型 %T", v)
	}
	f.mu.Lock()
	if f.fail {
		f.mu.Unlock()
		return errors.New("连接已断开")
	}
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()

	if m.Exec != nil && m.Exec.Done {
		f.doneCh <- *m.Exec
	}
	return nil
}

// waitDone 等待终结分片，超时则让测试失败。
func (f *fakeSender) waitDone(t *testing.T, within time.Duration) protocol.ExecResult {
	t.Helper()
	select {
	case r := <-f.doneCh:
		return r
	case <-time.After(within):
		t.Fatalf("等待执行结束超时（%v）", within)
		return protocol.ExecResult{}
	}
}

// output 拼接所有输出分片。
func (f *fakeSender) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, m := range f.msgs {
		if m.Exec != nil {
			b.Write(m.Exec.Data)
		}
	}
	return b.String()
}

// appendLoopCmd 返回一条持续向 path 追加内容的命令。
// 该命令必然产生孙进程（shell 之下再启动实际执行体），用于验证超时能否终止整棵进程树。
func appendLoopCmd(path string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`for /L %%i in (1,1,120) do @(echo x>>"%s" & ping -n 2 127.0.0.1>nul)`, path)
	}
	return fmt.Sprintf(`while true; do echo x >> "%s"; sleep 0.1; done`, path)
}

func TestExecTimeoutKillsProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "alive.txt")
	fs := newFakeSender()
	r := newExecRunner(true)

	start := time.Now()
	r.Handle(fs, protocol.ExecTask{ID: "t1", Cmd: appendLoopCmd(marker), Timeout: 2})

	res := fs.waitDone(t, 20*time.Second)
	elapsed := time.Since(start)

	if res.Error == "" {
		t.Fatalf("超时执行应带错误信息，实际 %+v", res)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("超时未能及时终止，耗时 %v", elapsed)
	}

	// 核心断言：孙进程必须一并死亡。
	// 若只杀了父 shell，循环体仍会继续向文件追加内容。
	time.Sleep(1500 * time.Millisecond)
	size1 := fileSize(t, marker)
	time.Sleep(1500 * time.Millisecond)
	size2 := fileSize(t, marker)

	if size2 != size1 {
		t.Fatalf("孙进程仍在运行：文件从 %d 字节增长到 %d 字节，进程树未被终止", size1, size2)
	}
}

// TestExecEscapedChildReleasesSlot 是本模块最关键的一条。
//
// setsid 出去的子孙不在原进程组内，KillTree 杀不到它，它会一直攥着 stdout
// 管道的写端 —— 于是 pumpPipe 永远读不到 EOF、ch 永不关闭、run 永久挂起，
// 而 run 不返回就意味着并发槽永不释放。execMaxConcurrent 是 4，
// 累计 4 次之后这台机器再也执行不了任何命令，只能重启 agent。
//
// 只在 Linux 上跑：
//   - macOS 默认没有 setsid，构造不出逃逸；
//   - Windows 用 Job Object 而非进程组，子孙在 Start 后被一并纳入 Job，
//     TerminateJobObject 收得到，本就不存在这条逃逸路径。
//
// appendLoopCmd 那条用例覆盖不到这里：它的孙进程与父 shell 同进程组，
// 必然被 SIGKILL 收走，所以这个洞此前在 CI 上完全不可见。
func TestExecEscapedChildReleasesSlot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("需要 setsid 构造进程组逃逸，当前平台 %s 不适用", runtime.GOOS)
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("本机没有 setsid，无法构造进程组逃逸")
	}

	r := newExecRunner(true)
	// 逃逸的子孙持有 stdout（不重定向），父 shell 立刻退出。
	escape := "setsid sleep 30 & echo spawned"

	const rounds = execMaxConcurrent + 1
	for i := 0; i < rounds; i++ {
		fs := newFakeSender()
		start := time.Now()
		r.Handle(fs, protocol.ExecTask{ID: fmt.Sprintf("esc%d", i), Cmd: escape, Timeout: 1})

		// 1 秒超时 + 5 秒排空宽限，留足余量。卡死的话这里会直接失败。
		res := fs.waitDone(t, 20*time.Second)
		elapsed := time.Since(start)

		if res.Error == "" {
			t.Fatalf("第 %d 轮：逃逸子进程导致的强制收尾必须带错误说明，实际 %+v", i, res)
		}
		if elapsed > 15*time.Second {
			t.Fatalf("第 %d 轮：未能在宽限内收尾，耗时 %v", i, elapsed)
		}

		// 核心断言：并发槽必须回落。不回落的话第 5 轮会被「并发执行数已达上限」拒绝。
		//
		// 必须等而不能立刻查：终结消息是在 run() **内部**发出的，
		// 而 defer r.done() 要等 run 返回才执行——收到 Done 的那一刻，
		// 槽通常还没还回来。直接断言会变成一条看心情的用例。
		waitSlotReleased(t, r, 5*time.Second, i)
	}
}

// runningCount 读取当前占用的并发槽数。
func runningCount(r *execRunner) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// waitSlotReleased 等并发槽回落到 0，超时即失败。
//
// 「等一小会儿」与「永远等不到」的区别正是这个 bug 的全部：卡住的话槽永不回落，
// 几秒的宽限足以区分两者，而瞬时断言只会在收到终结消息与 defer 执行之间的
// 那几微秒里随机翻车。
func waitSlotReleased(t *testing.T, r *execRunner, within time.Duration, round int) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runningCount(r) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("第 %d 轮结束后并发槽在 %v 内未释放，running=%d —— 逃逸子进程把执行槽永久占住了",
		round, within, runningCount(r))
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0 // 命令可能还没来得及写第一笔
		}
		t.Fatalf("读取文件信息失败: %v", err)
	}
	return fi.Size()
}

func TestExecDisabledByDefault(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(false)

	r.Handle(fs, protocol.ExecTask{ID: "t2", Cmd: "echo hello", Timeout: 5})

	res := fs.waitDone(t, 3*time.Second)
	if res.Error == "" {
		t.Fatal("未开启远程执行时必须拒绝，却返回了成功")
	}
	if out := fs.output(); strings.Contains(out, "hello") {
		t.Fatalf("命令不应被执行，却拿到了输出: %q", out)
	}
}

func TestExecIdempotentRejectsDuplicateID(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(true)

	r.Handle(fs, protocol.ExecTask{ID: "same", Cmd: echoCmd("first"), Timeout: 10})
	first := fs.waitDone(t, 15*time.Second)
	if first.Error != "" {
		t.Fatalf("首次执行不应失败: %s", first.Error)
	}

	// 重连后服务端重发同一任务，必须被幂等拦截，不能二次执行。
	r.Handle(fs, protocol.ExecTask{ID: "same", Cmd: echoCmd("second"), Timeout: 10})
	dup := fs.waitDone(t, 5*time.Second)
	if dup.Error == "" {
		t.Fatal("重复任务 ID 必须被拒绝")
	}
	if out := fs.output(); strings.Contains(out, "second") {
		t.Fatalf("重复任务被执行了第二次，输出: %q", out)
	}
}

func TestExecReportsExitCode(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(true)

	r.Handle(fs, protocol.ExecTask{ID: "t3", Cmd: exitCmd(3), Timeout: 10})

	res := fs.waitDone(t, 15*time.Second)
	if res.Error != "" {
		t.Fatalf("正常退出的命令不应报错: %s", res.Error)
	}
	if res.ExitCode != 3 {
		t.Fatalf("退出码应为 3，实际 %d", res.ExitCode)
	}
}

func TestExecCapturesStdoutAndStderr(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(true)

	r.Handle(fs, protocol.ExecTask{ID: "t4", Cmd: stdoutStderrCmd(), Timeout: 10})

	res := fs.waitDone(t, 15*time.Second)
	if res.Error != "" {
		t.Fatalf("执行不应失败: %s", res.Error)
	}
	out := fs.output()
	if !strings.Contains(out, "OUT") || !strings.Contains(out, "ERR") {
		t.Fatalf("stdout 与 stderr 都应被捕获，实际输出: %q", out)
	}
}

func TestExecEmptyCommandRejected(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(true)

	r.Handle(fs, protocol.ExecTask{ID: "t5", Cmd: "", Timeout: 5})

	res := fs.waitDone(t, 3*time.Second)
	if res.Error == "" {
		t.Fatal("空命令必须被拒绝")
	}
}

// TestExecSurvivesBrokenConnection 验证连接断开后命令仍执行完毕且不卡死。
// 设计取舍：部署跑到一半因网络抖动被中断，比拿不到输出更糟。
func TestExecSurvivesBrokenConnection(t *testing.T) {
	fs := newFakeSender()
	fs.fail = true // 从一开始就发不出去
	r := newExecRunner(true)

	done := make(chan struct{})
	go func() {
		r.run(fs, protocol.ExecTask{ID: "t6", Cmd: echoCmd("ignored"), Timeout: 10})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("连接断开后执行流程卡死，未能正常收尾")
	}
}

func echoCmd(s string) string {
	if runtime.GOOS == "windows" {
		return "echo " + s
	}
	return "echo " + s
}

func exitCmd(code int) string {
	return fmt.Sprintf("exit %d", code)
}

func stdoutStderrCmd() string {
	if runtime.GOOS == "windows" {
		return `echo OUT& echo ERR 1>&2`
	}
	return `echo OUT; echo ERR 1>&2`
}
