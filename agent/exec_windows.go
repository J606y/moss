//go:build windows

package main

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"moss/internal/protocol"
)

const errWindowsDetachUnsupported = "Windows 暂不支持 detach 后台托管模式"

// buildShellCmd 用 COMSPEC（通常是 cmd.exe）执行命令。
//
// 命令行整体通过 SysProcAttr.CmdLine 传入，绕开 os/exec 的默认参数转义：
// Go 按 CommandLineToArgvW 规则转义，而 cmd.exe 不遵循该规则，
// 命令中的引号、& 、| 等字符会被悄悄改写成非预期的形态。
func buildShellCmd(_ string, command string) *exec.Cmd {
	shell := os.Getenv("COMSPEC")
	if shell == "" {
		shell = "cmd.exe"
	}
	cmd := exec.Command(shell)
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
func newProcessKiller(_ string, cmd *exec.Cmd) (processKiller, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
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

func (r *execRunner) startDetachedJob(_ protocol.ExecTask, _ int) (string, error) {
	return "", errors.New(errWindowsDetachUnsupported)
}

func (r *execRunner) queryDetachedJob(q protocol.JobQuery) protocol.JobStatus {
	return protocol.JobStatus{
		ReqID: q.ReqID,
		JobID: q.JobID,
		Found: false,
		Error: errWindowsDetachUnsupported,
	}
}

func (r *execRunner) cancelDetachedJob(req protocol.JobCancel) protocol.JobStatus {
	return protocol.JobStatus{
		ReqID: req.ReqID,
		JobID: req.JobID,
		Found: false,
		Error: errWindowsDetachUnsupported,
	}
}
