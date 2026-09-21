package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// placeholderRe 匹配文案里的 {name} 占位符。
var placeholderRe = regexp.MustCompile(`\{(\w+)\}`)

func placeholders(s string) []string {
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// TestAlertTextsCoverAllEventTypes 每个告警事件类型都必须有文案。
//
// 漏一条的后果不是「显示成中文」而是**推送一条空消息**：renderAlert 查不到模板
// 就退回 ev.Text，而告警产生处已经不再拼文案了。
//
// 类型清单从 webhook.go 源码里扫，不在测试里手抄一份——手抄的那份不会因为
// 别人新增了一个 evtXxx 而失败，那就等于没有这道闸。
func TestAlertTextsCoverAllEventTypes(t *testing.T) {
	src, err := os.ReadFile("webhook.go")
	if err != nil {
		t.Fatalf("读 webhook.go: %v", err)
	}
	re := regexp.MustCompile(`evt\w+\s*=\s*"([^"]+)"`)
	found := re.FindAllStringSubmatch(string(src), -1)
	if len(found) < 10 {
		// 扫不到说明正则与源码写法脱节了，这时「全部通过」是假的。
		t.Fatalf("只扫到 %d 个事件类型，正则与 webhook.go 的写法已脱节", len(found))
	}
	for _, m := range found {
		if _, ok := alertTexts[m[1]]; !ok {
			t.Errorf("事件类型 %q 没有文案，推送会是一条空消息", m[1])
		}
	}
}

// TestAlertTextPlaceholdersMatchAcrossLangs 两种语言的占位符集合必须一致。
//
// 这是最容易静默出错的一处：英文少写一个 {detail}/{name}，那个参数就被悄悄吞掉，
// 编译器不管、「英文里没有汉字」也查不出来（少一个占位符照样没有汉字）。
// 前端那边实测确认过这一类错只有逐条比对才抓得到。
func TestAlertTextPlaceholdersMatchAcrossLangs(t *testing.T) {
	for key, tpl := range alertTexts {
		zh, en := placeholders(tpl.zh), placeholders(tpl.en)
		if strings.Join(zh, ",") != strings.Join(en, ",") {
			t.Errorf("%s 两种语言的占位符不一致：zh=%v en=%v", key, zh, en)
		}
	}
}

// TestAlertTextsNonEmpty 两种语言都不能留空——空模板会推出一条空消息。
func TestAlertTextsNonEmpty(t *testing.T) {
	for key, tpl := range alertTexts {
		if strings.TrimSpace(tpl.zh) == "" || strings.TrimSpace(tpl.en) == "" {
			t.Errorf("%s 有一种语言是空的：zh=%q en=%q", key, tpl.zh, tpl.en)
		}
	}
}

// TestAlertTextEnglishHasNoHan 英文文案里不许残留汉字。
//
// 与前端冒烟测试的「英文界面无汉字」同一个思路，只是推送这条路没有页面可扫，
// 只能静态查。照抄中文忘了译是这套表最常见的失误。
func TestAlertTextEnglishHasNoHan(t *testing.T) {
	hasHan := func(s string) bool {
		for _, r := range s {
			if unicode.Is(unicode.Han, r) {
				return true
			}
		}
		return false
	}
	for key, tpl := range alertTexts {
		if hasHan(tpl.en) {
			t.Errorf("%s 的英文文案里有汉字：%q", key, tpl.en)
		}
	}
	for code, name := range metricNames {
		if hasHan(name.en) {
			t.Errorf("指标 %s 的英文名里有汉字：%q", code, name.en)
		}
	}
}

func TestRenderAlert(t *testing.T) {
	cases := []struct {
		why  string
		ev   alertEvent
		lang string
		want string
	}{
		{
			why:  "中文渲染，{metric} 由 Metric 码查表",
			lang: langZH,
			ev: alertEvent{
				Type:   evtLoadAlert,
				Metric: metricMem,
				params: map[string]string{"name": "hk-01", "val": "95.0", "min": "5", "th": "90"},
			},
			want: "⚠️ 负载告警\nhk-01 内存 使用率 95.0%，已持续 5 分钟（阈值 90%）",
		},
		{
			why:  "英文渲染，指标名跟着换语言",
			lang: langEN,
			ev: alertEvent{
				Type:   evtLoadAlert,
				Metric: metricMem,
				params: map[string]string{"name": "hk-01", "val": "95.0", "min": "5", "th": "90"},
			},
			want: "⚠️ Load alert\nhk-01 Memory usage 95.0%, sustained for 5 min (threshold 90%)",
		},
		{
			why:  "textKey 选中变体：0 天换一句话，不说「剩余 0 天」",
			lang: langZH,
			ev: alertEvent{
				Type:    evtServerExpiring,
				textKey: txtExpiringToday,
				params:  map[string]string{"name": "jp-02", "date": "2026-09-21", "days": "0"},
			},
			want: "📅 到期提醒\njp-02 今天到期（2026-09-21）",
		},
		{
			why:  "textKey 为空时取 Type",
			lang: langZH,
			ev: alertEvent{
				Type:   evtServerExpiring,
				params: map[string]string{"name": "jp-02", "date": "2026-09-25", "days": "4"},
			},
			want: "📅 到期提醒\njp-02 将于 2026-09-25 到期（剩余 4 天）",
		},
		{
			why:  "缺参数时原样保留占位符，看得见",
			lang: langZH,
			ev: alertEvent{
				Type:   evtServerOffline,
				params: map[string]string{"name": "tw-03"},
			},
			want: "🔴 服务器离线\ntw-03 已离线超过 {sec} 秒",
		},
		{
			why:  "未登记的类型退回产生处给的文案",
			lang: langEN,
			ev:   alertEvent{Type: "test", Text: "Moss webhook test"},
			want: "Moss webhook test",
		},
		{
			why:  "未登记且没有兜底文案时退回类型名，不发空消息",
			lang: langEN,
			ev:   alertEvent{Type: "some.unregistered"},
			want: "some.unregistered",
		},
		{
			why:  "网速文案不带 {metric}，上下行数字自带语义",
			lang: langEN,
			ev: alertEvent{
				Type:   evtNetAlert,
				Metric: metricNet,
				params: map[string]string{"name": "hk-01", "up": "80.0", "down": "12.5", "sec": "60", "th": "50"},
			},
			want: "⚠️ Network alert\nhk-01 up 80.0 MB/s / down 12.5 MB/s, sustained for 60s (threshold 50 MB/s)",
		},
	}
	for _, c := range cases {
		if got := renderAlert(c.ev, c.lang); got != c.want {
			t.Errorf("%s\n  实际 %q\n  期望 %q", c.why, got, c.want)
		}
	}
}

// TestFireRendersBySiteLang 端到端：站点设成 English 时，推出去的文案就是英文。
//
// 推送这条路没有浏览器参与，语言只能由站点档位决定。单测 renderAlert 不够——
// 得确认 fire 真的去读了那个设置。
func TestFireRendersBySiteLang(t *testing.T) {
	for _, c := range []struct {
		site string
		want string
		why  string
	}{
		{site: langEN, want: "🔴 Server offline\nhk-01 has been offline for over 60s", why: "站点设 en → 英文推送"},
		{site: langZH, want: "🔴 服务器离线\nhk-01 已离线超过 60 秒", why: "站点设 zh → 中文推送"},
		// auto 在推送这条路上无从解析（没有浏览器），退回中文＝改造前的行为。
		{site: langAuto, want: "🔴 服务器离线\nhk-01 已离线超过 60 秒", why: "站点设 auto → 退回中文"},
	} {
		sink := newWebhookSink(t)
		n := webhookTestNotifier(t, sink.srv.URL, "", true)
		setSetting(n.db, keyLang, c.site)

		n.fire(notifyConfig{}, alertEvent{
			Type:       evtServerOffline,
			ServerID:   "srv1",
			ServerName: "hk-01",
			params:     map[string]string{"name": "hk-01", "sec": "60"},
		})

		if ev := sink.wait(t); ev.Text != c.want {
			t.Errorf("%s\n  实际 %q\n  期望 %q", c.why, ev.Text, c.want)
		}
	}
}
