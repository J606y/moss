package main

import (
	_ "embed"
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

func serveInstallSh(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(installSh)
}

func serveInstallPs1(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(installPs1)
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
//   assets/* 是 Vite 按内容哈希命名的产物（内容变即换名），可永久强缓存；
//   index.html（含所有 SPA 回退）绝不能强缓存，否则发版后浏览器仍读旧 HTML，
//   而它引用的旧哈希资源已被新构建删除 → 页面白屏/卡旧版，必须每次回源校验。
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
