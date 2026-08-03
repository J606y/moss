//go:build !windows

package main

import (
	"errors"
	"log"
	"os/exec"
	"sync"
	"syscall"
)

// buildShellCmd 用 /bin/sh 执行命令，并让子进程自成进程组，
// 以便超时时能向整组发信号，连子孙进程一并终止。
func buildShellCmd(command string) *exec.Cmd {
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

type unixKiller struct {
	mu     sync.Mutex
	pgid   int
	closed bool
}

func newProcessKiller(cmd *exec.Cmd) (processKiller, error) {
	// Setpgid 保证子进程自成进程组且 pgid == pid，故直接取 pid。
	// 不调用 Getpgid：命令秒退时它会返回 ESRCH，导致本可正常收尾的执行被误判为失败。
	return &unixKiller{pgid: cmd.Process.Pid}, nil
}

func (k *unixKiller) KillTree() {
	k.mu.Lock()
	defer k.mu.Unlock()
	// Close 之后一律不再发信号：此时进程已被回收，PID 可能已被系统复用，
	// 继续发信号会误杀一个无关进程。
	if k.closed {
		return
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
