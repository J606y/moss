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
//
// ---- 判定分三层，规则本身只管最内层 ----
//
// 曾经的写法是「一条正则匹配整条命令」，这让每条规则都要自己操心引号、重复斜杠、
// 命令链，漏一个就是一个洞。实测漏过的就有 11 种，例如 `iptables -L; iptables -F`
// （只读白名单没有结尾锚点，以查询开头就整串放行）、`dd of="/dev/sda"`（规则里的
// `/` 写死成裸字符）、`rm -rf ///`（斜杠后不是空白或 `*`）。
//
// 现在改成：
//  1. splitSegments —— 按 shell 连接符切段，引号内的分隔符不切；
//  2. normalizeSegment —— 每段剥引号、折叠重复斜杠；
//  3. 规则逐段匹配 —— 只读白名单天然变成「每段都必须只读」。
//
// 这样上面三类洞是被结构消掉的，不是靠给每条正则打补丁：下面的规则表除了
// 关机那条（见其注释）都保持原样，却不再能用引号或 `;` 绕过。
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
	//
	// 锚在段首而非全串任意位置：`halt` 是个太常见的英文词，不锚会误伤
	// `grep halt /var/log/syslog`、`systemctl status halt.target`。
	// 切段之后锚段首才是对的——`foo && shutdown -h now` 的第二段仍会命中。
	{regexp.MustCompile(`(?i)^(sudo\s+)?(shutdown|poweroff|halt)\b`), "关闭系统"},
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

// ---- 第一层：切段 ----

// splitSegments 按 shell 的连接符把命令切成独立的段。
//
// 引号内的分隔符不切：否则 `grep "a|b" f` 会被切成两段，凭空造出误判。
// `(`、`)`、反引号也算分隔符，这样 `echo $(rm -rf /)` 里的子命令会单独成段。
func splitSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte // 0 表示不在引号内

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
			// 反斜杠续行：物理换行被消掉，逻辑上仍是同一段
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

// ---- 第二层：归一化 ----

// normalizeSegment 归一化一个命令段，让规则不必各自处理引号与重复斜杠。
//
// 剥引号：shell 里 `of="/dev/sda"` 与 `of=/dev/sda` 语义完全相同，
// 规则不该因为一对引号就漏判。折叠重复斜杠：`///` 与 `/` 指向同一处。
//
// 归一化只服务于匹配，不参与执行——所以把 `https://x` 折成 `https:/x` 无害。
func normalizeSegment(s string) string {
	s = strings.NewReplacer("'", "", `"`, "").Replace(s)
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return strings.TrimSpace(s)
}

// ---- 第三层：工作目录 ----

// 相对路径的批量操作。目标必须正好是「当前目录的全部内容」——
// `rm -rf ./build` 这种指名道姓的不算，它的落点是确定的、跟 cwd 无关。
var relativeWipeRe = regexp.MustCompile(
	`(?i)\b(` +
		`rm\s+(-{1,2}[\w-]+\s+)*[.*]+/?(\s|$)` +
		`|(chmod|chown)\s+(-{1,2}[\w-]+\s+)*[\w:.-]+\s+[.*]+/?(\s|$)` +
		`|find\s+\.(\s[^;]*)?\s(-delete|-exec)\b` +
		`)`)

// 在这些目录下对相对路径做批量删除，后果与删根同级。
//
// 刻意不含 /var、/tmp、/home、/opt、/root —— `cd /var/log && rm -rf *` 是
// 正常的日志清理，把它拦掉会让功能不可用，而误伤比漏拦更容易发生。
var dangerousCwdPrefixes = []string{"/etc", "/boot", "/dev", "/proc", "/sys", "/usr", "/lib", "/bin", "/sbin"}

// dangerousCwd 判断工作目录是否属于「相对路径批量操作会造成系统级破坏」的位置。
func dangerousCwd(dir string) string {
	d := strings.TrimRight(normalizeSegment(dir), "/")
	if dir == "" {
		return ""
	}
	if d == "" { // 归一化后为空 = 根目录
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

// cdTarget 从一个段里认出字面量 cd 的目标；认不出返回 ok=false。
// 只认字面量——`cd $DIR` 静态判断不出落点，不猜。
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

// checkRelativeDanger 逐段推进工作目录，检查「危险目录 + 相对路径批量操作」。
//
// 这一层专治 `cd / && rm -rf *`：两段单独看都不命中任何规则，
// 但合起来等同删根，而「cd 的目标写错」正是最典型的手滑形态。
// 按段序推进而不是取最终目录，是因为 `rm -rf * && cd /` 里的 rm 跑在原目录。
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

// ---- 入口 ----

// checkLockoutSegment 对**已归一化的单段**做自断手脚类判定。
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

// checkLockout 返回自断手脚类的拦截原因；空串表示放行。
func checkLockout(cmd string) string {
	for _, raw := range splitSegments(cmd) {
		if why := checkLockoutSegment(normalizeSegment(raw)); why != "" {
			return why
		}
	}
	return ""
}

// checkDestructive 返回拦截原因；空串表示放行。
func checkDestructive(cmd string) string {
	return checkCommand(cmd, "")
}

// checkCommand 是命令拦截的唯一入口，dir 是该命令的工作目录（可为空）。
//
// dir 必须参与判定：它一路直达 agent 的 cmd.Dir，不过闸就等于给
// `{"cmd":"rm -rf *","dir":"/"}` 开了后门——命令本身完全无害，落点才是致命的。
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
