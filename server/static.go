package main

import (
	_ "embed"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed install/install.sh
var installSh []byte

//go:embed install/install.ps1
var installPs1 []byte

// 安装脚本里的默认版本行。server 下发时按自身版本改写，见 installScript。
// 这两个字面量必须与脚本逐字节一致，由 TestInstallScriptPinsVersion 锁住。
const (
	shVersionLine    = `VERSION="${MOSS_VERSION:-latest}"`
	shVersionFormat  = `VERSION="${MOSS_VERSION:-%s}"`
	ps1VersionLine   = `[string]$Version = "latest"`
	ps1VersionFormat = `[string]$Version = "%s"`
)

// defaultAgentVersion 返回安装脚本应当默认安装的 agent 版本（带 v 前缀的 tag 名）。
// 开发态未注入版本，没有对应的 release tag，返回空表示保持脚本原样走 latest。
func defaultAgentVersion() string {
	if serverVersion == "" || serverVersion == "dev" {
		return ""
	}
	return "v" + strings.TrimPrefix(serverVersion, "v")
}

// installScript 把安装脚本的默认 agent 版本钉到 server 自身版本。
//
// 面板是什么版本，它装出来的 agent 就是什么版本。两条理由：
//   - agent 与 server 的 WS 协议随版本演进（exec / write 均为 v2 新增），
//     版本错配不是“旧一点”，而是新功能静默失效；
//   - GitHub 的 latest 按定义跳过预发布版，beta 面板若走 latest，
//     会装回上一个正式版还照常提示安装成功。
//
// 未匹配到默认版本行时原样返回：脚本仍走 latest，不会因改写失败而拼出 404 的 URL。
func installScript(raw []byte, line, format string) []byte {
	v := defaultAgentVersion()
	if v == "" {
		return raw
	}
	return []byte(strings.Replace(string(raw), line, fmt.Sprintf(format, v), 1))
}

func serveInstallSh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(installScript(installSh, shVersionLine, shVersionFormat))
}

func serveInstallPs1(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(installScript(installPs1, ps1VersionLine, ps1VersionFormat))
}

// spaHandler 服务前端构建产物，未命中文件时回退到 index.html。
// 优先级：显式 --web 磁盘目录 > 编译期内嵌(-tags embed_web) > 默认 ./web/dist。
// 开发模式走 Vite 代理，不经过这里。
func spaHandler(webDir string) http.Handler {
	if webDir == "" && hasEmbeddedWeb {
		if sub, err := fs.Sub(embeddedWeb, "dist"); err == nil {
			return spaFromFS(sub)
		}
	}
	if webDir == "" {
		webDir = "./web/dist"
	}
	return spaFromDisk(webDir)
}

// setStaticCache 按文件名定缓存策略：
//
//	assets/* 是 Vite 按内容哈希命名的产物（内容变即换名），可永久强缓存；
//	index.html（含所有 SPA 回退）绝不能强缓存，否则发版后浏览器仍读旧 HTML，
//	而它引用的旧哈希资源已被新构建删除 → 页面白屏/卡旧版，必须每次回源校验。
func setStaticCache(w http.ResponseWriter, name string) {
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

// spaFromFS 基于内嵌 fs.FS 提供 SPA，未命中回退 index.html。
func spaFromFS(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
			setStaticCache(w, name)
			http.ServeFileFS(w, r, fsys, name)
			return
		}
		setStaticCache(w, "index.html")
		http.ServeFileFS(w, r, fsys, "index.html")
	})
}

// spaFromDisk 基于磁盘目录提供 SPA，目录缺少 index.html 时给出提示。
func spaFromDisk(webDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := filepath.Join(webDir, "index.html")
		if _, err := os.Stat(index); err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("Moss API 运行中。前端构建产物未找到，请先执行 cd web && npm run build，或开发模式下访问 Vite 端口。\n"))
			return
		}
		rel := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		p := filepath.Join(webDir, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			setStaticCache(w, filepath.ToSlash(rel))
			http.ServeFile(w, r, p)
			return
		}
		setStaticCache(w, "index.html")
		http.ServeFile(w, r, index)
	})
}
