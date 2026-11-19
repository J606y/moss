package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

var (
	cpuPercent  = cpu.Percent
	processPids = process.Pids
	loadAvg     = load.Avg
	netConns    = gnet.Connections
)

var (
	nodeUUIDOnce   sync.Once
	cachedNodeUUID string
)

// resolveNodeUUID 获取或持久化节点的固定 UUID。
//
// 优先级：
//  1. 环境变量 MOSS_NODE_UUID（支持显式指定）；
//  2. 本地持久化文件 /var/lib/moss-agent/node.uuid（跨重装、重注册稳定）；
//  3. 操作系统/硬件 machine-id（gopsutil host.Info().HostID 或 /etc/machine-id）；
//  4. 随机生成的 UUID v4 并落盘保存。
func resolveNodeUUID(hi *host.InfoStat, persistPath string) string {
	if v := strings.TrimSpace(os.Getenv("MOSS_NODE_UUID")); v != "" {
		return strings.ToLower(v)
	}
	if persistPath != "" {
		if b, err := os.ReadFile(persistPath); err == nil {
			if id := strings.ToLower(strings.TrimSpace(string(b))); len(id) >= 8 {
				return id
			}
		}
	}
	var candidate string
	if hi != nil && strings.TrimSpace(hi.HostID) != "" {
		candidate = strings.ToLower(strings.TrimSpace(hi.HostID))
	}
	if candidate == "" && runtime.GOOS == "linux" {
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil {
				if id := strings.ToLower(strings.TrimSpace(string(b))); len(id) >= 8 {
					candidate = id
					break
				}
			}
		}
	}
	if candidate == "" {
		var raw [16]byte
		_, _ = rand.Read(raw[:])
		raw[6] = (raw[6] & 0x0f) | 0x40
		raw[8] = (raw[8] & 0x3f) | 0x80
		candidate = fmt.Sprintf("%s-%s-%s-%s-%s",
			hex.EncodeToString(raw[0:4]),
			hex.EncodeToString(raw[4:6]),
			hex.EncodeToString(raw[6:8]),
			hex.EncodeToString(raw[8:10]),
			hex.EncodeToString(raw[10:16]))
	}
	if persistPath != "" {
		if err := os.MkdirAll(filepath.Dir(persistPath), 0o700); err == nil {
			_ = os.WriteFile(persistPath, []byte(candidate+"\n"), 0o600)
		}
	}
	return candidate
}

func getNodeUUID(hi *host.InfoStat) string {
	nodeUUIDOnce.Do(func() {
		persistPath := "/var/lib/moss-agent/node.uuid"
		if runtime.GOOS == "windows" {
			persistPath = filepath.Join(os.TempDir(), "moss-agent-node.uuid")
		}
		cachedNodeUUID = resolveNodeUUID(hi, persistPath)
	})
	return cachedNodeUUID
}

const collectWarnInterval = 10 * time.Minute

var (
	collectWarnMu sync.Mutex
	collectWarnAt = map[string]time.Time{}
)

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

var skipFs = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "devfs": true, "overlay": true, "squashfs": true,
	"iso9660": true, "ramfs": true, "proc": true, "sysfs": true, "cgroup": true,
	"cgroup2": true, "fuse.snapfuse": true, "nsfs": true, "autofs": true,
	"cifs": true, "smbfs": true, "smb3": true, "nfs": true, "nfs4": true,
	"fuse.sshfs": true, "fuse.s3fs": true, "fuse.rclone": true,
}

const diskCollectTimeout = 3 * time.Second

var (
	lastDiskMu    sync.Mutex
	lastDiskTotal uint64
	lastDiskUsed  uint64
)

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
	hi, err := host.Info()
	if err == nil {
		osName := hi.Platform
		if hi.PlatformVersion != "" && !strings.Contains(osName, hi.PlatformVersion) {
			osName = fmt.Sprintf("%s %s", osName, strings.SplitN(hi.PlatformVersion, ".", 2)[0])
		}
		info.OS = titleFirstRune(osName)
		if hi.KernelArch != "" {
			info.Arch = hi.KernelArch
		}
		info.Virtualization = detectVirt(hi)
	}
	info.UUID = getNodeUUID(hi)
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
	info.IP, info.IPv6, info.CountryCode = publicNetCachedOnly()
	return info
}

func titleFirstRune(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError && size <= 1 {
		return s
	}
	return strings.ToUpper(string(r)) + s[size:]
}

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
	if hi != nil && hi.VirtualizationSystem != "" {
		return normVirt(hi.VirtualizationSystem)
	}
	return "物理机"
}

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

func systemdDetectVirt() string {
	out, _ := exec.Command("systemd-detect-virt").Output()
	return strings.TrimSpace(string(out))
}

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

var virtualNicPrefixes = []string{
	"lo", "docker", "br-", "veth", "virbr", "tun", "tap", "wg", "zt",
	"cni", "flannel", "cali", "kube-", "vmnet", "utun",
	"loopback", "vethernet",
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
