package main

// GCP Spot 实例自动开机：节点确认离线后调用 Compute Engine API 重新拉起。
// 认证走 Service Account 的 OAuth2 JWT Bearer 流程（RFC 7523），全用标准库。
// 将来支持其他云厂商时，把 servers 表的 gcp_* 列泛化为 cloud_provider + 通用字段即可。

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

/* ---------- Service Account 凭证 ---------- */

type gcpSA struct {
	Type        string `json:"type"` // 须为 service_account
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"` // PKCS8 PEM
	ProjectID   string `json:"project_id"`
	TokenURI    string `json:"token_uri"` // 空则用默认 OAuth 端点
}

// parseGCPSA 解析并校验 Service Account JSON，错误信息面向用户可读（PUT 校验直接透传）。
func parseGCPSA(raw string) (*gcpSA, *rsa.PrivateKey, error) {
	var sa gcpSA
	if err := json.Unmarshal([]byte(raw), &sa); err != nil {
		return nil, nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if sa.Type != "service_account" {
		return nil, nil, errors.New("type 字段必须为 service_account")
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" || sa.ProjectID == "" {
		return nil, nil, errors.New("缺少 client_email / private_key / project_id 字段")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, nil, errors.New("private_key 不是有效的 PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("private_key 解析失败: %w", err)
	}
	key, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("private_key 不是 RSA 私钥")
	}
	return &sa, key, nil
}

/* ---------- Compute API 客户端 ---------- */

var gcpHTTP = &http.Client{Timeout: 15 * time.Second}

type gcpClient struct {
	sa   *gcpSA
	key  *rsa.PrivateKey
	base string // Compute API 前缀，测试时可换成 mock 地址

	mu    sync.Mutex
	token string
	exp   time.Time
}

func newGCPClient(saJSON string) (*gcpClient, error) {
	sa, key, err := parseGCPSA(saJSON)
	if err != nil {
		return nil, err
	}
	base := "https://compute.googleapis.com/compute/v1"
	if v := os.Getenv("MOSS_GCP_API_BASE"); v != "" {
		base = v
	}
	return &gcpClient{sa: sa, key: key, base: base}, nil
}

// accessToken 返回缓存的 access token，过期（含 60s 安全余量）则用 JWT Bearer 流程换新。
func (c *gcpClient) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.exp) {
		return c.token, nil
	}

	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iss":   c.sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/compute",
		"aud":   c.sa.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}
	input := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("JWT 签名失败: %w", err)
	}
	assertion := input + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.sa.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := gcpHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", gcpAPIError(resp)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("令牌响应无效")
	}
	c.token = tok.AccessToken
	c.exp = now.Add(time.Duration(tok.ExpiresIn-60) * time.Second)
	return c.token, nil
}

func (c *gcpClient) instanceURL(project, zone, instance string) string {
	return fmt.Sprintf("%s/projects/%s/zones/%s/instances/%s",
		c.base, url.PathEscape(project), url.PathEscape(zone), url.PathEscape(instance))
}

func (c *gcpClient) do(ctx context.Context, method, u string) (*http.Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return gcpHTTP.Do(req)
}

// InstanceStatus 查询实例状态。Spot 被抢占后为 TERMINATED（STOPPED 为等价历史值）。
func (c *gcpClient) InstanceStatus(ctx context.Context, project, zone, instance string) (string, error) {
	resp, err := c.do(ctx, "GET", c.instanceURL(project, zone, instance)+"?fields=status")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", gcpAPIError(resp)
	}
	var v struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.Status, nil
}

// StartInstance 发起开机。2xx 即视为受理（返回的是异步 Operation，不轮询：
// 容量不足等失败会让实例保持 TERMINATED，下一轮冷却后自动重试兜底）。
func (c *gcpClient) StartInstance(ctx context.Context, project, zone, instance string) error {
	resp, err := c.do(ctx, "POST", c.instanceURL(project, zone, instance)+"/start")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return gcpAPIError(resp)
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// gcpAPIError 提取 Google API 错误响应里的 message。
func gcpAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		// OAuth 端点错误格式不同
		ErrDesc string `json:"error_description"`
	}
	json.Unmarshal(body, &e)
	if e.Error.Message != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error.Message)
	}
	if e.ErrDesc != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.ErrDesc)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

/* ---------- 自动开机配置与状态机 ---------- */

// gcpConfig 自动开机全局配置，存 settings 表，独立于 notifyConfig
// （handlePutNotify 全量覆写，混入会互相重置）。
type gcpConfig struct {
	AutoOn   bool
	Delay    int // 秒，离线确认延迟，独立于 TG 离线告警延迟
	Cooldown int // 秒，两次尝试间冷却
	MaxTries int // 单次离线事件最大尝试次数
}

func loadGCPConfig(db *sql.DB) gcpConfig {
	return gcpConfig{
		AutoOn:   getSetting(db, keyGCPAutoOn, "0") == "1",
		Delay:    getSettingInt(db, keyGCPStartDelay, 120),
		Cooldown: getSettingInt(db, keyGCPStartCooldown, 300),
		MaxTries: getSettingInt(db, keyGCPStartMaxTries, 3),
	}
}

// gcpState 单节点的自动开机状态，节点重新上线或被删除时整体丢弃。
type gcpState struct {
	offlineAt time.Time // 首次观察到离线
	tries     int
	lastTry   time.Time
	lastErr   string // 前端 tooltip 展示
	inFlight  bool   // 防并发
	warnedRun bool   // RUNNING 但离线只提醒一次
	gaveUp    bool   // 达上限的「放弃」通知只发一次
}

// gcpDue 判定一台离线节点当前是否应发起一次自动开机。
// giveUp 表示已达最大尝试次数（首次判定时由调用方发放弃通知）。
func gcpDue(st *gcpState, cfg gcpConfig, now time.Time) (due, giveUp bool) {
	if now.Sub(st.offlineAt) < time.Duration(cfg.Delay)*time.Second {
		return false, false
	}
	if st.inFlight {
		return false, false
	}
	if st.tries >= cfg.MaxTries {
		return false, true
	}
	if !st.lastTry.IsZero() && now.Sub(st.lastTry) < time.Duration(cfg.Cooldown)*time.Second {
		return false, false
	}
	return true, false
}

/* ---------- 管理接口 ---------- */

type gcpSettingsView struct {
	Configured  bool   `json:"configured"`
	ClientEmail string `json:"clientEmail"`
	ProjectID   string `json:"projectId"`
	AutoOn      bool   `json:"autoOn"`
	Delay       int    `json:"delay"`
	Cooldown    int    `json:"cooldown"`
	MaxTries    int    `json:"maxTries"`
}

// handleGetGCP 返回配置概要，私钥不回显（SA 凭证能开关机器，仅展示 email/project）。
func (s *App) handleGetGCP(w http.ResponseWriter, r *http.Request) {
	cfg := loadGCPConfig(s.db)
	v := gcpSettingsView{AutoOn: cfg.AutoOn, Delay: cfg.Delay, Cooldown: cfg.Cooldown, MaxTries: cfg.MaxTries}
	if raw := decryptSecret(getSetting(s.db, keyGCPSAJSON, "")); strings.TrimSpace(raw) != "" {
		v.Configured = true
		if sa, _, err := parseGCPSA(raw); err == nil {
			v.ClientEmail = sa.ClientEmail
			v.ProjectID = sa.ProjectID
		}
	}
	writeJSON(w, 200, v)
}

func (s *App) handlePutGCP(w http.ResponseWriter, r *http.Request) {
	var f struct {
		SaJSON   string `json:"saJson"`
		ClearSa  bool   `json:"clearSa"`
		AutoOn   bool   `json:"autoOn"`
		Delay    int    `json:"delay"`
		Cooldown int    `json:"cooldown"`
		MaxTries int    `json:"maxTries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if f.ClearSa {
		setSetting(s.db, keyGCPSAJSON, "")
	} else if sa := strings.TrimSpace(f.SaJSON); sa != "" {
		if _, _, err := parseGCPSA(sa); err != nil {
			writeErr(w, 400, "凭证无效: "+err.Error())
			return
		}
		setSetting(s.db, keyGCPSAJSON, encryptSecret(sa)) // 私钥加密落库
	} // 留空且未清除 = 保留旧凭证
	on := "0"
	if f.AutoOn {
		on = "1"
	}
	setSetting(s.db, keyGCPAutoOn, on)
	setSetting(s.db, keyGCPStartDelay, strconv.Itoa(clampInt(f.Delay, 60, 3600, 120)))
	setSetting(s.db, keyGCPStartCooldown, strconv.Itoa(clampInt(f.Cooldown, 60, 3600, 300)))
	setSetting(s.db, keyGCPStartMaxTries, strconv.Itoa(clampInt(f.MaxTries, 1, 10, 3)))
	s.notifier.Reload()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleTestGCP 用当前保存的凭证真实换一次 access token，验证凭证可用。
func (s *App) handleTestGCP(w http.ResponseWriter, r *http.Request) {
	raw := decryptSecret(getSetting(s.db, keyGCPSAJSON, ""))
	if strings.TrimSpace(raw) == "" {
		writeErr(w, 400, "请先粘贴并保存 Service Account 凭证")
		return
	}
	cli, err := newGCPClient(raw)
	if err != nil {
		writeErr(w, 400, "凭证无效: "+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := cli.accessToken(ctx); err != nil {
		writeErr(w, 502, "换取访问令牌失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "clientEmail": cli.sa.ClientEmail, "projectId": cli.sa.ProjectID})
}

// handleGCPManualStart 手动立即开机：忽略冷却、不消耗自动尝试次数。
func (s *App) handleGCPManualStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var (
		name, project, zone, instance string
		enabled                       bool
	)
	if err := s.db.QueryRow(
		`SELECT name, gcp_enabled, gcp_project, gcp_zone, gcp_instance FROM servers WHERE id = ?`, id).
		Scan(&name, &enabled, &project, &zone, &instance); err != nil {
		writeErr(w, 404, "服务器不存在")
		return
	}
	if !enabled || zone == "" || instance == "" {
		writeErr(w, 400, "该服务器未启用 GCP 自动开机或未填写 zone/实例名")
		return
	}
	status, started, err := s.notifier.ManualStartGCP(id, project, zone, instance)
	if errors.Is(err, errGCPBusy) {
		writeErr(w, 409, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	msg := fmt.Sprintf("实例当前状态 %s，未执行开机", status)
	if started {
		msg = "已调用 instances.start，等待实例启动与节点上线"
	} else if status == "RUNNING" {
		msg = "实例已在运行；若节点仍离线，请检查 agent 或网络"
	}
	writeJSON(w, 200, map[string]any{"ok": true, "status": status, "started": started, "message": msg})
}
