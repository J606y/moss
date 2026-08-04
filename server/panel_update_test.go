package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub 起一个假的 Releases API。
// MOSS_GITHUB_API 本来是给境内镜像留的入口，正好也让这里能脱离网络测。
func fakeGitHub(t *testing.T, stable, beta string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			fmt.Fprintf(w, `{"tag_name":%q,"name":"正式版","body":"说明","prerelease":false}`, stable)
			return
		}
		fmt.Fprintf(w, `[{"tag_name":%q,"name":"测试版","body":"说明","prerelease":true}]`, beta)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MOSS_GITHUB_API", srv.URL)
}

func TestCompareToCurrent(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })

	serverVersion = "2.0.0-beta.3"
	cases := map[string]string{
		"v2.0.0-beta.4": "update",    // 同系列更新的测试版
		"v2.0.0":        "update",    // 正式版追上了预发布版——semver 里 2.0.0 > 2.0.0-beta.3
		"v2.1.0":        "update",    // 更高的正式版
		"v2.0.0-beta.3": "none",      // 已是最新
		"v1.4.0":        "downgrade", // 正式版通道还停在旧版本时的典型情况
		"v2.0.0-beta.1": "downgrade",
	}
	for target, want := range cases {
		if got := compareToCurrent(target).Action; got != want {
			t.Errorf("当前 %s → 目标 %s：判定为 %q，期望 %q", serverVersion, target, got, want)
		}
	}

	// 开发版本没有可比的发布号，只能如实说不知道，不能瞎猜成可更新
	serverVersion = "dev"
	if got := compareToCurrent("v2.0.0").Action; got != "unknown" {
		t.Errorf("开发版应判定为 unknown，实际 %q", got)
	}
}

// 跑测试版的人默认留在测试版通道；一律默认正式版的话，
// 他一进页面就会看到「降级到某个旧正式版」。
func TestDefaultChannel(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })

	serverVersion = "2.0.0-beta.3"
	if got := defaultChannel(); got != channelBeta {
		t.Errorf("测试版应默认测试版通道，实际 %q", got)
	}
	serverVersion = "2.0.0"
	if got := defaultChannel(); got != channelStable {
		t.Errorf("正式版应默认正式版通道，实际 %q", got)
	}
}

func TestFetchReleaseByChannel(t *testing.T) {
	fakeGitHub(t, "v1.4.0", "v2.0.0-beta.4")
	c := newReleaseCache()

	stable, err := c.fetch(channelStable, true)
	if err != nil || stable.Version != "v1.4.0" {
		t.Fatalf("正式版通道应取到 v1.4.0，实际 %+v err=%v", stable, err)
	}
	beta, err := c.fetch(channelBeta, true)
	if err != nil || beta.Version != "v2.0.0-beta.4" {
		t.Fatalf("测试版通道应取到 v2.0.0-beta.4，实际 %+v err=%v", beta, err)
	}
}

// 降级必须在后端挡住，不能只靠前端不显示按钮——绕过界面直接调接口同样要被拒。
// 数据库迁移是单向的，旧版本读不懂新版本写下的结构。
func TestPanelUpdateRejectsDowngrade(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.3"

	// 正式版通道最新只有 v1.4.0，比当前的测试版旧
	fakeGitHub(t, "v1.4.0", "v2.0.0-beta.3")
	app := mcpTestApp(t)
	app.releases = newReleaseCache()
	app.panelUpd = newPanelUpdater()
	if _, err := app.db.Exec(
		`INSERT INTO servers(id, token, name, grp, created_at, agent_version)
		 VALUES('host','t','面板机','默认',0,'2.0.0-beta.3')`); err != nil {
		t.Fatal(err)
	}
	setSetting(app.db, keyPanelChannel, channelStable)
	setSetting(app.db, keyPanelHostServer, "host")

	w := httptest.NewRecorder()
	app.handleStartPanelUpdate(w, httptest.NewRequest(http.MethodPost, "/api/admin/panel-update/start", nil))

	if w.Code == http.StatusOK {
		t.Fatal("降级请求必须被拒绝")
	}
	// 机器离线的拦截排在版本判定之前也算拦住了，但错误信息要能区分两者
	body := w.Body.String()
	if !strings.Contains(body, "更高") && !strings.Contains(body, "离线") {
		t.Errorf("拒绝原因应说明是版本或机器问题，实际: %s", body)
	}
}

// 版本查询失败时页面不能变成一块死砖：要如实报错，而不是让「已是最新」蒙混过去。
func TestPanelUpdateViewSurvivesCheckFailure(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })
	serverVersion = "2.0.0-beta.3"
	t.Setenv("MOSS_GITHUB_API", "http://127.0.0.1:1") // 必定连不上

	app := mcpTestApp(t)
	app.releases = newReleaseCache()
	app.panelUpd = newPanelUpdater()

	w := httptest.NewRecorder()
	app.handleGetPanelUpdate(w, httptest.NewRequest(http.MethodGet, "/api/admin/panel-update", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("查询失败也应返回 200 并带上错误说明，实际 %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "checkError") {
		t.Error("响应里应带上 checkError")
	}
	if !strings.Contains(body, `"action":"unknown"`) {
		t.Error("查不到版本时不能判成已是最新，应为 unknown")
	}
}
