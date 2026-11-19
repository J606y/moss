// 服务端命令执行：下发任务、聚合分片回传、后台长任务托管（detach）、分块文件拉取（read_file）、收敛异常、落审计。
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
	// detach 模式与 read_file 单次调用默认/最大聚合读取上限。
	// 每次底层 WS 传输以 protocol.ExecChunkSize（32KB）分帧拉取，不触碰 64KB WS 帧上限。
	detachDefaultReadWindow = 64 << 10
	detachMaxReadWindow     = 256 << 10
	readFileDefaultLength   = 64 << 10
	readFileMaxLength       = 256 << 10
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
	detach   bool

	mu        sync.Mutex
	chunks    []execChunk
	unit      string
	detached  bool
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
		return
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
	Detached   bool   `json:"detached,omitempty"`
	Unit       string `json:"unit,omitempty"`
}

func (j *execJob) outcome() ExecOutcome {
	j.mu.Lock()
	defer j.mu.Unlock()
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
		Detached:   j.detached,
		Unit:       j.unit,
	}
}

type finishedJob struct {
	outcome  ExecOutcome
	at       time.Time
	serverID string
}

type jobWaiter struct {
	serverID string
	ch       chan *protocol.JobStatus
}

type readWaiter struct {
	serverID string
	ch       chan *protocol.ReadResult
}

// execManager 跟踪所有在途执行，并暂存异步任务的结果。
type execManager struct {
	db       *sql.DB
	notifier *Notifier

	mu          sync.Mutex
	jobs        map[string]*execJob
	finished    map[string]finishedJob
	waiters     map[string]jobWaiter
	readWaiters map[string]readWaiter
}

const execBlockedPrefix = "命令被拦截："

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

const execResultTTL = 30 * time.Minute

func newExecManager(db *sql.DB) *execManager {
	return &execManager{
		db:          db,
		jobs:        make(map[string]*execJob),
		finished:    make(map[string]finishedJob),
		waiters:     make(map[string]jobWaiter),
		readWaiters: make(map[string]readWaiter),
	}
}

func (m *execManager) remember(jobID, serverID string, out ExecOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for id, f := range m.finished {
		if now.Sub(f.at) > execResultTTL {
			delete(m.finished, id)
		}
	}
	m.finished[jobID] = finishedJob{outcome: out, at: now, serverID: serverID}
	delete(m.jobs, jobID)
}

func (m *execManager) Result(jobID string) (out ExecOutcome, serverID string, running, found bool) {
	m.mu.Lock()
	now := time.Now()
	if j, ok := m.jobs[jobID]; ok {
		srv := j.serverID
		m.mu.Unlock()
		return j.outcome(), srv, true, true
	}
	if f, ok := m.finished[jobID]; ok {
		if now.Sub(f.at) > execResultTTL {
			delete(m.finished, jobID)
			m.mu.Unlock()
			return ExecOutcome{}, "", false, false
		}
		m.mu.Unlock()
		return f.outcome, f.serverID, false, true
	}
	m.mu.Unlock()
	return ExecOutcome{}, "", false, false
}

func (m *execManager) isRunningInMemory(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.jobs[jobID]
	return ok
}

func sanitizeUnitName(id string) string {
	var b strings.Builder
	b.WriteString("moss-job-")
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	b.WriteString(".service")
	return b.String()
}

func normalizeTaskTimeout(task *protocol.ExecTask) {
	if task.Detach {
		if task.Timeout <= 0 {
			task.Timeout = protocol.ExecDetachDefaultTimeout
		}
		if task.Timeout > protocol.ExecDetachMaxTimeout {
			task.Timeout = protocol.ExecDetachMaxTimeout
		}
		return
	}
	if task.Timeout <= 0 {
		task.Timeout = 60
	}
	if task.Timeout > protocol.ExecMaxTimeout {
		task.Timeout = protocol.ExecMaxTimeout
	}
}

func (m *execManager) prepare(hub *Hub, serverID, caller string, task *protocol.ExecTask) (*execJob, *ExecOutcome, error) {
	if task.ID == "" {
		task.ID = newExecJobID()
	}
	normalizeTaskTimeout(task)

	unit := ""
	if task.Detach {
		unit = sanitizeUnitName(task.ID)
	}
	job := &execJob{
		id:       task.ID,
		serverID: serverID,
		started:  time.Now(),
		detach:   task.Detach,
		unit:     unit,
		done:     make(chan struct{}),
	}

	if err := m.auditStart(job, serverID, caller, *task); err != nil {
		out := ExecOutcome{JobID: task.ID, Error: "审计写入失败，已拒绝执行（无法留痕的操作不予下发）"}
		return nil, &out, err
	}

	if why := checkCommand(task.Cmd, task.Dir); why != "" {
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

func (m *execManager) await(ctx context.Context, job *execJob, timeout int) (ExecOutcome, error) {
	wait := time.Duration(timeout)*time.Second + execWaitGrace
	timer := time.NewTimer(wait)
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
	m.auditFinish(job.id, out)
	return out, failure
}

// Submit 同步执行：阻塞直到执行结束、超时或 agent 掉线。
func (m *execManager) Submit(ctx context.Context, hub *Hub, serverID, caller string, task protocol.ExecTask) (ExecOutcome, error) {
	job, failed, err := m.prepare(hub, serverID, caller, &task)
	if failed != nil {
		return *failed, err
	}
	out, err := m.await(ctx, job, task.Timeout)
	m.unregister(job.id)
	return out, err
}

// Start 异步执行。
func (m *execManager) Start(hub *Hub, serverID, caller string, task protocol.ExecTask) (string, error) {
	job, failed, err := m.prepare(hub, serverID, caller, &task)
	if failed != nil {
		m.remember(task.ID, serverID, *failed)
		return task.ID, err
	}
	if task.Detach {
		timer := time.NewTimer(execWaitGrace)
		defer timer.Stop()
		select {
		case <-job.done:
		case <-timer.C:
			job.finish(0, "等待 agent 启动 detach 后台任务超时", false)
		}
		m.unregister(job.id)
		out := job.outcome()
		if out.Error != "" {
			m.auditFinish(job.id, out)
			m.remember(job.id, serverID, out)
			return job.id, errors.New(out.Error)
		}
		if out.Unit != "" {
			_, _ = m.db.Exec(`UPDATE exec_audit SET unit = ? WHERE job_id = ?`, out.Unit, job.id)
		}
		return job.id, nil
	}
	go func() {
		out, _ := m.await(context.Background(), job, task.Timeout)
		m.remember(job.id, serverID, out)
	}()
	return job.id, nil
}

// SubmitWrite 下发文件写入并等待结果。
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

	if err := m.auditStart(job, serverID, caller, protocol.ExecTask{
		Cmd:     "[write_file] " + task.Path,
		Dir:     "",
		Timeout: 0,
	}); err != nil {
		return ExecOutcome{JobID: task.ID, Error: "审计写入失败，已拒绝写入（无法留痕的操作不予下发）"}, err
	}

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

// requestReadChunk 向在线 agent 发送单帧文件读取请求（上限 32KB）并等待响应。
func (m *execManager) requestReadChunk(hub *Hub, serverID string, task protocol.ReadTask, timeout time.Duration) (*protocol.ReadResult, error) {
	conn := hub.AgentConn(serverID)
	if conn == nil {
		return nil, errAgentOffline
	}
	ch := make(chan *protocol.ReadResult, 1)
	m.mu.Lock()
	m.readWaiters[task.ReqID] = readWaiter{serverID: serverID, ch: ch}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.readWaiters, task.ReqID)
		m.mu.Unlock()
	}()

	if err := conn.send(protocol.ServerMsg{Type: "read", Read: &task}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case res, ok := <-ch:
		if !ok || res == nil {
			return nil, errAgentOffline
		}
		return res, nil
	case <-timer.C:
		return nil, errExecTimeout
	}
}

// SubmitRead 向目标机分块拉取文件内容（单次最多拉取 readFileMaxLength=256KB，底层按 32KB WS 帧拼接）。
func (m *execManager) SubmitRead(hub *Hub, serverID, path string, offset int64, length int) (*protocol.ReadResult, error) {
	if why := checkProtectedReadPath(path); why != "" {
		return nil, errors.New(execBlockedPrefix + why)
	}
	if offset < 0 {
		offset = 0
	}
	if length <= 0 {
		length = readFileDefaultLength
	}
	if length > readFileMaxLength {
		length = readFileMaxLength
	}

	firstChunk := protocol.ExecChunkSize
	if length < firstChunk {
		firstChunk = length
	}
	reqID := "r_" + randString(16)
	first, err := m.requestReadChunk(hub, serverID, protocol.ReadTask{
		ReqID:  reqID,
		Path:   path,
		Offset: offset,
		Length: firstChunk,
	}, 15*time.Second)
	if err != nil {
		return nil, err
	}
	if first.Error != "" {
		return first, errors.New(first.Error)
	}

	buf := append([]byte(nil), first.Data...)
	startOffset := first.Offset
	currNext := first.NextOffset

	for !first.EOF && currNext < first.TotalBytes && len(buf) < length && len(first.Data) > 0 {
		rem := length - len(buf)
		if rem > protocol.ExecChunkSize {
			rem = protocol.ExecChunkSize
		}
		nextReqID := "r_" + randString(16)
		next, err := m.requestReadChunk(hub, serverID, protocol.ReadTask{
			ReqID:  nextReqID,
			Path:   path,
			Offset: currNext,
			Length: rem,
		}, 15*time.Second)
		if err != nil || next.Error != "" || len(next.Data) == 0 {
			break
		}
		buf = append(buf, next.Data...)
		currNext = next.NextOffset
		first.TotalBytes = next.TotalBytes
		first.EOF = next.EOF
	}

	first.Data = buf
	first.Offset = startOffset
	first.NextOffset = currNext
	first.EOF = currNext >= first.TotalBytes
	return first, nil
}

// OnResult 收下 agent 回传的一片结果。
func (m *execManager) OnResult(fromServer string, res *protocol.ExecResult) {
	if res == nil {
		return
	}
	m.mu.Lock()
	job := m.jobs[res.ID]
	m.mu.Unlock()
	if job == nil {
		return
	}
	if job.serverID != fromServer {
		log.Printf("丢弃越权回传: 机器 %s 试图写入归属于 %s 的任务 %s", fromServer, job.serverID, res.ID)
		return
	}
	if len(res.Data) > 0 {
		job.addChunk(execChunk{seq: res.Seq, stream: res.Stream, data: res.Data})
	}
	if res.Detached || res.Unit != "" {
		job.mu.Lock()
		job.detached = res.Detached
		if res.Unit != "" {
			job.unit = res.Unit
		}
		job.mu.Unlock()
	}
	if res.Done {
		job.finish(res.ExitCode, res.Error, res.Truncated)
	}
}

// OnJobStatus 收下 agent 对 JobQuery / JobCancel 的状态与日志回执。
func (m *execManager) OnJobStatus(fromServer string, st *protocol.JobStatus) {
	if st == nil || st.ReqID == "" {
		return
	}
	m.mu.Lock()
	w, ok := m.waiters[st.ReqID]
	if ok && w.serverID == fromServer {
		delete(m.waiters, st.ReqID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	if w.serverID != fromServer {
		log.Printf("丢弃越权状态回执: 机器 %s 试图响应归属于 %s 的请求 %s", fromServer, w.serverID, st.ReqID)
		return
	}
	select {
	case w.ch <- st:
	default:
	}
}

// OnReadResult 收下 agent 对分块文件读取的响应。
func (m *execManager) OnReadResult(fromServer string, res *protocol.ReadResult) {
	if res == nil || res.ReqID == "" {
		return
	}
	m.mu.Lock()
	w, ok := m.readWaiters[res.ReqID]
	if ok && w.serverID == fromServer {
		delete(m.readWaiters, res.ReqID)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	if w.serverID != fromServer {
		log.Printf("丢弃越权文件读取回执: 机器 %s 试图响应归属于 %s 的请求 %s", fromServer, w.serverID, res.ReqID)
		return
	}
	select {
	case w.ch <- res:
	default:
	}
}

// OnAgentGone agent 掉线时立即收敛其名下所有在途流式任务，并唤醒等待中的状态/文件查询。
func (m *execManager) OnAgentGone(serverID string) {
	m.mu.Lock()
	var affected []*execJob
	for _, job := range m.jobs {
		if job.serverID == serverID {
			affected = append(affected, job)
		}
	}
	var pendingWaiters []chan *protocol.JobStatus
	for reqID, w := range m.waiters {
		if w.serverID == serverID {
			pendingWaiters = append(pendingWaiters, w.ch)
			delete(m.waiters, reqID)
		}
	}
	var pendingReads []chan *protocol.ReadResult
	for reqID, w := range m.readWaiters {
		if w.serverID == serverID {
			pendingReads = append(pendingReads, w.ch)
			delete(m.readWaiters, reqID)
		}
	}
	m.mu.Unlock()

	for _, job := range affected {
		job.finish(0, "执行期间 agent 掉线，输出已中断；命令可能仍在目标机上继续运行", false)
	}
	for _, ch := range pendingWaiters {
		close(ch)
	}
	for _, ch := range pendingReads {
		close(ch)
	}
}

func (m *execManager) requestJobStatus(hub *Hub, serverID string, msg protocol.ServerMsg, reqID string, timeout time.Duration) (*protocol.JobStatus, error) {
	conn := hub.AgentConn(serverID)
	if conn == nil {
		return nil, errAgentOffline
	}
	ch := make(chan *protocol.JobStatus, 1)
	m.mu.Lock()
	m.waiters[reqID] = jobWaiter{serverID: serverID, ch: ch}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.waiters, reqID)
		m.mu.Unlock()
	}()

	if err := conn.send(msg); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case st, ok := <-ch:
		if !ok || st == nil {
			return nil, errAgentOffline
		}
		return st, nil
	case <-timer.C:
		return nil, errExecTimeout
	}
}

// QueryDetached 向目标机 agent 分段拉取后台托管任务（detach=true）的最新状态与磁盘日志切片。
func (m *execManager) QueryDetached(hub *Hub, serverID, jobID string, offset, tailKB, limitBytes int64) (*protocol.JobStatus, error) {
	var wantBytes int64 = detachDefaultReadWindow
	if limitBytes > 0 {
		wantBytes = limitBytes
		if wantBytes > detachMaxReadWindow {
			wantBytes = detachMaxReadWindow
		}
	}
	var tailBytes int64
	if tailKB > 0 {
		tailBytes = tailKB * 1024
		if tailBytes > detachMaxReadWindow {
			tailBytes = detachMaxReadWindow
		}
		wantBytes = tailBytes
	}
	if offset < 0 {
		offset = 0
	}

	firstLimit := protocol.ExecChunkSize
	if wantBytes < int64(firstLimit) {
		firstLimit = int(wantBytes)
	}
	reqID := "q_" + randString(16)
	first, err := m.requestJobStatus(hub, serverID, protocol.ServerMsg{
		Type: "job_query",
		JobQuery: &protocol.JobQuery{
			ReqID:  reqID,
			JobID:  jobID,
			Offset: offset,
			Tail:   tailBytes,
			Limit:  firstLimit,
		},
	}, reqID, 10*time.Second)
	if err != nil {
		return nil, err
	}
	if !first.Found {
		return first, nil
	}

	buf := append([]byte(nil), first.Data...)
	startOffset := first.Offset
	currNext := first.NextOffset

	for currNext < first.TotalBytes && int64(len(buf)) < wantBytes && len(first.Data) > 0 {
		rem := int(wantBytes - int64(len(buf)))
		if rem > protocol.ExecChunkSize {
			rem = protocol.ExecChunkSize
		}
		nextReqID := "q_" + randString(16)
		next, err := m.requestJobStatus(hub, serverID, protocol.ServerMsg{
			Type: "job_query",
			JobQuery: &protocol.JobQuery{
				ReqID:  nextReqID,
				JobID:  jobID,
				Offset: currNext,
				Limit:  rem,
			},
		}, nextReqID, 10*time.Second)
		if err != nil || !next.Found || len(next.Data) == 0 {
			break
		}
		buf = append(buf, next.Data...)
		currNext = next.NextOffset
		first.TotalBytes = next.TotalBytes
		first.Running = next.Running
		first.Canceled = next.Canceled
		first.ExitCode = next.ExitCode
		first.Error = next.Error
	}

	first.Data = buf
	first.Offset = startOffset
	first.NextOffset = currNext

	m.syncDetachedAudit(jobID, first)
	return first, nil
}

func (m *execManager) syncDetachedAudit(jobID string, st *protocol.JobStatus) {
	if st == nil || !st.Found {
		return
	}
	if st.Running {
		_, _ = m.db.Exec(`UPDATE exec_audit SET log_bytes = ?, unit = COALESCE(NULLIF(?, ''), unit) WHERE job_id = ?`,
			st.TotalBytes, st.Unit, jobID)
		return
	}
	var finishedAt int64
	var existingStdout string
	if err := m.db.QueryRow(`SELECT finished_at, stdout FROM exec_audit WHERE job_id = ?`, jobID).
		Scan(&finishedAt, &existingStdout); err != nil {
		return
	}
	if finishedAt == 0 {
		finishedAt = time.Now().UnixMilli()
	}
	stdoutSummary := existingStdout
	if stdoutSummary == "" && len(st.Data) > 0 {
		stdoutSummary = clipForAudit(string(st.Data))
	}
	_, _ = m.db.Exec(
		`UPDATE exec_audit
		    SET finished_at = ?, exit_code = ?, error = ?, canceled = ?, log_bytes = ?,
		        unit = COALESCE(NULLIF(?, ''), unit), stdout = ?
		  WHERE job_id = ?`,
		finishedAt, st.ExitCode, st.Error, boolToInt(st.Canceled), st.TotalBytes,
		st.Unit, stdoutSummary, jobID,
	)
}

type auditJobMeta struct {
	JobID      string
	ServerID   string
	ServerName string
	Caller     string
	Cmd        string
	Dir        string
	Timeout    int
	Detach     bool
	Unit       string
	StartedAt  int64
	FinishedAt int64
	ExitCode   int
	Error      string
	Canceled   bool
	LogBytes   int64
	Stdout     string
	Stderr     string
	Truncated  bool
}

func (m *execManager) lookupAuditJob(jobID string) (*auditJobMeta, error) {
	var (
		meta                   auditJobMeta
		detach, canceled, trun int
	)
	err := m.db.QueryRow(`
		SELECT a.job_id, a.server_id, COALESCE(s.name, ''), a.caller, a.cmd, a.dir, a.timeout,
		       a.detach, a.unit, a.started_at, a.finished_at, a.exit_code, a.error,
		       a.canceled, a.log_bytes, a.stdout, a.stderr, a.truncated
		  FROM exec_audit a LEFT JOIN servers s ON s.id = a.server_id
		 WHERE a.job_id = ?`, jobID).Scan(
		&meta.JobID, &meta.ServerID, &meta.ServerName, &meta.Caller, &meta.Cmd, &meta.Dir, &meta.Timeout,
		&detach, &meta.Unit, &meta.StartedAt, &meta.FinishedAt, &meta.ExitCode, &meta.Error,
		&canceled, &meta.LogBytes, &meta.Stdout, &meta.Stderr, &trun,
	)
	if err != nil {
		return nil, err
	}
	meta.Detach = detach == 1
	meta.Canceled = canceled == 1
	meta.Truncated = trun == 1
	return &meta, nil
}

func (m *execManager) Cancel(hub *Hub, meta *auditJobMeta) (alreadyFinished bool, err error) {
	inMem := m.isRunningInMemory(meta.JobID)
	if meta.FinishedAt > 0 && !inMem {
		return true, nil
	}

	reqID := "c_" + randString(16)
	st, reqErr := m.requestJobStatus(hub, meta.ServerID, protocol.ServerMsg{
		Type: "job_cancel",
		JobCancel: &protocol.JobCancel{
			ReqID: reqID,
			JobID: meta.JobID,
		},
	}, reqID, 10*time.Second)

	m.mu.Lock()
	job := m.jobs[meta.JobID]
	m.mu.Unlock()
	if job != nil {
		job.finish(-1, "任务已取消", false)
	}

	if reqErr != nil && meta.Detach {
		return false, reqErr
	}
	if st != nil && st.Found && !st.Canceled && !st.Running && st.Error != "任务已取消" {
		m.syncDetachedAudit(meta.JobID, st)
		return true, nil
	}

	nowMs := time.Now().UnixMilli()
	var logBytes int64 = meta.LogBytes
	if st != nil && st.TotalBytes > 0 {
		logBytes = st.TotalBytes
	}
	_, _ = m.db.Exec(
		`UPDATE exec_audit
		    SET finished_at = CASE WHEN finished_at = 0 THEN ? ELSE finished_at END,
		        canceled = 1,
		        exit_code = CASE WHEN finished_at = 0 THEN -1 ELSE exit_code END,
		        error = CASE WHEN error = '' THEN '任务已取消' ELSE error END,
		        log_bytes = ?
		  WHERE job_id = ?`,
		nowMs, logBytes, meta.JobID,
	)
	return false, nil
}

func (m *execManager) auditStart(job *execJob, serverID, caller string, task protocol.ExecTask) error {
	if _, err := m.db.Exec(
		`INSERT INTO exec_audit(job_id, server_id, caller, cmd, dir, timeout, started_at, detach, unit)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.id, serverID, caller, task.Cmd, task.Dir, task.Timeout, job.started.UnixMilli(),
		boolToInt(task.Detach), job.unit,
	); err != nil {
		log.Printf("写入执行审计失败，拒绝执行: %v", err)
		return err
	}
	return nil
}

func (m *execManager) auditFinish(jobID string, out ExecOutcome) {
	canceled := out.Error == "任务已取消"
	if _, err := m.db.Exec(
		`UPDATE exec_audit
		    SET finished_at = ?, exit_code = ?, error = ?, stdout = ?, stderr = ?,
		        truncated = ?, canceled = ?, log_bytes = ?
		  WHERE job_id = ?`,
		time.Now().UnixMilli(), out.ExitCode, out.Error,
		clipForAudit(out.Stdout), clipForAudit(out.Stderr),
		boolToInt(out.Truncated), boolToInt(canceled), len(out.Stdout)+len(out.Stderr), jobID,
	); err != nil {
		log.Printf("更新执行审计失败: %v", err)
	}
}

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
