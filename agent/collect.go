package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
	"moss/internal/protocol"
)

// 采集入口做成变量，供测试注入故障。
//
// 权限不足、/proc 被容器屏蔽这类失败在 CI 里构造不出来（跑测试的进程通常有权限），
// 不注入就永远测不到失败分支——而失败分支恰恰是这几个指标最需要被守住的地方。
var (
	cpuPercent  = cpu.Percent
	processPids = process.Pids
	loadAvg     = load.Avg
	netConns    = gnet.Connections
)

// collectWarnInterval 同一类采集失败的日志间隔。
//
// 采集失败几乎都是持续性的（权限不足、/proc 不可读、内核不支持），
// 按每 2 秒一拍的上报频率不限流的话一天能刷出 4 万条相同的日志，
// 把 journal 撑爆并淹掉真正有价值的记录。按「错误类别」限流而不是按次数丢弃，
// 保证每一类故障始终看得见，运维至少能发现「这台机器的某个指标一直取不到」。
const collectWarnInterval = 10 * time.Minute

var (
	collectWarnMu sync.Mutex
	collectWarnAt = map[string]time.Time{}
)

// warnCollect 限流地记录一次采集失败。
//
// 必须记这条日志的原因：协议里这些字段是裸数值，没有「不可用」的表示法，
// 而结构体是 server 与 agent 的共享契约不能随手改。采集失败时字段保持零值上报，
// 与「系统真的空闲 / 连接数真的是 0」在界面上完全无法区分。
// 日志是这条信息唯一的出口——没有它，一台机器的 CPU 曲线可以永远是 0 而无人察觉。
func warnCollect(kind string, err error) {
	collectWarnMu.Lock()
	if last, ok := collectWarnAt[kind]; ok && time.Since(last) < collectWarnInterval {
		collectWarnMu.Unlock()
		return
	}
	collectWarnAt[kind] = time.Now()
	collectWarnMu.Unlock()
	log.Printf("采集%s失败，该指标将以 0 上报，请勿据此判断机器状态（同类问题 %v 内不再重复记录）: %v",
		kind, collectWarnInterval, err)
}

// 虚拟/只读文件系统，统计磁盘容量时跳过。
//
// 网络文件系统也在此列，但理由不同：它们不是「不真实」，而是**会挂死**。
// disk.Usage 底层是阻塞且不可中断的 statfs(2)，对端失联时会在内核态无限期挂起。
// gopsutil 的 Partitions(false) 已经滤掉了 NFS 与 sshfs（挂载源不以 / 开头），
// 但 CIFS/SMB 的挂载源是 //server/share，以 / 开头，会穿过那道过滤。
var skipFs = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "devfs": true, "overlay": true, "squashfs": true,
	"iso9660": true, "ramfs": true, "proc": true, "sysfs": true, "cgroup": true,
	"cgroup2": true, "fuse.snapfuse": true, "nsfs": true, "autofs": true,
	"cifs": true, "smbfs": true, "smb3": true, "nfs": true, "nfs4": true,
	"fuse.sshfs": true, "fuse.s3fs": true, "fuse.rclone": true,
}

// diskCollectTimeout 是磁盘采集的整体上限。
//
// statfs(2) 不接受 context、不可中断，挂起的挂载点无法用超时打断。所以这里
// 不是「取消它」，而是「不等它」——超时后沿用上一拍的值，让上报继续。
// 不设这个上限的话，一个失联的挂载点会永久冻结整个上报协程：
// 所有指标从此停更，而 WS 心跳在独立协程里仍然活着，面板还显示「在线」。
const diskCollectTimeout = 3 * time.Second

// 上一拍成功取到的磁盘用量。采集挂起时沿用，避免把「取不到」显示成 0。
var (
	lastDiskMu    sync.Mutex
	lastDiskTotal uint64
	lastDiskUsed  uint64
)

// diskTotals 汇总真实分区的容量与占用（按设备去重）。
//
// 采集放到独立协程里跑：挂起的挂载点会把那个协程永久泄漏掉，这是没办法的事
// （statfs 不可中断），但至少不会连累上报主循环。泄漏的协程只有一个，
// 因为挂起期间后续拍次都走「沿用上一拍」的快路径，不会再起新的。
func diskTotals() (total, used uint64) {
	type result struct{ total, used uint64 }
	ch := make(chan result, 1)
	go func() {
		t, u := diskTotalsBlocking()
		ch <- result{t, u}
	}()

	select {
	case r := <-ch:
		if r.total == 0 {
			// 一个分区都没取到，多半是采集失败而非机器真的没有磁盘，
			// 同样沿用上一拍，不要把界面刷成 0。
			return lastDisk()
		}
		lastDiskMu.Lock()
		lastDiskTotal, lastDiskUsed = r.total, r.used
		lastDiskMu.Unlock()
		return r.total, r.used
	case <-time.After(diskCollectTimeout):
		return lastDisk()
	}
}

func lastDisk() (uint64, uint64) {
	lastDiskMu.Lock()
	defer lastDiskMu.Unlock()
	return lastDiskTotal, lastDiskUsed
}

func diskTotalsBlocking() (total, used uint64) {
	parts, err := disk.Partitions(false)
	if err != nil {
		return 0, 0
	}
	seen := map[string]bool{}
	for _, p := range parts {
		if skipFs[strings.ToLower(p.Fstype)] || seen[p.Device] {
			continue
		}
		u, err := disk.Usage(p.Mountpoint)
		if err != nil || u.Total == 0 {
			continue
		}
		seen[p.Device] = true
		total += u.Total
		used += u.Used
	}
	return
}

func collectInfo() protocol.AgentInfo {
	info := protocol.AgentInfo{
		Arch:         runtime.GOARCH,
		CPUCores:     runtime.NumCPU(),
		AgentVersion: agentVersion,
	}
	if hi, err := host.Info(); err == nil {
		osName := hi.Platform
		if hi.PlatformVersion != "" && !strings.Contains(osName, hi.PlatformVersion) {
			// debian + 12 → Debian 12；Windows 的 Platform 已含版本号
			osName = fmt.Sprintf("%s %s", osName, strings.SplitN(hi.PlatformVersion, ".", 2)[0])
		}
		info.OS = titleFirstRune(osName)
		if hi.KernelArch != "" {
			info.Arch = hi.KernelArch
		}
		info.Virtualization = detectVirt(hi)
	}
	if cis, err := cpu.Info(); err == nil && len(cis) > 0 {
		info.CPUModel = strings.TrimSpace(cis[0].ModelName)
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		info.MemTotal = vm.Total
	}
	if sm, err := mem.SwapMemory(); err == nil {
		info.SwapTotal = sm.Total
	}
	info.DiskTotal, _ = diskTotals()
	// 只读 GeoIP 缓存，绝不在这里外呼：collectInfo 挂在 register 的关键路径上，
	// 外呼最坏要 15 秒，那期间 agent 对 server 而言是「连上了却迟迟不注册」。
	// 公网信息由 refreshGeoAndReregister 在后台取到后补发一条 register 更新。
	info.IP, info.IPv6, info.CountryCode = publicNetCachedOnly()
	return info
}

// titleFirstRune 把首字符转为大写（debian → Debian）。
//
// 不能写成 strings.ToUpper(s[:1]) + s[1:]：那是按**字节**切。平台名首字符是
// 非 ASCII 时（麒麟、统信这类中文发行版名，或本地化的 Windows 版本串），
// 切点落在多字节 UTF-8 序列中间，产出的是非法字节序列；encoding/json 会把它
// 静默替换成 U+FFFD 落库，界面上就是一个永远修不好的乱码。
// 当前 gopsutil 返回的平台名恰好都是 ASCII，但那是上游的实现细节而不是承诺。
func titleFirstRune(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		// 输入本身就不是合法 UTF-8，再切一刀只会更糟，原样返回。
		return s
	}
	// 用 strings.ToUpper 而非 unicode.ToUpper：前者带特殊大小写规则
	// （德语 ß → SS），后者会原样返回，与改动前的行为不一致。
	return strings.ToUpper(string(r)) + s[size:]
}

// detectVirt 判定虚拟化类型。gopsutil 在部分云厂商（DMI 被屏蔽）上识别不到
// 会误报物理机，故按可靠度依次兜底：systemd-detect-virt → DMI 厂商串 →
// CPU hypervisor 标志，全部判不出才认定物理机。
func detectVirt(hi *host.InfoStat) string {
	if hi != nil && hi.VirtualizationSystem != "" && hi.VirtualizationRole == "guest" {
		return normVirt(hi.VirtualizationSystem)
	}
	if runtime.GOOS == "linux" {
		if v := systemdDetectVirt(); v != "" && v != "none" {
			return normVirt(v)
		}
		if v := dmiVirt(); v != "" {
			return v
		}
		if cpuHasHypervisor() {
			return "虚拟机"
		}
	}
	// 其他平台 gopsutil 已尽力，给出非 guest 角色的检测结果兜底
	if hi != nil && hi.VirtualizationSystem != "" {
		return normVirt(hi.VirtualizationSystem)
	}
	return "物理机"
}

// normVirt 归一化虚拟化类型名（systemd-detect-virt / gopsutil 的小写标识）。
func normVirt(v string) string {
	switch strings.ToLower(v) {
	case "kvm":
		return "KVM"
	case "qemu", "bochs":
		return "QEMU"
	case "vmware":
		return "VMware"
	case "oracle", "virtualbox":
		return "VirtualBox"
	case "xen":
		return "Xen"
	case "microsoft", "hyperv", "hyper-v":
		return "Hyper-V"
	case "amazon":
		return "AWS"
	case "openstack":
		return "OpenStack"
	case "lxc", "lxc-libvirt":
		return "LXC"
	case "docker":
		return "Docker"
	case "podman":
		return "Podman"
	case "openvz":
		return "OpenVZ"
	default:
		return strings.ToUpper(v)
	}
}

// systemdDetectVirt 调 systemd-detect-virt（最权威）。物理机时命令返回非零退出码
// 且 stdout 为 "none"，故即便 err 也读取 stdout。命令不存在则返回空串。
func systemdDetectVirt() string {
	out, _ := exec.Command("systemd-detect-virt").Output()
	return strings.TrimSpace(string(out))
}

// dmiVirt 读取 DMI 厂商/产品串匹配已知虚拟化平台与云厂商。
func dmiVirt() string {
	var blob string
	for _, f := range []string{
		"/sys/class/dmi/id/product_name",
		"/sys/class/dmi/id/sys_vendor",
		"/sys/class/dmi/id/bios_vendor",
		"/sys/class/dmi/id/product_version",
	} {
		if b, err := os.ReadFile(f); err == nil {
			blob += " " + strings.ToLower(string(b))
		}
	}
	switch {
	case strings.Contains(blob, "kvm"):
		return "KVM"
	case strings.Contains(blob, "vmware"):
		return "VMware"
	case strings.Contains(blob, "virtualbox"), strings.Contains(blob, "oracle"):
		return "VirtualBox"
	case strings.Contains(blob, "xen"):
		return "Xen"
	case strings.Contains(blob, "hyper-v"), strings.Contains(blob, "microsoft corporation"):
		return "Hyper-V"
	case strings.Contains(blob, "amazon"), strings.Contains(blob, "ec2"):
		return "AWS"
	case strings.Contains(blob, "google"):
		return "GCP"
	case strings.Contains(blob, "alibaba"), strings.Contains(blob, "aliyun"):
		return "阿里云"
	case strings.Contains(blob, "tencent"):
		return "腾讯云"
	case strings.Contains(blob, "digitalocean"), strings.Contains(blob, "droplet"):
		return "DigitalOcean"
	case strings.Contains(blob, "openstack"):
		return "OpenStack"
	case strings.Contains(blob, "qemu"), strings.Contains(blob, "bochs"):
		return "QEMU"
	}
	return ""
}

// cpuHasHypervisor 检测 CPU 是否暴露 hypervisor 标志：物理机不会有，
// 几乎所有全/半虚拟化 guest 都会置位，作为最后兜底只能确定“是虚拟机”。
func cpuHasHypervisor() bool {
	b, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "hypervisor")
}

var (
	netMu       sync.Mutex
	prevNetTime time.Time
	prevSent    uint64
	prevRecv    uint64
)

// 不计入网速统计的接口名前缀。
//
// 之前用 IOCounters(false) 让 gopsutil 把**所有**接口求和，包括 lo、docker0、
// veth*、tun*。后果是容器出网流量在 eth0 和 veth 上各算一次，本机回环调用
// （本地服务互调、agent 自己跟本机组件通信）也被算成「网速」——
// 装了 Docker 的机器必现，包括跑面板的那台。数字系统性偏高且不可信。
var virtualNicPrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "tun", "tap", "wg", "zt",
	"cni", "flannel", "cali", "kube-", "vmnet", "utun",
	"loopback", "vethernet", // Windows 上的回环与 Hyper-V 虚拟交换机
}

func isVirtualNic(name string) bool {
	n := strings.ToLower(name)
	for _, p := range virtualNicPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// netRates 返回总收发字节数与瞬时速率（B/s）。
func netRates() (totalUp, totalDown uint64, up, down float64) {
	counters, err := gnet.IOCounters(true)
	if err != nil || len(counters) == 0 {
		return
	}
	var matched int
	for _, c := range counters {
		if isVirtualNic(c.Name) {
			continue
		}
		matched++
		totalUp += c.BytesSent
		totalDown += c.BytesRecv
	}
	// 一张物理网卡都没剩：纯 VPN 出网的机器主网卡可能就叫 tun0，
	// 这时宁可算多也不能算成 0——0 会被当成「网络不通」，比偏高更误导。
	if matched == 0 {
		totalUp, totalDown = 0, 0
		for _, c := range counters {
			totalUp += c.BytesSent
			totalDown += c.BytesRecv
		}
	}

	netMu.Lock()
	defer netMu.Unlock()
	now := time.Now()
	if !prevNetTime.IsZero() {
		dt := now.Sub(prevNetTime).Seconds()
		if dt > 0 && totalUp >= prevSent && totalDown >= prevRecv {
			up = float64(totalUp-prevSent) / dt
			down = float64(totalDown-prevRecv) / dt
		}
	}
	prevNetTime, prevSent, prevRecv = now, totalUp, totalDown
	return
}

// resetNetRates 将网速基准清零，使下一拍按"首样本"处理（只初始化基准、不输出速率）。
// 每次重连成功后调用，避免断线期间的 dt 拉出虚高毛刺。
func resetNetRates() {
	netMu.Lock()
	prevNetTime = time.Time{}
	prevSent = 0
	prevRecv = 0
	netMu.Unlock()
}

func connCount(kind string) int {
	conns, err := netConns(kind)
	if err != nil {
		warnCollect(kind+"连接数", err)
		return 0
	}
	return len(conns)
}

// collectStats 采集一拍实时指标。
//
// 每个失败分支都必须走 warnCollect：协议里这些字段没有「不可用」的表示法
// （改结构体会破坏与 server 的兼容），失败时上报的零值和真实的空闲值一模一样。
// 静默的 0 比缺数据更糟——它会被当成事实，进而被告警规则和容量判断采信。
func collectStats() (protocol.Stats, uint64) {
	var s protocol.Stats

	if pcts, err := cpuPercent(0, false); err != nil {
		warnCollect("CPU 使用率", err)
	} else if len(pcts) == 0 {
		warnCollect("CPU 使用率", errors.New("返回了空的采样结果"))
	} else {
		s.CPU = pcts[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemUsed = vm.Used
	} else {
		warnCollect("内存用量", err)
	}
	if sm, err := mem.SwapMemory(); err == nil {
		s.SwapUsed = sm.Used
	} else {
		warnCollect("交换区用量", err)
	}
	_, s.DiskUsed = diskTotals()
	s.TotalUp, s.TotalDown, s.NetUp, s.NetDown = netRates()
	s.TCP = connCount("tcp")
	if pids, err := processPids(); err == nil {
		s.Processes = len(pids)
	} else {
		warnCollect("进程数", err)
	}
	if avg, err := loadAvg(); err == nil {
		s.Load1, s.Load5, s.Load15 = avg.Load1, avg.Load5, avg.Load15
	} else {
		warnCollect("负载", err)
	}

	uptime, err := host.Uptime()
	if err != nil {
		warnCollect("运行时长", err)
	}
	return s, uptime
}
