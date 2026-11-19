package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testPKCEPair() (verifier, challenge string) {
	verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk-moss-oauth21-test"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

func TestValidRedirectURI(t *testing.T) {
	valid := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"http://localhost:6274/oauth/callback",
		"http://127.0.0.1:3333/callback",
		"http://[::1]:8080/cb",
		"cursor://anysphere.cursor-mcp/oauth/callback",
		"claude://oauth/callback",
	}
	for _, u := range valid {
		if !validRedirectURI(u) {
			t.Errorf("合法回调地址被拒: %q", u)
		}
	}

	invalid := []string{
		"",
		"http://evil.example.com/callback", // 非环回禁止明文 HTTP
		"https://claude.ai/cb#fragment",    // OAuth 2.1 禁止带 fragment
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"://missing-scheme",
	}
	for _, u := range invalid {
		if validRedirectURI(u) {
			t.Errorf("非法回调地址应被拒却放行了: %q", u)
		}
	}
}

func TestOAuthDiscoveryAndWWWAuthenticate(t *testing.T) {
	app := mcpTestApp(t)
	mux := http.NewServeMux()
	app.registerOAuthRoutes(mux)
	mux.HandleFunc("/mcp", app.handleMCP)

	// 1. 未带凭证请求 /mcp 应返回包含 resource_metadata 的 WWW-Authenticate
	r := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权请求应返回 401，实际 %d", w.Code)
	}
	authHeader := w.Header().Get("WWW-Authenticate")
	wantMeta := `resource_metadata="https://jk.20051212.xyz/.well-known/oauth-protected-resource"`
	if !strings.Contains(authHeader, wantMeta) {
		t.Errorf("WWW-Authenticate 应包含 %s，实际 %q", wantMeta, authHeader)
	}

	// 2. /.well-known/oauth-protected-resource 与 /mcp 子路径
	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		req := httptest.NewRequest(http.MethodGet, "https://jk.20051212.xyz"+path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 应返回 200，实际 %d", path, rec.Code)
		}
		var body struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析 %s JSON 失败: %v", path, err)
		}
		if body.Resource != "https://jk.20051212.xyz/mcp" {
			t.Errorf("resource = %q，期望 https://jk.20051212.xyz/mcp", body.Resource)
		}
		if len(body.AuthorizationServers) != 1 || body.AuthorizationServers[0] != "https://jk.20051212.xyz" {
			t.Errorf("authorization_servers = %v，期望 [https://jk.20051212.xyz]", body.AuthorizationServers)
		}
	}

	// 3. /.well-known/oauth-authorization-server
	req := httptest.NewRequest(http.MethodGet, "https://jk.20051212.xyz/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET oauth-authorization-server 应返回 200，实际 %d", rec.Code)
	}
	var asMeta struct {
		Issuer                        string   `json:"issuer"`
		AuthorizationEndpoint         string   `json:"authorization_endpoint"`
		TokenEndpoint                 string   `json:"token_endpoint"`
		RegistrationEndpoint          string   `json:"registration_endpoint"`
		CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &asMeta); err != nil {
		t.Fatalf("解析 AS 元数据失败: %v", err)
	}
	if asMeta.AuthorizationEndpoint != "https://jk.20051212.xyz/oauth/authorize" ||
		asMeta.TokenEndpoint != "https://jk.20051212.xyz/oauth/token" ||
		asMeta.RegistrationEndpoint != "https://jk.20051212.xyz/oauth/register" {
		t.Errorf("AS 端点地址不正确: %+v", asMeta)
	}
	if len(asMeta.CodeChallengeMethodsSupported) != 1 || asMeta.CodeChallengeMethodsSupported[0] != "S256" {
		t.Errorf("必须只声明 S256 PKCE，实际 %v", asMeta.CodeChallengeMethodsSupported)
	}
}

func TestOAuthRegisterValidation(t *testing.T) {
	app := mcpTestApp(t)

	// 非法回调地址应回 400 invalid_redirect_uri
	r := httptest.NewRequest(http.MethodPost, "/oauth/register",
		strings.NewReader(`{"client_name":"Bad","redirect_uris":["http://evil.com/cb"]}`))
	w := httptest.NewRecorder()
	app.handleOAuthRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 redirect_uri 应返回 400，实际 %d", w.Code)
	}

	// 合法注册应回 201 Created
	r = httptest.NewRequest(http.MethodPost, "/oauth/register",
		strings.NewReader(`{"client_name":"Claude Desktop","redirect_uris":["http://127.0.0.1:43210/callback"]}`))
	w = httptest.NewRecorder()
	app.handleOAuthRegister(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("合法注册应返回 201，实际 %d body=%s", w.Code, w.Body.String())
	}
	var reg struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil || !strings.HasPrefix(reg.ClientID, "mc_") {
		t.Fatalf("返回 client_id 非法: %+v, err=%v", reg, err)
	}
}

func TestOAuthEndToEndFlowAndKeyIntegration(t *testing.T) {
	resetLoginAttempts(t)
	app := loginTestApp(t, "correct-horse-battery")
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at, agent_version)
		 VALUES('s1','t1','AllowedNode','默认',0,'2.0.0'),
		       ('s2','t2','DeniedNode','默认',0,'2.0.0')`,
	); err != nil {
		t.Fatalf("插入测试节点失败: %v", err)
	}

	mux := http.NewServeMux()
	app.registerOAuthRoutes(mux)
	mux.HandleFunc("/mcp", app.handleMCP)

	// 步骤 1：动态客户端注册
	regReq := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/register",
		strings.NewReader(`{"client_name":"Claude Code","redirect_uris":["http://localhost:54321/callback"]}`))
	regRec := httptest.NewRecorder()
	mux.ServeHTTP(regRec, regReq)
	if regRec.Code != http.StatusCreated {
		t.Fatalf("DCR 失败: %d %s", regRec.Code, regRec.Body.String())
	}
	var regResp struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(regRec.Body.Bytes(), &regResp)

	verifier, challenge := testPKCEPair()
	cbURI := "http://localhost:54321/callback"

	// 步骤 2：GET /oauth/authorize 渲染授权页，且拒绝未注册的 redirect_uri
	badAuthReq := httptest.NewRequest(http.MethodGet,
		"https://jk.20051212.xyz/oauth/authorize?response_type=code&client_id="+regResp.ClientID+
			"&redirect_uri="+url.QueryEscape("https://other.example.com/cb")+
			"&code_challenge="+challenge+"&code_challenge_method=S256", nil)
	badAuthRec := httptest.NewRecorder()
	mux.ServeHTTP(badAuthRec, badAuthReq)
	if badAuthRec.Code != http.StatusBadRequest {
		t.Fatalf("不匹配的 redirect_uri 应返回 400 而非重定向，实际 %d", badAuthRec.Code)
	}

	getAuthReq := httptest.NewRequest(http.MethodGet,
		"https://jk.20051212.xyz/oauth/authorize?response_type=code&client_id="+regResp.ClientID+
			"&redirect_uri="+url.QueryEscape(cbURI)+
			"&code_challenge="+challenge+"&code_challenge_method=S256&state=xyz123&scope=read+exec", nil)
	getAuthRec := httptest.NewRecorder()
	mux.ServeHTTP(getAuthRec, getAuthReq)
	if getAuthRec.Code != http.StatusOK || !strings.Contains(getAuthRec.Body.String(), "Claude Code") {
		t.Fatalf("GET /oauth/authorize 应渲染包含客户端名的授权页，status=%d", getAuthRec.Code)
	}

	// 步骤 3：POST /oauth/authorize 密码错误应返回 401，密码正确则签发 code 并只授权 s1 节点 + read 能力
	makeApproveForm := func(pwd string) url.Values {
		f := url.Values{}
		f.Set("client_id", regResp.ClientID)
		f.Set("redirect_uri", cbURI)
		f.Set("response_type", "code")
		f.Set("code_challenge", challenge)
		f.Set("code_challenge_method", "S256")
		f.Set("state", "xyz123")
		f.Set("username", "admin")
		f.Set("password", pwd)
		f.Set("key_name", "OAuth: Claude Code")
		f.Add("caps", "read")
		f.Set("scope_mode", "selected")
		f.Add("servers", "s1")
		f.Set("days", "30")
		f.Set("decision", "approve")
		return f
	}

	wrongPost := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/authorize",
		strings.NewReader(makeApproveForm("wrong-password").Encode()))
	wrongPost.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	wrongPost.RemoteAddr = "198.51.100.20:40000"
	wrongRec := httptest.NewRecorder()
	mux.ServeHTTP(wrongRec, wrongPost)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("错误管理员密码应返回 401，实际 %d", wrongRec.Code)
	}

	okPost := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/authorize",
		strings.NewReader(makeApproveForm("correct-horse-battery").Encode()))
	okPost.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	okPost.RemoteAddr = "198.51.100.20:40000"
	okRec := httptest.NewRecorder()
	mux.ServeHTTP(okRec, okPost)
	if okRec.Code != http.StatusFound {
		t.Fatalf("正确密码授权应返回 302，实际 %d body=%s", okRec.Code, okRec.Body.String())
	}
	loc, err := url.Parse(okRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("解析重定向 Location 失败: %v", err)
	}
	code := loc.Query().Get("code")
	if !strings.HasPrefix(code, "moc_") || loc.Query().Get("state") != "xyz123" {
		t.Fatalf("重定向参数异常: location=%s", loc)
	}

	// 步骤 4：POST /oauth/token 用 code + code_verifier 换取 access_token
	tokForm := url.Values{}
	tokForm.Set("grant_type", "authorization_code")
	tokForm.Set("code", code)
	tokForm.Set("redirect_uri", cbURI)
	tokForm.Set("client_id", regResp.ClientID)
	tokForm.Set("code_verifier", verifier)

	tokReq := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/token",
		strings.NewReader(tokForm.Encode()))
	tokReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokRec := httptest.NewRecorder()
	mux.ServeHTTP(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("换取 token 失败: %d %s", tokRec.Code, tokRec.Body.String())
	}
	var tokResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(tokRec.Body.Bytes(), &tokResp); err != nil {
		t.Fatalf("解析 token 响应失败: %v", err)
	}
	if !strings.HasPrefix(tokResp.AccessToken, "mak_") || tokResp.TokenType != "Bearer" || tokResp.Scope != "read" {
		t.Fatalf("签发的 token 不符合预期: %+v", tokResp)
	}

	// 同一个 code 重放必须被拒绝（单次消费）
	replayReq := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/token",
		strings.NewReader(tokForm.Encode()))
	replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayRec := httptest.NewRecorder()
	mux.ServeHTTP(replayRec, replayReq)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("重放授权码应被拒，实际 %d", replayRec.Code)
	}

	// 步骤 5：用签发的 access_token 调用 /mcp，验证能力集与节点白名单完全生效
	wList := mcpPost(t, app, tokResp.AccessToken,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_servers","arguments":{}}}`)
	if wList.Code != http.StatusOK {
		t.Fatalf("持 OAuth access_token 调用 /mcp 应返回 200，实际 %d", wList.Code)
	}
	listText := wList.Body.String()
	if !strings.Contains(listText, "AllowedNode") || strings.Contains(listText, "DeniedNode") {
		t.Errorf("OAuth token 应严格遵守授权页勾选的节点白名单，实际响应: %s", listText)
	}

	// 步骤 6：后台停用该 Key 后，OAuth access_token 立即失效
	if _, err := app.db.Exec(`UPDATE api_keys SET revoked = 1 WHERE key_hash = ?`, hashKey(tokResp.AccessToken)); err != nil {
		t.Fatalf("停用 Key 失败: %v", err)
	}
	wAfterRevoke := mcpPost(t, app, tokResp.AccessToken, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if wAfterRevoke.Code != http.StatusUnauthorized {
		t.Fatalf("后台停用后 OAuth token 应立即返回 401，实际 %d", wAfterRevoke.Code)
	}
}

func TestOAuthAuthorizeWithExistingAdminSession(t *testing.T) {
	app := mcpTestApp(t)
	sessToken := randString(40)
	if _, err := app.db.Exec(`INSERT INTO sessions(token, expires) VALUES(?, ?)`,
		sessToken, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("插入管理员会话失败: %v", err)
	}

	verifier, challenge := testPKCEPair()
	cbURI := "http://127.0.0.1:9999/cb"

	f := url.Values{}
	f.Set("client_id", "preconfigured-client")
	f.Set("redirect_uri", cbURI)
	f.Set("response_type", "code")
	f.Set("code_challenge", challenge)
	f.Set("code_challenge_method", "S256")
	f.Set("key_name", "Session Authorized Client")
	f.Add("caps", "read")
	f.Add("caps", "exec")
	f.Set("scope_mode", "all")
	f.Set("days", "0")
	f.Set("decision", "approve")

	r := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/authorize", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessToken})
	w := httptest.NewRecorder()
	app.handleOAuthAuthorizePOST(w, r)

	if w.Code != http.StatusFound {
		t.Fatalf("已登录会话应无需输密码直接 302 签发授权码，实际 %d body=%s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")

	// 换取 token（测试 JSON 格式请求体）
	tokBody, _ := json.Marshal(map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  cbURI,
		"client_id":     "preconfigured-client",
		"code_verifier": verifier,
	})
	tokReq := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/token", strings.NewReader(string(tokBody)))
	tokReq.Header.Set("Content-Type", "application/json")
	tokRec := httptest.NewRecorder()
	app.handleOAuthToken(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("JSON 格式换取 token 应返回 200，实际 %d %s", tokRec.Code, tokRec.Body.String())
	}
}
