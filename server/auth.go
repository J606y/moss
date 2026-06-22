package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "moss_session"
const sessionTTL = 30 * 24 * time.Hour

// 登录失败限流：按来源 IP 统计连续失败次数并退避锁定，防并发爆破。
const (
	loginMaxFails = 3                // 连续失败达到该次数即锁定该 IP
	loginLockDur  = 30 * time.Minute // 锁定时长（固定）
)

type loginAttempt struct {
	fails     int       // 连续失败次数
	lockUntil time.Time // 锁定到期时间，零值表示未锁定
}

var (
	loginMu       sync.Mutex
	loginAttempts = make(map[string]*loginAttempt)
)

// loginLocked 判断该 IP 当前是否处于锁定期。需在持有 loginMu 时调用。
func loginLocked(ip string, now time.Time) bool {
	a := loginAttempts[ip]
	return a != nil && now.Before(a.lockUntil)
}

// recordLoginFail 累加失败次数，达到阈值即锁定该 IP 固定时长。
func recordLoginFail(ip string, now time.Time) {
	a := loginAttempts[ip]
	if a == nil {
		a = &loginAttempt{}
		loginAttempts[ip] = a
	}
	a.fails++
	if a.fails >= loginMaxFails {
		a.lockUntil = now.Add(loginLockDur)
	}
}

// clearLoginFail 登录成功后清除该 IP 的失败记录。
func clearLoginFail(ip string) {
	delete(loginAttempts, ip)
}

// ensurePassword 首次启动时初始化管理密码。
func (s *App) ensurePassword() {
	if getSetting(s.db, "password_hash", "") != "" {
		return
	}
	pwd := os.Getenv("MOSS_ADMIN_PASSWORD")
	generated := false
	if pwd == "" {
		pwd = randString(10)
		generated = true
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pwd), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("生成密码哈希失败: %v", err)
	}
	if err := setSetting(s.db, "password_hash", string(hash)); err != nil {
		log.Fatalf("保存密码失败: %v", err)
	}
	if generated {
		log.Printf("==============================================")
		log.Printf("已生成初始管理密码: %s", pwd)
		log.Printf("登录后可在 管理后台 → 站点设置 中修改")
		log.Printf("==============================================")
	} else {
		log.Printf("已使用 MOSS_ADMIN_PASSWORD 环境变量设置管理密码")
	}
}

func (s *App) checkPassword(pwd string) bool {
	hash := getSetting(s.db, "password_hash", "")
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pwd)) == nil
}

func (s *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	ip := realIP(r, s.trustProxy) // 真实访客 IP（--trust-proxy 下取最左 XFF）

	// 锁定期内直接拒绝，不去比对密码。
	loginMu.Lock()
	if loginLocked(ip, time.Now()) {
		loginMu.Unlock()
		writeErr(w, 429, "登录失败次数过多，该 IP 已被锁定 30 分钟")
		return
	}
	loginMu.Unlock()

	// 轻量防爆破：固定延迟提高单次试探成本。
	time.Sleep(300 * time.Millisecond)
	wantUser := getSetting(s.db, "username", "admin")
	if body.Username != wantUser || !s.checkPassword(body.Password) {
		loginMu.Lock()
		recordLoginFail(ip, time.Now())
		locked := loginLocked(ip, time.Now())
		loginMu.Unlock()
		if locked {
			writeErr(w, 429, "登录失败次数过多，该 IP 已被锁定 30 分钟")
			return
		}
		writeErr(w, 401, "用户名或密码错误")
		return
	}

	// 登录成功：清除该 IP 的失败记录。
	loginMu.Lock()
	clearLoginFail(ip)
	loginMu.Unlock()

	token := randString(40)
	expires := time.Now().Add(sessionTTL)
	if _, err := s.db.Exec(`INSERT INTO sessions(token, expires) VALUES(?, ?)`, token, expires.Unix()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 仅在 HTTPS 下置 Secure，避免本地 HTTP(:8787) 登录被破坏；
	// 上 TLS 后（直连或反代设置 X-Forwarded-Proto）自动生效。
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.db.Exec(`DELETE FROM sessions WHERE token = ?`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *App) isAuthed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	var expires int64
	if err := s.db.QueryRow(`SELECT expires FROM sessions WHERE token = ?`, c.Value).Scan(&expires); err != nil {
		return false
	}
	return time.Now().Unix() < expires
}

// requireAuth 管理接口中间件。
func (s *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAuthed(r) {
			writeErr(w, 401, "未登录")
			return
		}
		next(w, r)
	}
}
