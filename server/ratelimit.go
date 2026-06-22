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
// 仅当 --trust-proxy 开启时才信任反代头：取 X-Forwarded-For 的最左段。
// 部署拓扑为 访客 → 边缘 nginx → 回源 nginx → Moss(仅听 127.0.0.1)；边缘 nginx
// 会覆盖 XFF 为真实客户端 IP(丢弃客户端伪造值)，回源再追加它看到的边缘 IP，
// 因此 Moss 收到的 XFF = 真实客户端, 边缘…，最左段即真实访客。
// 又因 Moss 只监听回环、外部无法绕过边缘直连，所以此处取最左 XFF 不会被伪造绕过。
//
// 刻意不用 X-Real-IP：多层反代下回源处的 X-Real-IP=$remote_addr 等于「边缘 IP」而非访客。
// 未开 --trust-proxy 时回退 r.RemoteAddr 的 host，防止直连伪造头部。
func realIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				xff = xff[:i] // 最左段
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
		if !lim.allow(realIP(r, s.trustProxy)) {
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
		if strings.HasPrefix(r.URL.Path, "/api/") && !s.globalLimiter.allow(realIP(r, s.trustProxy)) {
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
