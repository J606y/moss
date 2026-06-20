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
