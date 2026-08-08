package main

import (
	"encoding/json"
	"strings"

	"moss/internal/protocol"
	"testing"
	"time"
)

// scopeTestApp 造一个带两台机器的 App。
func scopeTestApp(t *testing.T) *App {
	t.Helper()
	app := mcpTestApp(t)
	for _, id := range []string{"s1", "s2"} {
		if _, err := app.db.Exec(
			`INSERT INTO servers(id, name, token, created_at) VALUES(?, ?, ?, ?)`,
			id, "机器"+id, "tok-"+id, time.Now().Unix()); err != nil {
			t.Fatalf("插入测试服务器失败: %v", err)
		}
	}
	return app
}

func resultText(r mcpCallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// TestGetResultEnforcesServerScope get_result 必须过机器作用域这道闸。
//
// 它曾是六个工具里唯一只校验能力、不校验作用域的：任何持 exec 能力的 Key
// 都能读到别的机器上的完整 stdout（最多 1MB）。jobID 有 116 bit 熵、猜不到，
// 所以不是可用漏洞——但 jobId 会经审计界面、日志、AI 会话原文外泄，
// 一旦泄漏就是无闸直读。
func TestGetResultEnforcesServerScope(t *testing.T) {
	app := scopeTestApp(t)

	// 一条打在 s2 上的、已完成的任务
	app.exec.remember("job-on-s2", "s2", ExecOutcome{
		JobID:  "job-on-s2",
		Stdout: "机密输出：数据库密码在这里",
	})

	// 只被授权 s1 的 Key
	scoped := &apiKey{Caps: []string{capExec}, Servers: []string{"s1"}}
	res := app.toolGetResult(scoped, json.RawMessage(`{"job_id":"job-on-s2"}`))
	if !res.IsError {
		t.Fatal("越权读取别台机器的任务结果必须失败")
	}
	if strings.Contains(resultText(res), "机密输出") {
		t.Fatalf("越权时不能泄漏任何输出，实际返回: %s", resultText(res))
	}

	// 授权了 s2 的 Key 应当读得到
	ok := &apiKey{Caps: []string{capExec}, Servers: []string{"s2"}}
	res = app.toolGetResult(ok, json.RawMessage(`{"job_id":"job-on-s2"}`))
	if res.IsError {
		t.Fatalf("已授权的机器应当读得到: %s", resultText(res))
	}
	if !strings.Contains(resultText(res), "机密输出") {
		t.Fatalf("已授权时应返回完整输出，实际: %s", resultText(res))
	}

	// 不限机器（空白名单 = 全部）的 Key 同样读得到
	all := &apiKey{Caps: []string{capExec}}
	if res := app.toolGetResult(all, json.RawMessage(`{"job_id":"job-on-s2"}`)); res.IsError {
		t.Fatalf("不限机器的 Key 应当读得到: %s", resultText(res))
	}
}

// TestGetResultFallsBackToAudit 内存里查不到时回退查审计表。
//
// jobs 与 finished 都是纯内存，服务端一重启就全部归零，而 exec_audit 里明明
// 有记录。此前一律回「jobId 有误」，等于把「服务端重启过」说成「你参数写错了」，
// 会让模型往完全错误的方向排查，甚至把一条非幂等的命令重跑一遍。
func TestGetResultFallsBackToAudit(t *testing.T) {
	app := scopeTestApp(t)

	// 模拟「服务端重启过」：审计表里有，内存里没有
	job := &execJob{id: "restarted", serverID: "s1", started: time.Now(), done: make(chan struct{})}
	if err := app.exec.auditStart(job, "s1", "admin", protocol.ExecTask{Cmd: "echo hi", Timeout: 30}); err != nil {
		t.Fatalf("落审计失败: %v", err)
	}
	app.exec.auditFinish("restarted", ExecOutcome{JobID: "restarted", ExitCode: 0, Stdout: "hi"})

	key := &apiKey{Caps: []string{capExec}, Servers: []string{"s1"}}
	res := app.toolGetResult(key, json.RawMessage(`{"job_id":"restarted"}`))
	if res.IsError {
		t.Fatalf("审计表里有记录时不该报「jobId 有误」: %s", resultText(res))
	}
	txt := resultText(res)
	if !strings.Contains(txt, "hi") {
		t.Errorf("应返回审计里保存的输出，实际: %s", txt)
	}
	if !strings.Contains(txt, "audit") {
		t.Errorf("必须标明这是审计表来源的降级结果（输出被截到 32KB），实际: %s", txt)
	}

	// 降级这条路同样要过作用域闸
	other := &apiKey{Caps: []string{capExec}, Servers: []string{"s2"}}
	if res := app.toolGetResult(other, json.RawMessage(`{"job_id":"restarted"}`)); !res.IsError {
		t.Fatal("走审计降级分支时也必须校验机器作用域")
	}

	// 真的不存在的 jobId 仍然报错
	if res := app.toolGetResult(key, json.RawMessage(`{"job_id":"never-existed"}`)); !res.IsError {
		t.Fatal("不存在的 jobId 应当报错")
	}
}

// TestOnResultRejectsForeignServer agent 不能往别台机器的任务里写结果。
//
// 任一持有合法 token 的 agent（含被入侵的受监控机）此前都能对别的机器的 job
// 注入输出，或直接 Done:true / ExitCode:0 抢先收敛掉，真机输出随后被丢弃。
func TestOnResultRejectsForeignServer(t *testing.T) {
	m := newExecManager(testDB(t))
	job := newTestJob("j-scope", "s1")
	m.jobs["j-scope"] = job

	// s2 冒名往 s1 的任务里塞输出并抢先收敛
	m.OnResult("s2", &protocol.ExecResult{ID: "j-scope", Seq: 0, Stream: "stdout", Data: []byte("伪造输出"), Done: true})

	select {
	case <-job.done:
		t.Fatal("越权回传不应收敛任务：真机的输出会因此被全部丢弃")
	default:
	}
	if out := job.outcome(); strings.Contains(out.Stdout, "伪造") {
		t.Fatalf("越权回传的分片不应被接受，实际 stdout=%q", out.Stdout)
	}

	// 正主回传照常生效
	m.OnResult("s1", &protocol.ExecResult{ID: "j-scope", Seq: 0, Stream: "stdout", Data: []byte("真实输出"), Done: true})
	select {
	case <-job.done:
	case <-time.After(2 * time.Second):
		t.Fatal("归属机器的回传应当正常收敛任务")
	}
	if out := job.outcome(); !strings.Contains(out.Stdout, "真实输出") {
		t.Fatalf("归属机器的输出应被接受，实际 stdout=%q", out.Stdout)
	}
}

// TestResultExpiresOnRead 读取时也要判过期。
//
// remember 只在有新任务落袋时顺带清理，没有新任务的时段里 30 分钟前的结果
// 照样读得到、占的内存也不释放，与工具描述承诺的「结果保留 30 分钟」对不上。
func TestResultExpiresOnRead(t *testing.T) {
	m := newExecManager(testDB(t))
	m.remember("stale", "s1", ExecOutcome{JobID: "stale", Stdout: "旧结果"})

	// 把落袋时间拨到 TTL 之前
	m.mu.Lock()
	f := m.finished["stale"]
	f.at = time.Now().Add(-execResultTTL - time.Minute)
	m.finished["stale"] = f
	m.mu.Unlock()

	if _, _, _, found := m.Result("stale"); found {
		t.Fatal("超过保留期的结果不该还能读到")
	}
	m.mu.Lock()
	_, still := m.finished["stale"]
	m.mu.Unlock()
	if still {
		t.Error("过期结果应在读取时一并清掉，否则内存不释放")
	}
}

// TestAuditFailureBlocksExecution 审计写不进去就不执行。
//
// 原来 INSERT 失败只记一行日志、命令照常下发，之后 auditFinish 的 UPDATE
// 匹配 0 行，这条命令在库里完全无痕。而 MCP 的服务器说明对模型承诺
// 「每次操作都有完整审计记录」，审计失效的那一刻恰恰最需要它。
func TestAuditFailureBlocksExecution(t *testing.T) {
	db := testDB(t)
	m := newExecManager(db)
	job := &execJob{id: "dup", serverID: "s1", started: time.Now(), done: make(chan struct{})}

	if err := m.auditStart(job, "s1", "admin", protocol.ExecTask{Cmd: "echo one", Timeout: 30}); err != nil {
		t.Fatalf("首次落审计不该失败: %v", err)
	}
	// job_id 有唯一索引，同 ID 再落一次必然冲突——用它模拟审计写入失败
	if err := m.auditStart(job, "s1", "admin", protocol.ExecTask{Cmd: "echo two", Timeout: 30}); err == nil {
		t.Fatal("审计写入失败时必须返回错误，否则命令会在无痕的情况下执行")
	}
}
