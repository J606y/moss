package main

import (
	"regexp"
	"strings"
)

// 破坏性命令拦截。
//
// 定位必须说清楚：黑名单永远不完备，绕过它是容易的。它拦的是**手滑和模型犯浑**——
// AI 把本该在项目目录执行的清理命令发到了 `/`、把测试机的命令发到了生产机。
// 真正的安全边界是 Key 作用域（能碰哪几台）与 agent 侧的执行总开关，
// 黑名单只是最后一道便宜的保险，不能被当成主要防线。
//
// 命中即硬拒绝，不提供绕过开关：需要执行这类命令的场景，人应该自己上机器。
var destructiveRules = []struct {
	re  *regexp.Regexp
	why string
}{
	// rm 直接作用于根：`rm -rf /`、`rm -rf /*`、`rm --no-preserve-root -rf /`
	// 注意 `rm -rf /var/log` 不会命中——斜杠后必须是空白、结束或通配符。
	{regexp.MustCompile(`(?i)\brm\s+(-{1,2}[\w-]+\s+)*['"]?/['"]?(\s|$|\*)`), "删除根目录"},
	// 格式化文件系统
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`), "格式化文件系统"},
	// 裸写块设备
	{regexp.MustCompile(`(?i)\bdd\b[^|;&]*\bof=/dev/(sd|nvme|vd|hd|xvd|disk)`), "直接写入块设备"},
	{regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|vd|hd|xvd|disk)`), "重定向覆写块设备"},
	// 关停系统。关机走 GCP 守护那条明确路径，不从这里出去。
	{regexp.MustCompile(`(?i)\b(shutdown|poweroff|halt)\b`), "关闭系统"},
	{regexp.MustCompile(`(?i)\binit\s+0\b`), "关闭系统"},
	// fork 炸弹
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{.*\|.*&\s*\}\s*;?\s*:`), "fork 炸弹"},
	// 递归改根权限/属主
	{regexp.MustCompile(`(?i)\b(chmod|chown)\s+(-{1,2}[\w-]+\s+)*[\w:.-]+\s+['"]?/['"]?(\s|$|\*)`), "递归修改根目录权限"},
	// 篡改 moss 自身：agent 被卸载或被替换，等于把这台机器从中枢上摘掉且无从察觉。
	// 两种语法的参数顺序相反——systemctl 是「动作 服务名」，service 是「服务名 动作」，
	// 只写一种会漏掉另一种。
	{regexp.MustCompile(`(?i)\bsystemctl\s+(stop|disable|mask)\s+moss`), "停用 moss agent"},
	{regexp.MustCompile(`(?i)\bservice\s+moss\S*\s+(stop|disable)\b`), "停用 moss agent"},
	{regexp.MustCompile(`(?i)\brm\b[^|;&]*moss-agent`), "删除 moss agent"},
}

// 磁盘擦除类命令的目标若指向根分区，同样拦截。
var wipeRootRe = regexp.MustCompile(`(?i)\b(shred|wipefs)\b[^|;&]*\s/(\s|$|dev/)`)

// ---- 自断手脚类：执行完 moss 就再也够不到这台机器 ----
//
// 这一类比「破坏性」更该拦：删掉的文件还能从备份恢复，而一旦 SSH 或防火墙
// 被改错，agent 掉线、SSH 登不上，连补救的手都伸不进去，只能上云控制台。
// 因此不提供人工批准入口——真要动这些，人自己上机器，成本两分钟。

// 防火墙工具整类拦截，只放行查询。
//
// 不做「这条规则会不会挡住 SSH」的判断：一条规则的实际效果取决于规则顺序、
// 默认策略与既有规则，静态看命令根本判断不出来。`iptables -A INPUT -j DROP`
// 一个端口都没提，照样把人锁在门外。所以写操作一律拦，查询照常放行——
// AI 仍能看清防火墙现状来诊断问题，只是不能动它。
var firewallToolRe = regexp.MustCompile(`(?i)\b(iptables|ip6tables|iptables-restore|ip6tables-restore|nft|ufw|firewall-cmd)\b`)

var firewallReadOnlyRe = regexp.MustCompile(
	`(?i)^\s*(sudo\s+)?(` +
		`(iptables|ip6tables)\s+(-[a-z]*[LS][a-z]*|--list|--list-rules)\b` +
		`|ufw\s+(status|show)\b` +
		`|firewall-cmd\s+--(list|state|get|info)` +
		`|nft\s+list\b` +
		`)`)

// SSH 服务配置：读放行（排查时需要确认端口），任何写入形态一律拦。
var sshdConfigRe = regexp.MustCompile(`(?i)sshd_config`)

var sshdConfigReadOnlyRe = regexp.MustCompile(
	`(?i)^\s*(sudo\s+)?(cat|grep|egrep|fgrep|head|tail|less|more|wc|stat|ls|awk|diff|md5sum|sha256sum)\b[^|;&]*sshd_config[^|;&<>]*$`)

// 停用 SSH 服务等同于切断入口，与改配置同级。
// restart 不在此列：它不改变配置内容，是改完配置后的正常生效动作。
//
// 同样要覆盖两种相反的参数顺序：`systemctl stop sshd` 与 `service ssh stop`。
var sshServiceRe = regexp.MustCompile(
	`(?i)\bsystemctl\s+(stop|disable|mask)\s+(ssh|sshd)\b|\bservice\s+(ssh|sshd)\s+(stop|disable)\b`)

// 关闭网络接口同样会让 agent 立刻失联。
var netDownRe = regexp.MustCompile(`(?i)\b(ip\s+link\s+set\s+\S+\s+down|ifdown\s+\S+|ifconfig\s+\S+\s+down)\b`)

// checkLockout 返回自断手脚类的拦截原因；空串表示放行。
func checkLockout(cmd string) string {
	c := strings.TrimSpace(cmd)
	if firewallToolRe.MatchString(c) && !firewallReadOnlyRe.MatchString(c) {
		return "修改防火墙规则（一旦规则写错，agent 与 SSH 会一并失联，无法远程补救）"
	}
	if sshdConfigRe.MatchString(c) && !sshdConfigReadOnlyRe.MatchString(c) {
		return "修改 SSH 服务配置（端口或认证改错会导致再也登不上这台机器）"
	}
	if sshServiceRe.MatchString(c) {
		return "停用 SSH 服务（等同于切断远程入口）"
	}
	if netDownRe.MatchString(c) {
		return "关闭网络接口（agent 会立刻失联）"
	}
	return ""
}

// checkDestructive 返回拦截原因；空串表示放行。
func checkDestructive(cmd string) string {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return ""
	}
	for _, rule := range destructiveRules {
		if rule.re.MatchString(c) {
			return rule.why
		}
	}
	if wipeRootRe.MatchString(c) {
		return "擦除根分区"
	}
	return checkLockout(c)
}

// 受保护路径。写文件是命令黑名单绕不过去的另一条路：
// 不用 rm 也能靠覆写 /etc/passwd、sudoers 或 agent 的 systemd 单元搞垮机器
// ——甚至悄悄把 agent 换成别的二进制。两条路径必须各自设闸。
var protectedPaths = []struct {
	prefixes []string
	exact    []string
	why      string
}{
	{
		exact: []string{"/etc/passwd", "/etc/shadow", "/etc/gshadow", "/etc/group", "/etc/sudoers"},
		why:   "系统账户与提权配置",
	},
	{
		prefixes: []string{"/etc/sudoers.d/", "/etc/pam.d/"},
		why:      "系统账户与提权配置",
	},
	{
		prefixes: []string{"/etc/ssh/"},
		why:      "SSH 服务配置（改错会导致再也登不上这台机器）",
	},
	{
		prefixes: []string{"/root/.ssh/", "/home/"},
		exact:    []string{"/etc/hosts"},
		why:      "SSH 授权密钥或用户主目录",
	},
	{
		// 篡改 moss 自身：agent 被换成别的二进制或被停用，
		// 这台机器就从中枢上摘掉了，且无从察觉。
		prefixes: []string{"/etc/moss/", "/etc/systemd/system/moss", "/lib/systemd/system/moss", "/usr/lib/systemd/system/moss"},
		exact:    []string{"/usr/local/bin/moss-agent", "/usr/bin/moss-agent"},
		why:      "moss agent 自身的二进制或服务配置",
	},
	{
		prefixes: []string{"/boot/", "/dev/", "/proc/", "/sys/"},
		why:      "内核、设备或内核虚拟文件系统",
	},
}

// checkProtectedPath 返回拦截原因；空串表示放行。
//
// 只做前缀与精确匹配，不试图理解符号链接——真正的边界是 Key 作用域，
// 这一层拦的是模型把配置写错地方，不是对抗定向攻击。
func checkProtectedPath(path string) string {
	// 统一分隔符并去掉重复斜杠，挡住 //etc/passwd、/etc//ssh/ 这类等价写法。
	p := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	// 含 .. 的路径无法用前缀判断实际落点，一律拒绝而不是猜。
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "路径含 .. 无法判定实际落点，请使用规范化的绝对路径"
		}
	}
	lower := strings.ToLower(p)
	for _, rule := range protectedPaths {
		for _, e := range rule.exact {
			if lower == e {
				return rule.why
			}
		}
		for _, pre := range rule.prefixes {
			if strings.HasPrefix(lower, pre) {
				return rule.why
			}
		}
	}
	return ""
}
