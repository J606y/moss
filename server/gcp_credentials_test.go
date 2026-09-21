package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// waitFor 轮询等待条件成立。开机尝试是在 goroutine 里跑的，
// 固定 sleep 要么不够长（偶发失败），要么白等——轮询两头都不占。
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

/* ---------- 迁移 ---------- */

// seedLegacyCred 按老结构写入全局单份凭证，并造一台启用了守护的节点。
func seedLegacyCred(t *testing.T, app *App, stored string) {
	t.Helper()
	setSetting(app.db, keyGCPSAJSON, stored)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_zone, gcp_instance)
		 VALUES('s1', 'tok-s1', 'hk-node', 0, 1, 'asia-east2-a', 'vm-1')`); err != nil {
		t.Fatal(err)
	}
}

func credIDOf(t *testing.T, app *App, serverID string) string {
	t.Helper()
	var id string
	if err := app.db.QueryRow(`SELECT gcp_cred_id FROM servers WHERE id = ?`, serverID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestMigrateGCPCredKeepsCiphertextByteIdentical 密文必须原样搬。
//
// encryptSecret 不是幂等的：搬运时再加密一次会套成两层，之后永远解不出原文。
// 逐字节比对是唯一能钉死这件事的断言。
func TestMigrateGCPCredKeepsCiphertextByteIdentical(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	enc := mustEncrypt(t, saJSON)
	seedLegacyCred(t, app, enc)

	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	creds, err := listGCPCredentials(app.db)
	if err != nil || len(creds) != 1 {
		t.Fatalf("应迁出 1 份凭证: %+v %v", creds, err)
	}
	c := creds[0]
	if c.SaJSON != enc {
		t.Fatal("密文被重新加密了；encryptSecret 非幂等，这会让原文再也解不出来")
	}
	if c.ProjectID != "test-proj" || c.ClientEmail != "moss@test-proj.iam.gserviceaccount.com" {
		t.Fatalf("凭证元信息不对: %+v", c)
	}
	if got := credIDOf(t, app, "s1"); got != c.ID {
		t.Fatalf("节点未绑定到迁移出的凭证: %q", got)
	}
	// 旧键保留：降级回旧版本还能用，也是排障时的原始底本
	if getSetting(app.db, keyGCPSAJSON, "") != enc {
		t.Fatal("旧凭证键不应被清空")
	}
	// 迁出来的凭证必须真的能用
	if _, err := decryptSecretValue(c.SaJSON); err != nil {
		t.Fatalf("迁移后的凭证解不开: %v", err)
	}
}

// TestMigrateGCPCredEncryptsLegacyPlaintext 老库里的明文私钥必须在迁移时补加密。
//
// decryptSecretValue 对无前缀值原样透传，直接搬会把明文私钥永久固化在新表里——
// secret.go 那条「下次写入时自动升级为密文」的路径依赖有人再写一次 gcp_sa_json，
// 而搬进新表后这个键就再没人写了。
func TestMigrateGCPCredEncryptsLegacyPlaintext(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	seedLegacyCred(t, app, saJSON) // 无 enc:v1: 前缀 = 加密落地之前的老明文

	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	creds, _ := listGCPCredentials(app.db)
	if len(creds) != 1 {
		t.Fatalf("应迁出 1 份凭证: %+v", creds)
	}
	if !strings.HasPrefix(creds[0].SaJSON, encPrefix) {
		t.Fatalf("明文未被加密: %q", creds[0].SaJSON)
	}
	if strings.Contains(creds[0].SaJSON, "PRIVATE KEY") {
		t.Fatal("私钥明文被搬进了新表")
	}
	plain, err := decryptSecretValue(creds[0].SaJSON)
	if err != nil || plain != saJSON {
		t.Fatalf("加密后读回不一致: err=%v", err)
	}
}

// TestMigrateGCPCredUndecryptableIsRecoverable 解不开时必须留着重试的余地。
//
// 写下哨兵就等于替用户宣布这份凭证永久作废；正确做法是这次不做，
// 等用户找回主密钥重启后自动补上。
func TestMigrateGCPCredUndecryptableIsRecoverable(t *testing.T) {
	useTestKey(t, "key-A")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	seedLegacyCred(t, app, mustEncrypt(t, saJSON))

	useTestKey(t, "key-B") // 运维换了 MOSS_SECRET_KEY
	if err := migrateGCPCredentials(app.db); err == nil {
		t.Fatal("解不开时应返回错误")
	}
	if n, _ := countGCPCredentials(app.db); n != 0 {
		t.Fatalf("解不开却写了库: %d 行", n)
	}
	if getSetting(app.db, keyGCPCredMigrated, "") != "" {
		t.Fatal("解不开不该写哨兵，否则找回密钥后也不会再迁")
	}

	useTestKey(t, "key-A") // 找回原密钥
	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatalf("恢复密钥后应能补做迁移: %v", err)
	}
	if n, _ := countGCPCredentials(app.db); n != 1 {
		t.Fatalf("补做迁移后应有 1 份凭证，实为 %d", n)
	}
}

// TestMigrateGCPCredIdempotent 迁移只能发生一次，且用户删光凭证后不得复活。
func TestMigrateGCPCredIdempotent(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	seedLegacyCred(t, app, mustEncrypt(t, saJSON))

	for i := 0; i < 2; i++ {
		if err := migrateGCPCredentials(app.db); err != nil {
			t.Fatalf("第 %d 次迁移失败: %v", i+1, err)
		}
	}
	if n, _ := countGCPCredentials(app.db); n != 1 {
		t.Fatalf("重复迁移应仍为 1 份，实为 %d", n)
	}

	// 用户主动删光凭证后重启：旧凭证不能凭空复活。
	// 这正是用哨兵而不是「表为空」当判据的理由。
	if _, err := app.db.Exec(`DELETE FROM gcp_credentials`); err != nil {
		t.Fatal(err)
	}
	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatal(err)
	}
	if n, _ := countGCPCredentials(app.db); n != 0 {
		t.Fatalf("已删除的凭证复活了: %d 行", n)
	}
}

// TestMigrateGCPCredNoLegacyValue 从没配过凭证的库，迁移应安静完成并落下哨兵。
func TestMigrateGCPCredNoLegacyValue(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatal(err)
	}
	if n, _ := countGCPCredentials(app.db); n != 0 {
		t.Fatalf("不该凭空造出凭证: %d 行", n)
	}
	if getSetting(app.db, keyGCPCredMigrated, "") == "" {
		t.Fatal("应写下哨兵，避免每次启动重复检查")
	}
}

// TestMigrateGCPCredKeepsExistingBinding 已经绑好的节点不能被迁移覆盖。
func TestMigrateGCPCredKeepsExistingBinding(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	seedLegacyCred(t, app, mustEncrypt(t, saJSON))
	if _, err := app.db.Exec(`UPDATE servers SET gcp_cred_id = 'preset' WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateGCPCredentials(app.db); err != nil {
		t.Fatal(err)
	}
	if got := credIDOf(t, app, "s1"); got != "preset" {
		t.Fatalf("已有绑定被覆盖: %q", got)
	}
}

/* ---------- 客户端缓存与凭证解析 ---------- */

// twoCredApp 造一个装了两份不同账号凭证的面板，并让 Compute API 指向 mock。
func twoCredApp(t *testing.T) (*App, *mockGCP, string, string) {
	t.Helper()
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	m := newMockGCP(t, "TERMINATED")
	t.Setenv("MOSS_GCP_API_BASE", m.srv.URL+"/compute/v1")

	idA := addTestCred(t, app, m.registerSA(t, "a@proj-a.iam.gserviceaccount.com", "proj-a"))
	idB := addTestCred(t, app, m.registerSA(t, "b@proj-b.iam.gserviceaccount.com", "proj-b"))
	return app, m, idA, idB
}

// TestGCPClientCachePerCredential 两份凭证交替使用，各自的 token 都必须命中缓存。
//
// 这是本功能的核心回归测试：单槽缓存时两个账号会互相冲刷，
// 每轮开机都要重签 JWT、重换 token。
func TestGCPClientCachePerCredential(t *testing.T) {
	app, m, idA, idB := twoCredApp(t)
	n := app.notifier
	ctx := t.Context()

	for i := 0; i < 3; i++ {
		for _, id := range []string{idA, idB} {
			cli, err := n.getGCPClient(id)
			if err != nil {
				t.Fatalf("取客户端失败: %v", err)
			}
			if _, err := cli.accessToken(ctx); err != nil {
				t.Fatalf("换 token 失败: %v", err)
			}
		}
	}
	tokenBy, _ := m.counts()
	for email, want := range map[string]int{
		"a@proj-a.iam.gserviceaccount.com": 1,
		"b@proj-b.iam.gserviceaccount.com": 1,
	} {
		if tokenBy[email] != want {
			t.Fatalf("%s 换取 token %d 次，期望 %d（两份凭证在互相冲刷缓存）", email, tokenBy[email], want)
		}
	}
}

// TestGCPClientRebuildsAfterCredChange 同一个 id 的凭证内容变了，必须重建客户端。
func TestGCPClientRebuildsAfterCredChange(t *testing.T) {
	app, m, idA, _ := twoCredApp(t)
	if _, err := app.notifier.getGCPClient(idA); err != nil {
		t.Fatal(err)
	}
	// 就地换掉密钥（同一账号、新私钥），模拟运维绕过接口直接改库
	newRaw := m.registerSA(t, "a@proj-a.iam.gserviceaccount.com", "proj-a")
	if _, err := app.db.Exec(
		`UPDATE gcp_credentials SET sa_json = ? WHERE id = ?`, mustEncrypt(t, newRaw), idA); err != nil {
		t.Fatal(err)
	}
	cli, err := app.notifier.getGCPClient(idA)
	if err != nil {
		t.Fatal(err)
	}
	// 用新私钥签出的 JWT 必须能通过 mock 验签（mock 已用新公钥覆盖登记）
	if _, err := cli.accessToken(t.Context()); err != nil {
		t.Fatalf("换密钥后应能正常换取 token: %v", err)
	}
}

// TestResolveGCPCredIDDanglingIsHardError 悬空绑定必须报错，绝不回退到唯一那份凭证。
//
// 「反正只有一份就用那份」会把一次静默的数据损坏伪装成正常工作，
// 而多账号下猜错的代价是拿 A 账号的凭证去开 B 账号的机器。
func TestResolveGCPCredIDDanglingIsHardError(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	only := addTestCred(t, app, saJSON)

	got, err := resolveGCPCredID(app.db, "no-such-cred")
	if err == nil {
		t.Fatalf("悬空 id 应报错，却解析成了 %q", got)
	}
	if got == only {
		t.Fatal("绝不能回退到唯一那份凭证")
	}
	if !strings.Contains(err.Error(), "no-such-cred") {
		t.Fatalf("错误文案应点名悬空的 id: %v", err)
	}
}

// TestResolveGCPCredIDUnbound 未绑定时：只有一份就用它，多份则必须报「没指定」。
func TestResolveGCPCredIDUnbound(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)

	if _, err := resolveGCPCredID(app.db, ""); err != errGCPNoCredential {
		t.Fatalf("一份都没有时应报未配置，得到 %v", err)
	}

	saA, _ := makeSAAs(t, "a@proj-a.iam.gserviceaccount.com", "proj-a", "")
	idA := addTestCred(t, app, saA)
	if got, err := resolveGCPCredID(app.db, ""); err != nil || got != idA {
		t.Fatalf("只有一份时应直接采用: got=%q err=%v", got, err)
	}

	saB, _ := makeSAAs(t, "b@proj-b.iam.gserviceaccount.com", "proj-b", "")
	addTestCred(t, app, saB)
	if _, err := resolveGCPCredID(app.db, ""); err != errGCPCredAmbiguous {
		t.Fatalf("多份且未绑定时应报歧义，得到 %v", err)
	}
}

// TestGCPCredentialsAreIsolated 一份凭证坏掉不能牵连另一份。
func TestGCPCredentialsAreIsolated(t *testing.T) {
	app, _, idA, idB := twoCredApp(t)
	// 把 A 的密文改成解不开的垃圾
	if _, err := app.db.Exec(
		`UPDATE gcp_credentials SET sa_json = ? WHERE id = ?`, encPrefix+"bm90LWEtY2lwaGVy", idA); err != nil {
		t.Fatal(err)
	}
	if _, err := app.notifier.getGCPClient(idA); err == nil {
		t.Fatal("坏掉的凭证应报错")
	}
	cli, err := app.notifier.getGCPClient(idB)
	if err != nil {
		t.Fatalf("另一份凭证不该受牵连: %v", err)
	}
	if _, err := cli.accessToken(t.Context()); err != nil {
		t.Fatalf("另一份凭证应照常可用: %v", err)
	}
}

/* ---------- 自动开机循环 ---------- */

// TestCheckGCPStartUsesBoundCredential 两台节点分属两个账号，各自的实例都要被开机。
func TestCheckGCPStartUsesBoundCredential(t *testing.T) {
	app, m, idA, idB := twoCredApp(t)
	setSetting(app.db, keyGCPAutoOn, "1")
	setSetting(app.db, keyGCPStartDelay, "60")
	app.notifier.Reload()

	// 两台节点都已离线足够久：newNotifier 的 isOnline 默认恒 false，
	// 只要把 offlineAt 提前到确认延迟之外，本轮就该发起开机。
	for _, s := range []struct{ id, name, cred, project string }{
		{"s-a", "node-a", idA, "proj-a"},
		{"s-b", "node-b", idB, "proj-b"},
	} {
		if _, err := app.db.Exec(
			`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_cred_id, gcp_project, gcp_zone, gcp_instance)
			 VALUES(?, ?, ?, 0, 1, ?, ?, 'us-central1-a', 'vm-1')`,
			s.id, "tok-"+s.id, s.name, s.cred, s.project); err != nil {
			t.Fatal(err)
		}
	}
	app.notifier.checkGCPStart() // 第一轮只记录离线起点
	app.notifier.mu.Lock()
	for _, st := range app.notifier.gcp {
		st.offlineAt = st.offlineAt.Add(-10 * time.Minute) // 回拨，越过确认延迟
	}
	app.notifier.mu.Unlock()
	app.notifier.checkGCPStart()

	waitFor(t, func() bool {
		_, startBy := m.counts()
		return startBy["proj-a"] == 1 && startBy["proj-b"] == 1
	}, "两个账号的实例各应被开机 1 次")

	_, startBy := m.counts()
	if startBy["proj-a"] != 1 || startBy["proj-b"] != 1 {
		t.Fatalf("开机次数不对: %+v", startBy)
	}
}

// TestCheckGCPStartUnresolvableCredRecordsError 凭证解析不了时只记录错误，不碰任何实例。
func TestCheckGCPStartUnresolvableCredRecordsError(t *testing.T) {
	app, m, _, _ := twoCredApp(t)
	setSetting(app.db, keyGCPAutoOn, "1")
	setSetting(app.db, keyGCPStartDelay, "60")
	app.notifier.Reload()

	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_cred_id, gcp_zone, gcp_instance)
		 VALUES('s-x', 'tok-x', 'node-x', 0, 1, 'gone', 'us-central1-a', 'vm-1')`); err != nil {
		t.Fatal(err)
	}
	app.notifier.checkGCPStart()
	app.notifier.mu.Lock()
	for _, st := range app.notifier.gcp {
		st.offlineAt = st.offlineAt.Add(-10 * 60e9)
	}
	app.notifier.mu.Unlock()
	app.notifier.checkGCPStart()

	waitFor(t, func() bool {
		_, _, lastErr := app.notifier.GCPStatus("s-x")
		return strings.Contains(lastErr, "gone")
	}, "应把悬空凭证记进 lastErr")

	if _, startBy := m.counts(); len(startBy) != 0 {
		t.Fatalf("凭证解析失败时不该碰实例: %+v", startBy)
	}
}

/* ---------- 凭证接口 ---------- */

func addCredResp(t *testing.T, app *App, saJSON string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"saJson": saJSON})
	app.handleAddGCPCredential(w, httptest.NewRequest(http.MethodPost, "/api/admin/gcp/credentials", bytes.NewReader(body)))
	return w
}

func delCred(t *testing.T, app *App, id string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/admin/gcp/credentials/"+id, nil)
	req.SetPathValue("id", id)
	app.handleDeleteGCPCredential(w, req)
	return w
}

// TestAddGCPCredRejectsDuplicateEmail 同一个 SA 不能存两份，且报错必须是人话。
func TestAddGCPCredRejectsDuplicateEmail(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	addTestCred(t, app, saJSON)

	// 换一把私钥、同一个账号邮箱——真实的「换密钥」场景
	again, _ := makeSA(t, "")
	w := addCredResp(t, app, again)
	if w.Code != 409 {
		t.Fatalf("重复账号应返回 409，得到 %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "UNIQUE") {
		t.Fatalf("不能把 SQL 约束错误甩给用户: %s", w.Body.String())
	}
	if n, _ := countGCPCredentials(app.db); n != 1 {
		t.Fatalf("不该落库，实为 %d 行", n)
	}
}

// TestAddSecondGCPCredBindsOrphans 加第二份凭证时，未绑定的节点必须先钉到第一份上。
//
// 否则「全场只有一份」这个前提消失的瞬间，这些节点会集体停止守护，
// 而这条失败路径是不发 Telegram 的——用户唯一的线索是后台按钮上的一行 tooltip。
func TestAddSecondGCPCredBindsOrphans(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saA, _ := makeSAAs(t, "a@proj-a.iam.gserviceaccount.com", "proj-a", "")
	idA := addTestCred(t, app, saA)

	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_zone, gcp_instance)
		 VALUES('s1', 'tok-s1', 'hk-node', 0, 1, 'asia-east2-a', 'vm-1')`); err != nil {
		t.Fatal(err)
	}
	if got := credIDOf(t, app, "s1"); got != "" {
		t.Fatalf("前置条件应是未绑定，实为 %q", got)
	}

	saB, _ := makeSAAs(t, "b@proj-b.iam.gserviceaccount.com", "proj-b", "")
	addTestCred(t, app, saB)

	if got := credIDOf(t, app, "s1"); got != idA {
		t.Fatalf("未绑定节点应被钉到第一份凭证上，实为 %q", got)
	}
}

// TestDeleteGCPCredInUseIsRejected 有节点在用时不能删，且要说清是哪几台、怎么办。
func TestDeleteGCPCredInUseIsRejected(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	id := addTestCred(t, app, saJSON)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_cred_id, gcp_zone, gcp_instance)
		 VALUES('s1', 'tok-s1', 'hk-node', 0, 1, ?, 'asia-east2-a', 'vm-1')`, id); err != nil {
		t.Fatal(err)
	}

	w := delCred(t, app, id)
	if w.Code != 409 {
		t.Fatalf("被占用应返回 409，得到 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hk-node") {
		t.Fatalf("错误文案应点名占用的机器: %s", w.Body.String())
	}
	if n, _ := countGCPCredentials(app.db); n != 1 {
		t.Fatal("拒绝删除后凭证应仍在")
	}
}

// TestDeleteGCPCredUnbindsDisabledServers 关掉开关的节点不算占用，删除时顺带解绑。
//
// 这条判定是密钥轮换的唯一出口：按「全部绑定节点」判定的话，
// 只剩一份凭证且密钥被吊销时用户会被彻底锁死（新的加不进、旧的删不掉）。
func TestDeleteGCPCredUnbindsDisabledServers(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	saJSON, _ := makeSA(t, "")
	id := addTestCred(t, app, saJSON)
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, created_at, gcp_enabled, gcp_cred_id, gcp_zone, gcp_instance)
		 VALUES('s1', 'tok-s1', 'hk-node', 0, 0, ?, 'asia-east2-a', 'vm-1')`, id); err != nil {
		t.Fatal(err)
	}

	w := delCred(t, app, id)
	if w.Code != 200 {
		t.Fatalf("仅被已关开关的节点绑定时应可删除，得到 %d: %s", w.Code, w.Body.String())
	}
	if n, _ := countGCPCredentials(app.db); n != 0 {
		t.Fatalf("凭证应已删除，实为 %d 行", n)
	}
	if got := credIDOf(t, app, "s1"); got != "" {
		t.Fatalf("应解绑，不留悬空引用，实为 %q", got)
	}
}

/* ---------- 节点表单校验 ---------- */

func TestNormalizeGCPCredential(t *testing.T) {
	useTestKey(t, "test-master-key")
	app := mcpTestApp(t)
	base := func() serverForm {
		return serverForm{Name: "n", GcpEnabled: true, GcpZone: "us-central1-a", GcpInstance: "vm-1"}
	}

	// 断言错误码而不是中文措辞：文案会改，码不会——这正是错误码方案要买的东西。
	codeOf := func(e *apiErr) string {
		if e == nil {
			return ""
		}
		return e.Code
	}

	f := base()
	if got := codeOf(normalizeGCP(app.db, &f)); got != errGCPNoCred.Code {
		t.Fatalf("没有凭证时应提示去添加，得到 %q", got)
	}

	saA, _ := makeSAAs(t, "a@proj-a.iam.gserviceaccount.com", "proj-a", "")
	idA := addTestCred(t, app, saA)
	f = base()
	if got := codeOf(normalizeGCP(app.db, &f)); got != "" || f.GcpCredID != idA {
		t.Fatalf("只有一份凭证时应自动选上: code=%q credID=%q", got, f.GcpCredID)
	}

	f = base()
	f.GcpCredID = "nope"
	if got := codeOf(normalizeGCP(app.db, &f)); got != errGCPCredGone.Code {
		t.Fatalf("不存在的凭证应被拒绝，得到 %q", got)
	}

	saB, _ := makeSAAs(t, "b@proj-b.iam.gserviceaccount.com", "proj-b", "")
	idB := addTestCred(t, app, saB)
	f = base()
	if got := codeOf(normalizeGCP(app.db, &f)); got != errGCPPickCred.Code {
		t.Fatalf("多份凭证且未指定时应要求选择，得到 %q", got)
	}
	f = base()
	f.GcpCredID = idB
	if got := codeOf(normalizeGCP(app.db, &f)); got != "" {
		t.Fatalf("显式指定的合法凭证应通过: %q", got)
	}

	// 没开自动开机就不该被凭证卡住
	f = serverForm{Name: "n"}
	if got := codeOf(normalizeGCP(app.db, &f)); got != "" {
		t.Fatalf("未启用时不应校验凭证: %q", got)
	}
}
