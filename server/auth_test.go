package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// loginAttempts 是包级状态，测试之间必须互不污染，否则先跑的用例会把后跑的锁死。
func resetLoginAttempts(t *testing.T) {
	t.Helper()
	clear := func() {
		loginMu.Lock()
		loginAttempts = make(map[string]*loginAttempt)
		loginMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// loginTestApp 造一个能真正走完登录流程的 App。
// 口令哈希用 MinCost：这里要验的是并发控制，不是 bcrypt 的强度。
func loginTestApp(t *testing.T, password string) *App {
	t.Helper()
	app := mcpTestApp(t)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("生成测试口令哈希失败: %v", err)
	}
	if err := setSetting(app.db, keyPasswordHash, string(hash)); err != nil {
		t.Fatalf("写入测试口令失败: %v", err)
	}
	if err := setSetting(app.db, keyUsername, "admin"); err != nil {
		t.Fatalf("写入测试用户名失败: %v", err)
	}
	return app
}

func postLogin(app *App, ip, username, password string) (code int, dur time.Duration) {
	body := `{"username":"` + username + `","password":"` + password + `"}`
	r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
	r.RemoteAddr = ip + ":40000"
	w := httptest.NewRecorder()
	start := time.Now()
	app.handleLogin(w, r)
	return w.Code, time.Since(start)
}

// 并发爆破必须被挡住：同一 IP 同时打进来的请求，只有 loginMaxFails 个能走到口令比对。
//
// 判据用耗时而不是状态码：通过闸门的请求会吃掉 handleLogin 里那 300ms 固定延迟，
// 被闸门挡下的请求立刻返回。状态码区分不出来——旧写法下多余的请求同样会在
// 比对完口令之后拿到 429，看起来一切正常，代价却是每一个都真的试了一次口令。
func TestLoginConcurrentBruteForceBlocked(t *testing.T) {
	resetLoginAttempts(t)
	app := loginTestApp(t, "correct-horse-battery")

	const attempts = 20
	const ip = "192.0.2.9"
	var mu sync.Mutex
	verified := 0

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // 同时起跑，才构造得出「全部卡在锁定检查与记账之间」的时序
			_, dur := postLogin(app, ip, "admin", "wrong-guess")
			if dur >= 250*time.Millisecond {
				mu.Lock()
				verified++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if verified > loginMaxFails {
		t.Errorf("%d 个并发请求里有 %d 个走到了口令比对，上限应为 %d——"+
			"检查与记账不在同一临界区，退避对并发爆破完全无效", attempts, verified, loginMaxFails)
	}
	if verified == 0 {
		t.Error("一个请求都没走到口令比对，闸门开得过死")
	}
}

// 正确口令必须能登录成功，且成功后要把预登记的这次尝试撤销掉。
// 少了这一步，先手误两次再输对的用户，下次一错就被锁——预登记会变成新的坑。
func TestLoginSuccessClearsPreRegisteredAttempt(t *testing.T) {
	resetLoginAttempts(t)
	app := loginTestApp(t, "correct-horse-battery")
	const ip = "192.0.2.11"

	for i := 0; i < loginMaxFails-1; i++ {
		if code, _ := postLogin(app, ip, "admin", "wrong"); code != 401 {
			t.Fatalf("第 %d 次错误口令应返回 401，实际 %d", i+1, code)
		}
	}
	if code, _ := postLogin(app, ip, "admin", "correct-horse-battery"); code != 200 {
		t.Fatalf("正确口令应登录成功，实际 %d", code)
	}

	loginMu.Lock()
	_, still := loginAttempts[ip]
	loginMu.Unlock()
	if still {
		t.Error("登录成功后该 IP 不该再留有失败记录")
	}
}

// 连续失败达到阈值即锁定，锁定期内不再比对口令（同样用耗时判定）。
func TestLoginLocksAfterMaxFails(t *testing.T) {
	resetLoginAttempts(t)
	app := loginTestApp(t, "correct-horse-battery")
	const ip = "192.0.2.12"

	for i := 0; i < loginMaxFails; i++ {
		postLogin(app, ip, "admin", "wrong")
	}
	code, dur := postLogin(app, ip, "admin", "correct-horse-battery")
	if code != 429 {
		t.Errorf("锁定期内应返回 429，实际 %d", code)
	}
	if dur >= 250*time.Millisecond {
		t.Error("锁定期内不该再走口令比对")
	}
}

// 锁定期满后计数必须清零。
//
// 旧实现里 fails 没有任何路径能降回去，clearLoginFail 又只在登录成功时触发：
// 共用出口 IP 的正常用户只要历史上被别人打满过阈值，此后每次手误都换来 30 分钟锁定，
// 而他永远等不到一次成功去清账——把自己锁死在门外的死循环。
func TestLoginFailsResetAfterLockExpires(t *testing.T) {
	resetLoginAttempts(t)
	const ip = "192.0.2.13"
	base := time.Now()

	loginMu.Lock()
	defer loginMu.Unlock()

	for i := 0; i < loginMaxFails; i++ {
		recordLoginFail(ip, base)
	}
	if !loginLocked(ip, base) {
		t.Fatal("连续失败达到阈值应锁定")
	}

	after := base.Add(loginLockDur + time.Second)
	if loginLocked(ip, after) {
		t.Fatal("锁定期已过不该仍显示锁定")
	}

	// 锁定期满后再错一次：这是共用出口 IP 的用户最典型的处境，
	// 不该立刻又被锁 30 分钟。
	recordLoginFail(ip, after)
	if loginLocked(ip, after) {
		t.Fatal("锁定期满后错一次就重新锁定，等于永远解不开")
	}
	// 但也不能变成不设防：重新累计到阈值仍要锁。
	recordLoginFail(ip, after)
	if loginLocked(ip, after) {
		t.Fatal("第 2 次失败还不该锁定")
	}
	recordLoginFail(ip, after)
	if !loginLocked(ip, after) {
		t.Fatal("重新累计到阈值应再次锁定")
	}
}
