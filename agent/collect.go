package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
	"moss/internal/protocol"
)

// 虚拟/只读文件系统，统计磁盘容量时跳过
var skipFs = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "devfs": true, "overlay": true, "squashfs": true,
	"iso9660": true, "ramfs": true, "proc": true, "sysfs": true, "cgroup": true,
	"cgroup2": true, "fuse.snapfuse": true, "nsfs": true, "autofs": true,
}

// diskTotals 汇总真实分区的容量与占用（按设备去重）。
func diskTotals() (total, used uint64) {
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
		if len(osName) > 0 {
			info.OS = strings.ToUpper(osName[:1]) + osName[1:]
		}
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
	info.IP, info.CountryCode = publicNet()
	return info
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

// netRates 返回总收发字节数与瞬时速率（B/s）。
func netRates() (totalUp, totalDown uint64, up, down float64) {
	counters, err := gnet.IOCounters(false)
	if err != nil || len(counters) == 0 {
		return
	}
	totalUp, totalDown = counters[0].BytesSent, counters[0].BytesRecv

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

func connCount(kind string) int {
	conns, err := gnet.Connections(kind)
	if err != nil {
		return 0
	}
	return len(conns)
}

func collectStats() (protocol.Stats, uint64) {
	var s protocol.Stats

	if pcts, err := cpu.Percent(0, false); err == nil && len(pcts) > 0 {
		s.CPU = pcts[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		s.MemUsed = vm.Used
	}
	if sm, err := mem.SwapMemory(); err == nil {
		s.SwapUsed = sm.Used
	}
	_, s.DiskUsed = diskTotals()
	s.TotalUp, s.TotalDown, s.NetUp, s.NetDown = netRates()
	s.TCP = connCount("tcp")
	s.UDP = connCount("udp")
	if pids, err := process.Pids(); err == nil {
		s.Processes = len(pids)
	}
	if avg, err := load.Avg(); err == nil {
		s.Load1, s.Load5, s.Load15 = avg.Load1, avg.Load5, avg.Load15
	}

	uptime, _ := host.Uptime()
	return s, uptime
}
