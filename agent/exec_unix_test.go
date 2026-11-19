//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"moss/internal/protocol"
)

func TestScopeUnitSanitizes(t *testing.T) {
	const prefix = "moss-exec-"
	cases := []struct{ in, want string }{
		{"job_AbC123", prefix + "jobAbC123"},
		{"", prefix},
		{"a/b c;d`e$f", prefix + "abcdef"},
		{"中文任务", prefix},
	}
	for _, c := range cases {
		got := scopeUnit(c.in)
		if got != c.want {
			t.Errorf("scopeUnit(%q) = %q，期望 %q", c.in, got, c.want)
		}
		for _, r := range strings.TrimPrefix(got, prefix) {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("scopeUnit(%q) 产出了非法字符 %q", c.in, r)
			}
		}
	}
}

func TestJobUnitAndBuildDetachSystemdArgs(t *testing.T) {
	u := jobUnit("job_AbC-123;rm")
	if u != "moss-job-job_AbC-123rm" {
		t.Fatalf("jobUnit 过滤不符: %q", u)
	}
	args := buildDetachSystemdArgs(u, "/tmp/work", "/tmp/run.sh", "/tmp/post.sh", 7200)
	if !containsArg(args, "--unit=moss-job-job_AbC-123rm") {
		t.Errorf("应带 --unit=moss-job-<id>，实际: %v", args)
	}
	if !containsArg(args, "RuntimeMaxSec=7200") {
		t.Errorf("应带 RuntimeMaxSec=7200，实际: %v", args)
	}
	if !containsArg(args, "--working-directory=/tmp/work") {
		t.Errorf("应带 --working-directory，实际: %v", args)
	}
	if containsArg(args, "--scope") {
		t.Errorf("detach 模式必须走 service 而非 --scope，实际: %v", args)
	}
}

func TestReadLogSliceOffsetAndTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "output.log")
	content := strings.Repeat("0123456789ABCDEF", 200) // 3200 字节
	if err := os.WriteFile(logPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	// 分段增量读
	d1, off1, next1, tot1 := readLogSlice(logPath, 0, 0, 1000)
	if off1 != 0 || next1 != 1000 || tot1 != 3200 || string(d1) != content[:1000] {
		t.Fatalf("首段读取不符: off=%d next=%d tot=%d len=%d", off1, next1, tot1, len(d1))
	}
	d2, off2, next2, _ := readLogSlice(logPath, next1, 0, 1000)
	if off2 != 1000 || next2 != 2000 || string(d2) != content[1000:2000] {
		t.Fatalf("二段读取不符: off=%d next=%d", off2, next2)
	}

	// tail 模式：取末尾 500 字节
	dt, offt, nextt, tott := readLogSlice(logPath, 0, 500, 1000)
	if offt != 2700 || nextt != 3200 || tott != 3200 || string(dt) != content[2700:] {
		t.Fatalf("tail 读取不符: off=%d next=%d tot=%d", offt, nextt, tott)
	}
}

func TestParseDetachStatusFile(t *testing.T) {
	cases := []struct {
		in       string
		canceled bool
		code     int
		hasErr   bool
	}{
		{"exited:0", false, 0, false},
		{"exited:7", false, 7, false},
		{"canceled:130", true, -1, true},
		{"timeout:TERM", false, -1, true},
		{"oom-kill:KILL", false, -1, true},
	}
	for _, c := range cases {
		canceled, code, errMsg := parseDetachStatusFile(c.in)
		if canceled != c.canceled || code != c.code || (errMsg != "") != c.hasErr {
			t.Errorf("parseDetachStatusFile(%q) = (canceled=%v, code=%d, err=%q)", c.in, canceled, code, errMsg)
		}
	}
}

func systemdServiceUsable() bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	return exec.Command("systemd-run", "--quiet", "--collect", "true").Run() == nil
}

// TestExecDetachEndToEnd 在具备 systemd 的 Linux 机器上端到端验证：
// systemd-run --unit=moss-job-<id> 启动即返回、分段追读日志、状态文件写退出码、RuntimeMaxSec 超时与 cancel。
func TestExecDetachEndToEnd(t *testing.T) {
	if !systemdServiceUsable() {
		t.Skip("本机 systemd-run 服务不可用，跳过真实 systemd 端到端测试")
	}

	r := newExecRunner(true)
	r.jobDir = t.TempDir()

	// 1. 正常分段输出 + 非零退出码写状态文件
	fs := newFakeSender()
	jobID := "det_e2e_" + strings.ReplaceAll(t.Name(), "/", "_")
	r.Handle(fs, protocol.ExecTask{
		ID:      jobID,
		Cmd:     "echo chunk1; sleep 0.3; echo chunk2; exit 5",
		Timeout: 30,
		Detach:  true,
	})
	ack := fs.waitDone(t, 5*time.Second)
	if ack.Error != "" || !ack.Detached {
		t.Fatalf("detach 任务应立即成功拉起，实际: %+v", ack)
	}
	if !strings.HasPrefix(ack.Unit, "moss-job-") {
		t.Fatalf("单元名应以 moss-job- 开头，实际: %q", ack.Unit)
	}

	// 等待命令完成并验证分段读取与退出码
	var st protocol.JobStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st = r.queryDetachedJob(protocol.JobQuery{ReqID: "q1", JobID: jobID, Offset: 0})
		if !st.Running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st.Running {
		t.Fatal("detach 任务应在预期时间内结束")
	}
	if st.ExitCode != 5 {
		t.Fatalf("状态文件应记录退出码 5，实际 %d (error=%q)", st.ExitCode, st.Error)
	}
	if !strings.Contains(string(st.Data), "chunk1") || !strings.Contains(string(st.Data), "chunk2") {
		t.Fatalf("磁盘日志应包含完整输出，实际: %q", string(st.Data))
	}

	// 验证 offset 增量读取：从第一行之后读取只拿第二行
	idx := strings.Index(string(st.Data), "chunk2")
	inc := r.queryDetachedJob(protocol.JobQuery{ReqID: "q2", JobID: jobID, Offset: int64(idx)})
	if strings.TrimSpace(string(inc.Data)) != "chunk2" {
		t.Fatalf("offset 增量读取应只返回 chunk2，实际: %q", string(inc.Data))
	}

	// 2. RuntimeMaxSec 超时自动终止
	fsTimeout := newFakeSender()
	timeoutID := "det_to_" + strings.ReplaceAll(t.Name(), "/", "_")
	r.Handle(fsTimeout, protocol.ExecTask{
		ID:      timeoutID,
		Cmd:     "echo before_timeout; sleep 10",
		Timeout: 1,
		Detach:  true,
	})
	ackTO := fsTimeout.waitDone(t, 5*time.Second)
	if ackTO.Error != "" || !ackTO.Detached {
		t.Fatalf("超时用例启动失败: %+v", ackTO)
	}
	deadline = time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		st = r.queryDetachedJob(protocol.JobQuery{ReqID: "q3", JobID: timeoutID})
		if !st.Running {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if st.Running || !strings.Contains(st.Error, "超时") {
		t.Fatalf("RuntimeMaxSec 到点后应标记超时终止，实际: running=%v error=%q", st.Running, st.Error)
	}

	// 3. cancel 主动终止正在运行的后台任务
	fsCancel := newFakeSender()
	cancelID := "det_cancel_" + strings.ReplaceAll(t.Name(), "/", "_")
	r.Handle(fsCancel, protocol.ExecTask{
		ID:      cancelID,
		Cmd:     "echo before_cancel; sleep 60",
		Timeout: 120,
		Detach:  true,
	})
	ackCancel := fsCancel.waitDone(t, 5*time.Second)
	if ackCancel.Error != "" || !ackCancel.Detached {
		t.Fatalf("取消用例启动失败: %+v", ackCancel)
	}
	cst := r.cancelDetachedJob(protocol.JobCancel{ReqID: "c1", JobID: cancelID})
	if cst.Running || !cst.Canceled {
		t.Fatalf("cancel 后应处于已取消终态，实际: %+v", cst)
	}
}

func TestBuildShellCmdFallsBackWithoutSystemd(t *testing.T) {
	cmd := buildShellCmd("job_test1", "echo hi")

	if systemdRunUsable() {
		if !strings.HasSuffix(cmd.Path, "systemd-run") {
			t.Fatalf("systemd 可用时应走 scope 隔离，实际 Path=%q", cmd.Path)
		}
		if !containsArg(cmd.Args, "--unit=moss-exec-jobtest1") {
			t.Fatalf("应带上单元名，实际 Args=%v", cmd.Args)
		}
		if !containsArg(cmd.Args, "--collect") {
			t.Fatalf("必须带 --collect，否则失败的 scope 会堆积成僵尸单元；实际 Args=%v", cmd.Args)
		}
	} else if !strings.HasSuffix(cmd.Path, "/sh") {
		t.Fatalf("systemd 不可用时应回退到 /bin/sh，实际 Path=%q", cmd.Path)
	}

	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("两种模式都必须设置 Setpgid")
	}
}

func TestExecStillWorksUnderScope(t *testing.T) {
	fs := newFakeSender()
	r := newExecRunner(true)

	r.Handle(fs, protocol.ExecTask{ID: "scope-e2e", Cmd: "echo OUT; echo ERR 1>&2; exit 7", Timeout: 20})

	res := fs.waitDone(t, 30*time.Second)
	if res.Error != "" {
		t.Fatalf("命令应正常执行完，实际报错: %s", res.Error)
	}
	if res.ExitCode != 7 {
		t.Errorf("退出码应透传为 7，实际 %d —— scope 包装破坏了退出码传播", res.ExitCode)
	}
	out := fs.output()
	if !strings.Contains(out, "OUT") || !strings.Contains(out, "ERR") {
		t.Errorf("stdout 与 stderr 都应透传，实际输出: %q", out)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func systemdRunPresent() bool {
	_, err := exec.LookPath("systemd-run")
	return err == nil
}
