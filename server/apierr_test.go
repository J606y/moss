package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

/*
错误码与前端文案表的一致性闸门。

后端每新增一条码，`web/src/i18n/zh.ts` 与 `en.ts` 都必须跟着加一条 `err.<code>`。
漏了不会报错也不会崩——界面只是退回显示后端的中文原文，在英文界面上静默地
冒出一句中文。没有这道闸就没有任何东西会发现它。

思路与 `Dict = Record<TextKey, string>` 强制两张表对齐完全一样，只是跨了语言边界，
TypeScript 管不到 Go 这一侧，只能用测试补上。
*/

// codeRe 抓错误表里的 Code 与 Msg。Msg 可能是字符串字面量，也可能是个常量名
// （目前只有 upgradeOSUnsupportedHint 一处），后者由 constRe 解析出的表来还原。
var (
	codeRe  = regexp.MustCompile(`Code: *"([^"]+)", *Msg: *(?:"((?:[^"\\]|\\.)*)"|([A-Za-z]\w*))`)
	constRe = regexp.MustCompile(`(?m)^(?:const\s+)?\s*(\w+)\s+=\s+"((?:[^"\\]|\\.)*)"\s*$`)
	dictRe  = regexp.MustCompile(`'(err\.[^']+)':\s*'((?:[^'\\]|\\.)*)'`)
)

// backendCodes 扫本包所有非测试源码，返回 码 → Msg。
//
// 扫源码而不是在测试里手抄一份清单：手抄的那份不会因为别人新增了一条
// `apiErr{Code: ...}` 而失败，等于没有这道闸。
func backendCodes(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列出源码: %v", err)
	}
	var all strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s: %v", f, err)
		}
		all.Write(b)
		all.WriteString("\n")
	}
	src := all.String()

	// 先收常量，供 Msg 写成标识符的条目还原文案
	consts := map[string]string{}
	for _, m := range constRe.FindAllStringSubmatch(src, -1) {
		consts[m[1]] = m[2]
	}

	// Code 与 Msg 可能跨行写，压掉换行再抓
	flat := regexp.MustCompile(`\s*\n\s*`).ReplaceAllString(src, " ")
	out := map[string]string{}
	for _, m := range codeRe.FindAllStringSubmatch(flat, -1) {
		msg := m[2]
		if m[3] != "" {
			resolved, ok := consts[m[3]]
			if !ok {
				t.Errorf("码 %q 的 Msg 是常量 %s，但没在源码里找到它的字面量定义", m[1], m[3])
				continue
			}
			msg = resolved
		}
		out[m[1]] = msg
	}
	if len(out) < 40 {
		// 扫不到说明正则与源码写法脱节了，这时「全部通过」是假的。
		t.Fatalf("只扫到 %d 条错误码，正则与源码写法已脱节", len(out))
	}
	return out
}

// frontendTexts 读前端文案表，返回 码 → 文案。
func frontendTexts(t *testing.T, name string) map[string]string {
	t.Helper()
	path := filepath.Join("..", "web", "src", "i18n", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	out := map[string]string{}
	for _, m := range dictRe.FindAllStringSubmatch(string(b), -1) {
		out[strings.TrimPrefix(m[1], "err.")] = m[2]
	}
	return out
}

// TestErrorCodesHaveTranslations 每条后端错误码都必须在两张文案表里有对应条目。
func TestErrorCodesHaveTranslations(t *testing.T) {
	codes := backendCodes(t)
	for _, dict := range []string{"zh.ts", "en.ts"} {
		texts := frontendTexts(t, dict)
		for code := range codes {
			if _, ok := texts[code]; !ok {
				t.Errorf("%s 缺少 err.%s —— 英文界面下这条错误会冒出中文", dict, code)
			}
		}
		// 反向也查：文案表里多出来的条目多半是码被删了或名字拼错了，
		// 留着它只会让人以为某条错误已经翻译好了。
		for code := range texts {
			if _, ok := codes[code]; !ok {
				t.Errorf("%s 里的 err.%s 在后端没有对应的码（拼错，或码已删除）", dict, code)
			}
		}
	}
}

// TestErrorDetailPlaceholdersAligned 后端带 Detail 的码，文案里必须有 {detail}。
//
// 后端的硬约定是「细节一律放末尾走 Detail，Msg 以冒号收尾」（见 apierr.go）。
// 文案少一个 {detail}，机器名 / 版本号 / Go error 原文就被**静默吞掉**：
// 类型系统抓不到，前端「英文界面无汉字」也抓不到——少一个占位符照样没有汉字。
// 这一条是实测确认过的：抽掉 en.ts 里一个 {detail}，只有正向断言会红。
func TestErrorDetailPlaceholdersAligned(t *testing.T) {
	codes := backendCodes(t)
	zh := frontendTexts(t, "zh.ts")
	en := frontendTexts(t, "en.ts")

	var missing []string
	for code, msg := range codes {
		// Msg 以冒号收尾、或以「实词 + 空格」收尾（如「当前 」）即表示带 Detail
		wants := regexp.MustCompile(`：\s*$|\S\s$`).MatchString(msg)
		for _, d := range []struct {
			name string
			m    map[string]string
		}{{"zh.ts", zh}, {"en.ts", en}} {
			text, ok := d.m[code]
			if !ok {
				continue // 缺失由上一条测试报告，这里不重复
			}
			if has := strings.Contains(text, "{detail}"); has != wants {
				missing = append(missing,
					"  "+code+" @"+d.name+"：后端带 detail="+bool2s(wants)+"，文案有占位符="+bool2s(has))
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("{detail} 占位符与后端不一致，细节会被静默吞掉：\n%s", strings.Join(missing, "\n"))
	}
}

func bool2s(b bool) string {
	if b {
		return "是"
	}
	return "否"
}
