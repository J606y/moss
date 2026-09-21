package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedAudit 造若干条审计记录，其中每第 3 条是被拦截的。
//
// 时间戳必须用真实的当前时间：审计清理按 started_at 与保留期比较，
// 若用 1000+i 这种小数值，任何一次保留期清理都会把整张表当成 1970 年的记录删光。
// i 越大越新，便于验证「留下的是最新那批」。
func seedAudit(t *testing.T, app *App, n int) {
	t.Helper()
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('s1','t1','A','默认',0),('s2','t2','B','默认',0)`,
	); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		srv, errMsg := "s1", ""
		if i%2 == 1 {
			srv = "s2"
		}
		if i%3 == 0 {
			errMsg = execBlockedPrefix + "根目录递归删除"
		}
		started := base - int64(n-i)*1000 // 每条相隔 1 秒，i 越大越新
		if _, err := app.db.Exec(
			`INSERT INTO exec_audit(job_id, server_id, caller, cmd, started_at, finished_at, error)
			 VALUES(?, ?, 'k1', ?, ?, ?, ?)`,
			jobIDFor(i), srv, "cmd-"+jobIDFor(i), started, started+1, errMsg,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func jobIDFor(i int) string { return "job-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) }

func auditPage(t *testing.T, app *App, q string) execAuditPage {
	t.Helper()
	w := httptest.NewRecorder()
	app.handleExecAudit(w, httptest.NewRequest(http.MethodGet, "/api/admin/exec-audit?"+q, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d，body=%s", w.Code, w.Body.String())
	}
	var page execAuditPage
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return page
}

func auditQuery(t *testing.T, app *App, q string) []execAuditRow {
	t.Helper()
	return auditPage(t, app, q).Items
}

// 「仅看拦截」是这个页面存在的主要理由之一：被拦下的尝试混在日常执行流水里
// 很快就被刷到翻不到的地方，而它恰恰是审计里最该被单独捞出来的记录。
func TestExecAuditFilterBlocked(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 12) // i%3==0 → 4 条被拦截

	all := auditQuery(t, app, "limit=100")
	if len(all) != 12 {
		t.Fatalf("不筛选应返回全部 12 条，实际 %d", len(all))
	}

	blocked := auditQuery(t, app, "limit=100&blocked=1")
	if len(blocked) != 4 {
		t.Fatalf("应只返回 4 条拦截记录，实际 %d", len(blocked))
	}
	for _, r := range blocked {
		if r.Error == "" {
			t.Errorf("拦截筛选返回了非拦截记录: %+v", r)
		}
	}
}

func TestExecAuditFilterByServer(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 10) // 偶数 → s1，奇数 → s2

	got := auditQuery(t, app, "limit=100&server=s2")
	if len(got) != 5 {
		t.Fatalf("s2 应有 5 条，实际 %d", len(got))
	}
	for _, r := range got {
		if r.ServerID != "s2" {
			t.Errorf("机器筛选返回了别的机器: %+v", r)
		}
	}
}

// 两个筛选条件必须是 AND 关系，而不是互相覆盖。
func TestExecAuditFiltersCombine(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 12)

	got := auditQuery(t, app, "limit=100&server=s1&blocked=1")
	for _, r := range got {
		if r.ServerID != "s1" || r.Error == "" {
			t.Errorf("组合筛选结果不满足两个条件: %+v", r)
		}
	}
	// i 为偶数落 s1、i%3==0 为拦截 → 0/6 两条同时满足
	if len(got) != 2 {
		t.Fatalf("s1 上的拦截记录应为 2 条，实际 %d", len(got))
	}
}

// 按接入密钥筛选。caller 落库格式是 callerLabel 产出的 `key:{id}({name})`，
// 筛选按 id 前缀匹配而不是名字，这里把两个易碎点钉死：
//
//   - 改名：密钥改名后，历史记录里留的仍是旧名字。按名字筛会把改名前的
//     整段记录漏掉，按 id 才能连起来。
//   - 前缀混淆：`key=1` 绝不能把 `key:11(...)` 也捞出来。挡住它的是模式里
//     那个字面量左括号（`key:1(%`），一旦有人「顺手」把它去掉就会静默串号。
func TestExecAuditFilterByKey(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('s1','t1','A','默认',0)`,
	); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UnixMilli()
	callers := []string{
		"key:1(Hermes)",
		"key:1(Hermes-renamed)", // 同一密钥改名之后
		"key:2(claude)",
		"key:11(other)", // 前缀易混：不该被 key=1 匹配
		"admin",         // 后台手动执行
		"panel-update",  // 面板自更新
	}
	for i, c := range callers {
		started := base + int64(i)
		if _, err := app.db.Exec(
			`INSERT INTO exec_audit(job_id, server_id, caller, cmd, started_at, finished_at)
			 VALUES(?, 's1', ?, 'c', ?, ?)`,
			jobIDFor(i), c, started, started+1,
		); err != nil {
			t.Fatal(err)
		}
	}

	got := auditQuery(t, app, "limit=100&key=1")
	if len(got) != 2 {
		t.Fatalf("密钥 1 应有 2 条（含改名后那条），实际 %d", len(got))
	}
	for _, r := range got {
		if !strings.HasPrefix(r.Caller, "key:1(") {
			t.Errorf("密钥筛选返回了别的调用方: %+v", r)
		}
	}

	if got := auditQuery(t, app, "limit=100&key=11"); len(got) != 1 {
		t.Fatalf("密钥 11 应有 1 条，实际 %d —— key=1 与 key=11 串号了", len(got))
	}

	// 非法值一律忽略，等同于不筛选：既不该报错，也不该返回空表。
	// （注入不在这里试：值经 Atoi 过滤后才拼进 LIKE，且始终走参数化占位符。）
	for _, bad := range []string{"abc", "0", "-1", "1abc"} {
		if got := auditQuery(t, app, "limit=100&key="+bad); len(got) != len(callers) {
			t.Errorf("非法 key=%q 应被忽略并返回全部 %d 条，实际 %d", bad, len(callers), len(got))
		}
	}

	// 合法但没有记录的密钥：返回空表，而不是退化成「不筛选」。
	if got := auditQuery(t, app, "limit=100&key=999"); len(got) != 0 {
		t.Errorf("不存在的密钥应返回 0 条，实际 %d", len(got))
	}
}

// 条数上限的清理是 DELETE 语句，写错就是静默删掉不该删的记录。
// 必须验证：删够数量、留下的是最新的那批、且不足上限时一条都不动。
func TestPruneExecAuditByMaxRows(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 10)
	now := time.Now()

	// 不足上限：一条都不该删。
	pruneExecAudit(app.db, now, 90, 100)
	if n := auditCount(t, app); n != 10 {
		t.Fatalf("不足上限时不该删任何记录，实际剩 %d", n)
	}

	// 0 表示不限制，同样一条都不删。
	pruneExecAudit(app.db, now, 90, 0)
	if n := auditCount(t, app); n != 10 {
		t.Fatalf("0 表示不限制，实际剩 %d", n)
	}

	// 超出上限：删到只剩 4 条，且留下的必须正是最新的那 4 条——
	// 删错方向的话条数一样对，内容却全反了。
	want := auditJobIDs(t, app, `SELECT job_id FROM exec_audit ORDER BY started_at DESC LIMIT 4`)
	pruneExecAudit(app.db, now, 90, 4)
	if n := auditCount(t, app); n != 4 {
		t.Fatalf("应剩 4 条，实际 %d", n)
	}
	got := auditJobIDs(t, app, `SELECT job_id FROM exec_audit ORDER BY started_at DESC`)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("留下的不是最新 4 条：期望 %v，实际 %v", want, got)
		}
	}
}

func auditJobIDs(t *testing.T, app *App, query string) []string {
	t.Helper()
	rows, err := app.db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// 保留期与条数上限是两条独立规则，取先触发者。
func TestPruneExecAuditByAge(t *testing.T) {
	app := mcpTestApp(t)
	now := time.Now()
	if _, err := app.db.Exec(
		`INSERT INTO exec_audit(job_id, server_id, caller, cmd, started_at) VALUES
		 ('old','s1','k','a',?), ('new','s1','k','b',?)`,
		now.AddDate(0, 0, -100).UnixMilli(), now.UnixMilli(),
	); err != nil {
		t.Fatal(err)
	}
	pruneExecAudit(app.db, now, 90, 0) // 只按天数，不限条数
	if n := auditCount(t, app); n != 1 {
		t.Fatalf("超期记录应被删除，实际剩 %d", n)
	}
	var jobID string
	if err := app.db.QueryRow(`SELECT job_id FROM exec_audit`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID != "new" {
		t.Errorf("留下的应是未超期的那条，实际 %q", jobID)
	}
}

func auditCount(t *testing.T, app *App) int {
	t.Helper()
	var n int
	if err := app.db.QueryRow(`SELECT COUNT(*) FROM exec_audit`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// 没有 offset 就等于旧记录永远够不着——审计保留 90 天，界面却只能看到最近一页。
func TestExecAuditPagination(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 10)

	first := auditQuery(t, app, "limit=4&offset=0")
	second := auditQuery(t, app, "limit=4&offset=4")
	if len(first) != 4 || len(second) != 4 {
		t.Fatalf("每页应为 4 条，实际 %d / %d", len(first), len(second))
	}
	// 倒序排列，翻页之间不能有重叠，否则「加载更多」会重复渲染同一条
	for _, a := range first {
		for _, b := range second {
			if a.JobID == b.JobID {
				t.Fatalf("翻页出现重复记录: %s", a.JobID)
			}
		}
	}
	if first[0].StartedAt <= second[0].StartedAt {
		t.Error("应按时间倒序，第一页该比第二页新")
	}

	last := auditQuery(t, app, "limit=4&offset=8")
	if len(last) != 2 {
		t.Errorf("末页应只剩 2 条，实际 %d", len(last))
	}
}

// total 必须是筛选后的总数、且不受 LIMIT 影响——
// 它被 LIMIT 带偏的话，前端画出来的页码数就是错的。
func TestExecAuditTotalIgnoresPaging(t *testing.T) {
	app := mcpTestApp(t)
	seedAudit(t, app, 12) // 12 条，其中 4 条被拦截、6 条在 s2

	if p := auditPage(t, app, "limit=3&offset=0"); p.Total != 12 || len(p.Items) != 3 {
		t.Errorf("总数应为 12 且本页 3 条，实际 total=%d len=%d", p.Total, len(p.Items))
	}
	if p := auditPage(t, app, "limit=3&offset=9"); p.Total != 12 {
		t.Errorf("翻到末页后总数仍应为 12，实际 %d", p.Total)
	}
	// 筛选后总数必须跟着变，否则页码还是按全量算的
	if p := auditPage(t, app, "limit=3&blocked=1"); p.Total != 4 {
		t.Errorf("仅看拦截时总数应为 4，实际 %d", p.Total)
	}
	if p := auditPage(t, app, "limit=3&server=s2"); p.Total != 6 {
		t.Errorf("按机器筛选后总数应为 6，实际 %d", p.Total)
	}
}
