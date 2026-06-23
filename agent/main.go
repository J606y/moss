// moss-agent：单二进制探针，采集系统指标并通过 WebSocket 上报。
// Windows / Linux / macOS 通用，连接参数一致：--endpoint <服务端地址> --token <token>
package main

import (
	"context"
	"flag"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

const agentVersion = "0.4.6"

func wsURL(endpoint, token string) (string, error) {
	endpoint = strings.TrimRight(endpoint, "/")
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = "/api/agent/ws"
	u.RawQuery = "token=" + url.QueryEscape(token)
	return u.String(), nil
}

type client struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *client) send(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(v)
}

func main() {
	endpoint := flag.String("endpoint", "", "服务端地址，如 https://moss.example.com")
	token := flag.String("token", "", "服务器 token")
	flag.Parse()
	if *endpoint == "" || *token == "" {
		log.Fatal("用法: moss-agent --endpoint <服务端地址> --token <token>")
	}

	target, err := wsURL(*endpoint, *token)
	if err != nil {
		log.Fatalf("地址解析失败: %v", err)
	}

	const baseBackoff = 3 * time.Second
	backoff := baseBackoff
	for {
		start := time.Now()
		if err := runOnce(target); err != nil {
			log.Printf("连接中断: %v，%v 后重连", err, backoff)
		}
		// 维持过一段在线再断（如服务端重启/部署）属正常掉线 → 退避归零，下次快速重连；
		// 只有持续连不上（拨号失败/连上即断）才指数退避到 60s，避免雪崩。
		if time.Since(start) >= 30*time.Second {
			backoff = baseBackoff
		}
		time.Sleep(backoff)
		if backoff < 60*time.Second {
			backoff += baseBackoff
		}
	}
}

func runOnce(target string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(target, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("已连接服务端")
	resetNetRates() // 重连后重置网速基准，避免断线期 dt 产生虚高毛刺

	c := &client{conn: conn}

	// 注册主机信息
	info := collectInfo()
	if err := c.send(protocol.AgentMsg{Type: "register", Info: &info}); err != nil {
		return err
	}

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		c.mu.Lock()
		defer c.mu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteMessage(websocket.PongMessage, []byte(data))
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	intervalCh := make(chan int, 1)
	tasksCh := make(chan []protocol.PingTask, 1)

	// 读取服务端下发的配置
	go func() {
		defer cancel()
		for {
			var msg protocol.ServerMsg
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))
			if msg.Type == "config" {
				select {
				case intervalCh <- msg.Interval:
				default:
				}
				select {
				case tasksCh <- msg.Tasks:
				default:
				}
			}
		}
	}()

	go runProbes(ctx, c, tasksCh)

	// 上报循环
	interval := 2 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case n := <-intervalCh:
			if n >= 1 && time.Duration(n)*time.Second != interval {
				interval = time.Duration(n) * time.Second
				ticker.Reset(interval)
				log.Printf("上报间隔更新为 %v", interval)
			}
		case <-ticker.C:
			stats, uptime := collectStats()
			if err := c.send(protocol.AgentMsg{Type: "report", Stats: &stats, UptimeSec: uptime}); err != nil {
				return err
			}
		}
	}
}
