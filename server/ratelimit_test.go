package main

import (
	"net"
	"net/http"
	"testing"
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
