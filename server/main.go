package main

import (
	"database/sql"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const serverVersion = "0.6.0"

// App 聚合全局依赖。
type App struct {
	db             *sql.DB
	hub            *Hub
	notifier       *Notifier
	trustProxy     bool         // 是否信任反代转发头获取真实来源 IP
	trustedProxies []*net.IPNet // 可信代理网段；从 XFF 最右往左跳过这些地址取真实客户端

	globalLimiter *limiter // 全局 /api 限流（按真实访客 IP）
	authLimiter   *limiter // 登录等敏感端点的更严限流
}

// parseTrustedProxies 把逗号分隔的 CIDR / 裸 IP 列表解析为网段切片。
// 裸 IP 视为 /32（IPv4）或 /128（IPv6）。空项与非法项会被跳过并告警。
func parseTrustedProxies(s string) []*net.IPNet {
	var nets []*net.IPNet
	for _, raw := range strings.Split(s, ",") {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(item); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(item); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		log.Printf("--trusted-proxies 忽略非法项 %q（需为 CIDR 或裸 IP）", item)
	}
	return nets
}

func main() {
	listen := flag.String("listen", ":8787", "监听地址")
	dataDir := flag.String("data", "./data", "数据目录")
	webDir := flag.String("web", "", "前端构建产物目录（留空＝用内嵌产物，无内嵌则回退 ./web/dist）")
	trustProxy := flag.Bool("trust-proxy", false, "信任反代转发头(X-Forwarded-For)获取真实IP")
	trustedProxies := flag.String("trusted-proxies", "", "可信代理名单(逗号分隔的 CIDR 或裸 IP)；从 XFF 最右往左跳过这些地址取真实客户端，仅在 --trust-proxy 下生效")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	db, err := openDB(filepath.Join(*dataDir, "moss.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	app := &App{db: db, hub: newHub(db), trustProxy: *trustProxy, trustedProxies: parseTrustedProxies(*trustedProxies)}
	app.notifier = newNotifier(db)
	app.hub.notifier = app.notifier
	// 应用层限流：按真实访客 IP 计数（env 可调，设 0 关闭对应层）
	app.globalLimiter = newLimiter(envInt("MOSS_RATELIMIT_PER_MIN", 600))
	app.authLimiter = newLimiter(envInt("MOSS_RATELIMIT_AUTH_PER_MIN", 10))
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
	mux.Handle("POST /api/login", app.limit(app.authLimiter, http.HandlerFunc(app.handleLogin)))
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
	if err := http.ListenAndServe(*listen, app.apiRateLimit(mux)); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}
