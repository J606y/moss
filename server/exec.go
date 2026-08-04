// 服务端命令执行：下发任务、聚合分片回传、收敛异常、落审计。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"moss/internal/protocol"
)

var (
	errAgentOffline = errors.New("目标服务器的 agent 未连接")
	errExecTimeout  = errors.New("等待执行结果超时")
	errExecBlocked  = errors.New("命令被拦截")
)

const (
	// 审计里保存的输出上限。完整输出最大 1MB，全量入库会让 SQLite 迅速膨胀，
	// 排查问题时头部若干 KB 已足够。
	execAuditOutputCap = 32 << 10
	// 服务端等待结果的宽限：agent 自身超时后仍需时间回传收尾分片，
	// 这里留出余量，避免两端超时打架导致本可正常返回的执行被判为失败。
	execWaitGrace = 30 * time.Second
)

// execChunk 是一片带序号的输出，序号用于还原顺序。
type execChunk struct {
	seq    int
	stream string
	data   []byte
}

// execJob 一次执行在服务端的聚合状态。
type execJob struct {
	id       string
	serverID string
	started  time.Time

	mu        sync.Mutex
	chunks    []execChunk
	exitCode  int
	errMsg    string
	truncated bool
	finished  bool
	done      chan struct{}
}

// finish 收敛一次执行。重复调用无副作用——
// agent 回传 Done 与 agent 掉线可能同时发生，谁先到算谁。
func (j *execJob) finish(exitCode int, errMsg string, truncated bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return
	}
	j.finished = true
	j.exitCode = exitCode
	j.errMsg = errMsg
	if truncated {
		j.truncated = true
	}
	close(j.done)
}

func (j *execJob) addChunk(c execChunk) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.finished {
		return // 收敛之后到达的分片直接丢弃
	}
	j.chunks = append(j.chunks, c)
}

// ExecOutcome 一次执行的最终结果。
type ExecOutcome struct {
	JobID      string `json:"jobId"`
	ExitCode   int    `json:"exitCode"`
	Error      string `json:"error,omitempty"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"durationMs"`
}

func (j *execJob) outcome() ExecOutcome {
	j.mu.Lock()
	defer j.mu.Unlock()
	// 按 seq 还原顺序后再分流：不依赖传输层的到达顺序假设。
	sort.SliceStable(j.chunks, func(a, b int) bool { return j.chunks[a].seq < j.chunks[b].seq })
	var out, errOut strings.Builder
	for _, c := range j.chunks {
		if c.stream == "stderr" {
			errOut.Write(c.data)
		} else {
			out.Write(c.data)
		}
	}
	return ExecOutcome{
		JobID:      j.id,
		ExitCode:   j.exitCode,
		Error:      j.errMsg,
		Stdout:     out.String(),
		Stderr:     errOut.String(),
		Truncated:  j.truncated,
		DurationMs: time.Since(j.started).Milliseconds(),
	}
}

// finishedJob 是已收敛任务的完整结果，供异步调用方回来取。
//
// 不直接让调用方查审计表：审计里的输出被截到 32KB 以控制库体积，
// 而 AI 读一段日志时需要的是完整的 1MB。内存留全量，审计留摘要。
type finishedJob struct {
	outcome ExecOutcome
	at      time.Time
}

// execManager 跟踪所有在途执行，并暂存异步任务的结果。
type execManager struct {
	db *sql.DB
	// notifier 由 main.go 注入，仅用于推送拦截告警。
	notifier *Notifier

	mu       sync.Mutex
	jobs     map[string]*execJob
	finished map[string]finishedJob
}

// execBlockedPrefix 拦截记录的错误前缀。
//
// 审计的「仅看拦截」筛选靠它识别，落库与查询共用这一处定义——
// 散落成字面量的话，改一次文案就会让筛选静默失效，而「谁试图执行什么危险操作」
// 恰恰是审计里最不能丢的那类记录。
const execBlockedPrefix = "命令被拦截："

// reportBlocked 落审计并推送拦截告警。
//
// 命令拦截与路径拦截都收敛到这里，保证两条路径的留痕与告警行为一致——
// 「谁试图执行什么危险操作」是审计里最有价值的记录，不能因为拦得早就不记。
func (m *execManager) reportBlocked(job *execJob, serverID, caller, target, reason string) ExecOutcome {
	out := ExecOutcome{JobID: job.id, Error: execBlockedPrefix + reason}
	m.auditFinish(job.id, out)
	if m.notifier != nil {
		name := serverID
		var n string
		if err := m.db.QueryRow(`SELECT name FROM servers WHERE id = ?`, serverID).Scan(&n); err == nil && n != "" {
			name = n
		}
		m.notifier.NotifyBlocked(name, caller, target, reason)
	}
	return out
}

// 异步任务结果的保留时长。超时未取走即丢弃，审计表仍留有截断版记录。
const execResultTTL = 30 * time.Minute

func newExecManager(db *sql.DB) *execManager {
	return &execManager{
		db:       db,
		jobs:     make(map[string]*execJob),
		finished: make(map[string]finishedJob),
	}
}

// remember 暂存异步任务结果，并顺带清理过期项，省去一个常驻 goroutine。
func (m *execManager) remember(jobID string, out ExecOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, f := range m.finished {
		if now.Sub(f.at) > execResultTTL {
			delete(m.finished, id)
		}
	}
	m.finished[jobID] = finishedJob{outcome: out, at: now}
}

// Result 查询任务当前状态。
// running 为 true 表示仍在执行中；found 为 false 表示任务不存在或结果已过期。
func (m *execManager) Result(jobID string) (out ExecOutcome, running, found bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.jobs[jobID]; ok {
		return ExecOutcome{JobID: jobID}, true, true
	}
	if f, ok := m.finished[jobID]; ok {
		return f.outcome, false, true
	}
	return ExecOutcome{}, false, false
}

// prepare 校验、落审计、过拦截闸、下发，并把任务注册进在途表。
// 第二个返回值非 nil 表示尚未下发就已失败，调用方直接把它返回给上层。
func (m *execManager) prepare(hub *Hub, serverID, caller string, task *protocol.ExecTask) (*execJob, *ExecOutcome, error) {
	if task.ID == "" {
		task.ID = newExecJobID()
	}
	if task.Timeout <= 0 {
		task.Timeout = 60
	}
	if task.Timeout > protocol.ExecMaxTimeout {
		task.Timeout = protocol.ExecMaxTimeout
	}

	job := &execJob{
		id:       task.ID,
		serverID: serverID,
		started:  time.Now(),
		done:     make(chan struct{}),
	}

	// 先落审计再下发：服务端若在执行途中崩溃，仍留有「曾经下发过什么」的痕迹。
	// 只在完成后写审计，等于给了一个抹掉记录的窗口。
	m.auditStart(job, serverID, caller, *task)

	// 拦截同样要留痕——「谁试图执行什么危险命令」是审计里最有价值的记录之一。
	if why := checkDestructive(task.Cmd); why != "" {
		out := m.reportBlocked(job, serverID, caller, task.Cmd, why)
		return nil, &out, errExecBlocked
	}

	conn := hub.AgentConn(serverID)
	if conn == nil {
		out := ExecOutcome{JobID: task.ID, Error: errAgentOffline.Error()}
		m.auditFinish(job.id, out)
		return nil, &out, errAgentOffline
	}

	m.mu.Lock()
	m.jobs[job.id] = job
	m.mu.Unlock()

	if err := conn.send(protocol.ServerMsg{Type: "exec", Exec: task}); err != nil {
		m.unregister(job.id)
		out := ExecOutcome{JobID: task.ID, Error: "下发失败: " + err.Error()}
		m.auditFinish(job.id, out)
		return nil, &out, err
	}
	return job, nil, nil
}

func (m *execManager) unregister(jobID string) {
	m.mu.Lock()
	delete(m.jobs, jobID)
	m.mu.Unlock()
}

// await 等待任务收敛，落审计并返回结果。
func (m *execManager) await(ctx context.Context, job *execJob, timeout int) (ExecOutcome, error) {
	defer m.unregister(job.id)

	wait := time.Duration(timeout)*time.Second + execWaitGrace
	timer := time.NewTimer(wait)
	defer timer.Stop()

	var failure error
	select {
	case <-job.done:
	case <-timer.C:
		// agent 完全失联（网络黑洞），连超时收尾分片都没回来。
		job.finish(0, errExecTimeout.Error(), false)
		failure = errExecTimeout
	case <-ctx.Done():
		job.finish(0, "调用方取消: "+ctx.Err().Error(), false)
		failure = ctx.Err()
	}
	out := job.outcome()
	m.auditFinish(job.id, out)
	return out, failure
}

// Submit 同步执行：阻塞直到执行结束、超时或 agent 掉线。
// 适合秒级命令；AI 下发的部署动辄数分钟，应当走 Start。
func (m *execManager) Submit(ctx context.Context, hub *Hub, serverID, caller string, task protocol.ExecTask) (ExecOutcome, error) {
	job, failed, err := m.prepare(hub, serverID, caller, &task)
	if failed != nil {
		return *failed, err
	}
	return m.await(ctx, job, task.Timeout)
}

// Start 异步执行：下发后立即返回 jobID，结果由调用方稍后用 Result 取回。
//
// 后台等待用 context.Background 而非调用方的请求 context——HTTP 请求一返回，
// 请求 context 就被取消，若沿用它，任务会在提交的瞬间被判为「调用方取消」。
func (m *execManager) Start(hub *Hub, serverID, caller string, task protocol.ExecTask) (string, error) {
	job, failed, err := m.prepare(hub, serverID, caller, &task)
	if failed != nil {
		// 下发前就失败也要留住结果：调用方拿到 jobID 后来查，
		// 应当看到「命令被拦截」这类原因，而不是「任务不存在」。
		m.remember(task.ID, *failed)
		return task.ID, err
	}
	go func() {
		out, _ := m.await(context.Background(), job, task.Timeout)
		m.remember(job.id, out)
	}()
	return job.id, nil
}

// SubmitWrite 下发文件写入并等待结果。
//
// 写入是瞬时操作（无输出流、无长耗时），因此保持同步语义，不像 exec 那样提供异步模式。
// 复用 execJob 的收敛机制：agent 侧用同一条 exec_result 通道回传结果。
func (m *execManager) SubmitWrite(ctx context.Context, hub *Hub, serverID, caller string, task protocol.WriteTask) (ExecOutcome, error) {
	if task.ID == "" {
		task.ID = newExecJobID()
	}
	if len(task.Data) > protocol.WriteSizeCap {
		out := ExecOutcome{JobID: task.ID, Error: "内容超过上限"}
		return out, errors.New("内容超过上限")
	}

	job := &execJob{
		id:       task.ID,
		serverID: serverID,
		started:  time.Now(),
		done:     make(chan struct{}),
	}

	// 审计记录写入意图：命令位置存路径，输出位置存文件内容原文。
	// 这是 write_file 独立于 exec 的核心价值——「写了什么」可以逐字节回溯。
	m.auditStart(job, serverID, caller, protocol.ExecTask{
		Cmd:     "[write_file] " + task.Path,
		Dir:     "",
		Timeout: 0,
	})

	// 受保护路径的拦截与命令拦截同级：一样要留痕、一样要告警。
	// 放在这里而非工具层，是为了让两条拦截路径共用同一套审计与推送逻辑。
	if why := checkProtectedPath(task.Path); why != "" {
		out := m.reportBlocked(job, serverID, caller, "[write_file] "+task.Path, why)
		return out, errExecBlocked
	}

	conn := hub.AgentConn(serverID)
	if conn == nil {
		out := ExecOutcome{JobID: task.ID, Error: errAgentOffline.Error()}
		m.auditFinishWrite(job.id, out, task.Data)
		return out, errAgentOffline
	}

	m.mu.Lock()
	m.jobs[job.id] = job
	m.mu.Unlock()
	defer m.unregister(job.id)

	if err := conn.send(protocol.ServerMsg{Type: "write", Write: &task}); err != nil {
		out := ExecOutcome{JobID: task.ID, Error: "下发失败: " + err.Error()}
		m.auditFinishWrite(job.id, out, task.Data)
		return out, err
	}

	timer := time.NewTimer(execWaitGrace)
	defer timer.Stop()

	var failure error
	select {
	case <-job.done:
	case <-timer.C:
		job.finish(0, errExecTimeout.Error(), false)
		failure = errExecTimeout
	case <-ctx.Done():
		job.finish(0, "调用方取消: "+ctx.Err().Error(), false)
		failure = ctx.Err()
	}
	out := job.outcome()
	m.auditFinishWrite(job.id, out, task.Data)
	return out, failure
}

// auditFinishWrite 收尾写入审计，把文件内容存进 stdout 位置以便日后逐字节比对。
func (m *execManager) auditFinishWrite(jobID string, out ExecOutcome, content []byte) {
	if _, err := m.db.Exec(
		`UPDATE exec_audit SET finished_at = ?, exit_code = ?, error = ?, stdout = ?, truncated = ?
		 WHERE job_id = ?`,
		time.Now().UnixMilli(), out.ExitCode, out.Error,
		clipForAudit(string(content)), boolToInt(len(content) > execAuditOutputCap), jobID,
	); err != nil {
		log.Printf("更新写入审计失败: %v", err)
	}
}

// OnResult 处理 agent 回传的输出分片。
func (m *execManager) OnResult(res *protocol.ExecResult) {
	if res == nil {
		return
	}
	m.mu.Lock()
	job := m.jobs[res.ID]
	m.mu.Unlock()
	if job == nil {
		return // 任务已收敛或服务端重启过，迟到的分片无处安放
	}
	if len(res.Data) > 0 {
		job.addChunk(execChunk{seq: res.Seq, stream: res.Stream, data: res.Data})
	}
	if res.Done {
		job.finish(res.ExitCode, res.Error, res.Truncated)
	}
}

// OnAgentGone agent 掉线时立即收敛其名下所有在途任务。
// 不做这件事，调用方会一直等到宽限超时才拿到结果——而结果早已注定拿不到。
func (m *execManager) OnAgentGone(serverID string) {
	m.mu.Lock()
	var affected []*execJob
	for _, job := range m.jobs {
		if job.serverID == serverID {
			affected = append(affected, job)
		}
	}
	m.mu.Unlock()

	for _, job := range affected {
		// 命令可能仍在目标机上继续执行（agent 侧断线不杀进程），
		// 但输出已无从回传，如实告知调用方而非谎称失败。
		job.finish(0, "执行期间 agent 掉线，输出已中断；命令可能仍在目标机上继续运行", false)
	}
}

func (m *execManager) auditStart(job *execJob, serverID, caller string, task protocol.ExecTask) {
	if _, err := m.db.Exec(
		`INSERT INTO exec_audit(job_id, server_id, caller, cmd, dir, timeout, started_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		job.id, serverID, caller, task.Cmd, task.Dir, task.Timeout, job.started.UnixMilli(),
	); err != nil {
		log.Printf("写入执行审计失败: %v", err)
	}
}

func (m *execManager) auditFinish(jobID string, out ExecOutcome) {
	if _, err := m.db.Exec(
		`UPDATE exec_audit SET finished_at = ?, exit_code = ?, error = ?, stdout = ?, stderr = ?, truncated = ?
		 WHERE job_id = ?`,
		time.Now().UnixMilli(), out.ExitCode, out.Error,
		clipForAudit(out.Stdout), clipForAudit(out.Stderr), boolToInt(out.Truncated), jobID,
	); err != nil {
		log.Printf("更新执行审计失败: %v", err)
	}
}

// clipForAudit 截断入库文本，并显式标注截断，避免日后误读为「命令只输出了这些」。
func clipForAudit(s string) string {
	if len(s) <= execAuditOutputCap {
		return s
	}
	return s[:execAuditOutputCap] + "\n…[审计记录已截断]"
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newExecJobID() string { return "job_" + randString(20) }
