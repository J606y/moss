package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// netHTTP 返回强制走指定地址族（tcp4/tcp6）的短超时客户端，
// 用于分别探测公网 IPv4 / IPv6，避免双栈机只拿到默认路由那一个。
func netHTTP(network string) *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

var (
	v4HTTP = netHTTP("tcp4")
	v6HTTP = netHTTP("tcp6")
)

// publicNet 获取本机公网 IPv4 / IPv6 与国家码（ISO alpha-2 小写）。
// IPv4 与国家码走 geo 服务（强制 tcp4）；IPv6 单独走 v6 服务（强制 tcp6），
// 主机无 IPv6 时留空。两条并发，互不拖慢；任一失败不影响其他字段，
// 全失败时 server 端回退到连接来源 IP。
func publicNet() (ipv4, ipv6, country string) {
	var wg sync.WaitGroup
	var country6 string
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, fn := range []func(*http.Client) (string, string){ipAPI, ipSB, ipInfo} {
			if ip, cc := fn(v4HTTP); ip != "" {
				ipv4, country = ip, cc
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		// ip.sb 同端点走 tcp6 可一并拿到 IPv6 与国家码（v6-only 机用作国旗兜底）
		if ip, cc := ipSB(v6HTTP); ip != "" {
			ipv6, country6 = ip, cc
			return
		}
		for _, u := range []string{"https://6.ipw.cn", "https://api6.ipify.org"} {
			if ip := fetchPlainIP(v6HTTP, u); ip != "" {
				ipv6 = ip
				return
			}
		}
	}()
	wg.Wait()
	if country == "" {
		country = country6
	}
	return
}

const geoTTL = 30 * time.Minute

// geoRetryInterval 两次外呼之间的最小间隔，失败后同样适用。
//
// 没有这个节流的话，一台外网不通的机器每次重连都会重跑一遍全部端点：
// 重连退避最短 3 秒，而一轮失败要烧掉约 15 秒和 3 个请求。境内机器连不上
// ip-api / ipinfo 是常态，那就是一台机器持续不断地空转外呼——既打免费接口的
// 配额，也让 agent 长期挂着一个没有意义的协程。
const geoRetryInterval = 5 * time.Minute

var (
	geoMu      sync.Mutex
	geoCache   struct{ ipv4, ipv6, country string }
	geoCacheAt time.Time
	geoTriedAt time.Time // 上一次真正外呼的起始时刻，兼作单飞标记
)

// publicNetCachedOnly 只读缓存，永不外呼、永不阻塞。
//
// register 走这条路径。公网信息对注册来说是锦上添花（server 拿不到会回退到
// 连接来源 IP），却要用最坏 15 秒的外呼去换——那 15 秒里 agent 在面板上是
// 「连上了但没注册」，冷启动、升级重启、崩溃重启后每次都要再经历一遍。
func publicNetCachedOnly() (ipv4, ipv6, country string) {
	geoMu.Lock()
	defer geoMu.Unlock()
	if geoCacheAt.IsZero() || time.Since(geoCacheAt) >= geoTTL {
		return "", "", ""
	}
	return geoCache.ipv4, geoCache.ipv6, geoCache.country
}

// refreshPublicNet 外呼刷新缓存并返回最新结果，最坏阻塞约 15 秒，只能在后台调用。
//
// 缓存仍然有效、或距上次外呼不足 geoRetryInterval 时直接返回现有缓存不外呼，
// 后者同时起到单飞作用：重连风暴中后来的调用不会再叠一轮外呼上去。
// 仅在成功拿到 IPv4 时刷新缓存，避免把一次全失败缓存 30 分钟。
func refreshPublicNet() (ipv4, ipv6, country string) {
	geoMu.Lock()
	fresh := !geoCacheAt.IsZero() && time.Since(geoCacheAt) < geoTTL && geoCache.ipv4 != ""
	throttled := !geoTriedAt.IsZero() && time.Since(geoTriedAt) < geoRetryInterval
	if fresh || throttled {
		c := geoCache
		geoMu.Unlock()
		return c.ipv4, c.ipv6, c.country
	}
	geoTriedAt = time.Now() // 先占位再外呼，否则并发的第二个调用会同时穿过节流
	geoMu.Unlock()

	ipv4, ipv6, country = publicNet()
	if ipv4 != "" {
		geoMu.Lock()
		geoCache.ipv4, geoCache.ipv6, geoCache.country = ipv4, ipv6, country
		geoCacheAt = time.Now()
		geoMu.Unlock()
	}
	return
}

// fetchPlainIP 请求只回显纯文本 IP 的端点，校验为合法 IP 后返回。
func fetchPlainIP(c *http.Client, url string) string {
	resp, err := c.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return ""
	}
	return ip
}

// ipAPI 一次返回 IP + 国家码（免费版仅 http，且只有 IPv4）。
func ipAPI(c *http.Client) (string, string) {
	resp, err := c.Get("http://ip-api.com/json/?fields=status,countryCode,query")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var r struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		Query       string `json:"query"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil || r.Status != "success" {
		return "", ""
	}
	return r.Query, strings.ToLower(r.CountryCode)
}

func ipSB(c *http.Client) (string, string) {
	resp, err := c.Get("https://api.ip.sb/geoip")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var r struct {
		IP          string `json:"ip"`
		CountryCode string `json:"country_code"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return "", ""
	}
	return r.IP, strings.ToLower(r.CountryCode)
}

func ipInfo(c *http.Client) (string, string) {
	resp, err := c.Get("https://ipinfo.io/json")
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var r struct {
		IP      string `json:"ip"`
		Country string `json:"country"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return "", ""
	}
	return r.IP, strings.ToLower(r.Country)
}
