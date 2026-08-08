package main

import "testing"

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
