package main

import (
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
	// 默认流式模式：默认 60s，上限 1800s
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

	// detach 后台托管模式：默认 3600s（1 小时），上限 86400s（24 小时）
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

// attachMockAgent 建立一条真实的内存 WebSocket 通道连接到 app.handleAgentWS，
// 由 handler 回调模拟 agent 响应（detach 启动、分段日志查询、取消、Windows 不支持等）。
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

	// 等待服务端完成 RegisterAgent
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

// TestMCPDetachLifecycleOffsetTailCancelAndListJobs 完整验证：
//  1. exec 传 detach:true 拉起任务（超时放宽到 24 小时）；
//  2. get_result 传 offset 分段追读增量日志、传 tail 读取末尾 N KB；
//  3. 即使清空服务端内存（模拟超过 30 分钟 TTL 或服务端重启），get_result / list_jobs / cancel 仍能通过数据库元数据正常工作；
//  4. cancel 终止任务并更新数据库状态，list_jobs 正确反映状态并遵守 Key 的机器作用域。
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

	// 1. 下发 detach=true 且 timeout=7200（超过普通模式的 1800s，验证 24h 预算放开）
	execRes := app.toolExec(keyS1, json.RawMessage(`{"server_id":"s1","cmd":"apt-get dist-upgrade -y","timeout":7200,"detach":true}`))
	if execRes.IsError {
		t.Fatalf("toolExec detach 不应失败: %s", resultText(execRes))
	}
	var execPayload struct {
		JobID    string `json:"jobId"`
		Detached bool   `json:"detached"`
		Unit     string `json:"unit"`
		Timeout  int    `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(resultText(execRes)), &execPayload); err != nil {
		t.Fatalf("解析 exec 响应失败: %v", err)
	}
	if !execPayload.Detached || execPayload.Timeout != 7200 || !strings.HasPrefix(execPayload.Unit, "moss-job-") {
		t.Fatalf("detach 返回字段不符: %+v", execPayload)
	}

	// 2. 模拟服务端重启 / 超过 30 分钟内存 TTL：清空内存表，验证完全靠数据库元数据工作
	app.exec.mu.Lock()
	app.exec.jobs = make(map[string]*execJob)
	app.exec.finished = make(map[string]finishedJob)
	app.exec.mu.Unlock()

	// 3. get_result 从 offset=1200 增量读取后半段
	r1 := app.toolGetResult(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`","offset":1200}`))
	if r1.IsError {
		t.Fatalf("get_result offset 读取失败: %s", resultText(r1))
	}
	var gr1 struct {
		Running    bool   `json:"running"`
		Stdout     string `json:"stdout"`
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		TotalBytes int64  `json:"totalBytes"`
	}
	_ = json.Unmarshal([]byte(resultText(r1)), &gr1)
	if !gr1.Running || gr1.Offset != 1200 || gr1.NextOffset != 2400 || !strings.HasPrefix(gr1.Stdout, "STEP-B-TAIL") {
		t.Fatalf("offset 增量读取结果不符: offset=%d next=%d running=%v", gr1.Offset, gr1.NextOffset, gr1.Running)
	}

	// 4. get_result 用 tail=1（末尾 1KB = 1024 字节）读取
	rTail := app.toolGetResult(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`","tail":1}`))
	if rTail.IsError {
		t.Fatalf("get_result tail 读取失败: %s", resultText(rTail))
	}
	var grTail struct {
		Offset     int64  `json:"offset"`
		NextOffset int64  `json:"nextOffset"`
		Stdout     string `json:"stdout"`
	}
	_ = json.Unmarshal([]byte(resultText(rTail)), &grTail)
	if grTail.Offset != 2400-1024 || grTail.NextOffset != 2400 || len(grTail.Stdout) != 1024 {
		t.Fatalf("tail=1KB 应返回末尾 1024 字节，实际 offset=%d next=%d len=%d", grTail.Offset, grTail.NextOffset, len(grTail.Stdout))
	}

	// 5. list_jobs 能列出该在途任务，且 s2 的 Key 看不到它
	ljS2 := app.toolListJobs(keyS2, json.RawMessage(`{"running_only":true}`))
	if strings.Contains(resultText(ljS2), execPayload.JobID) {
		t.Fatalf("越权 Key 不应在 list_jobs 中看到 s1 的任务: %s", resultText(ljS2))
	}
	ljS1 := app.toolListJobs(keyS1, json.RawMessage(`{"running_only":true}`))
	if ljS1.IsError || !strings.Contains(resultText(ljS1), execPayload.JobID) {
		t.Fatalf("list_jobs 应列出正在运行的 detach 任务: %s", resultText(ljS1))
	}

	// 6. s2 的 Key 越权调用 cancel 必须被拒绝
	if res := app.toolCancel(keyS2, json.RawMessage(`{"job_id":"`+execPayload.JobID+`"}`)); !res.IsError {
		t.Fatal("越权 cancel 别台机器的任务必须失败")
	}

	// 7. s1 的 Key 正常 cancel 任务
	cRes := app.toolCancel(keyS1, json.RawMessage(`{"job_id":"`+execPayload.JobID+`"}`))
	if cRes.IsError || !strings.Contains(resultText(cRes), `"canceled": true`) {
		t.Fatalf("cancel 应成功取消任务，实际: %s", resultText(cRes))
	}

	// 8. cancel 之后再查 list_jobs(running_only=true) 应为空，查全部应为 status="canceled"
	ljAfterRun := app.toolListJobs(keyS1, json.RawMessage(`{"running_only":true}`))
	if strings.Contains(resultText(ljAfterRun), execPayload.JobID) {
		t.Fatalf("已取消的任务不应再出现在 running_only 列表中: %s", resultText(ljAfterRun))
	}
	ljAfterAll := app.toolListJobs(keyS1, json.RawMessage(`{}`))
	if !strings.Contains(resultText(ljAfterAll), `"status": "canceled"`) {
		t.Fatalf("list_jobs 应显示任务状态为 canceled，实际: %s", resultText(ljAfterAll))
	}
}

// TestMCPExecDetachWindowsUnsupported 验证 Windows 节点对 detach 返回明确的「不支持」。
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
