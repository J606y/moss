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

func registerTestOAuthClient(t *testing.T, app *App, name string, redirectURIs []string) string {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{
		"client_name":   name,
		"redirect_uris": redirectURIs,
	})
	r := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(payload)))
	w := httptest.NewRecorder()
	app.handleOAuthRegister(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("注册测试 OAuth 客户端失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.ClientID == "" {
		t.Fatalf("解析注册响应失败: %v body=%s", err, w.Body.String())
	}
	return resp.ClientID
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

	// 3. /.well-known/oauth-authorization-server 仅声明 none（PKCE 无密钥客户端）
	req := httptest.NewRequest(http.MethodGet, "https://jk.20051212.xyz/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET oauth-authorization-server 应返回 200，实际 %d", rec.Code)
	}
	var asMeta struct {
		Issuer                            string   `json:"issuer"`
		AuthorizationEndpoint             string   `json:"authorization_endpoint"`
		TokenEndpoint                     string   `json:"token_endpoint"`
		RegistrationEndpoint              string   `json:"registration_endpoint"`
		TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
		CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &asMeta); err != nil {
		t.Fatalf("解析 AS 元数据失败: %v", err)
	}
	if asMeta.AuthorizationEndpoint != "https://jk.20051212.xyz/oauth/authorize" ||
		asMeta.TokenEndpoint != "https://jk.20051212.xyz/oauth/token" ||
		asMeta.RegistrationEndpoint != "https://jk.20051212.xyz/oauth/register" {
		t.Errorf("AS 端点地址不正确: %+v", asMeta)
	}
	if len(asMeta.TokenEndpointAuthMethodsSupported) != 1 || asMeta.TokenEndpointAuthMethodsSupported[0] != "none" {
		t.Errorf("token_endpoint_auth_methods_supported 必须仅声明 [none]，实际 %v", asMeta.TokenEndpointAuthMethodsSupported)
	}
	if len(asMeta.CodeChallengeMethodsSupported) != 1 || asMeta.CodeChallengeMethodsSupported[0] != "S256" {
		t.Errorf("必须只声明 S256 PKCE，实际 %v", asMeta.CodeChallengeMethodsSupported)
	}
}

func TestOAuthBaseURLTrustProxyGating(t *testing.T) {
	app := mcpTestApp(t)

	// 1. 默认未开启 trustProxy 时，必须忽略伪造的 X-Forwarded-Host 与 X-Forwarded-Proto
	r := httptest.NewRequest(http.MethodGet, "http://moss.internal:8080/.well-known/oauth-protected-resource", nil)
	r.RemoteAddr = "198.51.100.42:54321"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.attacker.example")
	if got := app.baseURL(r); got != "http://moss.internal:8080" {
		t.Fatalf("trustProxy=false 时不应信任 X-Forwarded-*，期望 http://moss.internal:8080，实际 %q", got)
	}

	// 2. 开启 trustProxy 且未限制 trustedProxies 时，信任反代转发头
	app.trustProxy = true
	if got := app.baseURL(r); got != "https://evil.attacker.example" {
		t.Fatalf("trustProxy=true 且无网段限制时应读取 X-Forwarded-*，实际 %q", got)
	}

	// 3. 开启 trustProxy 且配置 trustedProxies 网段时，仅当直接对端属于可信代理/环回才信任转发头
	app.trustedProxies = parseTrustedProxies("10.0.0.0/8")
	if got := app.baseURL(r); got != "http://moss.internal:8080" {
		t.Fatalf("非可信代理对端 198.51.100.42 不应被信任 X-Forwarded-*，实际 %q", got)
	}
	r.RemoteAddr = "10.1.2.3:40000"
	if got := app.baseURL(r); got != "https://evil.attacker.example" {
		t.Fatalf("可信代理对端 10.1.2.3 应被信任 X-Forwarded-*，实际 %q", got)
	}

	// 4. 配置 MOSS_PUBLIC_URL 时始终以显式配置为准
	t.Setenv("MOSS_PUBLIC_URL", "https://panel.moss.example/")
	if got := app.baseURL(r); got != "https://panel.moss.example" {
		t.Fatalf("MOSS_PUBLIC_URL 应优先返回 https://panel.moss.example，实际 %q", got)
	}
}

func TestValidResourceIndicatorAndGCOAuthCodes(t *testing.T) {
	app := mcpTestApp(t)
	r := httptest.NewRequest(http.MethodGet, "https://jk.20051212.xyz/oauth/authorize", nil)

	validResources := []string{
		"https://jk.20051212.xyz",
		"https://jk.20051212.xyz/",
		"https://jk.20051212.xyz/mcp",
		"https://jk.20051212.xyz/mcp/",
		"HTTPS://JK.20051212.XYZ/mcp",
	}
	for _, res := range validResources {
		if !app.validResourceIndicator(r, res) {
			t.Errorf("合法 resource %q 应通过校验", res)
		}
	}

	invalidResources := []string{
		"https://evil.example.com/mcp",
		"http://jk.20051212.xyz/mcp",
		"https://jk.20051212.xyz/api",
	}
	for _, res := range invalidResources {
		if app.validResourceIndicator(r, res) {
			t.Errorf("非法 resource %q 不应通过校验", res)
		}
	}

	// 验证 gcOAuthCodes 精确清理过期授权码并保留未过期授权码
	now := time.Now()
	putOAuthCode(&oauthCodeEntry{
		Code:          "moc_expired_test_1",
		ClientID:      "mc_1",
		CodeExpiresAt: now.Add(-10 * time.Second),
	}, now.Add(-70*time.Second))
	putOAuthCode(&oauthCodeEntry{
		Code:          "moc_active_test_2",
		ClientID:      "mc_1",
		CodeExpiresAt: now.Add(30 * time.Second),
	}, now)

	gcOAuthCodes(now)

	if _, ok := popOAuthCode("moc_expired_test_1"); ok {
		t.Fatal("已过期授权码应在 gcOAuthCodes 后被清除")
	}
	if _, ok := popOAuthCode("moc_active_test_2"); !ok {
		t.Fatal("未过期授权码应在 gcOAuthCodes 后保留")
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

	// 声明不支持的 client_secret_post 应返回 400 invalid_client_metadata
	r = httptest.NewRequest(http.MethodPost, "/oauth/register",
		strings.NewReader(`{"client_name":"SecretClient","redirect_uris":["http://127.0.0.1:43210/callback"],"token_endpoint_auth_method":"client_secret_post"}`))
	w = httptest.NewRecorder()
	app.handleOAuthRegister(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不支持的 token_endpoint_auth_method 应返回 400，实际 %d", w.Code)
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

func TestOAuthRejectUnregisteredClientAndDefaultReadOnlyConsent(t *testing.T) {
	app := mcpTestApp(t)
	_, challenge := testPKCEPair()

	// 1. 未注册的 client_id 请求 GET /oauth/authorize 必须被拒绝（哪怕 redirect_uri 是合法 https 地址）
	unregGET := httptest.NewRequest(http.MethodGet,
		"https://jk.20051212.xyz/oauth/authorize?response_type=code&client_id=PhishingAttackerApp"+
			"&redirect_uri="+url.QueryEscape("https://attacker.example.com/cb")+
			"&code_challenge="+challenge+"&code_challenge_method=S256", nil)
	unregRec := httptest.NewRecorder()
	app.handleOAuthAuthorizeGET(unregRec, unregGET)
	if unregRec.Code != http.StatusBadRequest {
		t.Fatalf("未注册 client_id 请求 /oauth/authorize 应返回 400，实际 %d", unregRec.Code)
	}

	// 2. 未注册的 client_id 提交 POST /oauth/authorize 同样必须被拒绝
	postForm := url.Values{
		"client_id":             {"PhishingAttackerApp"},
		"redirect_uri":          {"https://attacker.example.com/cb"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"decision":              {"approve"},
	}
	unregPOST := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/authorize", strings.NewReader(postForm.Encode()))
	unregPOST.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unregPOSTRec := httptest.NewRecorder()
	app.handleOAuthAuthorizePOST(unregPOSTRec, unregPOST)
	if unregPOSTRec.Code != http.StatusBadRequest {
		t.Fatalf("未注册 client_id POST /oauth/authorize 应返回 400，实际 %d", unregPOSTRec.Code)
	}

	// 3. 已注册客户端请求 GET /oauth/authorize（即便 URL scope 请求了 read+exec+write），
	// 页面也必须默认只勾选 read，exec 和 write 默认不勾选
	cbURI := "http://127.0.0.1:43210/callback"
	clientID := registerTestOAuthClient(t, app, "ReadDefaultClient", []string{cbURI})

	getReq := httptest.NewRequest(http.MethodGet,
		"https://jk.20051212.xyz/oauth/authorize?response_type=code&client_id="+clientID+
			"&redirect_uri="+url.QueryEscape(cbURI)+
			"&code_challenge="+challenge+"&code_challenge_method=S256&scope=read+exec+write", nil)
	getRec := httptest.NewRecorder()
	app.handleOAuthAuthorizeGET(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("已注册客户端 GET /oauth/authorize 应返回 200，实际 %d", getRec.Code)
	}
	html := getRec.Body.String()
	if !strings.Contains(html, `value="read" checked`) {
		t.Errorf("授权页应默认勾选 read，实际 HTML 片段未命中")
	}
	if strings.Contains(html, `value="exec" checked`) || strings.Contains(html, `value="write" checked`) {
		t.Errorf("授权页绝不能默认勾选 exec 或 write，必须由管理员主动勾选")
	}
}

func TestOAuthTokenNegativeCases(t *testing.T) {
	app := mcpTestApp(t)
	cbURI := "http://127.0.0.1:51234/callback"
	clientA := registerTestOAuthClient(t, app, "ClientA", []string{cbURI, "http://127.0.0.1:51234/other"})
	clientB := registerTestOAuthClient(t, app, "ClientB", []string{cbURI})
	verifier, challenge := testPKCEPair()

	issueCode := func(ttl time.Duration) string {
		now := time.Now()
		code := "moc_" + randString(40)
		putOAuthCode(&oauthCodeEntry{
			Code:          code,
			ClientID:      clientA,
			RedirectURI:   cbURI,
			CodeChallenge: challenge,
			KeyName:       "OAuth: ClientA",
			Caps:          []string{capRead},
			ExpiresAt:     now.Add(24 * time.Hour).Unix(),
			CodeExpiresAt: now.Add(ttl),
		}, now)
		return code
	}

	postToken := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "https://jk.20051212.xyz/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		app.handleOAuthToken(rec, req)
		return rec
	}

	// 1. 缺少 client_id 应返回 400 invalid_request
	code1 := issueCode(oauthCodeTTL)
	rec := postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code1},
		"redirect_uri":  {cbURI},
		"code_verifier": {verifier},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_request"`) {
		t.Fatalf("缺少 client_id 应返回 400 invalid_request，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 2. 未注册的 client_id 应返回 400 invalid_client
	code2 := issueCode(oauthCodeTTL)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code2},
		"redirect_uri":  {cbURI},
		"client_id":     {"mc_unregistered_fake_client"},
		"code_verifier": {verifier},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_client"`) {
		t.Fatalf("未注册 client_id 应返回 400 invalid_client，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 3. client_id 与授权码绑定的客户端不匹配（ClientB 冒领 ClientA 的 code）
	code3 := issueCode(oauthCodeTTL)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code3},
		"redirect_uri":  {cbURI},
		"client_id":     {clientB},
		"code_verifier": {verifier},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_grant"`) {
		t.Fatalf("client_id 不匹配应返回 400 invalid_grant，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 4. token 端点 redirect_uri 与授权请求时的 redirect_uri 不匹配
	code4 := issueCode(oauthCodeTTL)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code4},
		"redirect_uri":  {"http://127.0.0.1:51234/other"},
		"client_id":     {clientA},
		"code_verifier": {verifier},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_grant"`) {
		t.Fatalf("token 端点 redirect_uri 不匹配应返回 400 invalid_grant，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 5. 过期授权码应返回 400 invalid_grant
	code5 := issueCode(-5 * time.Second)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code5},
		"redirect_uri":  {cbURI},
		"client_id":     {clientA},
		"code_verifier": {verifier},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_grant"`) {
		t.Fatalf("过期授权码应返回 400 invalid_grant，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 6. 错误的 PKCE code_verifier 应返回 400 invalid_grant
	code6 := issueCode(oauthCodeTTL)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code6},
		"redirect_uri":  {cbURI},
		"client_id":     {clientA},
		"code_verifier": {"wrong-verifier-that-is-at-least-43-characters-long-for-pkce-s256"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_grant"`) {
		t.Fatalf("错误的 code_verifier 应返回 400 invalid_grant，实际 %d %s", rec.Code, rec.Body.String())
	}

	// 7. 非法的 resource 指示符应返回 400 invalid_target
	code7 := issueCode(oauthCodeTTL)
	rec = postToken(url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code7},
		"redirect_uri":  {cbURI},
		"client_id":     {clientA},
		"code_verifier": {verifier},
		"resource":      {"https://other.example.com/mcp"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"invalid_target"`) {
		t.Fatalf("不匹配的 resource 应返回 400 invalid_target，实际 %d %s", rec.Code, rec.Body.String())
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
	clientID := registerTestOAuthClient(t, app, "Session Authorized Client", []string{cbURI})

	f := url.Values{}
	f.Set("client_id", clientID)
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
		"client_id":     clientID,
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
