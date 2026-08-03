// API Key 体系：AI 客户端接入 moss 的凭证与作用域（闸 1）。
//
// 一把 Key 绑定三件事：能做什么（能力集）、能碰哪几台机器（机器白名单）、活到什么时候（有效期）。
// 这是整个方案里真正的安全边界——命令黑名单只是最后一道便宜的保险。
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 能力集。最小授予：一把只需要读监控的 Key 不该拿到 exec。
const (
	capRead  = "read"  // 列机器、读指标、读审计
	capExec  = "exec"  // 执行命令
	capWrite = "write" // 写文件
)

var allCaps = []string{capRead, capExec, capWrite}

var (
	errKeyMissing  = errors.New("缺少 API Key")
	errKeyInvalid  = errors.New("API Key 无效或已吊销")
	errKeyExpired  = errors.New("API Key 已过期")
	errKeyNoCap    = errors.New("API Key 缺少所需能力")
	errKeyNoServer = errors.New("API Key 无权访问该服务器")
)

// apiKey 是一把 Key 的运行时视图。
type apiKey struct {
	ID        int64
	Name      string
	Caps      []string
	Servers   []string // 空切片表示不限制机器
	ExpiresAt int64
}

// hashKey 用 SHA-256 而非 bcrypt，这是有意选择：
//   - Key 是 160+ bit 的高熵随机串，不存在字典攻击面，bcrypt 的加盐慢哈希是为低熵人类密码设计的；
//   - bcrypt 无法按哈希建索引，校验时必须遍历全部 Key 逐个比对，而 Key 校验发生在每一次 API 调用上；
//   - 用户密码仍然走 bcrypt（见 auth.go），两者场景不同，不要混用。
func hashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newAPIKey 生成明文 Key。明文只在创建时返回一次，之后库里只有哈希。
func newAPIKey() string { return "mak_" + randString(32) }

// keyPrefix 取前若干字符用于在管理界面辨认，不足以反推原文。
func keyPrefix(raw string) string {
	if len(raw) <= 12 {
		return raw
	}
	return raw[:12]
}

func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// has 判断 Key 是否具备某项能力。
func (k *apiKey) has(cap string) bool {
	for _, c := range k.Caps {
		if c == cap {
			return true
		}
	}
	return false
}

// canAccess 判断 Key 是否可操作指定服务器。机器白名单为空表示不限制。
func (k *apiKey) canAccess(serverID string) bool {
	if len(k.Servers) == 0 {
		return true
	}
	for _, id := range k.Servers {
		if id == serverID {
			return true
		}
	}
	return false
}

// lookupKey 按明文 Key 查库并校验有效性。
func lookupKey(db *sql.DB, raw string) (*apiKey, error) {
	if raw == "" {
		return nil, errKeyMissing
	}
	var (
		k       apiKey
		caps    string
		servers string
		revoked int
	)
	err := db.QueryRow(
		`SELECT id, name, caps, servers, expires_at, revoked FROM api_keys WHERE key_hash = ?`,
		hashKey(raw),
	).Scan(&k.ID, &k.Name, &caps, &servers, &k.ExpiresAt, &revoked)
	if err != nil {
		return nil, errKeyInvalid
	}
	if revoked == 1 {
		return nil, errKeyInvalid
	}
	if k.ExpiresAt > 0 && time.Now().Unix() > k.ExpiresAt {
		return nil, errKeyExpired
	}
	k.Caps = splitList(caps)
	k.Servers = splitList(servers)
	return &k, nil
}

// bearerToken 从 Authorization 头取 Bearer 值。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// authKey 校验请求携带的 Key 并要求具备指定能力。
// 返回的 apiKey 供调用方继续做机器作用域校验。
func (s *App) authKey(r *http.Request, need string) (*apiKey, error) {
	k, err := lookupKey(s.db, bearerToken(r))
	if err != nil {
		return nil, err
	}
	if need != "" && !k.has(need) {
		return nil, errKeyNoCap
	}
	// 最后使用时间用于识别僵尸 Key。异步写入，避免让鉴权路径等一次磁盘 IO。
	go func(id int64) {
		if _, err := s.db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
			log.Printf("更新 Key 使用时间失败: %v", err)
		}
	}(k.ID)
	return k, nil
}

// keyErrStatus 把鉴权错误映射为 HTTP 状态码。
func keyErrStatus(err error) int {
	switch {
	case errors.Is(err, errKeyMissing):
		return http.StatusUnauthorized
	case errors.Is(err, errKeyInvalid), errors.Is(err, errKeyExpired):
		return http.StatusUnauthorized
	case errors.Is(err, errKeyNoCap), errors.Is(err, errKeyNoServer):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

/* ---------- 管理接口（管理员会话鉴权） ---------- */

type apiKeyRow struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	Prefix     string   `json:"prefix"`
	Caps       []string `json:"caps"`
	Servers    []string `json:"servers"`
	ExpiresAt  int64    `json:"expiresAt"`
	CreatedAt  int64    `json:"createdAt"`
	LastUsedAt int64    `json:"lastUsedAt"`
	Revoked    bool     `json:"revoked"`
}

func (s *App) handleListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, name, key_prefix, caps, servers, expires_at,
	                                created_at, last_used_at, revoked
	                         FROM api_keys ORDER BY id DESC`)
	if err != nil {
		log.Printf("handleListKeys query: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	defer rows.Close()

	list := make([]apiKeyRow, 0, 16)
	for rows.Next() {
		var (
			it            apiKeyRow
			caps, servers string
			revoked       int
		)
		if err := rows.Scan(&it.ID, &it.Name, &it.Prefix, &caps, &servers,
			&it.ExpiresAt, &it.CreatedAt, &it.LastUsedAt, &revoked); err != nil {
			log.Printf("handleListKeys scan: %v", err)
			writeErr(w, 500, "内部错误")
			return
		}
		it.Caps, it.Servers = splitList(caps), splitList(servers)
		if it.Caps == nil {
			it.Caps = []string{}
		}
		if it.Servers == nil {
			it.Servers = []string{}
		}
		it.Revoked = revoked == 1
		list = append(list, it)
	}
	if err := rows.Err(); err != nil {
		log.Printf("handleListKeys rows: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, list)
}

type keyForm struct {
	Name      string   `json:"name"`
	Caps      []string `json:"caps"`
	Servers   []string `json:"servers"`
	ExpiresAt int64    `json:"expiresAt"` // 秒级时间戳，0 表示永不过期
}

// normalizeCaps 过滤未知能力，防止写入库里一个永远不会被匹配的字符串。
func normalizeCaps(in []string) []string {
	var out []string
	for _, c := range in {
		c = strings.TrimSpace(c)
		for _, known := range allCaps {
			if c == known {
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func (s *App) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var f keyForm
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil || strings.TrimSpace(f.Name) == "" {
		writeErr(w, 400, "名称不能为空")
		return
	}
	caps := normalizeCaps(f.Caps)
	if len(caps) == 0 {
		writeErr(w, 400, "至少需要选择一项能力")
		return
	}
	// 机器白名单里的 ID 必须真实存在，否则用户会以为已授权、实际永远调不通。
	for _, id := range f.Servers {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(1) FROM servers WHERE id = ?`, id).Scan(&n); err != nil || n == 0 {
			writeErr(w, 400, "机器白名单包含不存在的服务器: "+id)
			return
		}
	}

	raw := newAPIKey()
	res, err := s.db.Exec(
		`INSERT INTO api_keys(name, key_hash, key_prefix, caps, servers, expires_at, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(f.Name), hashKey(raw), keyPrefix(raw),
		strings.Join(caps, ","), strings.Join(f.Servers, ","),
		f.ExpiresAt, time.Now().Unix(),
	)
	if err != nil {
		log.Printf("handleCreateKey insert: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	id, _ := res.LastInsertId()
	// 明文只在这里出现这一次。库里只有哈希，之后任何人都无法再取回。
	writeJSON(w, 200, map[string]any{"id": id, "key": raw})
}

func (s *App) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	// 吊销而非删除：Key 已经写进审计记录，删掉会让历史记录失去归属。
	if _, err := s.db.Exec(`UPDATE api_keys SET revoked = 1 WHERE id = ?`, id); err != nil {
		log.Printf("handleRevokeKey: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if _, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id); err != nil {
		log.Printf("handleDeleteKey: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
