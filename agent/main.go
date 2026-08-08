// moss-agent：单二进制探针，采集系统指标并通过 WebSocket 上报。
// Windows / Linux / macOS 通用，连接参数一致：--endpoint <服务端地址> --token <token>
package main

import (
	"context"
	"flag"
	"log"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"moss/internal/protocol"
)

// agentVersion 发版时由 -ldflags "-X main.agentVersion=..." 注入（见 release.yml）。
var agentVersion = "dev"

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

// resolveToken 按优先级取 token：--token > MOSS_TOKEN 环境变量 > --token-file。
// 安装脚本改用文件/环境变量，避免 token 出现在进程命令行（ps / schtasks /Query）。
func resolveToken(token, tokenFile string) string {
	if token != "" {
		return token
	}
	if v := os.Getenv("MOSS_TOKEN"); v != "" {
		return v
	}
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			log.Fatalf("读取 token 文件失败: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	return ""
}

func main() {
	endpoint := flag.String("endpoint", "", "服务端地址，如 https://moss.example.com")
	token := flag.String("token", "", "服务器 token（明文，不推荐；优先用 --token-file 或 MOSS_TOKEN 环境变量）")
	tokenFile := flag.String("token-file", "", "从文件读取 token（推荐，文件权限设 600）")
	allowExec := flag.Bool("allow-exec", false, "允许服务端下发命令在本机执行（默认关闭，也可用 MOSS_ALLOW_EXEC=1 开启）")
	guard := flag.Bool("rollback-guard", false, "内部使用：升级回滚守护，由自升级流程拉起，勿手动运行")
	guardTarget := flag.String("guard-target", "", "内部使用：回滚守护的目标二进制路径")
	guardGrace := flag.Int("guard-grace", protocol.UpgradeDefaultGrace, "内部使用：回滚守护等待新版本连回的秒数")
	flag.Parse()

	// 守护模式必须在 token 校验之前分流：它只负责重启与回滚，不连 server，
	// 也就没有 token 可用——升级中的机器此刻正处在两个版本之间。
	if *guard {
		if *guardTarget == "" {
			log.Fatal("--rollback-guard 需要 --guard-target")
		}
		os.Exit(runRollbackGuard(*guardTarget, *guardGrace))
	}

	tok := resolveToken(*token, *tokenFile)
	if *endpoint == "" || tok == "" {
		log.Fatal("用法: moss-agent --endpoint <服务端地址> (--token-file <文件> | --token <token>)")
	}

	target, err := wsURL(*endpoint, tok)
	if err != nil {
		log.Fatalf("地址解析失败: %v", err)
	}

	// 远程执行默认关闭：装了 agent 不等于接受被远程操作，需显式开启。
	allow := *allowExec || os.Getenv("MOSS_ALLOW_EXEC") == "1"
	if allow {
		log.Printf("远程执行已开启")
	}
	// runner 在重连间保持存活：幂等记录必须跨连接有效，
	// 否则服务端在重连后重发同一任务会造成二次执行（例如重复跑一遍部署）。
	runner := newExecRunner(allow)
	// upgrader 同样跨重连存活：幂等记录若随连接重置，server 重发同一升级任务
	// 会导致二次下载与二次替换。
	up := newUpgrader()

	const baseBackoff = 3 * time.Second
	backoff := baseBackoff
	for {
		start := time.Now()
		if err := runOnce(target, runner, up); err != nil {
			log.Printf("连接中断: %v，%v 后重连", err, backoff)
		}
		// 维持过一段在线再断（如服务端重启/部署）属正常掉线 → 退避归零，下次快速重连；
		// 只有持续连不上（拨号失败/连上即断）才线性退避（每次 +3s）到 60s，避免雪崩。
		if time.Since(start) >= 30*time.Second {
			backoff = baseBackoff
		}
		time.Sleep(backoff)
		if backoff < 60*time.Second {
			backoff += baseBackoff
		}
	}
}

func runOnce(target string, runner *execRunner, up *upgrader) error {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(target, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("已连接服务端")
	// 刷新连接标记：升级回滚守护靠它判断新版本是否真的活过来了。
	// 必须放在真正连上之后——拨号成功即代表 token 有效、server 可达。
	markConnected()
	resetNetRates() // 重连后重置网速基准，避免断线期 dt 产生虚高毛刺

	c := &client{conn: conn}

	// 注册主机信息。collectInfo 只读 GeoIP 缓存，不外呼，所以这条消息立刻就能发出去。
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

	// 公网 IP / 国家码在后台补：见 refreshGeoAndReregister。
	go refreshGeoAndReregister(ctx, c, info)

	// 容量 1 + replaceLatest = 「只保留最新一份配置」。见 replaceLatest 的注释。
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
			switch msg.Type {
			case "config":
				// 两个 channel 容量都是 1，语义是「只保留最新一份配置」。
				// 原来的写法是「塞不进去就丢弃本次」——丢的是新值、留的是旧值，
				// 正好反了：连续改两次配置或重连补发时，第二次的变更会被静默吞掉，
				// 探测任务与上报间隔停在过时状态，且没有任何日志。
				// 正确做法是先把陈旧的那份排空，再放新的进去。
				replaceLatest(intervalCh, msg.Interval)
				replaceLatest(tasksCh, msg.Tasks)
			case "exec":
				if msg.Exec != nil {
					// 异步受理：执行可能持续数分钟，不能阻塞本读取循环，
					// 否则期间的心跳与配置下发全部停摆。
					runner.Handle(c, *msg.Exec)
				}
			case "write":
				if msg.Write != nil {
					runner.HandleWrite(c, *msg.Write)
				}
			case "upgrade":
				// 刻意不检查 allow：--allow-exec 管的是「允许远程执行任意命令」，
				// 而升级只能升到 server 当前版本、从固定地址装带校验和的官方二进制，
				// 属于产品自身的受限维护动作，不是把机器交出去。
				//
				// ⚠️ 这条成立的前提是「特权保持受限」。一旦升级支持指定版本
				// （回滚到旧版）或自定义下载源，它就变成了任意代码分发通道，
				// 那时必须补上 allow 检查——理由见 docs/ai-ops.md 的决策表。
				if msg.Upgrade != nil {
					up.Handle(c, *msg.Upgrade)
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

// refreshGeoAndReregister 在后台取公网 IP / 国家码，拿到新值后补发一条 register。
//
// 为什么是「先注册再补发」而不是「注册前先查」：publicNet 的 v4 分支要依次试
// ip-api → ip.sb → ipinfo，每个 5 秒超时，全失败时顺序耗时可达 15 秒。
// 那 15 秒卡在 register 之前，agent 在面板上就是「连上了却迟迟不注册」，
// 而 ip-api 免费版只有 http、境内访问本就不稳，这是常见路径不是边缘路径。
// 30 分钟 TTL 缓存救不了冷启动：首次安装、升级重启、崩溃重启都从空缓存开始。
//
// 补发是安全的：server 的 register 分支是一条纯 UPDATE，重复收到只是再写一次
// 同样的值；升级成功与否在第一条 register 就已判定（版本号不依赖 GeoIP），
// 补发这条落在终态上不会改变结论。
//
// 取不到就不发——没有新信息就不必打扰 server；缓存已被这次外呼填上，
// 下次重连的第一条 register 自然会带上。
func refreshGeoAndReregister(ctx context.Context, c sender, sent protocol.AgentInfo) {
	ipv4, ipv6, country := refreshPublicNet()
	if ipv4 == sent.IP && ipv6 == sent.IPv6 && country == sent.CountryCode {
		return
	}
	// 外呼期间连接可能早就断了。此时不发：这条 register 属于那条已死的连接，
	// 新连接自己会带着刚填好的缓存重新注册。
	select {
	case <-ctx.Done():
		return
	default:
	}
	info := collectInfo()
	if err := c.send(protocol.AgentMsg{Type: "register", Info: &info}); err != nil {
		// 发失败无需重试，也不该把连接判死：真断了的话读取循环会先一步收尾。
		log.Printf("补发公网信息失败: %v", err)
		return
	}
	log.Printf("公网信息已补报: ip=%s ipv6=%s cc=%s", info.IP, info.IPv6, info.CountryCode)
}

// replaceLatest 把 v 放进容量为 1 的 channel，必要时先丢掉里面陈旧的那一份。
//
// 这是「只保留最新值」的正确写法。此前用的是非阻塞发送 + default，
// 那个写法在 channel 已满时丢弃的是**新**值、保留的是旧值——语义正好相反，
// 结果是最新配置静默不生效，且没有任何日志。
//
// 只有读取循环这一个生产者，所以最多循环两次即返回：要么直接发进去，
// 要么排掉旧值后发进去。
func replaceLatest[T any](ch chan T, v T) {
	for {
		select {
		case ch <- v:
			return
		default:
		}
		select {
		case <-ch: // 丢掉陈旧的那一份，回头再发
		default: // 已被消费者取走，直接重试
		}
	}
}
