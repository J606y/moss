package main

import (
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// useTestKey 把进程主密钥固定成可预期的值。secretKeyBytes 是全局量，
// 每个用例都自己设一次，避免依赖别的用例留下的残留状态。
func useTestKey(t *testing.T, seed string) {
	t.Helper()
	secretKeyBytes = sha256.Sum256([]byte(seed))
}

func mustEncrypt(t *testing.T, plain string) string {
	t.Helper()
	enc, err := encryptSecret(plain)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	return enc
}

func TestSecretRoundTrip(t *testing.T) {
	useTestKey(t, "test-master-key")

	cases := []string{"", "mk_short", `{"type":"service_account","private_key":"-----BEGIN..."}`}
	for _, plain := range cases {
		enc := mustEncrypt(t, plain)
		if plain != "" && !strings.HasPrefix(enc, encPrefix) {
			t.Fatalf("密文应带前缀 %q，得到 %q", encPrefix, enc)
		}
		if plain != "" && enc == plain {
			t.Fatalf("密文不应等于明文: %q", plain)
		}
		got, err := decryptSecretValue(enc)
		if err != nil {
			t.Fatalf("解密 %q 失败: %v", plain, err)
		}
		if got != plain {
			t.Fatalf("往返不一致: 明文 %q → 解出 %q", plain, got)
		}
	}
}

func TestDecryptPlaintextPassthrough(t *testing.T) {
	useTestKey(t, "test-master-key")
	// 历史明文（无前缀）应原样透传，保证升级零迁移。
	legacy := `{"type":"service_account"}`
	got, err := decryptSecretValue(legacy)
	if err != nil {
		t.Fatalf("历史明文不应报错: %v", err)
	}
	if got != legacy {
		t.Fatalf("历史明文应透传，得到 %q", got)
	}
}

// TestDecryptWrongKeyReportsError 换密钥后必须报错，而不是安静地返回空串。
//
// 空串与「没配过」不可区分，面板会显示成未配置，运维照着重填一遍就会用新密钥
// 覆盖掉本来还能救回的密文。错误出口是这条路上唯一的刹车。
func TestDecryptWrongKeyReportsError(t *testing.T) {
	useTestKey(t, "key-A")
	enc := mustEncrypt(t, "sensitive")
	useTestKey(t, "key-B")

	got, err := decryptSecretValue(enc)
	if err == nil {
		t.Fatalf("换密钥后应报错，却拿到 %q", got)
	}
	if !errors.Is(err, errSecretUndecryptable) {
		t.Fatalf("应可用 errors.Is 识别为「解不开」，得到 %v", err)
	}
	if got != "" {
		t.Fatalf("解不开时不应返回内容，得到 %q", got)
	}
	// 兼容包装仍返回空串（旧调用点靠日志止损），但不能把错误吞成 nil。
	if v := decryptSecret(enc); v != "" {
		t.Fatalf("包装函数应返回空串，得到 %q", v)
	}
}

// TestDecryptCorruptedCiphertextReportsError 密文被截断/篡改同样要走错误出口。
func TestDecryptCorruptedCiphertextReportsError(t *testing.T) {
	useTestKey(t, "test-master-key")
	for _, bad := range []string{
		encPrefix + "not-base64!!!",
		encPrefix + "AAAA", // 合法 base64，但短于 nonce
		mustEncrypt(t, "sensitive")[:len(encPrefix)+8],
	} {
		if _, err := decryptSecretValue(bad); err == nil {
			t.Errorf("损坏密文 %q 应报错", bad)
		}
	}
}

// TestEncryptFailsClosed 加密失败必须拒绝返回可入库的值。
//
// 旧实现在这里退回明文：明文不带 encPrefix，与历史明文无法区分，
// decryptSecret 照常透传，读写都「正常」，于是私钥永久躺在明文列里且无人察觉。
func TestEncryptFailsClosed(t *testing.T) {
	useTestKey(t, "test-master-key")
	orig := secretRandRead
	t.Cleanup(func() { secretRandRead = orig })
	secretRandRead = func(b []byte) (int, error) { return 0, errors.New("熵源不可用") }

	const plain = "super-secret-private-key"
	out, err := encryptSecret(plain)
	if err == nil {
		t.Fatalf("随机数不可用时必须报错，却返回 %q", out)
	}
	if out != "" {
		t.Fatalf("失败时不得返回任何可入库的值，得到 %q", out)
	}
	if out == plain {
		t.Fatal("绝不允许退回明文存储")
	}
}

/* ---------- 主密钥文件 ---------- */

// withoutEnvKey 清掉 MOSS_SECRET_KEY，否则用例测不到 secret.key 这条路径。
func withoutEnvKey(t *testing.T) {
	t.Helper()
	t.Setenv("MOSS_SECRET_KEY", "")
}

// TestLoadMasterKeyRejectsShortFile 存在但不合格的 secret.key 必须报错，且原文件不能被动过。
//
// 旧实现会跳过它、用新随机密钥覆写同一路径，日志上只留一句「已生成（自动密钥）」——
// 从备份恢复了一个被截断的 secret.key 就会触发，它加密过的全部密文就此永久不可解。
func TestLoadMasterKeyRejectsShortFile(t *testing.T) {
	withoutEnvKey(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "secret.key")
	truncated := []byte("short")
	if err := os.WriteFile(p, truncated, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadMasterKey(dir); err == nil {
		t.Fatal("secret.key 过短时必须报错拒绝启动")
	}
	after, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("原文件不应被删除: %v", err)
	}
	if string(after) != string(truncated) {
		t.Fatalf("原文件被覆写了：%q → %q", truncated, after)
	}
}

// TestLoadMasterKeyRejectsUnreadableFile 读不出来同样不能当成「不存在」去覆写。
func TestLoadMasterKeyRejectsUnreadableFile(t *testing.T) {
	withoutEnvKey(t)
	dir := t.TempDir()
	// 把 secret.key 造成一个目录：ReadFile 必然失败，且失败原因不是 ErrNotExist。
	if err := os.Mkdir(filepath.Join(dir, "secret.key"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMasterKey(dir); err == nil {
		t.Fatal("secret.key 不可读时必须报错，不能改用新密钥继续跑")
	}
}

// TestLoadMasterKeyGeneratesAndReuses 只有确实不存在时才生成，且第二次启动必须复用同一把。
func TestLoadMasterKeyGeneratesAndReuses(t *testing.T) {
	withoutEnvKey(t)
	dir := t.TempDir()

	k1, err := loadMasterKey(dir)
	if err != nil {
		t.Fatalf("首次应自动生成: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatalf("应写出 secret.key: %v", err)
	}
	if len(b) < minSecretKeyLen {
		t.Fatalf("自动生成的密钥只有 %d 字节", len(b))
	}
	k2, err := loadMasterKey(dir)
	if err != nil {
		t.Fatalf("二次启动失败: %v", err)
	}
	if k1 != k2 {
		t.Fatal("二次启动派生出了不同的主密钥，既有密文将解不开")
	}
}

func TestLoadMasterKeyPrefersEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MOSS_SECRET_KEY", "env-master-key")
	k, err := loadMasterKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if k != sha256.Sum256([]byte("env-master-key")) {
		t.Fatal("应优先使用 MOSS_SECRET_KEY")
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); !os.IsNotExist(err) {
		t.Fatal("用环境变量时不应生成 secret.key")
	}
}

/* ---------- 落库的敏感值必须是密文 ---------- */

// rawSetting 绕过解密，直接看库里躺着的是什么。
func rawSetting(t *testing.T, app *App, key string) string {
	t.Helper()
	return getSetting(app.db, key, "")
}

// TestTelegramTokenEncryptedAtRest Bot Token 明文入库等于「拿到 moss.db 就拿到 bot 控制权」，
// 而 secret.go 对主密钥的承诺恰恰是「仅拿到 moss.db 的人解不开敏感列」。
func TestTelegramTokenEncryptedAtRest(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	const token = "123456:AAH-REAL-BOT-TOKEN"

	w := httptest.NewRecorder()
	app.handlePutNotify(w, httptest.NewRequest(http.MethodPut, "/api/admin/notify",
		strings.NewReader(`{"tgToken":"`+token+`","tgChat":"42"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}

	stored := rawSetting(t, app, keyNotifyTgToken)
	if strings.Contains(stored, "AAH-REAL-BOT-TOKEN") {
		t.Fatalf("Bot Token 明文落库: %q", stored)
	}
	if !strings.HasPrefix(stored, encPrefix) {
		t.Fatalf("落库值应带加密前缀，得到 %q", stored)
	}
	if got := loadNotifyConfig(app.db).TgToken; got != token {
		t.Fatalf("读回应为原文，得到 %q", got)
	}
}

// TestWebhookSecretEncryptedAtRest 同理：webhook secret 是对端的 Bearer 凭证。
func TestWebhookSecretEncryptedAtRest(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	const secret = "wh-REAL-BEARER-SECRET"

	w := httptest.NewRecorder()
	app.handlePutWebhook(w, httptest.NewRequest(http.MethodPut, "/api/admin/webhook",
		strings.NewReader(`{"url":"https://example.com/hook","secret":"`+secret+`","on":true}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", w.Code, w.Body.String())
	}

	stored := rawSetting(t, app, keyWebhookSecret)
	if strings.Contains(stored, "REAL-BEARER-SECRET") {
		t.Fatalf("webhook 密钥明文落库: %q", stored)
	}
	if !strings.HasPrefix(stored, encPrefix) {
		t.Fatalf("落库值应带加密前缀，得到 %q", stored)
	}
	if got := loadWebhookConfig(app.db).Secret; got != secret {
		t.Fatalf("读回应为原文，得到 %q", got)
	}
}

// TestLegacyPlaintextSecretsStillReadable 升级零迁移：历史明文值必须照常能用。
func TestLegacyPlaintextSecretsStillReadable(t *testing.T) {
	useTestKey(t, "test-master-key")
	db := testDB(t)
	setSetting(db, keyNotifyTgToken, "123456:LEGACY-PLAINTEXT")
	setSetting(db, keyWebhookSecret, "legacy-plain-secret")

	if got := loadNotifyConfig(db).TgToken; got != "123456:LEGACY-PLAINTEXT" {
		t.Fatalf("历史明文 Bot Token 读不出: %q", got)
	}
	if got := loadWebhookConfig(db).Secret; got != "legacy-plain-secret" {
		t.Fatalf("历史明文 webhook 密钥读不出: %q", got)
	}
}
