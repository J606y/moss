//go:build !windows

package main

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"moss/internal/protocol"
)

// TestScopeUnitSanitizes 单元名必须只含合法字符。
//
// 一个非法的单元名会让 systemd-run 直接失败，那就等于这条命令根本执行不了——
// 用「过滤掉」而不是「相信上游」，是因为 jobID 的字母表将来可能变，
// 而这里出问题的表现是「所有命令都跑不了」，代价太大。
func TestScopeUnitSanitizes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"job_AbC123", "moss-execjobAbC123"},
		{"", "moss-exec"},
		{"a/b c;d`e$f", "moss-execabcdef"},
		{"中文任务", "moss-exec"},
	}
	for _, c := range cases {
		got := scopeUnit(c.in)
		if got != c.want {
			t.Errorf("scopeUnit(%q) = %q，期望 %q", c.in, got, c.want)
		}
		for _, r := range strings.TrimPrefix(got, "moss-exec") {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("scopeUnit(%q) 产出了非法字符 %q", c.in, r)
			}
		}
	}
}

// TestBuildShellCmdFallsBackWithoutSystemd systemd 不可用时必须回退，
// 而不是让 exec 整体不可用。
//
// 探测结果是包级缓存的，这里直接按当前环境断言：
//   - 探得通  → 命令应当被 systemd-run 包起来
//   - 探不通  → 命令应当是裸的 /bin/sh
//
// 两条分支在 CI 的 Linux runner（有 systemd）与 macOS runner（没有）上各覆盖一次。
func TestBuildShellCmdFallsBackWithoutSystemd(t *testing.T) {
	cmd := buildShellCmd("job_test1", "echo hi")

	if systemdRunUsable() {
		if !strings.HasSuffix(cmd.Path, "systemd-run") {
			t.Fatalf("systemd 可用时应走 scope 隔离，实际 Path=%q", cmd.Path)
		}
		if !containsArg(cmd.Args, "--unit=moss-execjobtest1") {
			t.Fatalf("应带上单元名，实际 Args=%v", cmd.Args)
		}
		if !containsArg(cmd.Args, "--collect") {
			t.Fatalf("必须带 --collect，否则失败的 scope 会堆积成僵尸单元；实际 Args=%v", cmd.Args)
		}
	} else if !strings.HasSuffix(cmd.Path, "/sh") {
		t.Fatalf("systemd 不可用时应回退到 /bin/sh，实际 Path=%q", cmd.Path)
	}

	// 无论走哪条路，进程组都要设上：scope 模式下它是兜底，回退模式下它是唯一手段
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("两种模式都必须设置 Setpgid")
	}
}

// TestExecStillWorksUnderScope 端到端：无论走哪条路，命令都要能正常执行并拿到输出。
//
// 这是 cgroup 隔离引入的最大风险——它给每条命令多套了一层进程，
// 一旦 stdio 透传或退出码传播出问题，就是所有机器的 exec 全部失效。
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

// systemdRunPresent 供人工排查时确认本机环境。
func systemdRunPresent() bool {
	_, err := exec.LookPath("systemd-run")
	return err == nil
}
