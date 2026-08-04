package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCheckProtectedPathBlocks(t *testing.T) {
	blocked := []string{
		"/etc/passwd",
		"/etc/shadow",
		"/etc/sudoers",
		"/etc/sudoers.d/90-cloud-init",
		"/etc/ssh/sshd_config",
		"/root/.ssh/authorized_keys",
		"/home/deploy/.ssh/authorized_keys",
		"/etc/systemd/system/moss-agent.service",
		"/usr/local/bin/moss-agent",
		"/boot/grub/grub.cfg",
		"/dev/sda",
		"/proc/sys/kernel/panic",
		// 等价写法必须一并拦住，否则黑名单形同虚设
		"//etc//passwd",
		"/etc/ssh//sshd_config",
		"/ETC/PASSWD",
		`\etc\passwd`,
	}
	for _, p := range blocked {
		if why := checkProtectedPath(p); why == "" {
			t.Errorf("受保护路径应被拦截却放行了: %q", p)
		}
	}
}

// TestCheckProtectedPathAllowsNormal 防误伤：拦错正常路径会让 write_file 不可用。
func TestCheckProtectedPathAllowsNormal(t *testing.T) {
	allowed := []string{
		"/etc/nginx/nginx.conf",
		"/etc/nginx/conf.d/app.conf",
		"/opt/app/docker-compose.yml",
		"/var/www/html/index.html",
		"/srv/app/config.yaml",
		"/tmp/deploy.sh",
		"/etc/hosts.allow", // 前缀相近但不是 /etc/hosts
		"/etc/logrotate.d/app",
		"/usr/local/bin/deploy.sh", // 不是 moss-agent
		"C:/apps/config.ini",
	}
	for _, p := range allowed {
		if why := checkProtectedPath(p); why != "" {
			t.Errorf("正常路径被误拦: %q（原因：%s）", p, why)
		}
	}
}

func TestCheckProtectedPathRejectsTraversal(t *testing.T) {
	// 含 .. 无法用前缀判断落点，必须拒绝而不是猜
	for _, p := range []string{"/opt/app/../../etc/passwd", "/tmp/../etc/shadow"} {
		if why := checkProtectedPath(p); why == "" {
			t.Errorf("含 .. 的路径应被拒绝: %q", p)
		}
	}
}

func TestParseFileMode(t *testing.T) {
	cases := []struct {
		in   string
		want uint32
		ok   bool
	}{
		{"", 0, true},
		{"644", 0o644, true},
		{"600", 0o600, true},
		{"0755", 0o755, true},
		{"999", 0, false},   // 9 不是合法八进制位
		{"77777", 0, false}, // 超出 0o7777
		{"abc", 0, false},
	}
	for _, c := range cases {
		got, err := parseFileMode(c.in)
		if c.ok && err != nil {
			t.Errorf("parseFileMode(%q) 不应报错: %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Errorf("parseFileMode(%q) 应报错却通过了", c.in)
		}
		if c.ok && got != c.want {
			t.Errorf("parseFileMode(%q) = %o，期望 %o", c.in, got, c.want)
		}
	}
}

func TestAPIKeyScope(t *testing.T) {
	k := &apiKey{Caps: []string{capRead, capExec}, Servers: []string{"srv1", "srv2"}}

	if !k.has(capRead) || !k.has(capExec) {
		t.Error("应具备已授予的能力")
	}
	if k.has(capWrite) {
		t.Error("不应具备未授予的 write 能力")
	}
	if !k.canAccess("srv1") || !k.canAccess("srv2") {
		t.Error("白名单内的机器应可访问")
	}
	if k.canAccess("srv3") {
		t.Error("白名单外的机器不应可访问")
	}

	// 空白名单表示不限制机器
	open := &apiKey{Caps: []string{capRead}}
	if !open.canAccess("anything") {
		t.Error("空白名单应表示不限制机器")
	}
}

func TestNormalizeCapsDropsUnknown(t *testing.T) {
	got := normalizeCaps([]string{"read", "admin", "exec", "", "root"})
	if len(got) != 2 || got[0] != capRead || got[1] != capExec {
		t.Fatalf("未知能力应被过滤，实际 %v", got)
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer mak_abc123": "mak_abc123",
		"bearer mak_abc123": "mak_abc123", // 头部方案名大小写不敏感
		"mak_abc123":        "",
		"Basic xyz":         "",
		"":                  "",
	}
	for header, want := range cases {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("bearerToken(%q) = %q，期望 %q", header, got, want)
		}
	}
}

func TestHashKeyStableAndDistinct(t *testing.T) {
	a, b := newAPIKey(), newAPIKey()
	if a == b {
		t.Fatal("两次生成的 Key 不应相同")
	}
	// 故意对同一个 Key 算两次：验证 hashKey 不是加盐哈希（那样每次结果都不同，
	// 就没法按哈希建索引查 Key 了）。拆成两个变量而非直接 hashKey(a) != hashKey(a)，
	// 后者会被静态检查当成恒假表达式报警，读代码的人也看不出是故意算两次。
	if h1, h2 := hashKey(a), hashKey(a); h1 != h2 {
		t.Error("同一 Key 的哈希应稳定")
	}
	if hashKey(a) == hashKey(b) {
		t.Error("不同 Key 的哈希应不同")
	}
	if strings.Contains(hashKey(a), a) {
		t.Error("哈希不应包含明文")
	}
}

/* ---------- MCP 传输层 ---------- */

func mcpTestApp(t *testing.T) *App {
	t.Helper()
	db := testDB(t)
	app := &App{db: db, hub: newHub(db)}
	app.exec = newExecManager(db)
	app.upgrade = newUpgradeManager() // 与 main.go 保持一致；handleAdminServers 会读它
	app.notifier = newNotifier(db)
	app.hub.notifier = app.notifier
	app.exec.notifier = app.notifier // 与 main.go 保持一致，否则测不到拦截告警路径
	return app
}

// mcpKey 建一把可用的 Key 并返回明文。
func mcpKey(t *testing.T, app *App, caps, servers string) string {
	t.Helper()
	raw := newAPIKey()
	if _, err := app.db.Exec(
		`INSERT INTO api_keys(name, key_hash, key_prefix, caps, servers, expires_at, created_at)
		 VALUES(?, ?, ?, ?, ?, 0, 0)`,
		"test", hashKey(raw), keyPrefix(raw), caps, servers,
	); err != nil {
		t.Fatalf("插入测试 Key 失败: %v", err)
	}
	return raw
}

func mcpPost(t *testing.T, app *App, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("MCP-Protocol-Version", mcpVersionLatest)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	app.handleMCP(w, r)
	return w
}

func decodeRPC(t *testing.T, w *httptest.ResponseRecorder) jsonrpcResponse {
	t.Helper()
	var resp jsonrpcResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON-RPC: %v，body=%s", err, w.Body.String())
	}
	return resp
}

func TestMCPRequiresAuth(t *testing.T) {
	app := mcpTestApp(t)
	w := mcpPost(t, app, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无 Key 应返回 401，实际 %d", w.Code)
	}
	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 应带 WWW-Authenticate 挑战头")
	}
}

func TestMCPRejectsInvalidKey(t *testing.T) {
	app := mcpTestApp(t)
	w := mcpPost(t, app, "mak_notarealkey", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("非法 Key 应返回 401，实际 %d", w.Code)
	}
}

func TestMCPRejectsRevokedKey(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")
	if _, err := app.db.Exec(`UPDATE api_keys SET revoked = 1 WHERE key_hash = ?`, hashKey(raw)); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	w := mcpPost(t, app, raw, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("已吊销的 Key 应返回 401，实际 %d", w.Code)
	}
}

func TestMCPInitializeNegotiatesVersion(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"}}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize 应返回 200，实际 %d", w.Code)
	}
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("initialize 不应报错: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got mcpInitializeResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("解析 initialize 结果失败: %v", err)
	}
	// 客户端请求的版本受支持时必须回同一版本
	if got.ProtocolVersion != "2025-06-18" {
		t.Errorf("应回客户端请求的版本，实际 %q", got.ProtocolVersion)
	}
	if got.Capabilities.Tools == nil {
		t.Error("应声明 tools 能力")
	}
	if got.ServerInfo.Name != "moss" {
		t.Errorf("serverInfo.name 应为 moss，实际 %q", got.ServerInfo.Name)
	}
}

func TestMCPInitializeFallsBackOnUnknownVersion(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpInitializeResult
	json.Unmarshal(b, &got)
	// 规范：不支持客户端版本时回服务端支持的版本（SHOULD 为最新）
	if got.ProtocolVersion != mcpVersionLatest {
		t.Errorf("未知版本应回落到最新版 %q，实际 %q", mcpVersionLatest, got.ProtocolVersion)
	}
}

func TestMCPRejectsUnsupportedVersionHeader(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	r := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("MCP-Protocol-Version", "1999-01-01")
	r.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	app.handleMCP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持的版本头应返回 400，实际 %d", w.Code)
	}
}

func TestMCPNotificationReturns202(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	// 通知无 id，规范要求受理后回 202 且无 body
	w := mcpPost(t, app, raw, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("通知应返回 202，实际 %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("通知响应不应有 body，实际 %q", w.Body.String())
	}
}

func TestMCPRejectsNonPost(t *testing.T) {
	app := mcpTestApp(t)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := httptest.NewRequest(method, "/mcp", nil)
		w := httptest.NewRecorder()
		app.handleMCP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s 应返回 405，实际 %d", method, w.Code)
		}
	}
}

func TestMCPRejectsForeignOrigin(t *testing.T) {
	app := mcpTestApp(t)
	r := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("Origin", "https://evil.example.com")
	w := httptest.NewRecorder()
	app.handleMCP(w, r)

	// 防 DNS rebinding：规范要求 Origin 非法时回 403
	if w.Code != http.StatusForbidden {
		t.Fatalf("外部 Origin 应返回 403，实际 %d", w.Code)
	}
}

func TestMCPToolsListShape(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	w := mcpPost(t, app, raw, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("tools/list 不应报错: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got mcpListToolsResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("解析 tools/list 失败: %v", err)
	}
	if len(got.Tools) == 0 {
		t.Fatal("工具列表不应为空")
	}
	for _, tool := range got.Tools {
		if tool.Name == "" {
			t.Error("工具必须有 name")
		}
		// 规范要求 inputSchema 根必须是 type:"object"
		if tool.InputSchema["type"] != "object" {
			t.Errorf("工具 %s 的 inputSchema 根必须是 object", tool.Name)
		}
	}
}

func TestMCPUnknownToolIsProtocolError(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	resp := decodeRPC(t, w)
	// 规范明确：找不到工具属于协议错误，不是 isError
	if resp.Error == nil {
		t.Fatal("未知工具应返回 JSON-RPC error 而非工具结果")
	}
}

// TestMCPCapDeniedIsToolError 验证关键区分：
// 权限不足属于工具执行失败（isError），不是协议错误——
// 模型必须能读到原因才不会反复重试。
func TestMCPCapDeniedIsToolError(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read", "") // 只有 read，没有 exec

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{"server_id":"x","cmd":"ls"}}}`)
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("权限不足不应是协议错误: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("权限不足应置 isError=true")
	}
	if len(got.Content) == 0 || !strings.Contains(got.Content[0].Text, "exec") {
		t.Errorf("失败说明应指出缺少哪项能力，实际 %+v", got.Content)
	}
}

func TestMCPServerScopeEnforced(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('allowed','t1','A','默认',0),
		 ('denied','t2','B','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read,exec", "allowed")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_metrics","arguments":{"server_id":"denied"}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("访问白名单外的机器应失败")
	}
	if !strings.Contains(got.Content[0].Text, "未被授权") {
		t.Errorf("应说明是权限问题，实际 %q", got.Content[0].Text)
	}
}

// TestMCPListServersHidesUnauthorized 验证列表按 Key 作用域过滤：
// 让模型看见碰不到的机器只会诱导它尝试越权。
func TestMCPListServersHidesUnauthorized(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('allowed','t1','A','默认',0),
		 ('denied','t2','B','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read", "allowed")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_servers","arguments":{}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if got.IsError {
		t.Fatalf("list_servers 不应失败: %+v", got.Content)
	}
	text := got.Content[0].Text
	if !strings.Contains(text, "allowed") {
		t.Error("应包含有权访问的机器")
	}
	if strings.Contains(text, "denied") {
		t.Error("不应泄露白名单外的机器")
	}
}

// TestMCPListServersReturnsAgentVersion 锁住 agentVersion 必须回给模型。
//
// 旧 agent 收到 exec / write 消息会静默丢弃（agent/main.go 的 switch 无 default
// 分支），症状只是干等到超时。没有这个字段，模型排查执行失败时唯一的线索就没了，
// 只能靠超时时长反推——实机上就这么浪费过一轮。
func TestMCPListServersReturnsAgentVersion(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at, agent_version)
		 VALUES('srv','t1','A','默认',0,'1.4.0')`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_servers","arguments":{}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if got.IsError {
		t.Fatalf("list_servers 不应失败: %+v", got.Content)
	}
	if !strings.Contains(got.Content[0].Text, `"agentVersion": "1.4.0"`) {
		t.Errorf("list_servers 必须返回 agentVersion，实际: %s", got.Content[0].Text)
	}
}

func TestMCPWriteFileRejectsProtectedPath(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('srv','t1','A','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read,write", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"server_id":"srv","path":"/etc/passwd","content":"x"}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("写入受保护路径应被拒绝")
	}
}

// TestMCPBlockedWriteIsAudited 验证拦截必须留痕。
// 「谁试图写入受保护路径」和「谁试图执行破坏性命令」同样重要，
// 早期实现里路径拦截在工具层直接返回，压根没进审计表。
func TestMCPBlockedWriteIsAudited(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('srv','t1','生产机','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read,write", "")

	mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"server_id":"srv","path":"/etc/ssh/sshd_config","content":"Port 2222"}}}`)

	var cmd, errMsg string
	var finishedAt int64
	if err := app.db.QueryRow(
		`SELECT cmd, error, finished_at FROM exec_audit ORDER BY id DESC LIMIT 1`,
	).Scan(&cmd, &errMsg, &finishedAt); err != nil {
		t.Fatalf("被拦截的写入应留有审计记录: %v", err)
	}
	if !strings.Contains(cmd, "/etc/ssh/sshd_config") {
		t.Errorf("审计应记录被拒的目标路径，实际 %q", cmd)
	}
	if !strings.Contains(errMsg, "拦截") {
		t.Errorf("审计应记录拦截原因，实际 %q", errMsg)
	}
	if finishedAt == 0 {
		t.Error("拦截是终态，finished_at 不应为 0")
	}
}

// TestMCPBlockedExecIsAudited 命令拦截同样必须留痕。
func TestMCPBlockedExecIsAudited(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('srv','t1','生产机','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read,exec", "")

	mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{"server_id":"srv","cmd":"iptables -A INPUT -j DROP"}}}`)

	var cmd, errMsg string
	if err := app.db.QueryRow(
		`SELECT cmd, error FROM exec_audit ORDER BY id DESC LIMIT 1`).Scan(&cmd, &errMsg); err != nil {
		t.Fatalf("被拦截的命令应留有审计记录: %v", err)
	}
	if !strings.Contains(cmd, "iptables") {
		t.Errorf("审计应记录原始命令，实际 %q", cmd)
	}
	if !strings.Contains(errMsg, "拦截") {
		t.Errorf("审计应记录拦截原因，实际 %q", errMsg)
	}
}

func TestMCPWriteFileRequiresAbsolutePath(t *testing.T) {
	app := mcpTestApp(t)
	raw := mcpKey(t, app, "read,write", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_file","arguments":{"server_id":"srv","path":"relative/path.conf","content":"x"}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("相对路径应被拒绝")
	}
}

func TestMCPExecBlockedCommandSurfacesReason(t *testing.T) {
	app := mcpTestApp(t)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at) VALUES('srv','t1','A','默认',0)`); err != nil {
		t.Fatalf("插入测试服务器失败: %v", err)
	}
	raw := mcpKey(t, app, "read,exec", "")

	w := mcpPost(t, app, raw,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exec","arguments":{"server_id":"srv","cmd":"rm -rf /"}}}`)
	resp := decodeRPC(t, w)
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	json.Unmarshal(b, &got)
	if !got.IsError {
		t.Fatal("破坏性命令应被拦截")
	}
	// 必须让模型知道这是安全拦截、重试无用，而不是偶发错误
	if !strings.Contains(got.Content[0].Text, "拦截") {
		t.Errorf("失败说明应指出是拦截，实际 %q", got.Content[0].Text)
	}
}

/* ---------- get_history ---------- */

// histResult 只解出断言要用的字段，其余留给 JSON 忽略。
type histResult struct {
	ServerID      string               `json:"serverId"`
	Hours         int                  `json:"hours"`
	BucketSeconds int64                `json:"bucketSeconds"`
	Points        int                  `json:"points"`
	Metrics       []string             `json:"metrics"`
	Times         []int64              `json:"times"`
	Series        map[string][]float64 `json:"series"`
	Summary       map[string]struct {
		Min   float64 `json:"min"`
		Max   float64 `json:"max"`
		Avg   float64 `json:"avg"`
		First float64 `json:"first"`
		Last  float64 `json:"last"`
		Trend string  `json:"trend"`
	} `json:"summary"`
	Note string `json:"note"`
}

func callHistory(t *testing.T, app *App, key, args string) mcpCallToolResult {
	t.Helper()
	w := mcpPost(t, app, key,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_history","arguments":`+args+`}}`)
	resp := decodeRPC(t, w)
	if resp.Error != nil {
		t.Fatalf("get_history 不应是协议错误: %+v", resp.Error)
	}
	b, _ := json.Marshal(resp.Result)
	var got mcpCallToolResult
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("解析工具结果失败: %v", err)
	}
	if len(got.Content) == 0 {
		t.Fatal("工具结果必须带 content")
	}
	return got
}

func historyOK(t *testing.T, app *App, key, args string) histResult {
	t.Helper()
	got := callHistory(t, app, key, args)
	if got.IsError {
		t.Fatalf("get_history 不应失败: %s", got.Content[0].Text)
	}
	var out histResult
	if err := json.Unmarshal([]byte(got.Content[0].Text), &out); err != nil {
		t.Fatalf("结果不是合法 JSON: %v，body=%s", err, got.Content[0].Text)
	}
	return out
}

// seedHistory 写入 n 条历史，最新一条距今 0 秒，往回每条间隔 stepSec。
// f 决定第 i 条（i=0 为最旧）的 cpu 值，其余列按固定值填。
func seedHistory(t *testing.T, app *App, serverID string, n, stepSec int, f func(i int) float64) {
	t.Helper()
	tx, err := app.db.Begin()
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	now := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		ts := now - int64(n-1-i)*int64(stepSec)*1000
		if _, err := tx.Exec(
			`INSERT INTO history(server_id, time, cpu, mem, swap, disk, load1, net_up, net_down, tcp, processes)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			serverID, ts, f(i), 50.0, 1.5, 30.0, 0.8, 1024.0, 2048.0, 100, 200); err != nil {
			t.Fatalf("插入历史数据失败: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("提交历史数据失败: %v", err)
	}
}

func historyTestApp(t *testing.T, serverIDs ...string) *App {
	t.Helper()
	app := mcpTestApp(t)
	for i, id := range serverIDs {
		if _, err := app.db.Exec(
			`INSERT INTO servers(id, token, name, grp, created_at) VALUES(?,?,?,'默认',0)`,
			id, "tok"+strconv.Itoa(i), id); err != nil {
			t.Fatalf("插入测试服务器失败: %v", err)
		}
	}
	return app
}

func TestMCPGetHistoryReturnsSeriesAndSummary(t *testing.T) {
	app := historyTestApp(t, "srv")
	// 半小时、10 秒一条：CPU 从 10 线性升到 90
	seedHistory(t, app, "srv", 180, 10, func(i int) float64 { return 10 + float64(i)*80/179 })
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv"}`)

	if got.ServerID != "srv" || got.Hours != 1 {
		t.Fatalf("应回显目标与实际时长，实际 serverId=%q hours=%d", got.ServerID, got.Hours)
	}
	// 默认只给 cpu/mem/disk，不能把九条曲线全塞回去
	if len(got.Metrics) != 3 || got.Series["swap"] != nil || got.Series["netUp"] != nil {
		t.Fatalf("默认应只返回 cpu/mem/disk，实际 %v", got.Metrics)
	}
	if got.Points == 0 || len(got.Times) != got.Points {
		t.Fatalf("points 应与 times 长度一致且非空，实际 points=%d times=%d", got.Points, len(got.Times))
	}
	for _, k := range got.Metrics {
		if len(got.Series[k]) != got.Points {
			t.Fatalf("指标 %s 的点数 %d 与 points=%d 不一致", k, len(got.Series[k]), got.Points)
		}
	}
	// 秒级时间戳：毫秒会大三个量级，用当前时间做量级校验
	if got.Times[0] > time.Now().Unix()+60 || got.Times[0] < time.Now().Unix()-7200 {
		t.Errorf("时间戳应是秒级 Unix 时间，实际 %d", got.Times[0])
	}
	if got.Times[0] >= got.Times[len(got.Times)-1] {
		t.Error("时间序列应按时间升序")
	}
	cpu := got.Summary["cpu"]
	if cpu.Trend != "rising" {
		t.Errorf("线性上升的 CPU 应判为 rising，实际 %q", cpu.Trend)
	}
	if cpu.Min < 10 || cpu.Max > 90 || cpu.First >= cpu.Last {
		t.Errorf("摘要与数据不符: %+v", cpu)
	}
	if cpu.Avg < 40 || cpu.Avg > 60 {
		t.Errorf("均值应落在 50 附近，实际 %v", cpu.Avg)
	}
	// 恒定 50 的内存必须判 flat，否则告警会被噪声带偏
	if mem := got.Summary["mem"]; mem.Trend != "flat" || mem.Min != 50 || mem.Max != 50 {
		t.Errorf("恒定值应判 flat 且极值相等，实际 %+v", mem)
	}
}

// TestMCPGetHistoryDownsamples 是本工具的命门：不降采样一定撑爆模型上下文。
func TestMCPGetHistoryDownsamples(t *testing.T) {
	app := historyTestApp(t, "srv")
	seedHistory(t, app, "srv", 600, 6, func(i int) float64 { return float64(i % 100) }) // 1 小时内 600 条
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv","metrics":["cpu","net_up","tcp"]}`)

	if got.Points > 120 {
		t.Fatalf("点数必须压到 120 以内，实际 %d", got.Points)
	}
	if got.Points < 30 {
		t.Fatalf("降采样过度会丢掉形状，1 小时不应少于 30 点，实际 %d", got.Points)
	}
	if got.BucketSeconds <= 6 {
		t.Errorf("聚合粒度应大于原始采样间隔，实际 %d 秒", got.BucketSeconds)
	}
	// 粒度必须如实告知，否则模型会把均值当瞬时值读
	if int64(got.Points)*got.BucketSeconds < 3000 {
		t.Errorf("points×bucketSeconds 应覆盖所查时段，实际 %d×%d", got.Points, got.BucketSeconds)
	}
	for _, k := range []string{"cpu", "netUp", "tcp"} {
		if len(got.Series[k]) != got.Points {
			t.Fatalf("指标 %s 缺失或点数不符: %d", k, len(got.Series[k]))
		}
	}
	// 聚合值保留 2 位小数
	for _, v := range got.Series["cpu"] {
		if v != math.Round(v*100)/100 {
			t.Fatalf("聚合值应保留 2 位小数，实际 %v", v)
		}
	}
}

// TestMCPGetHistoryDownsamplesLongRange 长时段同样要守住上限。
func TestMCPGetHistoryDownsamplesLongRange(t *testing.T) {
	app := historyTestApp(t, "srv")
	seedHistory(t, app, "srv", 2000, 120, func(i int) float64 { return float64(i % 50) }) // 覆盖约 66 小时
	raw := mcpKey(t, app, "read", "")

	for _, hours := range []int{1, 6, 24, 72, 168} {
		got := historyOK(t, app, raw, `{"server_id":"srv","hours":`+strconv.Itoa(hours)+`}`)
		if got.Points > 120 {
			t.Fatalf("hours=%d 时点数 %d 破了上限", hours, got.Points)
		}
	}
}

func TestMCPGetHistoryClampsHours(t *testing.T) {
	app := historyTestApp(t, "srv")
	if err := setSetting(app.db, keyHistoryDays, "3"); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv","hours":999}`)
	if got.Hours != 72 {
		t.Fatalf("应收敛到保留期 3 天=72 小时，实际 %d", got.Hours)
	}
	if !strings.Contains(got.Note, "收敛") {
		t.Errorf("收敛后必须说明，实际 note=%q", got.Note)
	}
}

// TestMCPGetHistoryEmptyIsNotError 没有历史不是故障：
// 报 isError 会诱导模型对同一个必然为空的查询反复重试。
func TestMCPGetHistoryEmptyIsNotError(t *testing.T) {
	app := historyTestApp(t, "srv")
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv"}`)
	if got.Points != 0 || len(got.Times) != 0 {
		t.Fatalf("无数据时应返回空序列，实际 points=%d", got.Points)
	}
	for _, k := range got.Metrics {
		if got.Series[k] == nil {
			t.Errorf("指标 %s 应为空数组而不是 null", k)
		}
		if len(got.Series[k]) != 0 {
			t.Errorf("指标 %s 应为空，实际 %v", k, got.Series[k])
		}
	}
	if got.Note == "" {
		t.Error("空结果必须说明原因，否则模型无从判断是没数据还是查错了")
	}
}

func TestMCPGetHistoryRequiresReadCap(t *testing.T) {
	app := historyTestApp(t, "srv")
	raw := mcpKey(t, app, "exec", "") // 只有 exec，没有 read

	got := callHistory(t, app, raw, `{"server_id":"srv"}`)
	if !got.IsError {
		t.Fatal("缺少 read 能力应置 isError")
	}
	if !strings.Contains(got.Content[0].Text, "read") {
		t.Errorf("应指出缺少哪项能力，实际 %q", got.Content[0].Text)
	}
}

func TestMCPGetHistoryScopeEnforced(t *testing.T) {
	app := historyTestApp(t, "allowed", "denied")
	seedHistory(t, app, "denied", 10, 10, func(int) float64 { return 42 })
	raw := mcpKey(t, app, "read", "allowed")

	got := callHistory(t, app, raw, `{"server_id":"denied"}`)
	if !got.IsError {
		t.Fatal("越权读取历史应失败")
	}
	if !strings.Contains(got.Content[0].Text, "未被授权") {
		t.Errorf("应说明是权限问题，实际 %q", got.Content[0].Text)
	}
	if strings.Contains(got.Content[0].Text, "42") {
		t.Error("越权失败不应泄露目标机数据")
	}
}

func TestMCPGetHistoryUnknownServer(t *testing.T) {
	app := historyTestApp(t)
	raw := mcpKey(t, app, "read", "")

	got := callHistory(t, app, raw, `{"server_id":"ghost"}`)
	if !got.IsError {
		t.Fatal("不存在的机器应失败")
	}
	if !strings.Contains(got.Content[0].Text, "不存在") {
		t.Errorf("应说明机器不存在，实际 %q", got.Content[0].Text)
	}
}

// TestMCPGetHistoryRejectsUnknownMetric 静默忽略会让模型以为拿到了那条曲线。
func TestMCPGetHistoryRejectsUnknownMetric(t *testing.T) {
	app := historyTestApp(t, "srv")
	raw := mcpKey(t, app, "read", "")

	got := callHistory(t, app, raw, `{"server_id":"srv","metrics":["cpu","iowait"]}`)
	if !got.IsError {
		t.Fatal("未知指标应报错而不是静默忽略")
	}
	if !strings.Contains(got.Content[0].Text, "iowait") || !strings.Contains(got.Content[0].Text, "load1") {
		t.Errorf("应指出错在哪并列出可选项，实际 %q", got.Content[0].Text)
	}
}

func TestMCPGetHistoryFallingTrend(t *testing.T) {
	app := historyTestApp(t, "srv")
	seedHistory(t, app, "srv", 180, 10, func(i int) float64 { return 90 - float64(i)*80/179 })
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv","metrics":["cpu"]}`)
	if got.Summary["cpu"].Trend != "falling" {
		t.Errorf("线性下降应判 falling，实际 %q", got.Summary["cpu"].Trend)
	}
}

// TestMCPGetHistoryToolAnnotations 只读工具的 hint 必须显式写全：
// 规范里 destructiveHint 与 openWorldHint 默认为 true，漏写会被客户端当成危险工具。
func TestMCPGetHistoryToolAnnotations(t *testing.T) {
	var tool *mcpTool
	for i, tl := range mcpTools() {
		if tl.Name == "get_history" {
			tool = &mcpTools()[i]
		}
	}
	if tool == nil {
		t.Fatal("工具清单里应有 get_history")
	}
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
		t.Fatal("应显式标注 readOnlyHint=true")
	}
	if a.DestructiveHint == nil || *a.DestructiveHint {
		t.Error("应显式标注 destructiveHint=false")
	}
	if a.IdempotentHint == nil || !*a.IdempotentHint {
		t.Error("应显式标注 idempotentHint=true")
	}
	if a.OpenWorldHint == nil || *a.OpenWorldHint {
		t.Error("应显式标注 openWorldHint=false")
	}
	if tool.InputSchema["additionalProperties"] != false {
		t.Error("inputSchema 应禁止额外参数")
	}
	req, _ := tool.InputSchema["required"].([]string)
	if len(req) != 1 || req[0] != "server_id" {
		t.Errorf("server_id 应是唯一必填项，实际 %v", tool.InputSchema["required"])
	}
}

// TestMCPGetHistoryIgnoresFutureRows agent 时钟走快会写入未来时间戳，
// 这类行的桶号会落到区间之外，一行脏数据就能把返回点数顶破上限。
func TestMCPGetHistoryIgnoresFutureRows(t *testing.T) {
	app := historyTestApp(t, "srv")
	seedHistory(t, app, "srv", 100, 30, func(i int) float64 { return float64(i) })
	future := time.Now().Add(48 * time.Hour).UnixMilli()
	if _, err := app.db.Exec(
		`INSERT INTO history(server_id, time, cpu, mem, swap, disk, load1, net_up, net_down, tcp, processes)
		 VALUES('srv',?,999,999,999,0,0,0,0,0,0)`, future); err != nil {
		t.Fatalf("插入超前时间戳失败: %v", err)
	}
	raw := mcpKey(t, app, "read", "")

	got := historyOK(t, app, raw, `{"server_id":"srv"}`)
	if got.Points > 120 {
		t.Fatalf("超前时间戳不应顶破点数上限，实际 %d", got.Points)
	}
	if got.Summary["cpu"].Max > 100 {
		t.Errorf("窗口之外的数据不应进入结果，实际 max=%v", got.Summary["cpu"].Max)
	}
	if last := got.Times[len(got.Times)-1]; last > time.Now().Unix()+60 {
		t.Errorf("返回的时间戳不应超出查询窗口，实际 %d", last)
	}
}

func TestHistoryBucketSecondsHoldsCap(t *testing.T) {
	// 桶宽必须严格大于 跨度÷120，否则末端会多出第 121 个桶
	for _, hours := range []int64{1, 2, 6, 12, 24, 72, 168, 720, 8760} {
		rangeSec := hours * 3600
		b := historyBucketSeconds(rangeSec)
		if points := rangeSec/b + 1; points > historyMaxPoints {
			t.Errorf("hours=%d 时桶宽 %d 会产生 %d 个点，超过上限", hours, b, points)
		}
	}
}
