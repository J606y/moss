package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func putSettings(t *testing.T, app *App, body string) {
	t.Helper()
	w := httptest.NewRecorder()
	app.handlePutSettings(w, httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("保存设置失败，状态码 %d，body=%s", w.Code, w.Body.String())
	}
}

func getSettings(t *testing.T, app *App) settingsView {
	t.Helper()
	w := httptest.NewRecorder()
	app.handleGetSettings(w, httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil))
	var v settingsView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析设置失败: %v", err)
	}
	return v
}

// 审计保留期的上下限都必须钳住。
//
// 上限 90 天是产品定的；下限 7 天是刻意的——少于一周意味着周末发生的事
// 周一就查不到了，而周末恰恰是无人值守、AI 自主处置最多的时候。
// 保留期本身是「审计不可删除」原则唯一的后门，钳不住等于原则失效。
func TestExecAuditDaysClamped(t *testing.T) {
	app := mcpTestApp(t)

	cases := []struct {
		in   int
		want int
		why  string
	}{
		{365, 90, "超过上限应钳到 90"},
		{90, 90, "上限本身应可设"},
		{30, 30, "范围内的值原样保留"},
		{7, 7, "下限本身应可设"},
		{1, 7, "低于下限应钳到 7"},
		{0, 90, "未设置时回退默认 90"},
	}
	for _, c := range cases {
		putSettings(t, app, `{"siteName":"Moss","username":"admin","execAuditDays":`+strconv.Itoa(c.in)+`}`)
		if got := getSettings(t, app).ExecAuditDays; got != c.want {
			t.Errorf("%s：输入 %d 得到 %d，期望 %d", c.why, c.in, got, c.want)
		}
	}
}

// 条数上限的边界。不再提供「不限制」：无上限意味着一次失控的高频调用
// 就能把库撑到 GB 级，而 SQLite 打满磁盘会带走整个面板。
func TestExecAuditMaxRowsClamped(t *testing.T) {
	app := mcpTestApp(t)

	cases := []struct {
		in   int
		want int
		why  string
	}{
		{99999, 5000, "超过上限钳到 5000"},
		{5000, 5000, "上限本身可设"},
		{1000, 1000, "范围内原样保留"},
		{100, 100, "下限本身可设"},
		{50, 100, "低于下限钳到 100"},
		{0, 5000, "未设置时回退默认 5000"},
	}
	for _, c := range cases {
		putSettings(t, app, `{"siteName":"Moss","username":"admin","execAuditMaxRows":`+strconv.Itoa(c.in)+`}`)
		if got := getSettings(t, app).ExecAuditMaxRows; got != c.want {
			t.Errorf("%s：输入 %d 得到 %d，期望 %d", c.why, c.in, got, c.want)
		}
	}
}

// 新增字段不能把同一批设置里的其它值带偏。
func TestSettingsRoundTrip(t *testing.T) {
	app := mcpTestApp(t)
	putSettings(t, app,
		`{"siteName":"我的面板","siteDesc":"测试","username":"admin",
		  "reportInterval":5,"sampleInterval":30,"historyDays":14,"pingDays":30,
		  "execAuditDays":60,"execAuditMaxRows":2000}`)

	v := getSettings(t, app)
	if v.SiteName != "我的面板" || v.SiteDesc != "测试" {
		t.Errorf("站点信息未保存: %+v", v)
	}
	if v.ReportInterval != 5 || v.SampleInterval != 30 || v.HistoryDays != 14 || v.PingDays != 30 {
		t.Errorf("采集设置被带偏: %+v", v)
	}
	if v.ExecAuditDays != 60 {
		t.Errorf("审计保留期未保存，实际 %d", v.ExecAuditDays)
	}
	if v.ExecAuditMaxRows != 2000 {
		t.Errorf("审计条数上限未保存，实际 %d", v.ExecAuditMaxRows)
	}
}

// 语言档位必须收敛到 auto/zh/en。
//
// 漏网的脏值不会报错，只会让前端拿着一个查不到文案表的语言码去渲染，
// 整页退化成一串 key——比报错更难查，因为服务端一切正常。
func TestLangClamped(t *testing.T) {
	app := mcpTestApp(t)

	cases := []struct {
		in   string
		want string
		why  string
	}{
		{`"en"`, "en", "英文可设"},
		{`"zh"`, "zh", "中文可设"},
		{`"auto"`, "auto", "跟随浏览器可设"},
		{`"fr"`, "auto", "未支持的语言回落 auto"},
		{`""`, "auto", "空串回落 auto"},
		{`"EN"`, "auto", "大小写不匹配即视为非法，不做纠正"},
	}
	for _, c := range cases {
		putSettings(t, app, `{"siteName":"Moss","username":"admin","lang":`+c.in+`}`)
		if got := getSettings(t, app).Lang; got != c.want {
			t.Errorf("%s：输入 %s 得到 %q，期望 %q", c.why, c.in, got, c.want)
		}
	}
}

// 首页对外公开，访客不登录就得拿到站点选定的语言，
// 否则 auto 之外的两档形同虚设：只有管理员自己能看到正确的语言。
func TestSiteExposesLang(t *testing.T) {
	app := mcpTestApp(t)
	putSettings(t, app, `{"siteName":"Moss","username":"admin","lang":"en"}`)

	w := httptest.NewRecorder()
	app.handleSite(w, httptest.NewRequest(http.MethodGet, "/api/site", nil))
	var site map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &site); err != nil {
		t.Fatalf("解析站点信息失败: %v", err)
	}
	if site["lang"] != "en" {
		t.Errorf("公开接口未下发语言，实际 %q", site["lang"])
	}
}

// 老库里没有 lang 这一行，读出来必须是 auto：
// 升级上来的站点不该被硬切成某一门语言，中文用户的浏览器仍会解析回中文。
func TestLangDefaultsToAutoOnUpgrade(t *testing.T) {
	app := mcpTestApp(t)
	if got := getSettings(t, app).Lang; got != "auto" {
		t.Errorf("未设置过语言时应为 auto，实际 %q", got)
	}
}
