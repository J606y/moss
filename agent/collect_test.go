package main

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v4/load"
	gnet "github.com/shirou/gopsutil/v4/net"
)

// lockedBuf 带锁的日志缓冲。
//
// log 包内部会串行化写入，但用例读取缓冲时不受那把锁保护——而 agent 里有若干
// 后台协程（执行输出回传、磁盘采集）可能在任意时刻打日志，不加锁就是数据竞争。
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// captureLog 把标准 log 的输出接到内存，返回读取已写内容的函数。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var lb lockedBuf
	prev := log.Writer()
	log.SetOutput(&lb)
	t.Cleanup(func() { log.SetOutput(prev) })
	return lb.String
}

// resetCollectWarn 清空限流状态，避免用例之间互相压制日志。
func resetCollectWarn(t *testing.T) {
	t.Helper()
	clear := func() {
		collectWarnMu.Lock()
		collectWarnAt = map[string]time.Time{}
		collectWarnMu.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// TestTitleFirstRuneKeepsValidUTF8 首字符大写不能按字节切。
//
// 旧写法 strings.ToUpper(s[:1]) + s[1:] 是按**字节**切：平台名首字符是非 ASCII 时
// （中文发行版名、本地化的版本串），切点落在多字节 UTF-8 序列中间，产出非法字节；
// encoding/json 会把它静默换成 U+FFFD 落库，界面上就是一个永远修不好的乱码。
func TestTitleFirstRuneKeepsValidUTF8(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"debian 12", "Debian 12"},
		{"Windows Server 2022", "Windows Server 2022"},
		{"ubuntu", "Ubuntu"},
		{"银河麒麟 V10", "银河麒麟 V10"},   // 首字符是 3 字节，按字节切必然切坏
		{"überlinux", "Überlinux"}, // 首字符是 2 字节
	}
	for _, c := range cases {
		got := titleFirstRune(c.in)
		if !utf8.ValidString(got) {
			t.Errorf("titleFirstRune(%q) 产出了非法 UTF-8: %q", c.in, got)
			continue
		}
		if got != c.want {
			t.Errorf("titleFirstRune(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	// 输入本身就不合法时不能越改越糟
	bad := "\xff\xfe abc"
	if got := titleFirstRune(bad); got != bad {
		t.Errorf("输入已是非法 UTF-8，应原样返回，实际 %q", got)
	}
}

// TestCollectStatsWarnsWhenMetricUnavailable 采集失败不能悄无声息。
//
// 协议里这些字段是裸数值、没有「不可用」的表示法，结构体又是与 server 的共享契约
// 不能改。于是采集失败时上报的 0 和真实的空闲值一模一样：权限不足、/proc 被容器
// 屏蔽的机器会长期显示成「CPU 0%、0 个连接、0 个进程」，还会被告警规则采信。
// 日志是这条信息唯一的出口。
func TestCollectStatsWarnsWhenMetricUnavailable(t *testing.T) {
	resetCollectWarn(t)
	logs := captureLog(t)

	prevCPU, prevPids, prevLoad, prevConns := cpuPercent, processPids, loadAvg, netConns
	t.Cleanup(func() {
		cpuPercent, processPids, loadAvg, netConns = prevCPU, prevPids, prevLoad, prevConns
	})

	boom := errors.New("permission denied")
	cpuPercent = func(time.Duration, bool) ([]float64, error) { return nil, boom }
	processPids = func() ([]int32, error) { return nil, boom }
	loadAvg = func() (*load.AvgStat, error) { return nil, boom }
	netConns = func(string) ([]gnet.ConnectionStat, error) { return nil, boom }

	s, _ := collectStats()

	// 字段仍然是 0——这是协议决定的，改不了，所以才必须有日志
	if s.CPU != 0 || s.TCP != 0 || s.Processes != 0 || s.Load1 != 0 {
		t.Fatalf("采集失败时字段应保持零值，实际 %+v", s)
	}

	out := logs()
	for _, kind := range []string{"CPU 使用率", "进程数", "负载", "tcp连接数"} {
		if !strings.Contains(out, kind) {
			t.Errorf("%s 采集失败却没有任何日志，运维无从发现这台机器的指标一直取不到；实际日志:\n%s", kind, out)
		}
	}
}

// TestCollectStatsWarnsOnEmptyCPUSample cpu.Percent 返回空切片同样是取不到，
// 不能因为 err 是 nil 就当成「CPU 空闲」。
func TestCollectStatsWarnsOnEmptyCPUSample(t *testing.T) {
	resetCollectWarn(t)
	logs := captureLog(t)

	prev := cpuPercent
	t.Cleanup(func() { cpuPercent = prev })
	cpuPercent = func(time.Duration, bool) ([]float64, error) { return nil, nil }

	collectStats()
	if !strings.Contains(logs(), "CPU 使用率") {
		t.Fatalf("空采样结果也应记日志，实际日志:\n%s", logs())
	}
}

// TestWarnCollectRateLimited 日志必须限流。
//
// 采集失败几乎都是持续性的，而上报是每 2 秒一拍：不限流的话一天刷出 4 万条
// 相同的日志，把 journal 撑爆并淹掉真正有价值的记录——那等于换了一种方式丢信息。
func TestWarnCollectRateLimited(t *testing.T) {
	resetCollectWarn(t)
	logs := captureLog(t)
	boom := errors.New("boom")

	for i := 0; i < 50; i++ {
		warnCollect("CPU 使用率", boom)
	}
	if n := strings.Count(logs(), "CPU 使用率"); n != 1 {
		t.Fatalf("同一类错误连刷 50 次只应记 1 条，实际 %d 条", n)
	}

	// 不同类别互不压制：否则第一个坏掉的指标会把其余的全部盖住
	warnCollect("负载", boom)
	if !strings.Contains(logs(), "负载") {
		t.Fatal("不同类别的采集失败不应被彼此的限流窗口压制")
	}

	// 过了窗口要能重新记录，否则一台机器的故障只会在启动时说一次
	collectWarnMu.Lock()
	collectWarnAt["CPU 使用率"] = time.Now().Add(-collectWarnInterval - time.Second)
	collectWarnMu.Unlock()
	warnCollect("CPU 使用率", boom)
	if n := strings.Count(logs(), "CPU 使用率"); n != 2 {
		t.Fatalf("超过限流窗口后应再记 1 条，实际累计 %d 条", n)
	}
}

// TestIsVirtualNic 网速口径。
//
// 之前用 IOCounters(false) 让 gopsutil 把所有接口求和，容器出网流量在 eth0 与
// veth 上各算一次、本机回环也被算成「网速」——装了 Docker 的机器必现。
// 指标算错等于监控没用，所以这条要有用例守着。
func TestIsVirtualNic(t *testing.T) {
	virtual := []string{
		"lo", "lo0",
		"docker0", "docker_gwbridge",
		"br-1a2b3c4d5e6f",
		"veth1a2b3c", "vethpair0",
		"virbr0", "virbr0-nic",
		"tun0", "tap0", "wg0", "zt5u4uptmyq",
		"cni0", "flannel.1", "cali1234", "kube-ipvs0",
		"Loopback Pseudo-Interface 1", // Windows
		"vEthernet (Default Switch)",  // Windows Hyper-V
	}
	for _, n := range virtual {
		if !isVirtualNic(n) {
			t.Errorf("虚拟接口应被排除在网速统计外: %q", n)
		}
	}

	physical := []string{
		"eth0", "eth1",
		"ens5", "enp0s3", "eno1",
		"wlan0", "wlp2s0",
		"em1", "bond0",
		"以太网", // Windows 中文系统的默认网卡名
		"Ethernet", "Wi-Fi",
	}
	for _, n := range physical {
		if isVirtualNic(n) {
			t.Errorf("物理接口不应被排除，否则网速会算少甚至变 0: %q", n)
		}
	}
}

// TestReplaceLatestKeepsNewest 配置下发「只保留最新值」。
//
// 原来的写法是非阻塞发送 + default——channel 满时丢弃的是**新**值、
// 保留的是旧值，语义正好反了：连续改两次配置时第二次会被静默吞掉，
// 探测任务与上报间隔停在过时状态，且没有任何日志。
func TestReplaceLatestKeepsNewest(t *testing.T) {
	ch := make(chan int, 1)

	replaceLatest(ch, 1)
	replaceLatest(ch, 2)
	replaceLatest(ch, 3)

	select {
	case got := <-ch:
		if got != 3 {
			t.Fatalf("应保留最新值 3，实际拿到 %d —— 旧值覆盖了新值", got)
		}
	default:
		t.Fatal("channel 里应当有值")
	}

	// 排空后再放，不应残留旧值
	replaceLatest(ch, 9)
	if got := <-ch; got != 9 {
		t.Fatalf("排空后应拿到 9，实际 %d", got)
	}
	select {
	case v := <-ch:
		t.Fatalf("channel 应已排空，却还有 %d", v)
	default:
	}
}

// TestDiskTotalsFallsBackToLastSample 磁盘采集挂起时沿用上一拍。
//
// statfs(2) 不接受 context、不可中断，失联的 CIFS 挂载点会让它在内核态
// 无限期挂起。不设上限的话整个上报协程被永久冻结：所有指标停更，
// 而 WS 心跳在独立协程里仍活着，面板还显示「在线」。
func TestDiskTotalsFallsBackToLastSample(t *testing.T) {
	lastDiskMu.Lock()
	prevTotal, prevUsed := lastDiskTotal, lastDiskUsed
	lastDiskTotal, lastDiskUsed = 1000, 400
	lastDiskMu.Unlock()
	t.Cleanup(func() {
		lastDiskMu.Lock()
		lastDiskTotal, lastDiskUsed = prevTotal, prevUsed
		lastDiskMu.Unlock()
	})

	total, used := lastDisk()
	if total != 1000 || used != 400 {
		t.Fatalf("应沿用上一拍的值，实际 total=%d used=%d", total, used)
	}

	// 真实采集要么给出非零值、要么回落到上一拍，绝不能返回 0
	// ——0 会被界面显示成「磁盘为空」，比偏差更误导。
	gotTotal, _ := diskTotals()
	if gotTotal == 0 {
		t.Fatal("diskTotals 不应返回 0：取不到就该沿用上一拍")
	}
}
