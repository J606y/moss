//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// buildShellCmd 用 COMSPEC（通常是 cmd.exe）执行命令。
//
// 命令行整体通过 SysProcAttr.CmdLine 传入，绕开 os/exec 的默认参数转义：
// Go 按 CommandLineToArgvW 规则转义，而 cmd.exe 不遵循该规则，
// 命令中的引号、& 、| 等字符会被悄悄改写成非预期的形态。
func buildShellCmd(command string) *exec.Cmd {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.Command(shell)
	// CmdLine 必须以程序名开头：cmd.exe 解析命令行时会跳过第一个 token 当作 argv[0]，
	// 少了它会把 "/C" 当成程序名吞掉，真正的命令反而被当成参数。
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `"` + shell + `" /C ` + command,
	}
	return cmd
}

type winKiller struct {
	mu     sync.Mutex
	job    windows.Handle
	closed bool
}

// newProcessKiller 把子进程纳入一个 Job Object。
// Job Object 绑定的是内核对象而非 PID，因此不存在 Unix 侧的 PID 复用误杀问题。
func newProcessKiller(cmd *exec.Cmd) (processKiller, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	// KILL_ON_JOB_CLOSE：agent 进程若异常退出，句柄被内核关闭，
	// 整棵进程树随之终止，不留孤儿。
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	return &winKiller{job: job}, nil
}

func (k *winKiller) KillTree() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	// 终止 Job 内所有进程。进程已自然结束时该调用无副作用。
	if err := windows.TerminateJobObject(k.job, 1); err != nil {
		log.Printf("终止 Job Object 失败: %v", err)
	}
}

func (k *winKiller) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	k.closed = true
	if err := windows.CloseHandle(k.job); err != nil {
		log.Printf("关闭 Job Object 句柄失败: %v", err)
	}
}
