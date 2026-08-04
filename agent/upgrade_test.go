package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upgradeServer 起一个假的 release 服务：/bin 给二进制，/SHA256SUMS 给清单。
func upgradeServer(t *testing.T, body []byte, sumName string) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(body)
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), sumName)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moss-agent-linux-amd64":
			w.Write(body)
		case "/SHA256SUMS":
			w.Write([]byte("deadbeef  other-file\n" + line))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadAndVerify(t *testing.T) {
	body := []byte("fake agent binary")
	srv := upgradeServer(t, body, "moss-agent-linux-amd64")
	dst := filepath.Join(t.TempDir(), "agent.new")

	if err := downloadTo(dst, srv.URL+"/moss-agent-linux-amd64", upgradeBinCap); err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(body) {
		t.Fatalf("内容不一致: %q, err=%v", got, err)
	}
	if err := verifySum(dst, srv.URL+"/SHA256SUMS", "moss-agent-linux-amd64"); err != nil {
		t.Errorf("校验应通过: %v", err)
	}
}

// 校验和是自升级唯一的完整性保障：装错一次就是机器失联，
// 且现场没有人盯着告警。以下每一种情况都必须拒绝安装，绝不能"告警后继续"。
func TestVerifySumRejects(t *testing.T) {
	body := []byte("fake agent binary")
	srv := upgradeServer(t, body, "moss-agent-linux-amd64")
	dir := t.TempDir()

	tampered := filepath.Join(dir, "tampered")
	if err := os.WriteFile(tampered, []byte("tampered binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifySum(tampered, srv.URL+"/SHA256SUMS", "moss-agent-linux-amd64"); err == nil {
		t.Error("内容被篡改时必须拒绝")
	}

	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifySum(good, srv.URL+"/SHA256SUMS", "not-in-list"); err == nil {
		t.Error("清单里没有该文件名时必须拒绝")
	}
	if err := verifySum(good, "", "moss-agent-linux-amd64"); err == nil {
		t.Error("未提供校验和地址时必须拒绝")
	}
	if err := verifySum(good, srv.URL+"/missing", "moss-agent-linux-amd64"); err == nil {
		t.Error("取不到清单时必须拒绝")
	}
}

// SHA256SUMS 的二进制模式会在文件名前加 *，两种写法都要能匹配。
func TestVerifySumAcceptsBinaryMarker(t *testing.T) {
	body := []byte("bin")
	sum := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  *moss-agent-linux-amd64\n", hex.EncodeToString(sum[:]))
	}))
	t.Cleanup(srv.Close)

	f := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(f, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := verifySum(f, srv.URL, "moss-agent-linux-amd64"); err != nil {
		t.Errorf("带 * 前缀的清单行应能匹配: %v", err)
	}
}

// 下载地址若被指向超大文件，必须在写爆磁盘前停手。
func TestDownloadRejectsOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 1024))
	}))
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "big")
	if err := downloadTo(dst, srv.URL, 512); err == nil {
		t.Error("超过上限必须拒绝")
	}
	// 恰好等于上限属于正常，不能误伤。
	if err := downloadTo(dst, srv.URL, 1024); err != nil {
		t.Errorf("恰好等于上限不应报错: %v", err)
	}
}

func TestDownloadRejectsEmptyAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/empty" {
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	dst := filepath.Join(t.TempDir(), "x")
	if err := downloadTo(dst, srv.URL+"/empty", 1024); err == nil {
		t.Error("空内容必须拒绝")
	}
	if err := downloadTo(dst, srv.URL+"/404", 1024); err == nil {
		t.Error("HTTP 错误必须拒绝")
	}
}

// 连接标记是回滚守护判断「新版本是否活过来」的唯一依据，
// 每次连上都必须刷新 mtime，否则守护会误判失败并回滚一个本来正常的升级。
func TestMarkConnectedRefreshesModTime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "connected")
	if !markerModTime(p).IsZero() {
		t.Fatal("文件不存在时应返回零值时间")
	}
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	before := markerModTime(p)

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	if !markerModTime(p).After(before) {
		t.Error("刷新后 mtime 必须变新，否则守护会把成功的升级误判为失败")
	}
}

// guardFixture 造出「升级已替换完成」的现场：target 是新二进制，backup 是旧的。
func guardFixture(t *testing.T) (backup, target, marker string) {
	t.Helper()
	dir := t.TempDir()
	backup = filepath.Join(dir, "moss-agent.bak")
	target = filepath.Join(dir, "moss-agent")
	marker = filepath.Join(dir, "connected")
	if err := os.WriteFile(backup, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	return backup, target, marker
}

func stubRestart(t *testing.T, fn func() error) {
	t.Helper()
	old := restartAgentService
	t.Cleanup(func() { restartAgentService = old })
	restartAgentService = fn
}

// 新版本连回了 server：保留新二进制、删掉备份。
func TestRollbackGuardKeepsNewOnSuccess(t *testing.T) {
	backup, target, marker := guardFixture(t)
	stubRestart(t, func() error {
		// 模拟新 agent 起来后连上 server 刷新了标记
		f, err := os.OpenFile(marker, os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		return f.Close()
	})

	if code := rollbackGuard(backup, target, 5, marker, 10*time.Millisecond); code != 0 {
		t.Fatalf("成功路径应返回 0，实际 %d", code)
	}
	if b, _ := os.ReadFile(target); string(b) != "NEW" {
		t.Errorf("成功后应保留新二进制，实际内容 %q", b)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Error("成功后应删除备份")
	}
}

// 新版本起来了却连不回 server（token 失效 / endpoint 写错）——
// 这与失联无异，必须回滚。只看进程是否存活会漏掉这一整类故障。
func TestRollbackGuardRestoresWhenNeverConnects(t *testing.T) {
	backup, target, marker := guardFixture(t)
	stubRestart(t, func() error { return nil }) // 重启成功，但标记永不刷新

	if code := rollbackGuard(backup, target, 1, marker, 10*time.Millisecond); code == 0 {
		t.Fatal("未连回 server 必须判失败")
	}
	if b, _ := os.ReadFile(target); string(b) != "OLD" {
		t.Errorf("应已回滚为旧二进制，实际内容 %q", b)
	}
}

// 连重启都失败，别再等 grace 了，立刻回滚。
func TestRollbackGuardRestoresWhenRestartFails(t *testing.T) {
	backup, target, marker := guardFixture(t)
	stubRestart(t, func() error { return fmt.Errorf("systemctl 挂了") })

	start := time.Now()
	if code := rollbackGuard(backup, target, 30, marker, 10*time.Millisecond); code == 0 {
		t.Fatal("重启失败必须判失败")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("重启失败应立即回滚，不该干等满 grace")
	}
	if b, _ := os.ReadFile(target); string(b) != "OLD" {
		t.Errorf("应已回滚为旧二进制，实际内容 %q", b)
	}
}

// Windows 本版不支持自升级，必须如实报错而不是假装成功。
func TestUpgradeSupportedReportsPlatform(t *testing.T) {
	err := upgradeSupported()
	if err == nil {
		return // Linux root 环境下支持，属正常
	}
	if !strings.Contains(err.Error(), "Windows") &&
		!strings.Contains(err.Error(), "systemctl") &&
		!strings.Contains(err.Error(), "root") {
		t.Errorf("不支持时的错误信息应说明原因，实际: %v", err)
	}
}
