package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"time"

	probing "github.com/prometheus-community/pro-bing"
	"moss/internal/protocol"
)

// runProbes 管理探测任务调度：收到新任务列表时整组重启。
func runProbes(ctx context.Context, c *client, tasksCh <-chan []protocol.PingTask) {
	var cancel context.CancelFunc
	for {
		select {
		case <-ctx.Done():
			if cancel != nil {
				cancel()
			}
			return
		case tasks := <-tasksCh:
			if cancel != nil {
				cancel()
			}
			var sub context.Context
			sub, cancel = context.WithCancel(ctx)
			log.Printf("收到 %d 个探测任务", len(tasks))
			for _, t := range tasks {
				go probeLoop(sub, c, t)
			}
		}
	}
}

func probeLoop(ctx context.Context, c *client, t protocol.PingTask) {
	interval := time.Duration(t.Interval) * time.Second
	if interval < 10*time.Second {
		interval = time.Minute
	}
	run := func() {
		ms := probe(t)
		c.send(protocol.AgentMsg{Type: "ping", TaskID: t.ID, Ms: ms})
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// probe 执行一次探测，返回毫秒延迟，失败返回 -1。
func probe(t protocol.PingTask) int {
	switch t.Type {
	case "icmp":
		return probeICMP(t.Target)
	case "tcp":
		return probeTCP(t.Target)
	case "http":
		return probeHTTP(t.Target)
	}
	return -1
}

func probeICMP(target string) int {
	p, err := probing.NewPinger(target)
	if err != nil {
		return -1
	}
	// Windows 必须用特权模式；Linux/macOS 上 root 用特权 raw socket，普通用户走 UDP ping
	p.SetPrivileged(runtime.GOOS == "windows" || os.Geteuid() == 0)
	p.Count = 1
	p.Timeout = 5 * time.Second
	if err := p.Run(); err != nil {
		return -1
	}
	st := p.Statistics()
	if st.PacketsRecv == 0 {
		return -1
	}
	return clampMs(st.AvgRtt)
}

func probeTCP(target string) int {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return -1
	}
	conn.Close()
	return clampMs(time.Since(start))
}

func probeHTTP(target string) int {
	client := &http.Client{
		Timeout: 8 * time.Second,
		// 只测首跳，不跟随 302/重定向（防止下发 target 借跳转探测非预期地址）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	start := time.Now()
	resp, err := client.Get(target)
	if err != nil {
		return -1
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return -1
	}
	return clampMs(time.Since(start))
}

func clampMs(d time.Duration) int {
	ms := int(d.Milliseconds())
	if ms < 1 {
		ms = 1
	}
	return ms
}
