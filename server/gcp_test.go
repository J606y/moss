package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeSA 生成一份可用的假 Service Account JSON 及其密钥。
func makeSA(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()
	return makeSAAs(t, "moss@test-proj.iam.gserviceaccount.com", "test-proj", tokenURI)
}

// makeSAAs 同上，但可指定账号与项目——多凭证用例需要两个 client_email 各异的 SA，
// 否则会撞上 gcp_credentials 的唯一索引。
func makeSAAs(t *testing.T, email, project, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": email,
		"private_key":  pemStr,
		"project_id":   project,
		"token_uri":    tokenURI,
	})
	return string(raw), key
}

// addTestCred 走 HTTP 接口加一份凭证，返回其 id。
func addTestCred(t *testing.T, app *App, saJSON string) string {
	t.Helper()
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"saJson": saJSON})
	app.handleAddGCPCredential(w, httptest.NewRequest(http.MethodPost, "/api/admin/gcp/credentials", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("添加凭证失败 %d: %s", w.Code, w.Body.String())
	}
	var res struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || res.ID == "" {
		t.Fatalf("添加凭证响应异常: %s", w.Body.String())
	}
	return res.ID
}

// getGCPView 读一次设置视图，同时把原始响应体交给调用方做泄漏断言。
func getGCPView(t *testing.T, app *App) (gcpSettingsView, string) {
	t.Helper()
	w := httptest.NewRecorder()
	app.handleGetGCP(w, httptest.NewRequest(http.MethodGet, "/api/admin/gcp", nil))
	if w.Code != 200 {
		t.Fatalf("读取 GCP 设置失败 %d: %s", w.Code, w.Body.String())
	}
	var v gcpSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v, w.Body.String()
}

func TestParseGCPSA(t *testing.T) {
	valid, _ := makeSA(t, "")
	if sa, key, err := parseGCPSA(valid); err != nil || key == nil {
		t.Fatalf("合法凭证应通过: %v", err)
	} else if sa.TokenURI != "https://oauth2.googleapis.com/token" {
		t.Fatalf("token_uri 缺省值错误: %s", sa.TokenURI)
	}

	bad := []struct{ name, raw string }{
		{"非 JSON", "not json"},
		{"type 错误", `{"type":"user","client_email":"a@b","private_key":"x","project_id":"p"}`},
		{"缺字段", `{"type":"service_account","client_email":"a@b"}`},
		{"坏 PEM", `{"type":"service_account","client_email":"a@b","private_key":"not pem","project_id":"p"}`},
	}
	for _, c := range bad {
		if _, _, err := parseGCPSA(c.raw); err == nil {
			t.Errorf("%s: 应报错", c.name)
		}
	}
}

// mockGCP 同时扮演 OAuth 端点与 Compute API，记录调用。
//
// 一个 mock 可以同时服务多份凭证：accounts 按 client_email 登记公钥，
// 计数也按账号/项目分开记。这是多凭证用例的基础设施——
// gcpClient.base 取自进程级环境变量 MOSS_GCP_API_BASE（见 newGCPClient），
// 一个变量指不到两个 mock，只能让同一个 mock 按 URL 里的 project 分流。
type mockGCP struct {
	t   *testing.T
	pub *rsa.PublicKey
	// accounts 非空时按 iss 选公钥（多账号模式），否则只认 makeSA 的默认账号。
	accounts map[string]*rsa.PublicKey

	mu           sync.Mutex
	tokenCalls   int
	startCalls   int
	tokenCallsBy map[string]int // client_email → 换取 token 次数
	startCallsBy map[string]int // project → start 次数

	status    string // 实例状态
	startCode int    // start 响应码，0 = 200
	srv       *httptest.Server

	// omitExpiresIn 模拟不返回 expires_in 的 token 端点（自建/代理网关常见，
	// 真实 Google 端点总会返回，所以这条路只能靠 mock 复现）。
	omitExpiresIn bool
}

func newMockGCP(t *testing.T, status string) *mockGCP {
	m := &mockGCP{
		t: t, status: status,
		accounts:     map[string]*rsa.PublicKey{},
		tokenCallsBy: map[string]int{},
		startCallsBy: map[string]int{},
	}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

// registerSA 生成一份指向本 mock 的 SA JSON 并登记其公钥，供多账号用例使用。
func (m *mockGCP) registerSA(t *testing.T, email, project string) string {
	t.Helper()
	raw, key := makeSAAs(t, email, project, m.srv.URL+"/token")
	m.mu.Lock()
	m.accounts[email] = &key.PublicKey
	m.mu.Unlock()
	return raw
}

func (m *mockGCP) counts() (tokenBy, startBy map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tokenBy, startBy = map[string]int{}, map[string]int{}
	for k, v := range m.tokenCallsBy {
		tokenBy[k] = v
	}
	for k, v := range m.startCallsBy {
		startBy[k] = v
	}
	return
}

// projectFromPath 从 /compute/v1/projects/{proj}/zones/... 里取出项目 ID。
func projectFromPath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		if s == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

func (m *mockGCP) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			m.t.Error("grant_type 错误")
		}
		iss := m.verifyJWT(r.Form.Get("assertion"))
		m.mu.Lock()
		m.tokenCalls++
		m.tokenCallsBy[iss]++
		omit := m.omitExpiresIn
		m.mu.Unlock()
		body := map[string]any{"access_token": "tok-1", "expires_in": 3600}
		if omit {
			delete(body, "expires_in")
		}
		json.NewEncoder(w).Encode(body)
	case strings.HasSuffix(r.URL.Path, "/start") && r.Method == "POST":
		m.mu.Lock()
		m.startCalls++
		m.startCallsBy[projectFromPath(r.URL.Path)]++
		code := m.startCode
		m.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			m.t.Error("start 缺少 Bearer token")
		}
		if code != 0 {
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "mock failure"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "operation-1"})
	case r.Method == "GET":
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			m.t.Error("status 查询缺少 Bearer token")
		}
		m.mu.Lock()
		status := m.status
		m.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"status": status})
	default:
		w.WriteHeader(404)
	}
}

// verifyJWT 用公钥验签并核对 claims，返回 iss（即 client_email）。
func (m *mockGCP) verifyJWT(assertion string) string {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		m.t.Error("JWT 应为三段")
		return ""
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
	}
	json.Unmarshal(payload, &claims)

	// 先按 iss 定位公钥再验签：多账号模式下每个 SA 各有一把私钥。
	m.mu.Lock()
	pub, multi := m.accounts[claims.Iss], len(m.accounts) > 0
	m.mu.Unlock()
	if !multi {
		pub = m.pub
		if claims.Iss != "moss@test-proj.iam.gserviceaccount.com" {
			m.t.Errorf("iss 错误: %s", claims.Iss)
		}
	} else if pub == nil {
		m.t.Errorf("未登记的 iss: %s", claims.Iss)
		return claims.Iss
	}

	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		m.t.Errorf("签名 base64 解码失败: %v", err)
		return claims.Iss
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		m.t.Errorf("JWT 验签失败: %v", err)
	}
	if claims.Scope != "https://www.googleapis.com/auth/compute" {
		m.t.Errorf("scope 错误: %s", claims.Scope)
	}
	if claims.Aud != m.srv.URL+"/token" {
		m.t.Errorf("aud 错误: %s", claims.Aud)
	}
	return claims.Iss
}

func newTestClient(t *testing.T, m *mockGCP) *gcpClient {
	raw, key := makeSA(t, m.srv.URL+"/token")
	m.pub = &key.PublicKey
	cli, err := newGCPClient(raw)
	if err != nil {
		t.Fatal(err)
	}
	cli.base = m.srv.URL + "/compute/v1"
	return cli
}

func TestAccessTokenCache(t *testing.T) {
	m := newMockGCP(t, "TERMINATED")
	cli := newTestClient(t, m)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if tok, err := cli.accessToken(ctx); err != nil || tok != "tok-1" {
			t.Fatalf("accessToken: %v", err)
		}
	}
	if m.tokenCalls != 1 {
		t.Fatalf("缓存未命中，token 端点被调用 %d 次", m.tokenCalls)
	}
	// 缓存过期后应刷新
	cli.mu.Lock()
	cli.exp = time.Now().Add(-time.Second)
	cli.mu.Unlock()
	if _, err := cli.accessToken(ctx); err != nil {
		t.Fatal(err)
	}
	if m.tokenCalls != 2 {
		t.Fatalf("过期后未刷新，调用 %d 次", m.tokenCalls)
	}
}

// TestAccessTokenCachedWithoutExpiresIn 端点不返回 expires_in 时缓存仍须生效。
//
// 旧写法 now+expiresIn-60s 会把到期时刻算成 now-60s，缓存判断恒假，于是每次
// API 调用都要重签一次 RSA JWT 并重换 token——功能看着正常，代价全在延迟与配额上。
func TestAccessTokenCachedWithoutExpiresIn(t *testing.T) {
	m := newMockGCP(t, "TERMINATED")
	m.omitExpiresIn = true
	cli := newTestClient(t, m)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if tok, err := cli.accessToken(ctx); err != nil || tok != "tok-1" {
			t.Fatalf("accessToken: %v", err)
		}
	}
	if m.tokenCalls != 1 {
		t.Fatalf("缺 expires_in 时缓存失效，token 端点被调用 %d 次", m.tokenCalls)
	}
}

func TestGCPTokenExpiry(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		expiresIn int
		wantTTL   time.Duration // 相对 now 的缓存时长
	}{
		{"正常一小时", 3600, 3540 * time.Second},
		{"缺字段（解成 0）", 0, 3540 * time.Second},
		{"负值同样不可信", -1, 3540 * time.Second},
		{"短寿命按比例留余量", 30, 27 * time.Second},
		{"恰好 60 秒", 60, 54 * time.Second},
		{"离谱的长寿命封顶", 86400, 3540 * time.Second},
	}
	for _, c := range cases {
		got := gcpTokenExpiry(now, c.expiresIn)
		if !got.After(now) {
			t.Errorf("%s: 到期时刻 %v 不在 now 之后，缓存等于没开", c.name, got.Sub(now))
			continue
		}
		if d := got.Sub(now); d != c.wantTTL {
			t.Errorf("%s: 缓存时长 %v，期望 %v", c.name, d, c.wantTTL)
		}
		// 提前量不能把到期时刻推到 token 真实寿命之外，否则缓存命中即 401。
		if c.expiresIn > 0 && got.After(now.Add(time.Duration(c.expiresIn)*time.Second)) {
			t.Errorf("%s: 缓存超出了 token 实际寿命", c.name)
		}
	}
}

// TestAddGCPCredFailsClosedWhenEncryptFails 加密不可用时必须整单拒绝，不能把私钥明文写进库。
func TestAddGCPCredFailsClosedWhenEncryptFails(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")

	orig := secretRandRead
	t.Cleanup(func() { secretRandRead = orig })
	secretRandRead = func(b []byte) (int, error) { return 0, errors.New("熵源不可用") }

	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"saJson": saJSON})
	app.handleAddGCPCredential(w, httptest.NewRequest(http.MethodPost, "/api/admin/gcp/credentials", bytes.NewReader(body)))
	if w.Code != 500 {
		t.Fatalf("加密失败时应返回 500，得到 %d: %s", w.Code, w.Body.String())
	}
	if n, _ := countGCPCredentials(app.db); n != 0 {
		t.Fatalf("加密失败却落了库: %d 行", n)
	}
}

// TestAddGCPCredStoresCiphertext 正常路径下库里必须是密文，视图能读回 project/email。
func TestAddGCPCredStoresCiphertext(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	id := addTestCred(t, app, saJSON)

	var stored string
	if err := app.db.QueryRow(`SELECT sa_json FROM gcp_credentials WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, encPrefix) || strings.Contains(stored, "PRIVATE KEY") {
		t.Fatalf("凭证未加密落库: %q", stored)
	}

	v, raw := getGCPView(t, app)
	if len(v.Credentials) != 1 {
		t.Fatalf("应有 1 份凭证: %+v", v)
	}
	c := v.Credentials[0]
	if c.ProjectID != "test-proj" || c.ClientEmail != "moss@test-proj.iam.gserviceaccount.com" || !c.Decryptable {
		t.Fatalf("凭证概要不对: %+v", c)
	}
	// 响应体整体不得含私钥或密文。断言原始 body 而非结构体字段：
	// 将来谁给视图加了个新字段并顺手带上 sa_json，这里立刻炸。
	if strings.Contains(raw, "PRIVATE KEY") || strings.Contains(raw, encPrefix) {
		t.Fatalf("响应体泄漏了凭证内容: %s", raw)
	}
}

// TestTestGCPCredReportsUndecryptable 主密钥变更后，「测试」入口必须说出真实原因。
//
// 只把解不开当成没配（400 请先粘贴凭证）会把排查方向从「密钥丢了」带偏到
// 「再填一遍」，而再填一遍会覆盖掉还救得回来的密文。
func TestTestGCPCredReportsUndecryptable(t *testing.T) {
	useTestKey(t, "key-A")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	id := addTestCred(t, app, saJSON)
	useTestKey(t, "key-B") // 运维换了 MOSS_SECRET_KEY

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/gcp/credentials/"+id+"/test", nil)
	req.SetPathValue("id", id)
	app.handleTestGCPCredential(w, req)
	if w.Code == 400 {
		t.Fatalf("解不开不能报成「没配」: %s", w.Body.String())
	}
	if w.Code != 500 {
		t.Fatalf("应返回 500，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "解密") {
		t.Fatalf("错误文案应点明解密失败，得到 %s", w.Body.String())
	}

	// 解不开也必须照常列出来，且明确标记 decryptable=false——
	// 显示成「未配置」会诱导用户重填，那才是真正把密文毁掉的一步。
	v, _ := getGCPView(t, app)
	if len(v.Credentials) != 1 || v.Credentials[0].Decryptable {
		t.Fatalf("解不开的凭证应照常列出并标记不可解: %+v", v.Credentials)
	}
}

// TestPutGCPRejectsLegacyCredFields 老接口不再接受凭证字段，且必须显式报错。
// 静默忽略会让调用方拿着 200 以为私钥存好了，而实际上它从未落库。
func TestPutGCPRejectsLegacyCredFields(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")

	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"带 saJson", map[string]any{"saJson": saJSON, "autoOn": true}},
		{"带 clearSa", map[string]any{"clearSa": true, "autoOn": true}},
	} {
		w := httptest.NewRecorder()
		body, _ := json.Marshal(c.body)
		app.handlePutGCP(w, httptest.NewRequest(http.MethodPut, "/api/admin/gcp", bytes.NewReader(body)))
		if w.Code != 400 {
			t.Fatalf("%s: 应返回 400，得到 %d: %s", c.name, w.Code, w.Body.String())
		}
		if n, _ := countGCPCredentials(app.db); n != 0 {
			t.Fatalf("%s: 不该落库", c.name)
		}
	}
}

func TestInstanceStatusAndStart(t *testing.T) {
	m := newMockGCP(t, "TERMINATED")
	cli := newTestClient(t, m)
	ctx := context.Background()

	status, err := cli.InstanceStatus(ctx, "test-proj", "us-central1-a", "vm-1")
	if err != nil || status != "TERMINATED" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if err := cli.StartInstance(ctx, "test-proj", "us-central1-a", "vm-1"); err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if m.startCalls != 1 {
		t.Fatalf("start 调用 %d 次", m.startCalls)
	}
}

func TestStartInstanceError(t *testing.T) {
	m := newMockGCP(t, "TERMINATED")
	m.startCode = 403
	cli := newTestClient(t, m)

	err := cli.StartInstance(context.Background(), "test-proj", "us-central1-a", "vm-1")
	if err == nil || !strings.Contains(err.Error(), "mock failure") {
		t.Fatalf("应透传 Google 错误信息，得到: %v", err)
	}
}

func TestGCPDue(t *testing.T) {
	cfg := gcpConfig{AutoOn: true, Delay: 120, Cooldown: 300, MaxTries: 3}
	now := time.Now()
	cases := []struct {
		name       string
		st         gcpState
		wantDue    bool
		wantGiveUp bool
	}{
		{"未过确认延迟", gcpState{offlineAt: now.Add(-60 * time.Second)}, false, false},
		{"过延迟应触发", gcpState{offlineAt: now.Add(-121 * time.Second)}, true, false},
		{"执行中跳过", gcpState{offlineAt: now.Add(-200 * time.Second), inFlight: true}, false, false},
		{"冷却中跳过", gcpState{offlineAt: now.Add(-500 * time.Second), tries: 1, lastTry: now.Add(-100 * time.Second)}, false, false},
		{"冷却结束重试", gcpState{offlineAt: now.Add(-500 * time.Second), tries: 1, lastTry: now.Add(-301 * time.Second)}, true, false},
		{"达上限放弃", gcpState{offlineAt: now.Add(-9999 * time.Second), tries: 3, lastTry: now.Add(-9000 * time.Second)}, false, true},
	}
	for _, c := range cases {
		due, giveUp := gcpDue(&c.st, cfg, now)
		if due != c.wantDue || giveUp != c.wantGiveUp {
			t.Errorf("%s: due=%v giveUp=%v, 期望 %v/%v", c.name, due, giveUp, c.wantDue, c.wantGiveUp)
		}
	}
}
