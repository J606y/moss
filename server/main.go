package main

import (
	"context"
	"database/sql"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// serverVersion 发版时由 -ldflags "-X main.serverVersion=..." 注入（见 release.yml / Dockerfile）。
var serverVersion = "dev"

// App 聚合全局依赖。
type App struct {
	db             *sql.DB
	hub            *Hub
	notifier       *Notifier
	trustProxy     bool         // 是否信任反代转发头获取真实来源 IP
	trustedProxies []*net.IPNet // 可信代理网段；从 XFF 最右往左跳过这些地址取真实客户端

	exec     *execManager    // 命令下发、分片聚合与审计
	releases *releaseCache   // GitHub 版本查询缓存
	panelUpd *panelUpdater   // 面板自更新状态
	upgrade  *upgradeManager // agent 一键升级的下发与状态跟踪

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

	initSecret(*dataDir) // 敏感列（GCP SA 凭证）加密主密钥

	app := &App{db: db, hub: newHub(db), trustProxy: *trustProxy, trustedProxies: parseTrustedProxies(*trustedProxies)}
	app.exec = newExecManager(db)
	app.upgrade = newUpgradeManager()
	app.releases = newReleaseCache()
	app.panelUpd = newPanelUpdater()
	app.notifier = newNotifier(db)
	app.exec.notifier = app.notifier // 拦截告警走同一套通知配置
	app.hub.notifier = app.notifier
	app.notifier.isOnline = func(id string) bool {
		_, _, online := app.hub.Snapshot(id)
		return online
	}
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
	mux.HandleFunc("POST /api/admin/tasks/reorder", app.requireAuth(app.handleReorderTasks))
	mux.HandleFunc("PUT /api/admin/tasks/{id}", app.requireAuth(app.handleUpdateTask))
	mux.HandleFunc("DELETE /api/admin/tasks/{id}", app.requireAuth(app.handleDeleteTask))
	mux.HandleFunc("GET /api/admin/notify", app.requireAuth(app.handleGetNotify))
	mux.HandleFunc("PUT /api/admin/notify", app.requireAuth(app.handlePutNotify))
	mux.HandleFunc("POST /api/admin/notify/test", app.requireAuth(app.handleTestNotify))
	mux.HandleFunc("GET /api/admin/webhook", app.requireAuth(app.handleGetWebhook))
	mux.HandleFunc("PUT /api/admin/webhook", app.requireAuth(app.handlePutWebhook))
	mux.HandleFunc("POST /api/admin/webhook/test", app.requireAuth(app.handleTestWebhook))
	mux.HandleFunc("GET /api/admin/gcp", app.requireAuth(app.handleGetGCP))
	mux.HandleFunc("PUT /api/admin/gcp", app.requireAuth(app.handlePutGCP))
	mux.HandleFunc("POST /api/admin/gcp/test", app.requireAuth(app.handleTestGCP))
	mux.HandleFunc("POST /api/admin/servers/{id}/gcp-start", app.requireAuth(app.handleGCPManualStart))
	// agent 一键升级：只挂在后台管理接口下，刻意不进 MCP 工具清单——
	// 让 AI 升级自己脚下的 agent 等于绕过闸 2「篡改 moss 自身」的硬拦。
	mux.HandleFunc("POST /api/admin/servers/{id}/upgrade", app.requireAuth(app.handleUpgradeAgent))
	mux.HandleFunc("GET /api/admin/keys", app.requireAuth(app.handleListKeys))
	mux.HandleFunc("POST /api/admin/keys", app.requireAuth(app.handleCreateKey))
	mux.HandleFunc("PUT /api/admin/keys/{id}", app.requireAuth(app.handleUpdateKey))
	mux.HandleFunc("PUT /api/admin/keys/{id}/disabled", app.requireAuth(app.handleToggleKey))
	mux.HandleFunc("DELETE /api/admin/keys/{id}", app.requireAuth(app.handleDeleteKey))
	mux.HandleFunc("POST /api/admin/servers/{id}/exec", app.requireAuth(app.handleExec))
	mux.HandleFunc("GET /api/admin/exec-audit", app.requireAuth(app.handleExecAudit))
	mux.HandleFunc("GET /api/admin/exec-audit/{jobId}", app.requireAuth(app.handleExecAuditDetail))
	// 面板自更新：同样只在后台管理接口下。更新面板意味着替换整个中枢，
	// 比升级单台 agent 的权限更高，更不该出现在 MCP 工具清单里。
	mux.HandleFunc("GET /api/admin/panel-update", app.requireAuth(app.handleGetPanelUpdate))
	mux.HandleFunc("PUT /api/admin/panel-update", app.requireAuth(app.handlePutPanelUpdate))
	mux.HandleFunc("POST /api/admin/panel-update/start", app.requireAuth(app.handleStartPanelUpdate))
	mux.HandleFunc("GET /api/admin/settings", app.requireAuth(app.handleGetSettings))
	mux.HandleFunc("PUT /api/admin/settings", app.requireAuth(app.handlePutSettings))
	mux.HandleFunc("PUT /api/admin/password", app.requireAuth(app.handleChangePassword))

	// MCP 端点（AI 客户端接入，Bearer API Key 鉴权）
	mux.HandleFunc("/mcp", app.handleMCP)

	// 安装脚本
	mux.HandleFunc("GET /install.sh", serveInstallSh)
	mux.HandleFunc("GET /install.ps1", serveInstallPs1)

	// 前端静态资源（SPA 回退）
	mux.Handle("/", spaHandler(*webDir))

	srv := &http.Server{
		Addr:    *listen,
		Handler: app.apiRateLimit(mux),
		// 挡 Slowloris 慢连接；不设 WriteTimeout，否则会掐断 /api/ws 与
		// /api/agent/ws 长连接（慢写保护已由各 handler 的 SetWriteDeadline 承担）。
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("Moss server v%s 启动，监听 %s", serverVersion, *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，优雅关闭中…")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shCtx); err != nil {
		log.Printf("优雅关闭超时: %v", err)
	}
}
