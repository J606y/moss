// MCP 标准 OAuth 2.1（Authorization Code + PKCE S256）接入层。
//
// 设计原则：
//  1. 复用现有 API Key 体系（apikey.go）：OAuth 授权最终在 api_keys 表签发一把真实 mak_ 密钥
//     作为 access_token 下发。这样 /mcp 的鉴权、能力集（read/exec/write）、机器白名单、
//     面板「AI 接入」里的实时编辑/停用/删除、以及执行审计（caller）无需任何额外分支。
//  2. 复用现有管理员鉴权（auth.go）：授权页检测到浏览器已有 moss_session 会话时可直接批准；
//     未登录时在同页校验管理员账号密码，并共享 beginLoginAttempt 的并发防爆破锁定机制。
//  3. 完整实现 RFC 9728（受保护资源元数据）、RFC 8414（授权服务器元数据）、
//     RFC 7591（动态客户端注册 DCR）与 OAuth 2.1 PKCE（强制 S256）。
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	oauthCodeTTL        = 60 * time.Second
	oauthMaxRegisterLen = 64 << 10
	oauthMaxClients     = 1000
	oauthMaxCodes       = 512
)

// oauthCodeEntry 是一次性授权码的内存视图（60 秒有效，单次换取即销毁）。
type oauthCodeEntry struct {
	Code          string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Resource      string
	KeyName       string
	Caps          []string
	Servers       []string
	ExpiresAt     int64
	CodeExpiresAt time.Time
}

var (
	oauthMu    sync.Mutex
	oauthCodes = make(map[string]*oauthCodeEntry)
)

func putOAuthCode(e *oauthCodeEntry, now time.Time) {
	oauthMu.Lock()
	defer oauthMu.Unlock()
	for k, v := range oauthCodes {
		if !now.Before(v.CodeExpiresAt) {
			delete(oauthCodes, k)
		}
	}
	// 防极端情况下的内存堆积：超出上限时淘汰最早到期的一条
	if len(oauthCodes) >= oauthMaxCodes {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range oauthCodes {
			if oldestKey == "" || v.CodeExpiresAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.CodeExpiresAt
			}
		}
		if oldestKey != "" {
			delete(oauthCodes, oldestKey)
		}
	}
	oauthCodes[e.Code] = e
}

// popOAuthCode 原子取出并销毁授权码，保证同一个 code 绝不可能被消费两次。
func popOAuthCode(code string) (*oauthCodeEntry, bool) {
	oauthMu.Lock()
	defer oauthMu.Unlock()
	e, ok := oauthCodes[code]
	if ok {
		delete(oauthCodes, code)
	}
	return e, ok
}

func gcOAuthCodes(now time.Time) {
	oauthMu.Lock()
	defer oauthMu.Unlock()
	for k, v := range oauthCodes {
		if !now.Before(v.CodeExpiresAt) {
			delete(oauthCodes, k)
		}
	}
}

// pruneOAuthClients 限制动态注册客户端表行数上限，防无鉴权 DCR 接口被刷爆磁盘。
func pruneOAuthClients(db *sql.DB, maxRows int) {
	if maxRows <= 0 {
		return
	}
	if _, err := db.Exec(
		`DELETE FROM oauth_clients WHERE client_id IN (
			SELECT client_id FROM oauth_clients ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?
		)`,
		maxRows,
	); err != nil {
		log.Printf("清理 oauth_clients 失败: %v", err)
	}
}

// baseURL 推断当前请求的对外基准地址（scheme://host），用于拼装元数据与 WWW-Authenticate。
// 若配置了 MOSS_PUBLIC_URL（例如反代回源改写了 Host 头），优先使用它；其次读取 X-Forwarded-Host。
func (s *App) baseURL(r *http.Request) string {
	if pub := strings.TrimRight(strings.TrimSpace(os.Getenv("MOSS_PUBLIC_URL")), "/"); pub != "" {
		return pub
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if i := strings.IndexByte(host, ','); i >= 0 {
		host = strings.TrimSpace(host[:i])
	}
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host
}

// registerOAuthRoutes 注册 MCP OAuth 2.1 相关端点。
func (s *App) registerOAuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleOAuthProtectedResource)
	mux.HandleFunc("OPTIONS /.well-known/oauth-protected-resource", s.handleOAuthOPTIONS)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleOAuthProtectedResource)
	mux.HandleFunc("OPTIONS /.well-known/oauth-protected-resource/mcp", s.handleOAuthOPTIONS)

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleOAuthServerMetadata)
	mux.HandleFunc("OPTIONS /.well-known/oauth-authorization-server", s.handleOAuthOPTIONS)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/mcp", s.handleOAuthServerMetadata)
	mux.HandleFunc("OPTIONS /.well-known/oauth-authorization-server/mcp", s.handleOAuthOPTIONS)

	mux.Handle("POST /oauth/register", s.limit(s.authLimiter, http.HandlerFunc(s.handleOAuthRegister)))
	mux.HandleFunc("OPTIONS /oauth/register", s.handleOAuthOPTIONS)

	mux.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorizeGET)
	mux.Handle("POST /oauth/authorize", s.limit(s.authLimiter, http.HandlerFunc(s.handleOAuthAuthorizePOST)))

	mux.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	mux.HandleFunc("OPTIONS /oauth/token", s.handleOAuthOPTIONS)
}

func setOAuthCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
}

func (s *App) handleOAuthOPTIONS(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	setOAuthCORS(w)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

/* ---------- 1. 元数据发现（RFC 9728 / RFC 8414） ---------- */

func (s *App) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w)
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         allCaps,
	})
}

func (s *App) handleOAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w)
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      allCaps,
	})
}

/* ---------- 2. 动态客户端注册（RFC 7591） ---------- */

var customSchemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

var forbiddenSchemes = map[string]bool{
	"javascript": true,
	"data":       true,
	"vbscript":   true,
	"file":       true,
	"about":      true,
	"blob":       true,
	"ws":         true,
	"wss":        true,
	"ftp":        true,
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || h == "[::1]"
}

// validRedirectURI 按 OAuth 2.1 与 RFC 8252 校验回调地址：
//   - 必须可解析且不含 fragment（#）；
//   - https:// 必须带合法 host；
//   - http:// 仅允许环回地址（localhost / 127.0.0.1 / ::1）；
//   - 允许合法的桌面/移动端自定义 scheme（如 cursor://、claude://），拒绝 javascript:/data: 等危险协议。
func validRedirectURI(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 1024 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if forbiddenSchemes[scheme] || !customSchemeRe.MatchString(scheme) {
		return false
	}
	switch scheme {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHost(u.Hostname())
	default:
		return u.Host != "" || u.Path != "" || u.Opaque != ""
	}
}

// redirectURIMatches 判断请求的 redirect_uri 是否匹配已注册条目。
// 除精确匹配外，按 RFC 8252 §7.3 允许环回地址在动态端口上匹配（scheme/host/path 均相同）。
func redirectURIMatches(registered []string, requested string) bool {
	for _, reg := range registered {
		if reg == requested {
			return true
		}
		ru, err1 := url.Parse(reg)
		qu, err2 := url.Parse(requested)
		if err1 == nil && err2 == nil &&
			strings.EqualFold(ru.Scheme, "http") && strings.EqualFold(qu.Scheme, "http") &&
			isLoopbackHost(ru.Hostname()) && strings.EqualFold(ru.Hostname(), qu.Hostname()) &&
			ru.Path == qu.Path && ru.RawQuery == qu.RawQuery {
			return true
		}
	}
	return false
}

func (s *App) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthMaxRegisterLen))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "请求体读取失败")
		return
	}
	var req struct {
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "JSON 解析失败")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 16 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris 必填且不超过 16 条")
		return
	}
	cleanURIs := make([]string, 0, len(req.RedirectURIs))
	for _, u := range req.RedirectURIs {
		u = strings.TrimSpace(u)
		if !validRedirectURI(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "非法的回调地址: "+u)
			return
		}
		cleanURIs = append(cleanURIs, u)
	}

	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "MCP Client"
	}
	if runes := []rune(name); len(runes) > 64 {
		name = string(runes[:64])
	}

	clientID := "mc_" + randString(24)
	now := time.Now().Unix()
	urisJSON, _ := json.Marshal(cleanURIs)
	if _, err := s.db.Exec(
		`INSERT INTO oauth_clients(client_id, client_name, redirect_uris, created_at) VALUES(?, ?, ?, ?)`,
		clientID, name, string(urisJSON), now,
	); err != nil {
		log.Printf("handleOAuthRegister insert: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "保存客户端注册信息失败")
		return
	}
	pruneOAuthClients(s.db, oauthMaxClients)

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_id_issued_at":        now,
		"client_name":                name,
		"redirect_uris":              cleanURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

func (s *App) lookupOAuthClient(clientID string) (name string, redirectURIs []string, found bool) {
	var rawURIs string
	err := s.db.QueryRow(
		`SELECT client_name, redirect_uris FROM oauth_clients WHERE client_id = ?`,
		clientID,
	).Scan(&name, &rawURIs)
	if err != nil {
		return "", nil, false
	}
	_ = json.Unmarshal([]byte(rawURIs), &redirectURIs)
	return name, redirectURIs, true
}

/* ---------- 3. 授权端点（/oauth/authorize） ---------- */

type oauthAuthorizeParams struct {
	ClientID            string
	ClientName          string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	State               string
	Scope               string
	Resource            string
}

// validateAuthorizeParams 校验授权请求参数。
// 注意：client_id 或 redirect_uri 校验失败时绝不能 302 重定向到 redirect_uri，必须本地直接报错。
func (s *App) validateAuthorizeParams(r *http.Request, q url.Values) (*oauthAuthorizeParams, string) {
	p := &oauthAuthorizeParams{
		ClientID:            strings.TrimSpace(q.Get("client_id")),
		RedirectURI:         strings.TrimSpace(q.Get("redirect_uri")),
		ResponseType:        strings.TrimSpace(q.Get("response_type")),
		CodeChallenge:       strings.TrimSpace(q.Get("code_challenge")),
		CodeChallengeMethod: strings.TrimSpace(q.Get("code_challenge_method")),
		State:               q.Get("state"),
		Scope:               strings.TrimSpace(q.Get("scope")),
		Resource:            strings.TrimSpace(q.Get("resource")),
	}
	if p.ClientID == "" {
		return nil, "缺少 client_id 参数"
	}
	if p.RedirectURI == "" || !validRedirectURI(p.RedirectURI) {
		return nil, "redirect_uri 缺失或不符合 OAuth 2.1 安全要求（需为 HTTPS 或本地环回地址）"
	}
	if regName, regURIs, ok := s.lookupOAuthClient(p.ClientID); ok {
		if !redirectURIMatches(regURIs, p.RedirectURI) {
			return nil, "redirect_uri 与客户端注册的回调地址不匹配"
		}
		p.ClientName = regName
	} else {
		p.ClientName = p.ClientID
	}
	if p.ResponseType != "code" {
		return nil, "仅支持 response_type=code"
	}
	if p.CodeChallenge == "" || p.CodeChallengeMethod != "S256" {
		return nil, "OAuth 2.1 强制要求提供 PKCE code_challenge 且 code_challenge_method=S256"
	}
	if p.Resource != "" && !s.validResourceIndicator(r, p.Resource) {
		return nil, "resource 参数与当前 MCP 服务端点不匹配"
	}
	return p, ""
}

func (s *App) validResourceIndicator(r *http.Request, res string) bool {
	clean := strings.TrimRight(strings.TrimSpace(res), "/")
	base := strings.TrimRight(s.baseURL(r), "/")
	return strings.EqualFold(clean, base) || strings.EqualFold(clean, base+"/mcp")
}

func parseRequestedCaps(scope string) []string {
	if scope == "" {
		return allCaps
	}
	fields := strings.FieldsFunc(scope, func(r rune) bool {
		return r == ' ' || r == ','
	})
	caps := normalizeCaps(fields)
	if len(caps) == 0 {
		return allCaps
	}
	return caps
}

type oauthServerOption struct {
	ID      string
	Name    string
	Checked bool
}

type oauthPageData struct {
	Params       *oauthAuthorizeParams
	CallbackHost string
	Authed       bool
	Username     string
	KeyName      string
	CapRead      bool
	CapExec      bool
	CapWrite     bool
	AllServers   bool
	Servers      []oauthServerOption
	Days         int
	ErrorMsg     string
}

func (s *App) listServerOptions(selected []string) []oauthServerOption {
	rows, err := s.db.Query(`SELECT id, name FROM servers ORDER BY sort, id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	pick := make(map[string]bool, len(selected))
	for _, id := range selected {
		pick[id] = true
	}
	var out []oauthServerOption
	for rows.Next() {
		var it oauthServerOption
		if err := rows.Scan(&it.ID, &it.Name); err == nil {
			it.Checked = pick[it.ID]
			out = append(out, it)
		}
	}
	return out
}

func (s *App) handleOAuthAuthorizeGET(w http.ResponseWriter, r *http.Request) {
	p, errMsg := s.validateAuthorizeParams(r, r.URL.Query())
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}
	caps := parseRequestedCaps(p.Scope)
	hasCap := func(c string) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	cbHost := p.RedirectURI
	if u, err := url.Parse(p.RedirectURI); err == nil && u.Host != "" {
		cbHost = u.Scheme + "://" + u.Host
	}
	data := oauthPageData{
		Params:       p,
		CallbackHost: cbHost,
		Authed:       s.isAuthed(r),
		Username:     getSetting(s.db, keyUsername, "admin"),
		KeyName:      "OAuth: " + p.ClientName,
		CapRead:      hasCap(capRead),
		CapExec:      hasCap(capExec),
		CapWrite:     hasCap(capWrite),
		AllServers:   true,
		Servers:      s.listServerOptions(nil),
		Days:         90,
	}
	renderOAuthConsentPage(w, http.StatusOK, data)
}

func (s *App) handleOAuthAuthorizePOST(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单解析失败", http.StatusBadRequest)
		return
	}
	p, errMsg := s.validateAuthorizeParams(r, r.Form)
	if errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	if r.FormValue("decision") == "deny" {
		http.Redirect(w, r, buildRedirectURL(p.RedirectURI, map[string]string{
			"error":             "access_denied",
			"error_description": "管理员拒绝了授权请求",
			"state":             p.State,
		}), http.StatusFound)
		return
	}

	cbHost := p.RedirectURI
	if u, err := url.Parse(p.RedirectURI); err == nil && u.Host != "" {
		cbHost = u.Scheme + "://" + u.Host
	}

	keyName := strings.TrimSpace(r.FormValue("key_name"))
	if keyName == "" {
		keyName = "OAuth: " + p.ClientName
	}
	caps := normalizeCaps(r.Form["caps"])
	allServers := r.FormValue("scope_mode") != "selected"
	var pickedServers []string
	if !allServers {
		pickedServers = s.filterExistingServers(r.Form["servers"])
	}
	days, _ := strconv.Atoi(r.FormValue("days"))
	if days < 0 || days > 3650 {
		days = 90
	}

	hasCap := func(c string) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	pageData := oauthPageData{
		Params:       p,
		CallbackHost: cbHost,
		Authed:       s.isAuthed(r),
		Username:     strings.TrimSpace(r.FormValue("username")),
		KeyName:      keyName,
		CapRead:      hasCap(capRead),
		CapExec:      hasCap(capExec),
		CapWrite:     hasCap(capWrite),
		AllServers:   allServers,
		Servers:      s.listServerOptions(pickedServers),
		Days:         days,
	}
	if pageData.Username == "" {
		pageData.Username = getSetting(s.db, keyUsername, "admin")
	}

	// 管理员身份校验：
	// 若已持有有效 moss_session Cookie，则校验同源请求头防 CSRF；
	// 否则走与 /api/login 完全一致的账号密码校验 + 并发爆破锁定。
	if pageData.Authed {
		if origin := r.Header.Get("Origin"); origin != "" {
			pubHost := ""
			if pu, err := url.Parse(s.baseURL(r)); err == nil {
				pubHost = pu.Host
			}
			if u, err := url.Parse(origin); err != nil || (!strings.EqualFold(u.Host, r.Host) && (pubHost == "" || !strings.EqualFold(u.Host, pubHost))) {
				http.Error(w, "跨站提交被拒绝", http.StatusForbidden)
				return
			}
		}
	} else {
		ip := realIP(r, s.trustProxy, s.trustedProxies)
		if !beginLoginAttempt(ip, time.Now()) {
			pageData.ErrorMsg = errLockedOut.Msg
			renderOAuthConsentPage(w, http.StatusTooManyRequests, pageData)
			return
		}
		time.Sleep(300 * time.Millisecond)
		wantUser := getSetting(s.db, keyUsername, "admin")
		userOK := subtle.ConstantTimeCompare([]byte(r.FormValue("username")), []byte(wantUser)) == 1
		passOK := s.checkPassword(r.FormValue("password"))
		if !userOK || !passOK {
			loginMu.Lock()
			locked := loginLocked(ip, time.Now())
			loginMu.Unlock()
			if locked {
				pageData.ErrorMsg = errLockedOut.Msg
				renderOAuthConsentPage(w, http.StatusTooManyRequests, pageData)
				return
			}
			pageData.ErrorMsg = errBadCredentials.Msg
			renderOAuthConsentPage(w, http.StatusUnauthorized, pageData)
			return
		}
		loginMu.Lock()
		clearLoginFail(ip)
		loginMu.Unlock()
	}

	if len(caps) == 0 {
		pageData.ErrorMsg = errCapsRequired.Msg
		renderOAuthConsentPage(w, http.StatusBadRequest, pageData)
		return
	}
	if !allServers && len(pickedServers) == 0 {
		pageData.ErrorMsg = "请至少选择一台服务器，或切换为「全部服务器」"
		renderOAuthConsentPage(w, http.StatusBadRequest, pageData)
		return
	}

	var expiresAt int64
	if days > 0 {
		expiresAt = time.Now().Unix() + int64(days)*86400
	}

	now := time.Now()
	code := "moc_" + randString(40)
	putOAuthCode(&oauthCodeEntry{
		Code:          code,
		ClientID:      p.ClientID,
		RedirectURI:   p.RedirectURI,
		CodeChallenge: p.CodeChallenge,
		Resource:      p.Resource,
		KeyName:       keyName,
		Caps:          caps,
		Servers:       pickedServers,
		ExpiresAt:     expiresAt,
		CodeExpiresAt: now.Add(oauthCodeTTL),
	}, now)

	http.Redirect(w, r, buildRedirectURL(p.RedirectURI, map[string]string{
		"code":  code,
		"state": p.State,
	}), http.StatusFound)
}

func (s *App) filterExistingServers(ids []string) []string {
	var out []string
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM servers WHERE id = ?`, id).Scan(&n); err == nil && n > 0 {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func buildRedirectURL(base string, params map[string]string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

/* ---------- 4. 令牌端点（/oauth/token） ---------- */

// verifyPKCE 校验 RFC 7636 S256 code_verifier。
// 规范要求 code_verifier 长度为 43..128 个允许字符，SHA-256 后做无填充 base64url 编码。
func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 || challenge == "" {
		return false
	}
	for i := 0; i < len(verifier); i++ {
		c := verifier[i]
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~'
		if !ok {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

func (s *App) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	setOAuthCORS(w)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

	var (
		grantType    string
		code         string
		redirectURI  string
		clientID     string
		codeVerifier string
		resource     string
	)

	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "application/json") {
		var body struct {
			GrantType    string `json:"grant_type"`
			Code         string `json:"code"`
			RedirectURI  string `json:"redirect_uri"`
			ClientID     string `json:"client_id"`
			CodeVerifier string `json:"code_verifier"`
			Resource     string `json:"resource"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "JSON 解析失败")
			return
		}
		grantType = body.GrantType
		code = body.Code
		redirectURI = body.RedirectURI
		clientID = body.ClientID
		codeVerifier = body.CodeVerifier
		resource = body.Resource
	} else {
		if err := r.ParseForm(); err != nil {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "表单解析失败")
			return
		}
		grantType = r.FormValue("grant_type")
		code = r.FormValue("code")
		redirectURI = r.FormValue("redirect_uri")
		clientID = r.FormValue("client_id")
		codeVerifier = r.FormValue("code_verifier")
		resource = r.FormValue("resource")
	}

	if basicUser, _, ok := r.BasicAuth(); ok && clientID == "" {
		clientID = basicUser
	}

	if grantType != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "仅支持 grant_type=authorization_code")
		return
	}
	if code == "" || codeVerifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "缺少 code 或 code_verifier")
		return
	}

	entry, ok := popOAuthCode(code)
	if !ok || !time.Now().Before(entry.CodeExpiresAt) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "授权码无效或已过期")
		return
	}
	if clientID != "" && clientID != entry.ClientID {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id 不匹配")
		return
	}
	if redirectURI != entry.RedirectURI {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri 不匹配")
		return
	}
	if resource != "" && !s.validResourceIndicator(r, resource) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource 与当前服务端点不匹配")
		return
	}
	if !verifyPKCE(codeVerifier, entry.CodeChallenge) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE code_verifier 校验失败")
		return
	}

	rawKey := newAPIKey()
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO api_keys(name, key_hash, key_prefix, caps, servers, expires_at, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		entry.KeyName, hashKey(rawKey), keyPrefix(rawKey),
		strings.Join(entry.Caps, ","), strings.Join(entry.Servers, ","),
		entry.ExpiresAt, now,
	); err != nil {
		log.Printf("handleOAuthToken insert key: %v", err)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "签发 API Key 失败")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	resp := map[string]any{
		"access_token": rawKey,
		"token_type":   "Bearer",
		"scope":        strings.Join(entry.Caps, " "),
	}
	if entry.ExpiresAt > now {
		resp["expires_in"] = entry.ExpiresAt - now
	}
	writeJSON(w, http.StatusOK, resp)
}

/* ---------- 5. 授权页 HTML 模板 ---------- */

var oauthConsentTmpl = template.Must(template.New("oauth_consent").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Moss MCP 授权接入</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f4f4f5;
    --card: rgba(255, 255, 255, 0.88);
    --border: rgba(0, 0, 0, 0.08);
    --text: #18181b;
    --muted: #71717a;
    --input-bg: rgba(255, 255, 255, 0.9);
    --accent: #10b981;
    --accent-hover: #059669;
    --danger: #f43f5e;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #09090b;
      --card: rgba(24, 24, 27, 0.88);
      --border: rgba(255, 255, 255, 0.1);
      --text: #f4f4f5;
      --muted: #a1a1aa;
      --input-bg: rgba(39, 39, 42, 0.8);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 20px;
    background: var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    font-size: 14px;
    line-height: 1.5;
  }
  .card {
    width: 100%;
    max-width: 440px;
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 18px;
    padding: 24px;
    box-shadow: 0 12px 32px rgba(0, 0, 0, 0.12);
    backdrop-filter: blur(16px);
  }
  .header { text-align: center; margin-bottom: 18px; }
  .dot {
    width: 36px; height: 36px; border-radius: 50%;
    background: radial-gradient(circle at 35% 35%, #ef4444, #991b1b);
    margin: 0 auto 10px;
    box-shadow: 0 0 16px rgba(239, 68, 68, 0.45);
  }
  h1 { margin: 0; font-size: 18px; font-weight: 700; }
  .sub { margin-top: 4px; color: var(--muted); font-size: 12px; word-break: break-all; }
  .field { margin-top: 14px; }
  .label { display: block; font-size: 12px; font-weight: 600; color: var(--muted); margin-bottom: 5px; }
  input[type="text"], input[type="password"], select {
    width: 100%;
    padding: 8px 11px;
    border-radius: 10px;
    border: 1px solid var(--border);
    background: var(--input-bg);
    color: var(--text);
    font-size: 14px;
    outline: none;
  }
  .box {
    border: 1px solid var(--border);
    border-radius: 12px;
    padding: 8px 10px;
    background: var(--input-bg);
    max-height: 150px;
    overflow-y: auto;
  }
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 4px 2px;
    cursor: pointer;
    user-select: none;
  }
  .check-row small { color: var(--muted); font-size: 12px; }
  .badge {
    display: inline-block;
    padding: 4px 10px;
    border-radius: 999px;
    background: rgba(16, 185, 129, 0.14);
    color: var(--accent);
    font-size: 12px;
    font-weight: 600;
  }
  .err {
    margin-top: 12px;
    padding: 8px 10px;
    border-radius: 10px;
    background: rgba(244, 63, 94, 0.12);
    color: var(--danger);
    font-size: 12px;
  }
  .actions {
    margin-top: 20px;
    display: flex;
    gap: 10px;
  }
  button {
    flex: 1;
    padding: 9px 14px;
    border-radius: 10px;
    border: 1px solid var(--border);
    font-size: 14px;
    font-weight: 600;
    cursor: pointer;
  }
  .btn-primary {
    background: var(--accent);
    color: #fff;
    border-color: transparent;
  }
  .btn-primary:hover { background: var(--accent-hover); }
  .btn-ghost {
    background: transparent;
    color: var(--text);
  }
</style>
</head>
<body>
<form class="card" method="POST" action="/oauth/authorize">
  <input type="hidden" name="client_id" value="{{.Params.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.Params.RedirectURI}}">
  <input type="hidden" name="response_type" value="{{.Params.ResponseType}}">
  <input type="hidden" name="code_challenge" value="{{.Params.CodeChallenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.Params.CodeChallengeMethod}}">
  <input type="hidden" name="state" value="{{.Params.State}}">
  <input type="hidden" name="scope" value="{{.Params.Scope}}">
  <input type="hidden" name="resource" value="{{.Params.Resource}}">

  <div class="header">
    <div class="dot"></div>
    <h1>授权接入 Moss MCP</h1>
    <div class="sub">客户端 <strong>{{.Params.ClientName}}</strong> 请求连接，回调至 {{.CallbackHost}}</div>
  </div>

  {{if .Authed}}
  <div class="field" style="text-align:center;">
    <span class="badge">已通过当前管理员会话验证</span>
  </div>
  {{else}}
  <div class="field">
    <label class="label">管理员用户名</label>
    <input type="text" name="username" value="{{.Username}}" autocomplete="username">
  </div>
  <div class="field">
    <label class="label">管理员密码</label>
    <input type="password" name="password" placeholder="请输入 Moss 管理密码" autocomplete="current-password" autofocus>
  </div>
  {{end}}

  <div class="field">
    <label class="label">密钥备注名称（可在后台「AI 接入」随时调整或停用）</label>
    <input type="text" name="key_name" value="{{.KeyName}}">
  </div>

  <div class="field">
    <label class="label">授予能力</label>
    <div class="box">
      <label class="check-row">
        <input type="checkbox" name="caps" value="read" {{if .CapRead}}checked{{end}}>
        <span>read</span>
        <small>只读监控（列机器、读指标与历史）</small>
      </label>
      <label class="check-row">
        <input type="checkbox" name="caps" value="exec" {{if .CapExec}}checked{{end}}>
        <span>exec</span>
        <small>执行命令（在受控节点运行命令）</small>
      </label>
      <label class="check-row">
        <input type="checkbox" name="caps" value="write" {{if .CapWrite}}checked{{end}}>
        <span>write</span>
        <small>写入文件（向受控节点写入配置）</small>
      </label>
    </div>
  </div>

  <div class="field">
    <label class="label">服务器范围</label>
    <select name="scope_mode" onchange="document.getElementById('srv-box').style.display=this.value==='selected'?'block':'none'">
      <option value="all" {{if .AllServers}}selected{{end}}>全部服务器</option>
      <option value="selected" {{if not .AllServers}}selected{{end}}>仅限勾选的服务器</option>
    </select>
    <div id="srv-box" class="box" style="margin-top:6px;display:{{if .AllServers}}none{{else}}block{{end}};">
      {{if .Servers}}
        {{range .Servers}}
        <label class="check-row">
          <input type="checkbox" name="servers" value="{{.ID}}" {{if .Checked}}checked{{end}}>
          <span>{{.Name}}</span>
          <small>({{.ID}})</small>
        </label>
        {{end}}
      {{else}}
        <div class="sub">当前暂无已注册节点</div>
      {{end}}
    </div>
  </div>

  <div class="field">
    <label class="label">有效期</label>
    <select name="days">
      <option value="30" {{if eq .Days 30}}selected{{end}}>30 天</option>
      <option value="90" {{if eq .Days 90}}selected{{end}}>90 天</option>
      <option value="365" {{if eq .Days 365}}selected{{end}}>365 天</option>
      <option value="0" {{if eq .Days 0}}selected{{end}}>永不过期</option>
    </select>
  </div>

  {{if .ErrorMsg}}
  <div class="err">{{.ErrorMsg}}</div>
  {{end}}

  <div class="actions">
    <button type="submit" name="decision" value="deny" class="btn-ghost">拒绝</button>
    <button type="submit" name="decision" value="approve" class="btn-primary">授权接入</button>
  </div>
</form>
</body>
</html>`))

func renderOAuthConsentPage(w http.ResponseWriter, status int, data oauthPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.WriteHeader(status)
	if err := oauthConsentTmpl.Execute(w, data); err != nil {
		log.Printf("renderOAuthConsentPage: %v", err)
	}
}
