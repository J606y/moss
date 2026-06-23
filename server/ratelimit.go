package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// realIP 取用于限流与日志的「真实访客 IP」。
//
// 各层 nginx 用 $proxy_add_x_forwarded_for「追加」转发，到 Moss 时 XFF 形如：
//
//	[客户端可伪造段...], 真实客户端, 边缘IP   ← 经边缘节点（多层）
//	[客户端可伪造段...], 真实客户端          ← 直连回源（单层）
//
// 跳数可变（用户可能经边缘也可能直连回源），所以「取最左」可被客户端伪造、
// 「取最右」会把边缘后所有访客并成一个 IP，都不对。正确做法是用可信代理名单
// （--trusted-proxies，列出自家边缘/回源节点公网 IP）从 XFF **最右往左**遍历，
// 跳过属于名单或环回的地址，返回第一个非可信地址 = 真实客户端。攻击者只能控制
// 名单左侧的伪造段，够不到这个位置，故不可伪造。
//
// 未开 --trust-proxy → 忽略 XFF，回退 RemoteAddr 的 host（安全默认，防直连伪造头）。
// 开了但名单为空 → 取 XFF 最右段（单跳安全默认：那是与 Moss 直接握手的可信反代
// 所追加的对端 IP，客户端伪造不到）。
func realIP(r *http.Request, trustProxy bool, trustedProxies []*net.IPNet) string {
	host := hostOnly(r.RemoteAddr)
	if !trustProxy {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	var parts []string
	for _, p := range strings.Split(xff, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return host
	}
	if len(trustedProxies) == 0 {
		return parts[len(parts)-1] // 最右段：直接对端，不可伪造
	}
	for i := len(parts) - 1; i >= 0; i-- {
		ip := net.ParseIP(parts[i])
		if ip == nil || ip.IsLoopback() || ipInNets(ip, trustedProxies) {
			continue // 解析失败 / 环回 / 可信代理 → 继续往左找
		}
		return parts[i] // 第一个非可信地址即真实客户端
	}
	return parts[0] // 全是可信代理：回退最左段
}

// hostOnly 去掉 host:port 中的端口，返回纯 host（IPv6 安全）；无端口时原样返回。
func hostOnly(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ipInNets 判断 ip 是否落在任一网段内。
func ipInNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// limiter 是按 IP 计数的固定 1 分钟窗口限流器（单进程内存版）。
// 多实例/多进程部署需改用共享存储（如 Redis）替换这里的本地 map。
type limiter struct {
	perMin int
	mu     sync.Mutex
	hits   map[string]*hit
}

type hit struct {
	count       int
	windowStart time.Time
}

// newLimiter 创建限流器；perMin <= 0 返回 nil，表示该层关闭。
func newLimiter(perMin int) *limiter {
	if perMin <= 0 {
		return nil
	}
	l := &limiter{perMin: perMin, hits: make(map[string]*hit)}
	go l.gcLoop()
	return l
}

// allow 在当前 1 分钟窗口内未超额则计数并放行，超额返回 false。
func (l *limiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hits[ip]
	if h == nil || now.Sub(h.windowStart) >= time.Minute {
		l.hits[ip] = &hit{count: 1, windowStart: now}
		return true
	}
	if h.count >= l.perMin {
		return false
	}
	h.count++
	return true
}

// gcLoop 周期清理过期窗口条目，防止 map 随访客 IP 无限增长。
func (l *limiter) gcLoop() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		l.mu.Lock()
		for ip, h := range l.hits {
			if now.Sub(h.windowStart) >= time.Minute {
				delete(l.hits, ip)
			}
		}
		l.mu.Unlock()
	}
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeErr(w, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
}

// limit 用指定限流器包住 handler，超额返回 429；lim 为 nil 时（该层关闭）原样放行。
func (s *App) limit(lim *limiter, next http.Handler) http.Handler {
	if lim == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lim.allow(realIP(r, s.trustProxy, s.trustedProxies)) {
			tooMany(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiRateLimit 仅对 /api/ 路径施加全局限流；静态资源（含首屏多张国旗 SVG 的并发突发）
// 一律放行，避免误伤正常浏览。/api 才是要保护的攻击面。
func (s *App) apiRateLimit(next http.Handler) http.Handler {
	if s.globalLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.globalLimiter.allow(realIP(r, s.trustProxy, s.trustedProxies)) {
			tooMany(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// envInt 读取整型环境变量，缺省或非法时回退 fallback。
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("环境变量 %s=%q 非法，改用默认值 %d", key, v, fallback)
		return fallback
	}
	return n
}
