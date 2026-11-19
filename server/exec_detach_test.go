package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

func TestNormalizeTaskTimeout(t *testing.T) {
	normal := protocol.ExecTask{Cmd: "echo hi", Detach: false}
	normalizeTaskTimeout(&normal)
	if normal.Timeout != 60 {
		t.Errorf("普通模式默认超时应为 60，实际 %d", normal.Timeout)
	}
	normalHigh := protocol.ExecTask{Cmd: "echo hi", Timeout: 99999, Detach: false}
	normalizeTaskTimeout(&normalHigh)
	if normalHigh.Timeout != protocol.ExecMaxTimeout {
		t.Errorf("普通模式上限应为 %d，实际 %d", protocol.ExecMaxTimeout, normalHigh.Timeout)
	}

	det := protocol.ExecTask{Cmd: "apt upgrade -y", Detach: true}
	normalizeTaskTimeout(&det)
	if det.Timeout != protocol.ExecDetachDefaultTimeout {
		t.Errorf("detach 模式默认超时应为 %d，实际 %d", protocol.ExecDetachDefaultTimeout, det.Timeout)
	}
	detHigh := protocol.ExecTask{Cmd: "make -j8", Timeout: 200000, Detach: true}
	normalizeTaskTimeout(&detHigh)
	if detHigh.Timeout != protocol.ExecDetachMaxTimeout {
		t.Errorf("detach 模式超时上限应为 86400（24h），实际 %d", detHigh.Timeout)
	}
}

func attachMockAgent(t *testing.T, app *App, serverID, token string, onMsg func(send func(protocol.AgentMsg), msg protocol.ServerMsg)) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/ws", app.handleAgentWS)
	srv := httptest.NewServer(mux)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/agent/ws?token=" + token
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("模拟 agent 连接失败: %v", err)
	}
	t.Cleanup(func() {
		conn.Close()
		srv.CloseClientConnections()
		srv.Close()
	})

	var writeMu sync.Mutex
	send := func(m protocol.AgentMsg) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(m)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if app.hub.AgentConn(serverID) != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if app.hub.AgentConn(serverID) == nil {
		t.Fatalf("agent %s 未能注册到 hub", serverID)
	}

	go func() {
		for {
			var msg protocol.ServerMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			onMsg(send, msg)
		}
	}()
}

// TestMCPDetachLifecycleOffsetTailCancelAndListJobs 验证第一项：
// detach=true 后台托管、offset/limit/tail 分页与尾部读取、跨重启持久化、cancel 与 list_jobs。
func TestMCPDetachLifecycleOffsetTailCancelAndListJobs(t *testing.T) {
	app := scopeTestApp(t)
	fullLog := strings.Repeat("STEP-A-LINE\n", 100) + strings.Repeat("STEP-B-TAIL\n", 100) // 2400 字节

	var mu sync.Mutex
	running := true
	canceled := false
	exitCode := 0

	attachMockAgent(t, app, "s1", "tok-s1", func(send func(protocol.AgentMsg), msg protocol.ServerMsg) {
		switch msg.Type {
		case "exec":
			if msg.Exec != nil && msg.Exec.Detach {
				send(protocol.AgentMsg{
					Type: "exec_result",
					Exec: &protocol.ExecResult{
						ID:       msg.Exec.ID,
						Done:     true,
						Detached: true,
						Unit:     "moss-job-" + msg.Exec.ID + ".service",
					},
				})
			}
		case "job_query":
			q := msg.JobQuery
			mu.Lock()
			isRun := running
			isCan := canceled
			code := exitCode
			mu.Unlock()

			total := int64(len(fullLog))
			start := q.Offset
			if q.Tail > 0 {
				start = total - q.Tail
			}
			if start < 0 {
				start = 0
			}
			if start > total {
				start = total
			}
			end := total
			if q.Limit > 0 && start+int64(q.Limit) < end {
				end = start + int64(q.Limit)
			}
			var data []byte
			if q.Limit >= 0 {
				data = []byte(fullLog[start:end])
			}
			errMsg := ""
			if isCan {
				errMsg = "任务已取消"
			}
			send(protocol.AgentMsg{
				Type: "job_status",
				JobStatus: &protocol.JobStatus{
					ReqID:      q.ReqID,
					JobID:      q.JobID,
					Unit:       "moss-job-" + q.JobID + ".service",
					Found:      true,
					Running:    isRun,
					Canceled:   isCan,
					ExitCode:   code,
					Error:      errMsg,
					Data:       data,
					Offset:     start,
					NextOffset: end,
					TotalBytes: total,
				},
			})
		case "job_cancel":
			c := msg.JobCancel
			mu.Lock()
			running = false
			canceled = true
			exitCode = -1
			mu.Unlock()
			send(protocol.AgentMsg{
				Type: "job_status",
				JobStatus: &protocol.JobStatus{
					ReqID:      c.ReqID,
					JobID:      c.JobID,
					Unit:       "moss-job-" + c.JobID + ".service",
					Found:      true,
					Running:    false,
					Canceled:   true,
					ExitCode:   -1,
					Error:      "任务已取消",
					TotalBytes: int64(len(fullLog)),
					NextOffset: int64(len(fullLog)),
				},
			})
		}
	})

	keyS1 := &apiKey{ID: 1, Name: "ops", Caps: []string{capExec, capRead}, Servers: []string{"s1"}}
	keyS2 := &apiKey{ID: 2, Name: "other", Caps: []string{capExec, capRead}, Servers: []string{"s2"}}

	execRes := app.toolExec(keyS1, json.RawMessage(`{"server_id":"s1","cmd":"apt-get dist-upgrade -y","timeout":7200,"detach":true}`))
	if execRes.IsError {
		t.Fatalf("toolExec detach 不应失败: %s", resultText(execRes))
	}
	var execPayload struct {
		JobID    string `json:"jobId"`
		Status   string `json:"status"`
		Running  bool   `json:"running"`
		Detached bool   `json:"detached"`
		Unit     string `json:"unit"`
		Timeout  int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(resultText(execRes)), &execPayload); err != nil {
		t.Fatalf("解析 exec 响应失败: %v", err)
	}
	if !execPayload.Detached || execPayload.Status != "running" || !execPayload.Running || execPayload.Timeout != 7200 {
		t.Fatalf("detach 返回字段不符: %+v", execPayload)
	}

	// 清空服务端内存（模拟服务端重启或超过 30 分钟 TTL）
	app.exec.mu.Lock()
	app.exec.jobs = make(map[string]*execJob)
	app.exec.finished = make(map[string]finishedJob)
	app.exec.mu.Unlock()

	r1 := app.toolGetResult(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`","offset":1200,"limit":600}`))
	if r1.IsError {
		t.Fatalf("get_result offset/limit 读取失败: %s", resultText(r1))
	}
	var gr1 struct {
		Status     string `json:"status"`
		Running    bool   `json:"running"`
		Stdout     string `json:"stdout"`
		Output     string `json:"output"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		TotalBytes int64  `json:"totalBytes"`
		HasMore    bool   `json:"hasMore"`
	}
	_ = json.Unmarshal([]byte(resultText(r1)), &gr1)
	if !gr1.Running || gr1.Status != "running" || gr1.Offset != 1200 || gr1.NextOffset != 1800 || !gr1.HasMore || gr1.Output != gr1.Stdout {
		t.Fatalf("offset+limit 分页读取结果不符: %+v", gr1)
	}

	rTail := app.toolGetResult(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`","tail":1}`))
	if rTail.IsError {
		t.Fatalf("get_result tail 读取失败: %s", resultText(rTail))
	}
	var grTail struct {
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		Stdout     string `json:"stdout"`
		Output     string `json:"output"`
	}
	_ = json.Unmarshal([]byte(resultText(rTail)), &grTail)
	if grTail.Offset != 2400-1024 || grTail.NextOffset != 2400 || len(grTail.Stdout) != 1024 || grTail.Output != grTail.Stdout {
		t.Fatalf("tail=1KB 应返回末尾 1024 字节，实际 offset=%d next=%d len=%d", grTail.Offset, grTail.NextOffset, len(grTail.Stdout))
	}

	ljS2 := app.toolListJobs(keyS2, json.RawMessage(`{"running_only":true}`))
	if strings.Contains(resultText(ljS2), execPayload.JobID) {
		t.Fatalf("越权 Key 不应在 list_jobs 中看到 s1 的任务: %s", resultText(ljS2))
	}
	ljS1 := app.toolListJobs(keyS1, json.RawMessage(`{"running_only":true}`))
	if ljS1.IsError || !strings.Contains(resultText(ljS1), execPayload.JobID) {
		t.Fatalf("list_jobs 应列出正在运行的 detach 任务: %s", resultText(ljS1))
	}

	if res := app.toolCancel(keyS2, json.RawMessage(`{"job_id":"`+execPayload.JobID+`"}`)); !res.IsError {
		t.Fatal("越权 cancel 别台机器的任务必须失败")
	}

	cRes := app.toolCancel(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`"}`))
	if cRes.IsError || !strings.Contains(resultText(cRes), `"canceled": true`) {
		t.Fatalf("cancel 应成功取消任务，实际: %s", resultText(cRes))
	}

	ljAfterRun := app.toolListJobs(keyS1, json.RawMessage(`{"running_only":true}`))
	if strings.Contains(resultText(ljAfterRun), execPayload.JobID) {
		t.Fatalf("已取消的任务不应再出现在 running_only 列表中: %s", resultText(ljAfterRun))
	}
	ljAfterAll := app.toolListJobs(keyS1, json.RawMessage(`{}`))
	if !strings.Contains(resultText(ljAfterAll), `"status": "canceled"`) {
		t.Fatalf("list_jobs 应显示任务状态为 canceled，实际: %s", resultText(ljAfterAll))
	}
}

func TestMCPExecDetachWindowsUnsupported(t *testing.T) {
	app := scopeTestApp(t)
	attachMockAgent(t, app, "s1", "tok-s1", func(send func(protocol.AgentMsg), msg protocol.ServerMsg) {
		if msg.Type == "exec" && msg.Exec != nil && msg.Exec.Detach {
			send(protocol.AgentMsg{
				Type: "exec_result",
				Exec: &protocol.ExecResult{
					ID:    msg.Exec.ID,
					Done:  true,
					Error: "Windows 暂不支持 detach 后台托管模式",
				},
			})
		}
	})

	key := &apiKey{ID: 1, Name: "ops", Caps: []string{capExec}, Servers: []string{"s1"}}
	res := app.toolExec(key, json.RawMessage(`{"server_id":"s1","cmd":"dir","detach":true}`))
	if !res.IsError {
		t.Fatalf("Windows 上的 detach 应当报错返回，实际: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "Windows 暂不支持 detach") {
		t.Fatalf("应明确提示 Windows 暂不支持 detach，实际: %s", resultText(res))
	}
}

// TestMCPReadFileChunkedAndBinary 验证第二项：
// read_file 支持按 offset + length 原生分块读取大文件/二进制包，返回 nextOffset、eof、sha256，并拦截敏感凭证路径。
func TestMCPReadFileChunkedAndBinary(t *testing.T) {
	app := scopeTestApp(t)
	// 构造一段含 NUL 字节的模拟二进制 deb 包（70KB，跨两个 32KB WS 帧）
	binaryData := make([]byte, 70000)
	for i := range binaryData {
		binaryData[i] = byte(i % 251)
	}
	binaryData[10] = 0x00

	attachMockAgent(t, app, "s1", "tok-s1", func(send func(protocol.AgentMsg), msg protocol.ServerMsg) {
		if msg.Type == "read" && msg.Read != nil {
			req := msg.Read
			total := int64(len(binaryData))
			start := req.Offset
			if start > total {
				start = total
			}
			limit := req.Length
			if limit <= 0 || limit > protocol.ExecChunkSize {
				limit = protocol.ExecChunkSize
			}
			end := start + int64(limit)
			if end > total {
				end = total
			}
			send(protocol.AgentMsg{
				Type: "read_result",
				Read: &protocol.ReadResult{
					ReqID:      req.ReqID,
					Path:       req.Path,
					Data:       binaryData[start:end],
					Offset:     start,
					NextOffset: end,
					TotalBytes: total,
					EOF:        end >= total,
				},
			})
		}
	})

	keyS1 := &apiKey{ID: 1, Name: "ops", Caps: []string{capRead}, Servers: []string{"s1"}}
	keyS2 := &apiKey{ID: 2, Name: "other", Caps: []string{capRead}, Servers: []string{"s2"}}

	// 1. 越权与敏感路径拦截
	if res := app.toolReadFile(keyS2, json.RawMessage(`{"server_id":"s1","path":"/tmp/kernel.deb"}`)); !res.IsError {
		t.Fatal("越权读取别台机器的文件必须失败")
	}
	if res := app.toolReadFile(keyS1, json.RawMessage(`{"server_id":"s1","path":"/etc/shadow"}`)); !res.IsError {
		t.Fatal("读取 /etc/shadow 必须被拦截")
	}
	if res := app.toolReadFile(keyS1, json.RawMessage(`{"server_id":"s1","path":"/etc/moss-agent.env"}`)); !res.IsError {
		t.Fatal("读取 /etc/moss-agent.env 必须被拦截")
	}

	// 2. 第一块读取 50000 字节（底层自动分 32KB + 18KB 两帧拉取，auto 自动识别为 base64）
	r1 := app.toolReadFile(keyS1, json.RawMessage(`{"server_id":"s1","path":"/tmp/kernel.deb","offset":0,"length":50000}`))
	if r1.IsError {
		t.Fatalf("read_file 第一块不应失败: %s", resultText(r1))
	}
	var p1 struct {
		Status     string `json:"status"`
		Encoding   string `json:"encoding"`
		Content    string `json:"content"`
		Bytes      int    `json:"bytes"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		TotalBytes int64  `json:"totalBytes"`
		HasMore    bool   `json:"hasMore"`
		EOF        bool   `json:"eof"`
		SHA256     string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(resultText(r1)), &p1); err != nil {
		t.Fatalf("解析 read_file 响应失败: %v", err)
	}
	if p1.Status != "completed" || p1.Encoding != "base64" || p1.Bytes != 50000 || p1.NextOffset != 50000 || !p1.HasMore || p1.EOF || p1.SHA256 == "" {
		t.Fatalf("read_file 第一块元数据不符: %+v", p1)
	}
	raw1, err := base64.StdEncoding.DecodeString(p1.Content)
	if err != nil || len(raw1) != 50000 || string(raw1) != string(binaryData[:50000]) {
		t.Fatalf("read_file 第一块内容解码不匹配")
	}

	// 3. 第二块从 nextOffset=50000 读到 EOF
	r2 := app.toolReadFile(keyS1, json.RawMessage(`{"server_id":"s1","path":"/tmp/kernel.deb","offset":50000,"length":50000}`))
	if r2.IsError {
		t.Fatalf("read_file 第二块不应失败: %s", resultText(r2))
	}
	var p2 struct {
		Content    string `json:"content"`
		Bytes      int    `json:"bytes"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		HasMore    bool   `json:"hasMore"`
		EOF        bool   `json:"eof"`
	}
	_ = json.Unmarshal([]byte(resultText(r2)), &p2)
	if p2.Offset != 50000 || p2.Bytes != 20000 || p2.NextOffset != 70000 || p2.HasMore || !p2.EOF {
		t.Fatalf("read_file 第二块元数据不符: %+v", p2)
	}
	raw2, _ := base64.StdEncoding.DecodeString(p2.Content)
	if string(append(raw1, raw2...)) != string(binaryData) {
		t.Fatal("分块拼接后的文件与源文件不一致")
	}
}

// TestMCPStateMachineAndDecoupledExitCode 验证第三项：
//  1. get_result 始终包含统一的 status 字段与合并后的 output 字段（content 文本与 structuredContent 一致）；
//  2. 命令非零退出码（exitCode != 0）与通道异常解耦，不再置 isError=true；
//  3. 节点离线/掉线返回结构化 JSON（含 status="offline"/"disconnected"），不再是无结构纯文本。
func TestMCPStateMachineAndDecoupledExitCode(t *testing.T) {
	app := scopeTestApp(t)

	// 1. 正在运行的任务：get_result 必须带 status="running" 与 output
	runningJob := newTestJob("job-running", "s1")
	runningJob.addChunk(execChunk{seq: 0, stream: "stdout", data: []byte("progress-1\n")})
	app.exec.jobs["job-running"] = runningJob

	key := &apiKey{ID: 1, Name: "ops", Caps: []string{capExec, capRead}, Servers: []string{"s1"}}
	resRun := app.toolGetResult(key, json.RawMessage(`{"job_id":"job-running"}`))
	if resRun.IsError {
		t.Fatalf("运行中任务不应报错: %s", resultText(resRun))
	}
	var payloadRun struct {
		Status  string `json:"status"`
		Running bool   `json:"running"`
		Stdout  string `json:"stdout"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(resultText(resRun)), &payloadRun); err != nil {
		t.Fatalf("content 文本应是合法 JSON: %v", err)
	}
	if payloadRun.Status != "running" || !payloadRun.Running || payloadRun.Output != "progress-1\n" {
		t.Fatalf("运行中状态字段或 output 不符: %+v", payloadRun)
	}
	delete(app.exec.jobs, "job-running")

	// 2. 命令正常结束但返回非零退出码（如 grep 未命中返回 exitCode=1）：
	//    isError 必须为 false，status 为 "completed"，ok 为 false，output 合并 stdout 与 stderr。
	app.exec.remember("job-nonzero", "s1", ExecOutcome{
		JobID:    "job-nonzero",
		ExitCode: 1,
		Stdout:   "some out",
		Stderr:   "some err",
	})
	resNZ := app.toolGetResult(key, json.RawMessage(`{"job_id":"job-nonzero"}`))
	if resNZ.IsError {
		t.Fatalf("非零退出码应与通道错误解耦，不应置 isError=true: %s", resultText(resNZ))
	}
	var payloadNZ struct {
		Status   string `json:"status"`
		Running  bool   `json:"running"`
		OK       bool   `json:"ok"`
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal([]byte(resultText(resNZ)), &payloadNZ); err != nil {
		t.Fatalf("解析非零退出码结果失败: %v", err)
	}
	if payloadNZ.Status != "completed" || payloadNZ.Running || payloadNZ.OK || payloadNZ.ExitCode != 1 {
		t.Fatalf("非零退出码状态机不符: %+v", payloadNZ)
	}
	if payloadNZ.Output != "some out\nsome err" {
		t.Fatalf("output 应合并 stdout 与 stderr，实际: %q", payloadNZ.Output)
	}

	// 3. 节点离线时调 exec：返回结构化 JSON（含 status="offline"）且置 isError=true
	resOff := app.toolExec(key, json.RawMessage(`{"server_id":"s1","cmd":"uname -a"}`))
	if !resOff.IsError {
		t.Fatal("离线节点执行应置 isError=true")
	}
	var payloadOff struct {
		Status  string `json:"status"`
		Running bool   `json:"running"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(resultText(resOff)), &payloadOff); err != nil {
		t.Fatalf("离线报错的 content 必须是结构化 JSON 而非裸文本: %v (raw=%s)", err, resultText(resOff))
	}
	if payloadOff.Status != "offline" || payloadOff.Error == "" {
		t.Fatalf("离线状态机字段不符: %+v", payloadOff)
	}
}

// TestMCPBatchExecConcurrentAggregation 验证第四项：
// batch_exec 一次下发并发执行多台在线服务器并在服务端聚合结果返回。
func TestMCPBatchExecConcurrentAggregation(t *testing.T) {
	app := scopeTestApp(t)

	// s1 与 s2 同时在线：s1 返回 exitCode=0，s2 返回 exitCode=1
	attachMockAgent(t, app, "s1", "tok-s1", func(send func(protocol.AgentMsg), msg protocol.ServerMsg) {
		if msg.Type == "exec" && msg.Exec != nil {
			send(protocol.AgentMsg{
				Type: "exec_result",
				Exec: &protocol.ExecResult{
					ID:     msg.Exec.ID,
					Seq:    0,
					Stream: "stdout",
					Data:   []byte("kernel-s1-ok"),
				},
			})
			send(protocol.AgentMsg{
				Type: "exec_result",
				Exec: &protocol.ExecResult{
					ID:       msg.Exec.ID,
					Seq:      1,
					Done:     true,
					ExitCode: 0,
				},
			})
		}
	})
	attachMockAgent(t, app, "s2", "tok-s2", func(send func(protocol.AgentMsg), msg protocol.ServerMsg) {
		if msg.Type == "exec" && msg.Exec != nil {
			send(protocol.AgentMsg{
				Type: "exec_result",
				Exec: &protocol.ExecResult{
					ID:     msg.Exec.ID,
					Seq:    0,
					Stream: "stderr",
					Data:   []byte("cve-detected-on-s2"),
				},
			})
			send(protocol.AgentMsg{
				Type: "exec_result",
				Exec: &protocol.ExecResult{
					ID:       msg.Exec.ID,
					Seq:      1,
					Done:     true,
					ExitCode: 1,
				},
			})
		}
	})

	keyAll := &apiKey{ID: 1, Name: "ops", Caps: []string{capExec, capRead}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	res := app.toolBatchExec(req, keyAll, json.RawMessage(`{"cmd":"uname -r"}`))
	if res.IsError {
		t.Fatalf("batch_exec 不应报错: %s", resultText(res))
	}

	var batch struct {
		Status      string                `json:"status"`
		Total       int                   `json:"total"`
		Completed   int                   `json:"completed"`
		Succeeded   int                   `json:"succeeded"`
		NonZeroExit int                   `json:"nonZeroExit"`
		Failed      int                   `json:"failed"`
		Results     []batchExecNodeResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &batch); err != nil {
		t.Fatalf("解析 batch_exec 响应失败: %v", err)
	}
	if batch.Status != "completed" || batch.Total != 2 || batch.Completed != 2 || batch.Succeeded != 1 || batch.NonZeroExit != 1 || batch.Failed != 0 {
		t.Fatalf("batch_exec 聚合统计不符: %+v", batch)
	}
	if batch.Results[0].ServerID != "s1" || batch.Results[0].Output != "kernel-s1-ok" || !batch.Results[0].OK {
		t.Fatalf("s1 结果不符: %+v", batch.Results[0])
	}
	if batch.Results[1].ServerID != "s2" || batch.Results[1].Output != "cve-detected-on-s2" || batch.Results[1].ExitCode != 1 || batch.Results[1].OK {
		t.Fatalf("s2 结果不符: %+v", batch.Results[1])
	}
}
