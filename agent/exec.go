// moss-agent 命令执行端：接收服务端下发的执行任务，流式回传输出。
//
// 安全前提：执行能力默认关闭。装了 agent 不等于接受被远程操作，
// 必须通过 --allow-exec 或 MOSS_ALLOW_EXEC=1 显式开启。
package main

import (
	"errors"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"moss/internal/protocol"
)

const (
	execMaxConcurrent  = 4                // 同时执行的命令数上限，防止一次性下发过多把机器压垮
	execSeenTTL        = 30 * time.Minute // 幂等记录保留时长，超时后清理避免无限增长
	execDefaultTimeout = 60               // 服务端未指定超时时的默认值（秒）
)

// sender 是执行端回传消息的出口。抽成接口而非直接依赖 *client，
// 是为了让执行逻辑（超时强杀、分片、截断）能脱离真实 WebSocket 连接被测试。
type sender interface {
	send(v any) error
}

// execRunner 管理命令执行的准入、幂等与并发。
// 生命周期跨越 WS 重连：幂等记录必须在重连后依然有效，
// 否则服务端重发同一任务会导致二次执行（例如重复跑一次部署）。
type execRunner struct {
	allow bool

	mu      sync.Mutex
	seen    map[string]time.Time // 任务 ID → 首次受理时间
	running int
}

func newExecRunner(allow bool) *execRunner {
	return &execRunner{allow: allow, seen: make(map[string]time.Time)}
}

// admit 校验准入并登记，返回拒绝原因（空串表示放行）。
func (r *execRunner) admit(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 顺带清理过期幂等记录，省去一个常驻 goroutine
	now := time.Now()
	for k, t := range r.seen {
		if now.Sub(t) > execSeenTTL {
			delete(r.seen, k)
		}
	}

	if _, dup := r.seen[id]; dup {
		return "任务已受理，忽略重复下发"
	}
	if r.running >= execMaxConcurrent {
		return "并发执行数已达上限"
	}
	r.seen[id] = now
	r.running++
	return ""
}

func (r *execRunner) done() {
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
}

// Handle 受理一个执行任务，异步执行。调用方不阻塞。
func (r *execRunner) Handle(c sender, task protocol.ExecTask) {
	if !r.allow {
		sendExecFinal(c, task.ID, 0, "本机未开启远程执行（需 --allow-exec）", false)
		return
	}
	if task.ID == "" || task.Cmd == "" {
		sendExecFinal(c, task.ID, 0, "任务 ID 或命令为空", false)
		return
	}
	if reason := r.admit(task.ID); reason != "" {
		sendExecFinal(c, task.ID, 0, reason, false)
		return
	}
	go func() {
		defer r.done()
		r.run(c, task)
	}()
}

// chunk 是输出读取协程与发送协程之间传递的数据单元。
type chunk struct {
	stream string
	data   []byte
}

// processKiller 终止一棵进程树。由平台文件实现：
// Unix 走进程组信号，Windows 走 Job Object。
// KillTree 与 Close 可能并发发生（定时器回调恰好在收尾时触发），实现必须自行加锁并保证幂等。
type processKiller interface {
	KillTree()
	Close()
}

func (r *execRunner) run(c sender, task protocol.ExecTask) {
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = execDefaultTimeout
	}
	if timeout > protocol.ExecMaxTimeout {
		timeout = protocol.ExecMaxTimeout
	}

	// buildShellCmd 由平台文件实现：选定 shell、处理各自的命令行转义规则，
	// 并预置进程组 / Job Object 所需属性，使超时后能连子孙进程一并终止。
	// 只杀父进程会留下孤儿：`sh -c "sleep 999"` 里真正睡着的是 sleep 不是 sh。
	cmd := buildShellCmd(task.Cmd)
	cmd.Dir = task.Dir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sendExecFinal(c, task.ID, 0, "创建 stdout 管道失败: "+err.Error(), false)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		sendExecFinal(c, task.ID, 0, "创建 stderr 管道失败: "+err.Error(), false)
		return
	}

	if err := cmd.Start(); err != nil {
		sendExecFinal(c, task.ID, 0, "启动失败: "+err.Error(), false)
		return
	}

	killer, err := newProcessKiller(cmd)
	if err != nil {
		// 拿不到可靠的终止手段就不执行：宁可失败，也不留下杀不掉的进程。
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		sendExecFinal(c, task.ID, 0, "无法建立进程终止句柄: "+err.Error(), false)
		return
	}
	defer killer.Close()

	// 超时强杀。timedOut 必须用原子量：写在定时器协程，读在 Wait() 之后的主协程，
	// 而“杀进程 → Wait 返回”是经由操作系统传递的，不构成 Go 内存模型的 happens-before。
	var timedOut atomic.Bool
	timer := time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		timedOut.Store(true)
		killer.KillTree()
	})

	ch := make(chan chunk, 8)
	var readers sync.WaitGroup
	readers.Add(2)
	go pumpPipe(&readers, ch, "stdout", stdout)
	go pumpPipe(&readers, ch, "stderr", stderr)
	go func() {
		readers.Wait()
		close(ch)
	}()

	// 单一发送协程：保证 Seq 全局单调，服务端可据此重组顺序。
	var sent int
	var total int
	var truncated bool
	broken := false // 连接已断，停止回传但不影响命令继续执行
	for ck := range ch {
		if broken {
			continue // 必须继续排空 channel，否则读取协程阻塞 → 子进程写管道阻塞 → 永不退出
		}
		if total >= protocol.ExecOutputCap {
			truncated = true
			continue
		}
		data := ck.data
		if total+len(data) > protocol.ExecOutputCap {
			data = data[:protocol.ExecOutputCap-total]
			truncated = true
		}
		total += len(data)
		err := c.send(protocol.AgentMsg{Type: "exec_result", Exec: &protocol.ExecResult{
			ID: task.ID, Seq: sent, Stream: ck.stream, Data: data,
		}})
		if err != nil {
			// 连接断了。不杀进程：部署跑到一半被中断，比拿不到输出更糟。
			log.Printf("回传执行输出失败: %v，命令继续在本机执行", err)
			broken = true
			continue
		}
		sent++
	}

	waitErr := cmd.Wait()
	timer.Stop()
	// 进程已回收，立即让 killer 失效，把「超时回调恰好在此刻触发」的窗口压到最小。
	// Close 幂等，与函数出口的 defer 重复调用无害。
	killer.Close()

	if timedOut.Load() {
		sendExecFinal(c, task.ID, sent, "执行超时，进程树已终止", truncated)
		return
	}
	exitCode := 0
	if waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			sendExecFinal(c, task.ID, sent, "等待进程结束失败: "+waitErr.Error(), truncated)
			return
		}
	}
	if broken {
		return // 连接已断，收尾消息没有去处
	}
	if err := c.send(protocol.AgentMsg{Type: "exec_result", Exec: &protocol.ExecResult{
		ID: task.ID, Seq: sent, Done: true, ExitCode: exitCode, Truncated: truncated,
	}}); err != nil {
		log.Printf("回传执行结果失败: %v", err)
	}
}

// pumpPipe 持续读取管道并按 ExecChunkSize 切片投递，直到管道关闭。
func pumpPipe(wg *sync.WaitGroup, ch chan<- chunk, stream string, rc io.ReadCloser) {
	defer wg.Done()
	buf := make([]byte, protocol.ExecChunkSize)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ch <- chunk{stream: stream, data: data}
		}
		if err != nil {
			return // 含 io.EOF：进程退出后管道关闭
		}
	}
}

// sendExecFinal 发送一条终结分片。用于拒绝、启动失败、超时等无输出的收尾场景。
func sendExecFinal(c sender, id string, seq int, errMsg string, truncated bool) {
	if id == "" {
		log.Printf("拒绝执行任务: %s", errMsg)
		return
	}
	if err := c.send(protocol.AgentMsg{Type: "exec_result", Exec: &protocol.ExecResult{
		ID: id, Seq: seq, Done: true, Error: errMsg, Truncated: truncated,
	}}); err != nil {
		log.Printf("回传执行结果失败: %v", err)
	}
}
