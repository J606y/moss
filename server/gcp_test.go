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
	"testing"
	"time"
)

// makeSA 生成一份可用的假 Service Account JSON 及其密钥。
func makeSA(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
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
		"client_email": "moss@test-proj.iam.gserviceaccount.com",
		"private_key":  pemStr,
		"project_id":   "test-proj",
		"token_uri":    tokenURI,
	})
	return string(raw), key
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
type mockGCP struct {
	t          *testing.T
	pub        *rsa.PublicKey
	tokenCalls int
	startCalls int
	status     string // 实例状态
	startCode  int    // start 响应码，0 = 200
	srv        *httptest.Server

	// omitExpiresIn 模拟不返回 expires_in 的 token 端点（自建/代理网关常见，
	// 真实 Google 端点总会返回，所以这条路只能靠 mock 复现）。
	omitExpiresIn bool
}

func newMockGCP(t *testing.T, status string) *mockGCP {
	m := &mockGCP{t: t, status: status}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockGCP) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/token":
		m.tokenCalls++
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			m.t.Error("grant_type 错误")
		}
		m.verifyJWT(r.Form.Get("assertion"))
		body := map[string]any{"access_token": "tok-1", "expires_in": 3600}
		if m.omitExpiresIn {
			delete(body, "expires_in")
		}
		json.NewEncoder(w).Encode(body)
	case strings.HasSuffix(r.URL.Path, "/start") && r.Method == "POST":
		m.startCalls++
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			m.t.Error("start 缺少 Bearer token")
		}
		if m.startCode != 0 {
			w.WriteHeader(m.startCode)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": m.startCode, "message": "mock failure"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "operation-1"})
	case r.Method == "GET":
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			m.t.Error("status 查询缺少 Bearer token")
		}
		json.NewEncoder(w).Encode(map[string]string{"status": m.status})
	default:
		w.WriteHeader(404)
	}
}

// verifyJWT 用公钥验签并核对 claims。
func (m *mockGCP) verifyJWT(assertion string) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		m.t.Error("JWT 应为三段")
		return
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		m.t.Errorf("签名 base64 解码失败: %v", err)
		return
	}
	if err := rsa.VerifyPKCS1v15(m.pub, crypto.SHA256, sum[:], sig); err != nil {
		m.t.Errorf("JWT 验签失败: %v", err)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
	}
	json.Unmarshal(payload, &claims)
	if claims.Iss != "moss@test-proj.iam.gserviceaccount.com" {
		m.t.Errorf("iss 错误: %s", claims.Iss)
	}
	if claims.Scope != "https://www.googleapis.com/auth/compute" {
		m.t.Errorf("scope 错误: %s", claims.Scope)
	}
	if claims.Aud != m.srv.URL+"/token" {
		m.t.Errorf("aud 错误: %s", claims.Aud)
	}
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

// TestPutGCPFailsClosedWhenEncryptFails 加密不可用时必须整单拒绝，不能把私钥明文写进库。
func TestPutGCPFailsClosedWhenEncryptFails(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")

	orig := secretRandRead
	t.Cleanup(func() { secretRandRead = orig })
	secretRandRead = func(b []byte) (int, error) { return 0, errors.New("熵源不可用") }

	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"saJson": saJSON, "autoOn": true})
	app.handlePutGCP(w, httptest.NewRequest(http.MethodPut, "/api/admin/gcp", bytes.NewReader(body)))
	if w.Code != 500 {
		t.Fatalf("加密失败时应返回 500，得到 %d: %s", w.Code, w.Body.String())
	}

	stored := getSetting(app.db, keyGCPSAJSON, "")
	if stored != "" {
		t.Fatalf("加密失败却写了库: %q", stored)
	}
	if strings.Contains(stored, "PRIVATE KEY") {
		t.Fatal("私钥明文落库")
	}
}

// TestGetGCPStoresCiphertext 正常路径下库里必须是密文，读回是原文。
func TestGetGCPStoresCiphertext(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")

	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"saJson": saJSON})
	app.handlePutGCP(w, httptest.NewRequest(http.MethodPut, "/api/admin/gcp", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}
	stored := getSetting(app.db, keyGCPSAJSON, "")
	if !strings.HasPrefix(stored, encPrefix) || strings.Contains(stored, "PRIVATE KEY") {
		t.Fatalf("凭证未加密落库: %q", stored)
	}

	w = httptest.NewRecorder()
	app.handleGetGCP(w, httptest.NewRequest(http.MethodGet, "/api/admin/gcp", nil))
	var v gcpSettingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Configured || v.ProjectID != "test-proj" {
		t.Fatalf("读回的概要不对: %+v", v)
	}
}

// TestTestGCPReportsUndecryptable 主密钥变更后，「测试」入口必须说出真实原因。
//
// 只把解不开当成没配（400 请先粘贴凭证）会把排查方向从「密钥丢了」带偏到
// 「再填一遍」，而再填一遍会覆盖掉还救得回来的密文。
func TestTestGCPReportsUndecryptable(t *testing.T) {
	useTestKey(t, "key-A")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	enc, err := encryptSecret(saJSON)
	if err != nil {
		t.Fatal(err)
	}
	setSetting(app.db, keyGCPSAJSON, enc)
	useTestKey(t, "key-B") // 运维换了 MOSS_SECRET_KEY

	w := httptest.NewRecorder()
	app.handleTestGCP(w, httptest.NewRequest(http.MethodPost, "/api/admin/gcp/test", nil))
	if w.Code == 400 {
		t.Fatalf("解不开不能报成「没配」: %s", w.Body.String())
	}
	if w.Code != 500 {
		t.Fatalf("应返回 500，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "解密") {
		t.Fatalf("错误文案应点明解密失败，得到 %s", w.Body.String())
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
