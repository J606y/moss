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
	"log"
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
	c.exp = gcpTokenExpiry(now, tok.ExpiresIn)
	return c.token, nil
}

const (
	gcpTokenSkew       = 60 * time.Second   // 提前量：避免拿着「刚好到期」的 token 上路
	gcpTokenDefaultTTL = 3600 * time.Second // 端点未给 expires_in 时的假定寿命，与 Google 的实际值一致
	gcpTokenMaxTTL     = 3600 * time.Second // 上限：再长也当一小时用，多换几次无害，抱着假的长寿命有害
)

// gcpTokenExpiry 由 expires_in 推出缓存到期时刻。
//
// 直接 now+expiresIn-60s 有两个坑：expires_in 缺失（解码只校验 access_token 非空，
// 缺字段会解成 0）时到期时刻落在 now-60s，缓存判断恒假，每次 API 调用都要重签 JWT
// 再换一次 token；expires_in 本身小于 60 时同理。真实 Google 端点总会返回 3600，
// 所以这条路只在换成自建或代理的 token 端点时才暴露。
//
// 提前量按寿命取比例封顶，而不是固定 60s：对一个 30 秒的短寿命 token，固定 60s
// 会把到期时刻推到过去（还是不缓存），而直接去掉提前量又会缓存到真正失效之后换来 401。
func gcpTokenExpiry(now time.Time, expiresIn int) time.Time {
	ttl := time.Duration(expiresIn) * time.Second
	if expiresIn <= 0 {
		ttl = gcpTokenDefaultTTL
	}
	if ttl > gcpTokenMaxTTL {
		ttl = gcpTokenMaxTTL
	}
	skew := gcpTokenSkew
	if skew > ttl/10 {
		skew = ttl / 10
	}
	return now.Add(ttl - skew)
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

// gcpCredentialView 凭证的对外视图。**没有 sa_json 字段，也不该有**：
// 私钥一旦回显就等于把开关机器的权限交给了任何能打开后台页面的人。
type gcpCredentialView struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	ClientEmail string `json:"clientEmail"`
	ServerCount int    `json:"serverCount"` // 绑定本凭证且已开启自动开机的节点数
	CreatedAt   int64  `json:"createdAt"`
	// Decryptable 为 false 表示密文还在但解不开（主密钥变更/secret.key 丢失）。
	// 单凭证时代这种情况只能显示成「未配置」，会诱导用户重填一遍，
	// 而重填就用新密钥盖掉了原本还救得回来的密文。
	Decryptable bool `json:"decryptable"`
}

type gcpSettingsView struct {
	Credentials []gcpCredentialView `json:"credentials"`
	// UnboundCount 已开启自动开机却没绑任何凭证的节点数，>0 时前端出警示。
	UnboundCount int  `json:"unboundCount"`
	AutoOn       bool `json:"autoOn"`
	Delay        int  `json:"delay"`
	Cooldown     int  `json:"cooldown"`
	MaxTries     int  `json:"maxTries"`
}

// handleGetGCP 返回全局参数与凭证列表，私钥不回显。
func (s *App) handleGetGCP(w http.ResponseWriter, r *http.Request) {
	cfg := loadGCPConfig(s.db)
	v := gcpSettingsView{
		Credentials: []gcpCredentialView{},
		AutoOn:      cfg.AutoOn, Delay: cfg.Delay, Cooldown: cfg.Cooldown, MaxTries: cfg.MaxTries,
	}
	creds, err := listGCPCredentials(s.db)
	if err != nil {
		log.Printf("列出 GCP 凭证失败: %v", err)
		writeErr(w, errInternal)
		return
	}
	for _, c := range creds {
		raw, err := decryptSecretValue(c.SaJSON)
		if err != nil {
			log.Printf("GCP 凭证 %s（%s）解密失败: %v", c.ID, c.ClientEmail, err)
		}
		v.Credentials = append(v.Credentials, gcpCredentialView{
			ID: c.ID, ProjectID: c.ProjectID, ClientEmail: c.ClientEmail,
			ServerCount: c.ServerCount, CreatedAt: c.CreatedAt,
			Decryptable: err == nil && strings.TrimSpace(raw) != "",
		})
	}
	s.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE gcp_enabled = 1 AND gcp_cred_id = ''`).Scan(&v.UnboundCount)
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
		writeErr(w, errBadJSON)
		return
	}
	// 凭证已改为多份，由 /api/admin/gcp/credentials 管理。这里必须显式报错：
	// 悄悄忽略一次私钥写入，调用方（缓存的旧页面、别人的脚本）会拿着 200 以为存好了，
	// 而实际上凭证从未落库——静默吞掉写密钥是最坏的一种「兼容」。
	if strings.TrimSpace(f.SaJSON) != "" || f.ClearSa {
		writeErr(w, errGCPLegacyEndpoint)
		return
	}
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

// handleAddGCPCredential 新增一份 Service Account 凭证。
func (s *App) handleAddGCPCredential(w http.ResponseWriter, r *http.Request) {
	var f struct {
		SaJSON string `json:"saJson"`
	}
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, errBadJSON)
		return
	}
	raw := strings.TrimSpace(f.SaJSON)
	if raw == "" {
		writeErr(w, errGCPJSONRequired)
		return
	}
	sa, _, err := parseGCPSA(raw)
	if err != nil {
		writeErr(w, errGCPCredInvalid.with(err.Error()))
		return
	}
	// 先按邮箱查重，别等唯一索引把 "UNIQUE constraint failed" 甩到用户脸上。
	var dup string
	if err := s.db.QueryRow(
		`SELECT id FROM gcp_credentials WHERE client_email = ?`, sa.ClientEmail).Scan(&dup); err == nil {
		writeErr(w, errGCPCredDuplicate.with(sa.ClientEmail))
		return
	}
	enc, err := encryptSecret(raw)
	if err != nil {
		// 加密不成就不落库：明文入库看不出异常，只会让私钥悄悄裸奔在数据库里。
		log.Printf("加密 GCP 凭证失败: %v", err)
		writeErr(w, errGCPEncrypt)
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleAddGCPCredential begin: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer tx.Rollback()

	// 从「只有一份」变成「不止一份」的这一刻，是唯一能安全推断未绑定节点归属的时机。
	//
	// 未绑定节点平时靠 resolveGCPCredID 的「全场只有一份就用那份」活着。一旦加进第二份，
	// 这条推断立刻失效，它们会集体停止守护——而凭证解析失败走的是 gcpStartAttempt 里
	// cli==nil 那条不发 Telegram 的分支，用户唯一的线索是后台按钮上的一行 tooltip。
	// 所以在这里把它们显式钉到原来那份凭证上，前提消失之前先把账认掉。
	var n int
	var only string
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(MIN(id), '') FROM gcp_credentials`).Scan(&n, &only); err != nil {
		log.Printf("handleAddGCPCredential count: %v", err)
		writeErr(w, errInternal)
		return
	}
	if n == 1 {
		if _, err := tx.Exec(`UPDATE servers SET gcp_cred_id = ? WHERE gcp_cred_id = ''`, only); err != nil {
			log.Printf("handleAddGCPCredential 回填绑定: %v", err)
			writeErr(w, errInternal)
			return
		}
	}
	id := randString(8)
	if _, err := tx.Exec(
		`INSERT INTO gcp_credentials(id, project_id, client_email, sa_json, created_at) VALUES(?, ?, ?, ?, ?)`,
		id, sa.ProjectID, sa.ClientEmail, enc, time.Now().Unix()); err != nil {
		// 查重与插入之间被另一个请求抢先时唯一索引兜底，仍给人话。
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, errGCPCredDuplicate.with(sa.ClientEmail))
			return
		}
		log.Printf("handleAddGCPCredential insert: %v", err)
		writeErr(w, errInternal)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleAddGCPCredential commit: %v", err)
		writeErr(w, errInternal)
		return
	}
	s.notifier.Reload()
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "clientEmail": sa.ClientEmail, "projectId": sa.ProjectID})
}

// handleDeleteGCPCredential 删除一份凭证。
//
// 占用判定只看「已开启自动开机」的节点，不看全部绑定节点。这不是宽松，是留出口：
// 没有「更换密钥」按钮 + client_email 唯一索引，如果按全部绑定节点判定，
// 那么「只剩一份凭证且密钥被 GCP 吊销」时，新的加不进（邮箱重复）、旧的删不掉（有节点绑着），
// 用户被彻底锁死。按已启用判定后，关掉那几台的开关就能删旧加新再重选。
func (s *App) handleDeleteGCPCredential(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cred, err := loadGCPCredential(s.db, id)
	if err != nil {
		writeErr(w, errCredNotFound)
		return
	}
	rows, err := s.db.Query(
		`SELECT name FROM servers WHERE gcp_cred_id = ? AND gcp_enabled = 1 ORDER BY sort, created_at`, id)
	if err != nil {
		log.Printf("handleDeleteGCPCredential query: %v", err)
		writeErr(w, errInternal)
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil {
			names = append(names, name)
		}
	}
	rows.Close()
	if len(names) > 0 {
		shown := names
		suffix := ""
		if len(shown) > 5 {
			shown, suffix = shown[:5], fmt.Sprintf(" 等 %d 台", len(names))
		}
		// 被哪几台占用是排查的关键信息，整段进 Detail 原样带给前端；
		// 机器名本身不翻译，句子结构由前端文案决定。
		writeErr(w, errGCPCredInUse.with(fmt.Sprintf("%s（%d 台：%s%s）",
			cred.ClientEmail, len(names), strings.Join(shown, "、"), suffix)))
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		log.Printf("handleDeleteGCPCredential begin: %v", err)
		writeErr(w, errInternal)
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM gcp_credentials WHERE id = ?`, id); err != nil {
		log.Printf("handleDeleteGCPCredential delete: %v", err)
		writeErr(w, errInternal)
		return
	}
	// 关着开关但绑着它的节点在这里解绑，不留悬空引用——
	// 悬空 id 在 resolveGCPCredID 那边是硬错误，留着只会让用户下次打开开关时莫名其妙。
	if _, err := tx.Exec(`UPDATE servers SET gcp_cred_id = '' WHERE gcp_cred_id = ?`, id); err != nil {
		log.Printf("handleDeleteGCPCredential 解绑: %v", err)
		writeErr(w, errInternal)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("handleDeleteGCPCredential commit: %v", err)
		writeErr(w, errInternal)
		return
	}
	s.notifier.Reload()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleTestGCPCredential 用指定凭证真实换一次 access token，验证其可用。
//
// 直接建客户端而不走 notifier 的缓存：「测试」这个动作必须是一次真实往返，
// 拿缓存里的旧 token 说「连接成功」，正是用户点这个按钮时最不想要的答案。
func (s *App) handleTestGCPCredential(w http.ResponseWriter, r *http.Request) {
	cred, err := loadGCPCredential(s.db, r.PathValue("id"))
	if err != nil {
		writeErr(w, errCredNotFound)
		return
	}
	raw, err := decryptSecretValue(cred.SaJSON)
	if err != nil {
		// 「测试」正是运维用来定位问题的入口，这里必须说出真实原因，
		// 而不是把解不开伪装成没填、让人去重填一遍。
		writeErr(w, errGCPCredUndecryptable)
		return
	}
	if strings.TrimSpace(raw) == "" {
		writeErr(w, errGCPCredEmpty)
		return
	}
	cli, err := newGCPClient(raw)
	if err != nil {
		writeErr(w, errGCPCredInvalid.with(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if _, err := cli.accessToken(ctx); err != nil {
		writeErr(w, errGCPTokenFailed.with(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "clientEmail": cli.sa.ClientEmail, "projectId": cli.sa.ProjectID})
}

// handleGCPManualStart 手动立即开机：忽略冷却、不消耗自动尝试次数。
func (s *App) handleGCPManualStart(w http.ResponseWriter, r *http.Request) {
	t := gcpTarget{id: r.PathValue("id")}
	var enabled bool
	if err := s.db.QueryRow(
		`SELECT name, gcp_enabled, gcp_cred_id, gcp_project, gcp_zone, gcp_instance FROM servers WHERE id = ?`, t.id).
		Scan(&t.name, &enabled, &t.credID, &t.project, &t.zone, &t.instance); err != nil {
		writeErr(w, errServerNotFound)
		return
	}
	if !enabled || t.zone == "" || t.instance == "" {
		writeErr(w, errGCPNotEnabled)
		return
	}
	status, started, err := s.notifier.ManualStartGCP(t)
	if errors.Is(err, errGCPBusy) {
		writeErrFrom(w, 409, err, errInternal)
		return
	}
	if err != nil {
		writeErrFrom(w, 502, err, errInternal)
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
