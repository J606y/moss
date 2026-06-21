package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
)

const serverVersion = "0.3"

// App 聚合全局依赖。
type App struct {
	db         *sql.DB
	hub        *Hub
	notifier   *Notifier
	trustProxy bool // 是否信任反代转发头获取真实来源 IP
}

func main() {
	listen := flag.String("listen", ":8787", "监听地址")
	dataDir := flag.String("data", "./data", "数据目录")
	webDir := flag.String("web", "", "前端构建产物目录（留空＝用内嵌产物，无内嵌则回退 ./web/dist）")
	trustProxy := flag.Bool("trust-proxy", false, "信任反代转发头(X-Real-IP/X-Forwarded-For)获取真实IP")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	db, err := openDB(filepath.Join(*dataDir, "moss.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	app := &App{db: db, hub: newHub(db), trustProxy: *trustProxy}
	app.notifier = newNotifier(db)
	app.hub.notifier = app.notifier
	app.ensurePassword()
	go cleanupLoop(db)
	go app.notifier.Run()

	mux := http.NewServeMux()

	// 公开接口
	mux.HandleFunc("GET /api/site", app.handleSite)
	mux.HandleFunc("GET /api/servers", app.handleServers)
	mux.HandleFunc("GET /api/servers/{id}/recent", app.handleRecent)
	mux.HandleFunc("GET /api/servers/{id}/history", app.handleHistory)
	mux.HandleFunc("GET /api/servers/{id}/ping", app.handlePing)
	mux.HandleFunc("GET /api/ws", app.handleBrowserWS)
	mux.HandleFunc("GET /api/agent/ws", app.handleAgentWS)

	// 认证
	mux.HandleFunc("POST /api/login", app.handleLogin)
	mux.HandleFunc("POST /api/logout", app.handleLogout)
	mux.HandleFunc("GET /api/admin/me", app.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
	}))

	// 管理接口
	mux.HandleFunc("GET /api/admin/servers", app.requireAuth(app.handleAdminServers))
	mux.HandleFunc("POST /api/admin/servers", app.requireAuth(app.handleAddServer))
	mux.HandleFunc("POST /api/admin/servers/reorder", app.requireAuth(app.handleReorderServers))
	mux.HandleFunc("PUT /api/admin/servers/{id}", app.requireAuth(app.handleUpdateServer))
	mux.HandleFunc("DELETE /api/admin/servers/{id}", app.requireAuth(app.handleDeleteServer))
	mux.HandleFunc("GET /api/admin/tasks", app.requireAuth(app.handleAdminTasks))
	mux.HandleFunc("POST /api/admin/tasks", app.requireAuth(app.handleAddTask))
	mux.HandleFunc("PUT /api/admin/tasks/{id}", app.requireAuth(app.handleUpdateTask))
	mux.HandleFunc("DELETE /api/admin/tasks/{id}", app.requireAuth(app.handleDeleteTask))
	mux.HandleFunc("GET /api/admin/notify", app.requireAuth(app.handleGetNotify))
	mux.HandleFunc("PUT /api/admin/notify", app.requireAuth(app.handlePutNotify))
	mux.HandleFunc("POST /api/admin/notify/test", app.requireAuth(app.handleTestNotify))
	mux.HandleFunc("GET /api/admin/settings", app.requireAuth(app.handleGetSettings))
	mux.HandleFunc("PUT /api/admin/settings", app.requireAuth(app.handlePutSettings))
	mux.HandleFunc("PUT /api/admin/password", app.requireAuth(app.handleChangePassword))

	// 安装脚本
	mux.HandleFunc("GET /install.sh", serveInstallSh)
	mux.HandleFunc("GET /install.ps1", serveInstallPs1)

	// 前端静态资源（SPA 回退）
	mux.Handle("/", spaHandler(*webDir))

	log.Printf("Moss server v%s 启动，监听 %s", serverVersion, *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
