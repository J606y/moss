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

// rateWindow 限流窗口长度。perMin 这个名字就是按它定义的，改这里要一起改字段名。
const rateWindow = time.Minute

// limiter 是按 IP 计数的滑动窗口限流器（单进程内存版）。
// 多实例/多进程部署需改用共享存储（如 Redis）替换这里的本地 map。
//
// 用「当前窗口 + 上一窗口按重叠比例加权」的两段计数近似滑窗，而不是固定窗口：
// 固定窗口整体重置，第 59.9 秒可以打满一整份额度、第 60.1 秒再打满一份，
// 约 2 秒内拿到 2×perMin 次。对 authLimiter（默认 10/min）就是 20 次登录尝试
// 挤在一起，退避形同虚设。
//
// 也不逐条记请求时间戳：那是精确滑窗，但内存随 perMin 线性增长（600/min ×
// 每个访客 IP），无鉴权的公开端点上足以被打爆。两段计数是 O(1)/IP，
// 边界误差最多一格窗口的加权残留，对限流这个用途足够。
type limiter struct {
	perMin int
	mu     sync.Mutex
	hits   map[string]*hit
}

type hit struct {
	prev        int       // 上一窗口的计数，按重叠比例折算进当前判定
	count       int       // 当前窗口已计数
	windowStart time.Time // 当前窗口起点
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

// allow 在滑动窗口内未超额则计数并放行，超额返回 false。
func (l *limiter) allow(ip string) bool { return l.allowAt(ip, time.Now()) }

// allowAt 是 allow 的可注入时钟版本。窗口边界处的行为只能靠构造时序来验证，
// 用真实时钟意味着测试要真等一分钟——那种测试没人会跑。
func (l *limiter) allowAt(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	h := l.hits[ip]
	if h == nil {
		l.hits[ip] = &hit{count: 1, windowStart: now}
		return true
	}
	l.roll(h, now)
	// 上一窗口尚未滑出视野的比例：刚跨过边界时几乎整份旧计数仍然算数，
	// 越接近下一个边界残留越少。固定窗口相当于把这一项直接当成 0。
	overlap := float64(rateWindow-now.Sub(h.windowStart)) / float64(rateWindow)
	if float64(h.prev)*overlap+float64(h.count) >= float64(l.perMin) {
		return false
	}
	h.count++
	return true
}

// roll 把窗口推进到 now 所属的那一格，并决定旧计数如何结转。需在持有 l.mu 时调用。
func (l *limiter) roll(h *hit, now time.Time) {
	switch elapsed := now.Sub(h.windowStart); {
	case elapsed >= 2*rateWindow:
		// 隔了两格以上，旧计数已完全滑出视野，等同全新访客。
		h.prev, h.count, h.windowStart = 0, 0, now
	case elapsed >= rateWindow:
		// 只跨了一格：当前窗口降级为「上一窗口」继续按比例参与判定。
		// 这一步正是固定窗口漏掉的——直接丢弃旧计数，边界处就会放行两倍额度。
		h.prev, h.count = h.count, 0
		h.windowStart = h.windowStart.Add(rateWindow)
	}
}

// gcLoop 周期清理过期窗口条目，防止 map 随访客 IP 无限增长。
//
// 阈值是两格窗口而不是一格：一格之内的条目其 prev 仍要参与判定，提前删掉
// 等于白送半份额度，滑窗就退化回固定窗口了。
func (l *limiter) gcLoop() {
	for range time.Tick(5 * time.Minute) {
		now := time.Now()
		l.mu.Lock()
		for ip, h := range l.hits {
			if now.Sub(h.windowStart) >= 2*rateWindow {
				delete(l.hits, ip)
			}
		}
		l.mu.Unlock()
	}
}

func tooMany(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeErr(w, errRateLimited)
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

// rateLimited 判断该路径是否纳入全局限流。
//
// 这是挂在 http.Server.Handler 上的唯一一道全局闸门，凡是不在这里的路径就是
// **完全无限速**的。因此判据不能只写 /api/ 前缀：
//
//   - /mcp 注册在根路径下（main.go），不带 /api/ 前缀。它是 API Key 的鉴权入口，
//     每打一次就换来一次 SHA-256 加一次 api_keys 索引查询；持合法 Key 者还能
//     无限速提交 exec。Key 有 185 bit 熵，爆破本就不成立，问题是这条鉴权路径上
//     一个吞吐闸门都没有。
//   - 安装脚本只是内存里的静态字节，危害远小于上面两条，但它们同样是无鉴权的
//     公开端点，而默认额度 600/min 远高于任何正常用法（装一台机器只 curl 一次），
//     纳入不会误伤，却能把「无限速公开端点」这一类彻底消掉。
//
// 前端静态资源仍然放行：首屏会并发拉多张国旗 SVG，纳入会误伤正常浏览。
func rateLimited(path string) bool {
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	return path == "/mcp" || path == "/install.sh" || path == "/install.ps1"
}

// apiRateLimit 对鉴权面路径施加全局限流，判据见 rateLimited。
func (s *App) apiRateLimit(next http.Handler) http.Handler {
	if s.globalLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rateLimited(r.URL.Path) && !s.globalLimiter.allow(realIP(r, s.trustProxy, s.trustedProxies)) {
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
