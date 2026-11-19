package main

import (
	"regexp"
	"strings"
)

// 破坏性命令拦截。
var destructiveRules = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)\brm\s+(-{1,2}[\w-]+\s+)*['"]?/['"]?(\s|$|\*)`), "删除根目录"},
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`), "格式化文件系统"},
	{regexp.MustCompile(`(?i)\bdd\b[^|;&]*\bof=/dev/(sd|nvme|vd|hd|xvd|disk)`), "直接写入块设备"},
	{regexp.MustCompile(`(?i)>\s*/dev/(sd|nvme|vd|hd|xvd|disk)`), "重定向覆写块设备"},
	{regexp.MustCompile(`(?i)^(sudo\s+)?(shutdown|poweroff|halt)\b`), "关闭系统"},
	{regexp.MustCompile(`(?i)\binit\s+0\b`), "关闭系统"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{.*\|.*&\s*\}\s*;?\s*:`), "fork 炸弹"},
	{regexp.MustCompile(`(?i)\b(chmod|chown)\s+(-{1,2}[\w-]+\s+)*[\w:.-]+\s+['"]?/['"]?(\s|$|\*)`), "递归修改根目录权限"},
	{regexp.MustCompile(`(?i)\bsystemctl\s+(stop|disable|mask)\s+moss`), "停用 moss agent"},
	{regexp.MustCompile(`(?i)\bservice\s+moss\S*\s+(stop|disable)\b`), "停用 moss agent"},
	{regexp.MustCompile(`(?i)\brm\b[^|;&]*moss-agent`), "删除 moss agent"},
}

var wipeRootRe = regexp.MustCompile(`(?i)\b(shred|wipefs)\b[^|;&]*\s/(\s|$|dev/)`)

var firewallToolRe = regexp.MustCompile(`(?i)\b(iptables|ip6tables|iptables-restore|ip6tables-restore|nft|ufw|firewall-cmd)\b`)

var firewallReadOnlyRe = regexp.MustCompile(
	`(?i)^\s*(sudo\s+)?(` +
		`(iptables|ip6tables)\s+(-[a-z]*[LS][a-z]*|--list|--list-rules)\b` +
		`|ufw\s+(status|show)\b` +
		`|firewall-cmd\s+--(list|state|get|info)` +
		`|nft\s+list\b` +
		`)`)

var sshdConfigRe = regexp.MustCompile(`(?i)sshd_config`)

var sshdConfigReadOnlyRe = regexp.MustCompile(
	`(?i)^\s*(sudo\s+)?(cat|grep|egrep|fgrep|head|tail|less|more|wc|stat|ls|awk|diff|md5sum|sha256sum)\b[^|;&]*sshd_config[^|;&<>]*$`)

var sshServiceRe = regexp.MustCompile(
	`(?i)\bsystemctl\s+(stop|disable|mask)\s+(ssh|sshd)\b|\bservice\s+(ssh|sshd)\s+(stop|disable)\b`)

var netDownRe = regexp.MustCompile(`(?i)\b(ip\s+link\s+set\s+\S+\s+down|ifdown\s+\S+|ifconfig\s+\S+\s+down)\b`)

func splitSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte

	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			segs = append(segs, s)
		}
		cur.Reset()
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '\\':
			if i+1 < len(cmd) && cmd[i+1] == '\n' {
				i++
				cur.WriteByte(' ')
				continue
			}
			cur.WriteByte(c)
		case ';', '\n', '\r', '|', '&', '(', ')', '`':
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return segs
}

func normalizeSegment(s string) string {
	s = strings.NewReplacer("'", "", `"`, "").Replace(s)
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return strings.TrimSpace(s)
}

var relativeWipeRe = regexp.MustCompile(
	`(?i)\b(` +
		`rm\s+(-{1,2}[\w-]+\s+)*[.*]+/?(\s|$)` +
		`|(chmod|chown)\s+(-{1,2}[\w-]+\s+)*[\w:.-]+\s+[.*]+/?(\s|$)` +
		`|find\s+\.(\s[^;]*)?\s(-delete|-exec)\b` +
		`)`)

var dangerousCwdPrefixes = []string{"/etc", "/boot", "/dev", "/proc", "/sys", "/usr", "/lib", "/bin", "/sbin"}

func dangerousCwd(dir string) string {
	d := strings.TrimRight(normalizeSegment(dir), "/")
	if dir == "" {
		return ""
	}
	if d == "" {
		return "在根目录下对相对路径做批量删除或改权限（等同于作用于整个系统）"
	}
	lower := strings.ToLower(d)
	for _, p := range dangerousCwdPrefixes {
		if lower == p || strings.HasPrefix(lower, p+"/") {
			return "在系统目录 " + d + " 下对相对路径做批量删除或改权限"
		}
	}
	return ""
}

func cdTarget(seg, cur string) (string, bool) {
	f := strings.Fields(seg)
	if len(f) < 2 || f[0] != "cd" {
		return "", false
	}
	p := f[1]
	if strings.HasPrefix(p, "/") {
		return p, true
	}
	if cur == "" {
		return "", false
	}
	return strings.TrimRight(cur, "/") + "/" + p, true
}

func checkRelativeDanger(baseDir string, segs []string) string {
	dir := baseDir
	for _, raw := range segs {
		seg := normalizeSegment(raw)
		if why := dangerousCwd(dir); why != "" && relativeWipeRe.MatchString(seg) {
			return why
		}
		if d, ok := cdTarget(seg, dir); ok {
			dir = d
		}
	}
	return ""
}

func checkLockoutSegment(seg string) string {
	if firewallToolRe.MatchString(seg) && !firewallReadOnlyRe.MatchString(seg) {
		return "修改防火墙规则（一旦规则写错，agent 与 SSH 会一并失联，无法远程补救）"
	}
	if sshdConfigRe.MatchString(seg) && !sshdConfigReadOnlyRe.MatchString(seg) {
		return "修改 SSH 服务配置（端口或认证改错会导致再也登不上这台机器）"
	}
	if sshServiceRe.MatchString(seg) {
		return "停用 SSH 服务（等同于切断远程入口）"
	}
	if netDownRe.MatchString(seg) {
		return "关闭网络接口（agent 会立刻失联）"
	}
	return ""
}

func checkLockout(cmd string) string {
	for _, raw := range splitSegments(cmd) {
		if why := checkLockoutSegment(normalizeSegment(raw)); why != "" {
			return why
		}
	}
	return ""
}

func checkDestructive(cmd string) string {
	return checkCommand(cmd, "")
}

func checkCommand(cmd, dir string) string {
	segs := splitSegments(cmd)
	if len(segs) == 0 {
		return ""
	}
	for _, raw := range segs {
		seg := normalizeSegment(raw)
		if seg == "" {
			continue
		}
		for _, rule := range destructiveRules {
			if rule.re.MatchString(seg) {
				return rule.why
			}
		}
		if wipeRootRe.MatchString(seg) {
			return "擦除根分区"
		}
		if why := checkLockoutSegment(seg); why != "" {
			return why
		}
	}
	return checkRelativeDanger(dir, segs)
}

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
		prefixes: []string{"/etc/moss/", "/etc/systemd/system/moss", "/lib/systemd/system/moss", "/usr/lib/systemd/system/moss"},
		exact:    []string{"/usr/local/bin/moss-agent", "/usr/bin/moss-agent", "/etc/moss-agent.env"},
		why:      "moss agent 自身的二进制或服务配置",
	},
	{
		prefixes: []string{"/boot/", "/dev/", "/proc/", "/sys/"},
		why:      "内核、设备或内核虚拟文件系统",
	},
}

func checkProtectedPath(path string) string {
	p := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
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

// checkProtectedReadPath 检查 read_file 不允许直接读取的高危凭证文件（如 shadow 或 moss-agent token）。
func checkProtectedReadPath(path string) string {
	p := strings.ReplaceAll(strings.TrimSpace(path), "\\", "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "路径含 .. 无法判定实际落点，请使用规范化的绝对路径"
		}
	}
	lower := strings.ToLower(p)
	for _, exact := range []string{"/etc/shadow", "/etc/gshadow", "/etc/moss-agent.env"} {
		if lower == exact {
			return "系统密码哈希或 moss agent 凭证文件禁止通过 read_file 读取"
		}
	}
	if strings.HasPrefix(lower, "/etc/moss/") {
		return "moss agent 凭证目录禁止通过 read_file 读取"
	}
	return ""
}
