package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// geoHTTP 单独的短超时客户端，避免公网查询拖慢注册。
var geoHTTP = &http.Client{Timeout: 5 * time.Second}

// publicNet 获取本机公网 IP 与国家码（ISO alpha-2 小写）。
// 依次尝试多个免费服务，任一成功即返回；全部失败返回空串，
// server 端会回退到连接来源 IP，国旗则保持后台手动设置。
func publicNet() (ip, country string) {
	for _, fn := range []func() (string, string){tryIPAPI, trySB, tryIPInfo} {
		if ip, country = fn(); ip != "" {
			return ip, country
		}
	}
	return "", ""
}

// tryIPAPI 一次返回 IP + 国家码（免费版仅 http）。
func tryIPAPI() (string, string) {
	resp, err := geoHTTP.Get("http://ip-api.com/json/?fields=status,countryCode,query")
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

func trySB() (string, string) {
	resp, err := geoHTTP.Get("https://api.ip.sb/geoip")
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

func tryIPInfo() (string, string) {
	resp, err := geoHTTP.Get("https://ipinfo.io/json")
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
