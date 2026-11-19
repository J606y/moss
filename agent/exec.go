// moss-agent 命令执行端：接收服务端下发的执行任务，流式回传输出或托管 detach 后台长任务。
//
// 安全前提：执行能力默认关闭。装了 agent 不等于接受被远程操作，
// 必须通过 --allow-exec 或 MOSS_ALLOW_EXEC=1 显式开启。
package main

import (
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"moss/internal/protocol"
)

const (
	execMaxConcurrent  = 4                // 同时执行的流式命令数上限，防止一次性下发过多把机器压垮
	execSeenTTL        = 30 * time.Minute // 幂等记录保留时长，超时后清理避免无限增长
	execDefaultTimeout = 60               // 服务端未指定超时时的默认值（秒）
	detachKeepAge      = 7 * 24 * time.Hour

	// execDrainGrace 是强杀之后仍等待输出管道关闭的宽限。
	//
	// 这个上限是必须的，不是保险。systemd 可用时 KillTree 走 cgroup、收得走
	// 逃逸子孙（见 exec_unix.go），但容器里、非 root 无 polkit 授权时会回退到
	// 进程组信号——那时已经 setsid 的子孙不在原进程组内、杀不掉，
	// 它们会继续持有 stdout 管道的写端，
	// 于是 pumpPipe 永远读不到 EOF、ch 永不关闭、run 永久挂起——
	// 而 run 挂起意味着 defer r.done() 不执行、并发槽永不回落。
	// 累计 execMaxConcurrent 次之后，这台机器再也无法执行任何命令，
	// 只能重启 agent，且服务端只看到宽限超时，没有任何线索指向真因。
	//
	// 触发它不需要恶意：`setsid sleep 3600 &`、自行 fork+setsid 守护化的程序、
	// 部分 `xxx start` 脚本都会。panel_update 的下发命令刻意写了
	// `>/dev/null 2>&1 < /dev/null` 来避开，但 agent 侧本身必须有兜底。
	execDrainGrace = 5 * time.Second
)

// sender 是执行端回传消息的出口。抽成接口而非直接依赖 *client，
// 是为了让执行逻辑（超时强杀、分片、截断、后台托管）能脱离真实 WebSocket 连接被测试。
type sender interface {
	send(v any) error
}

type activeTask struct {
	killer   processKiller
	canceled atomic.Bool
}

// execRunner 管理命令执行的准入、幂等与并发。
// 生命周期跨越 WS 重连：幂等记录必须在重连后依然有效，
// 否则服务端重发同一任务会导致二次执行（例如重复跑一次部署）。
type execRunner struct {
	allow  bool
	jobDir string // detach 后台托管任务的落盘根目录

	mu      sync.Mutex
	seen    map[string]time.Time // 任务 ID → 首次受理时间
	running int
	active  map[string]*activeTask // 普通在途任务的终止句柄，供 cancel 终止
}

func newExecRunner(allow bool) *execRunner {
	return &execRunner{
		allow:  allow,
		jobDir: defaultDetachBaseDir(),
		seen:   make(map[string]time.Time),
		active: make(map[string]*activeTask),
	}
}

// defaultDetachBaseDir 选择 detach 后台任务的落盘目录。
//
// 优先用 /var/lib/moss-agent/jobs（跨重启持久且不与 systemd PrivateTmp 隔离冲突）；
// 非 root 环境（如本地单测）写不进去时回退到临时目录。
func defaultDetachBaseDir() string {
	const prodDir = "/var/lib/moss-agent/jobs"
	if err := os.MkdirAll(prodDir, 0o700); err == nil {
		return prodDir
	}
	fallback := filepath.Join(os.TempDir(), "moss-jobs")
	_ = os.MkdirAll(fallback, 0o700)
	return fallback
}

// sanitizeJobToken 过滤任务 ID 中的特殊字符，仅保留字母、数字、横线与下划线。
func sanitizeJobToken(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// jobUnit 生成 detach 后台托管任务的 systemd 单元名（moss-job-<id>）。
func jobUnit(id string) string {
	return "moss-job-" + sanitizeJobToken(id)
}

// jobPath 返回某个 detach 任务在磁盘上的工作目录路径。
func (r *execRunner) jobPath(id string) string {
	tok := sanitizeJobToken(id)
	if tok == "" {
		tok = "hex_" + hex.EncodeToString([]byte(id))
	}
	base := r.jobDir
	if base == "" {
		base = defaultDetachBaseDir()
	}
	return filepath.Join(base, tok)
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
	if task.Detach {
		go func() {
			// systemd-run 拉起后台服务后立刻返回，并发槽随即释放，长任务不占流式槽位
			defer r.done()
			r.runDetach(c, task)
		}()
		return
	}
	go func() {
		defer r.done()
		r.run(c, task)
	}()
}

func (r *execRunner) runDetach(c sender, task protocol.ExecTask) {
	timeout := task.Timeout
	if timeout <= 0 {
		timeout = protocol.ExecDetachDefaultTimeout
	}
	if timeout > protocol.ExecDetachMaxTimeout {
		timeout = protocol.ExecDetachMaxTimeout
	}
	unit, err := r.startDetachedJob(task, timeout)
	if err != nil {
		sendExecFinal(c, task.ID, 0, err.Error(), false)
		return
	}
	if err := c.send(protocol.AgentMsg{
		Type: "exec_result",
		Exec: &protocol.ExecResult{
			ID:       task.ID,
			Done:     true,
			Detached: true,
			Unit:     unit,
		},
	}); err != nil {
		log.Printf("回传 detach 启动结果失败: %v", err)
	}
}

// HandleJobQuery 处理服务端发来的后台任务状态与日志分段读取请求。
func (r *execRunner) HandleJobQuery(c sender, q protocol.JobQuery) {
	go func() {
		st := r.queryDetachedJob(q)
		if err := c.send(protocol.AgentMsg{Type: "job_status", JobStatus: &st}); err != nil {
			log.Printf("回传任务状态失败: %v", err)
		}
	}()
}

// HandleJobCancel 处理服务端发来的任务取消请求（兼顾普通在途任务与 detach 后台任务）。
func (r *execRunner) HandleJobCancel(c sender, req protocol.JobCancel) {
	go func() {
		r.mu.Lock()
		act := r.active[req.JobID]
		r.mu.Unlock()
		if act != nil {
			act.canceled.Store(true)
			act.killer.KillTree()
			st := protocol.JobStatus{
				ReqID:    req.ReqID,
				JobID:    req.JobID,
				Found:    true,
				Running:  false,
				Canceled: true,
				ExitCode: -1,
				Error:    "任务已取消",
			}
			_ = c.send(protocol.AgentMsg{Type: "job_status", JobStatus: &st})
			return
		}
		st := r.cancelDetachedJob(req)
		if err := c.send(protocol.AgentMsg{Type: "job_status", JobStatus: &st}); err != nil {
			log.Printf("回传任务取消结果失败: %v", err)
		}
	}()
}

// parseDetachStatusFile 解析状态文件内容（格式：<kind>:<code>）。
func parseDetachStatusFile(raw string) (canceled bool, exitCode int, errMsg string) {
	raw = strings.TrimSpace(raw)
	kind, codeStr, _ := strings.Cut(raw, ":")
	code, err := strconv.Atoi(strings.TrimSpace(codeStr))
	if err != nil {
		code = -1
	}
	switch kind {
	case "exited", "success", "exit-code":
		if kind == "success" && codeStr == "" {
			code = 0
		}
		return false, code, ""
	case "canceled":
		return true, -1, "任务已取消"
	case "timeout":
		return false, -1, "执行超时，后台任务已由 systemd 终止 (RuntimeMaxSec)"
	case "oom-kill":
		return false, -1, "后台任务因内存不足 (OOM) 被终止"
	case "lost":
		return false, -1, "后台任务单元已停止，未捕获到退出码"
	default:
		return false, code, "后台任务异常终止 (" + raw + ")"
	}
}

// writeDetachStatusAtomic 原子写入状态文件，避免读取方看到半截内容。
func writeDetachStatusAtomic(dir, content string) error {
	statusPath := filepath.Join(dir, "status")
	tmpPath := filepath.Join(dir, "status.tmp")
	if err := os.WriteFile(tmpPath, []byte(content+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, statusPath)
}

// readLogSlice 从磁盘日志文件按 offset 或 tail 读取一段字节（上限收敛至 ExecChunkSize）。
//
// limit < 0 表示仅获取当前文件总大小而不读取内容。
func readLogSlice(logPath string, offset, tailBytes int64, limit int) (data []byte, actualOffset, nextOffset, totalBytes int64) {
	fi, err := os.Stat(logPath)
	if err != nil {
		return nil, 0, 0, 0
	}
	totalBytes = fi.Size()
	if limit < 0 {
		return nil, totalBytes, totalBytes, totalBytes
	}
	if limit == 0 || limit > protocol.ExecChunkSize {
		limit = protocol.ExecChunkSize
	}
	start := offset
	if tailBytes > 0 {
		start = totalBytes - tailBytes
	}
	if start < 0 {
		start = 0
	}
	if start > totalBytes {
		start = totalBytes
	}
	remain := totalBytes - start
	if remain <= 0 {
		return nil, start, start, totalBytes
	}
	toRead := int64(limit)
	if remain < toRead {
		toRead = remain
	}
	f, err := os.Open(logPath)
	if err != nil {
		return nil, start, start, totalBytes
	}
	defer f.Close()
	buf := make([]byte, toRead)
	n, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) && n == 0 {
		return nil, start, start, totalBytes
	}
	buf = buf[:n]
	return buf, start, start + int64(n), totalBytes
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
	cmd := buildShellCmd(task.ID, task.Cmd)
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

	killer, err := newProcessKiller(task.ID, cmd)
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
	act := &activeTask{killer: killer}
	r.mu.Lock()
	r.active[task.ID] = act
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.active, task.ID)
		r.mu.Unlock()
	}()

	var timedOut atomic.Bool
	var forcedDrain atomic.Bool
	timer := time.AfterFunc(time.Duration(timeout)*time.Second, func() {
		timedOut.Store(true)
		killer.KillTree()

		// 强杀之后再给一段宽限收尾。到点仍未 EOF，说明有子孙逃出了进程组、
		// 正攥着管道写端不放——这时主动关掉父进程侧的读端，逼 pumpPipe 的
		// Read 出错退出，ch 才能关闭、run 才能返回、并发槽才能释放。
		//
		// 关读端而不是直接 return 不排空：后者会漏掉两个 pumpPipe goroutine
		// 和一对管道 fd，把「槽泄漏」换成「fd 泄漏」，没解决问题。
		//
		// 也不能改成并发 cmd.Wait() 来提前得知进程已退出——Wait 在进程回收后
		// 会立即关闭父进程侧管道（closeDescriptors(parentIOPipes)），
		// 那会把还缓冲在管道里、尚未读出的输出直接截断。
		time.AfterFunc(execDrainGrace, func() {
			forcedDrain.Store(true)
			stdout.Close()
			stderr.Close()
		})
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

	if act.canceled.Load() {
		sendExecFinal(c, task.ID, sent, "任务已取消", truncated)
		return
	}
	if timedOut.Load() {
		msg := "执行超时，进程树已终止"
		if forcedDrain.Load() {
			// 有子孙逃出了进程组，输出是被强制掐断的，不是自然结束——
			// 这一点必须说出来，否则使用者会把不完整的输出当成全部。
			msg = "执行超时；有子进程脱离进程组未能终止，输出已强制截断"
			truncated = true
		}
		sendExecFinal(c, task.ID, sent, msg, truncated)
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
