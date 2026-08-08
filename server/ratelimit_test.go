package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mustNets 把若干 CIDR/裸 IP 解析为可信代理名单，便于在表里写。
func mustNets(t *testing.T, items ...string) []*net.IPNet {
	t.Helper()
	var nets []*net.IPNet
	for _, it := range items {
		if _, n, err := net.ParseCIDR(it); err == nil {
			nets = append(nets, n)
			continue
		}
		ip := net.ParseIP(it)
		if ip == nil {
			t.Fatalf("无法解析可信代理项 %q", it)
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return nets
}

func TestRealIP(t *testing.T) {
	const (
		edgeIP   = "203.0.113.10" // 东京/泰国边缘节点公网 IP
		originIP = "198.51.100.5" // 美西回源 nginx 公网 IP（直连 Moss 的对端）
		client   = "192.0.2.77"   // 真实客户端
		forged   = "9.9.9.9"      // 客户端伪造塞进 XFF 最左的值
	)

	tests := []struct {
		name       string
		trustProxy bool
		trusted    []*net.IPNet
		remoteAddr string
		xff        string // 空串表示不带 X-Forwarded-For 头
		want       string
	}{
		{
			name:       "不信任反代则忽略XFF回退RemoteAddr",
			trustProxy: false,
			remoteAddr: originIP + ":40000",
			xff:        forged + ", " + client,
			want:       originIP,
		},
		{
			name:       "信任反代无名单取最右段而非伪造的最左",
			trustProxy: true,
			remoteAddr: "127.0.0.1:8787",
			xff:        forged + ", " + client, // origin 追加后最右是真实客户端
			want:       client,
		},
		{
			name:       "两层拓扑名单含边缘IP从右跳过取真实客户端",
			trustProxy: true,
			trusted:    mustNets(t, edgeIP),
			remoteAddr: "127.0.0.1:8787",
			xff:        forged + ", " + client + ", " + edgeIP,
			want:       client,
		},
		{
			name:       "单层直连origin名单含边缘IP仍取真实客户端",
			trustProxy: true,
			trusted:    mustNets(t, edgeIP),
			remoteAddr: "127.0.0.1:8787",
			xff:        forged + ", " + client,
			want:       client,
		},
		{
			name:       "环回与可信代理混合从右跳过取真实客户端",
			trustProxy: true,
			trusted:    mustNets(t, edgeIP, originIP),
			remoteAddr: "127.0.0.1:8787",
			xff:        forged + ", " + client + ", " + edgeIP + ", 127.0.0.1",
			want:       client,
		},
		{
			name:       "CIDR名单匹配边缘网段",
			trustProxy: true,
			trusted:    mustNets(t, "203.0.113.0/24"),
			remoteAddr: "127.0.0.1:8787",
			xff:        forged + ", " + client + ", " + edgeIP,
			want:       client,
		},
		{
			name:       "全为可信代理回退最左段",
			trustProxy: true,
			trusted:    mustNets(t, edgeIP, originIP),
			remoteAddr: "127.0.0.1:8787",
			xff:        edgeIP + ", " + originIP,
			want:       edgeIP,
		},
		{
			name:       "信任反代但无XFF回退RemoteAddr",
			trustProxy: true,
			trusted:    mustNets(t, edgeIP),
			remoteAddr: originIP + ":40000",
			xff:        "",
			want:       originIP,
		},
		{
			name:       "IPv6 RemoteAddr在无XFF时正确去端口",
			trustProxy: false,
			remoteAddr: "[2001:db8::1]:40000",
			xff:        "",
			want:       "2001:db8::1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := http.NewRequest(http.MethodGet, "/api/login", nil)
			if err != nil {
				t.Fatalf("构造请求失败: %v", err)
			}
			r.RemoteAddr = tt.remoteAddr
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := realIP(r, tt.trustProxy, tt.trusted); got != tt.want {
				t.Errorf("realIP() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

// newTestLimiter 直接造实例而不走 newLimiter：后者会起一个 gc 协程，测试不需要。
func newTestLimiter(perMin int) *limiter {
	return &limiter{perMin: perMin, hits: make(map[string]*hit)}
}

// 固定窗口在边界处会放行两倍额度：额度在窗口切换的瞬间整体重置，
// 于是第 59.9 秒打满一份、第 60.1 秒再打满一份，两秒内拿到 2×perMin 次。
// 对 authLimiter（默认 10/min）就是 20 次登录尝试挤在一起。
func TestLimiterSlidingWindowBoundary(t *testing.T) {
	const ip = "192.0.2.1"
	l := newTestLimiter(10)
	base := time.Now()

	for i := 1; i <= 10; i++ {
		if !l.allowAt(ip, base) {
			t.Fatalf("额度内第 %d 次请求不该被拒", i)
		}
	}
	if l.allowAt(ip, base) {
		t.Fatal("超出额度的请求必须被拒")
	}

	// 跨过窗口边界 1 秒：上一窗口 59/60 仍在视野内，最多只该漏进 1 次。
	next := base.Add(rateWindow + time.Second)
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.allowAt(ip, next) {
			allowed++
		}
	}
	if allowed > 1 {
		t.Errorf("跨窗口边界放行了 %d 次，最多只该漏 1 次（固定窗口会放行 10 次，等于双倍额度）", allowed)
	}

	// 静默两格窗口后旧计数完全滑出视野，额度应当完整恢复——
	// 滑窗不能变成「一次超额就永久压制」。
	later := base.Add(3 * rateWindow)
	for i := 1; i <= 10; i++ {
		if !l.allowAt(ip, later) {
			t.Fatalf("旧计数已滑出视野，第 %d 次请求不该被拒", i)
		}
	}
}

// 限流是按 IP 隔离的，一个访客打满不能连累其他人。
func TestLimiterPerIP(t *testing.T) {
	l := newTestLimiter(1)
	now := time.Now()
	if !l.allowAt("192.0.2.1", now) || l.allowAt("192.0.2.1", now) {
		t.Fatal("同一 IP 的额度判定不正确")
	}
	if !l.allowAt("192.0.2.2", now) {
		t.Error("另一个 IP 不该被别人的计数连累")
	}
}

// 全局限流中间件是挂在 http.Server.Handler 上的唯一一道闸门，
// 不在它判据里的路径就是完全无限速。/mcp 不带 /api/ 前缀，早期只判前缀，
// 等于给 API Key 的鉴权入口留了一个没有吞吐闸门的口子。
func TestRateLimitCoversAuthSurface(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	cases := []struct {
		path    string
		limited bool
	}{
		{"/api/site", true},
		{"/api/admin/servers", true},
		{"/mcp", true},         // MCP 鉴权入口：每次请求一次 SHA-256 + 一次索引查询
		{"/install.sh", true},  // 无鉴权公开端点，600/min 的额度不会误伤正常安装
		{"/install.ps1", true}, // 同上
		{"/", false},           // 前端首屏会并发拉多张国旗 SVG，纳入会误伤正常浏览
		{"/assets/app.js", false},
	}
	for _, c := range cases {
		// 每条用例一套全新状态，避免相互干扰；perMin=1 让第二次请求就能看出结论。
		app := &App{globalLimiter: newTestLimiter(1)}
		h := app.apiRateLimit(next)

		var last int
		for i := 0; i < 2; i++ {
			r := httptest.NewRequest(http.MethodGet, c.path, nil)
			r.RemoteAddr = "192.0.2.55:40000"
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			last = w.Code
		}
		if c.limited && last != http.StatusTooManyRequests {
			t.Errorf("%s 应被全局限流覆盖，第二次请求返回 %d", c.path, last)
		}
		if !c.limited && last != http.StatusOK {
			t.Errorf("%s 不该被限流，第二次请求返回 %d", c.path, last)
		}
	}
}

func TestParseTrustedProxies(t *testing.T) {
	nets := parseTrustedProxies("203.0.113.10, 198.51.100.0/24 , , bogus, 2001:db8::1")
	// 裸 IP + CIDR + IPv6 共 3 项有效，空项与 bogus 被丢弃。
	if len(nets) != 3 {
		t.Fatalf("解析得到 %d 个网段，期望 3", len(nets))
	}
	cases := []struct {
		ip   string
		want bool
	}{
		{"203.0.113.10", true},  // 裸 IPv4 → /32 命中
		{"203.0.113.11", false}, // /32 之外不命中
		{"198.51.100.200", true},
		{"198.51.101.1", false},
		{"2001:db8::1", true},
		{"2001:db8::2", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := ipInNets(ip, nets); got != c.want {
			t.Errorf("ipInNets(%s) = %v, 期望 %v", c.ip, got, c.want)
		}
	}
}
