package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": 3600})
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
