package main

import (
	"fmt"
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
		if hi.VirtualizationSystem != "" && hi.VirtualizationRole == "guest" {
			info.Virtualization = strings.ToUpper(hi.VirtualizationSystem)
		} else {
			info.Virtualization = "物理机"
		}
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
	return info
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
