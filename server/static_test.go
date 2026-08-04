package main

import (
	"strings"
	"testing"
)

// 安装脚本的默认版本行一旦被改动，installScript 的字符串替换就静默失效，
// 面板悄悄退回 latest —— 而 latest 按定义跳过预发布版，beta 面板会装回上一个
// 正式版还提示安装成功。这个失败模式没有任何运行时报错，只能靠测试锁住。
func TestInstallScriptVersionLineExists(t *testing.T) {
	if !strings.Contains(string(installSh), shVersionLine) {
		t.Errorf("install.sh 缺少默认版本行 %q，installScript 的改写已失效", shVersionLine)
	}
	if !strings.Contains(string(installPs1), ps1VersionLine) {
		t.Errorf("install.ps1 缺少默认版本行 %q，installScript 的改写已失效", ps1VersionLine)
	}
}

func TestInstallScriptPinsVersion(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })

	serverVersion = "2.0.0-beta.1"
	sh := string(installScript(installSh, shVersionLine, shVersionFormat))
	if !strings.Contains(sh, `VERSION="${MOSS_VERSION:-v2.0.0-beta.1}"`) {
		t.Error("install.sh 未钉到 server 自身版本")
	}
	if strings.Contains(sh, shVersionLine) {
		t.Error("install.sh 仍残留 latest 默认值")
	}
	ps := string(installScript(installPs1, ps1VersionLine, ps1VersionFormat))
	if !strings.Contains(ps, `[string]$Version = "v2.0.0-beta.1"`) {
		t.Error("install.ps1 未钉到 server 自身版本")
	}

	// ldflags 注入的是去掉 v 的版本号，而下载 URL 用的是带 v 的 tag 名。
	// 两种写法都要归一到带 v，且不能把已有的 v 再加一遍。
	serverVersion = "v2.0.0"
	if sh := string(installScript(installSh, shVersionLine, shVersionFormat)); !strings.Contains(sh, `{MOSS_VERSION:-v2.0.0}`) {
		t.Error("带 v 前缀的版本号未被正确归一")
	}
}

// 开发态没有对应的 release tag，钉上去只会 404，必须保持 latest。
func TestInstallScriptDevKeepsLatest(t *testing.T) {
	old := serverVersion
	t.Cleanup(func() { serverVersion = old })

	for _, v := range []string{"dev", ""} {
		serverVersion = v
		if sh := string(installScript(installSh, shVersionLine, shVersionFormat)); !strings.Contains(sh, shVersionLine) {
			t.Errorf("serverVersion=%q 时应保持 latest", v)
		}
		if ps := string(installScript(installPs1, ps1VersionLine, ps1VersionFormat)); !strings.Contains(ps, ps1VersionLine) {
			t.Errorf("serverVersion=%q 时应保持 latest", v)
		}
	}
}
