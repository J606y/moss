// moss-agent 自升级：接收 server 下发的 UpgradeTask，就地替换二进制并重启，
// 失败自动回滚。
//
// **为什么升级逻辑内置、而不是由 server 下发升级脚本**
// 闸 2 硬拦「动 agent 二进制 / 配置 / systemd 单元」，走 exec 通道会被自己的闸拦下；
// 更要紧的是，能下发任意升级脚本等于开了一条绕过命令黑名单的通道。
// server 只给「升到哪个版本、从哪下载」，可执行逻辑全部在这里。
//
// **为什么回滚必须由旧二进制守护**
// 新二进制若根本起不来（架构不对、下载损坏、依赖缺失），就没有任何进程能执行回滚，
// 而 systemd 的 Restart=always 会把失败进程反复拉起，机器永久失联。
// 因此升级把旧二进制留在 <bin>.bak，并用**它**（不是新的）以独立会话拉起
// --rollback-guard 守护。旧二进制此刻正在运行，是唯一能确定可执行的东西。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"moss/internal/protocol"
)

// upgradeDownloadTimeout 下载单个文件的超时。跨境拉 GitHub 可能很慢，给足余量；
// 超时只是放弃本次升级，不影响正在运行的 agent。
const upgradeDownloadTimeout = 10 * time.Minute

// upgradeBinCap 二进制大小上限，防止下载地址被指向超大文件打爆磁盘。
// agent 实际约 10MB 量级，64MB 留足余量。
const upgradeBinCap = 64 << 20

// upgrader 串行化升级任务并去重。
type upgrader struct {
	mu    sync.Mutex
	done  map[string]bool // 幂等 ID，重连后重复下发不会二次升级
	busy  bool
	files upgradeFiles
}

// upgradeFiles 把路径计算集中一处，便于测试注入。
type upgradeFiles struct {
	self   func() (string, error) // 当前可执行文件路径
	marker string                 // 连接标记文件
}

func newUpgrader() *upgrader {
	return &upgrader{
		done:  map[string]bool{},
		files: upgradeFiles{self: os.Executable, marker: connectedMarkerPath()},
	}
}

// Handle 异步受理升级任务：下载可能持续数分钟，不能阻塞 WS 读取循环，
// 否则期间的心跳与配置下发全部停摆。
func (u *upgrader) Handle(c *client, t protocol.UpgradeTask) {
	u.mu.Lock()
	switch {
	case u.done[t.ID]:
		u.mu.Unlock()
		return
	case u.busy:
		u.mu.Unlock()
		u.report(c, t.ID, protocol.UpgradeStageDownloading, errors.New("已有升级任务在执行"))
		return
	}
	u.done[t.ID] = true
	u.busy = true
	u.mu.Unlock()

	go func() {
		err := u.run(c, t)
		u.mu.Lock()
		u.busy = false
		u.mu.Unlock()
		if err != nil {
			// 失败在这里就地回报并终止：二进制尚未被替换，agent 仍在正常运行，
			// 不需要回滚，也不会重启。
			log.Printf("升级失败: %v", err)
			u.report(c, t.ID, protocol.UpgradeStageDownloading, err)
		}
	}()
}

// run 执行升级。返回 nil 表示已进入重启流程（此后连接会中断，不再有回执）。
func (u *upgrader) run(c *client, t protocol.UpgradeTask) error {
	// 平台与权限检查必须前置：等下载完几十兆才发现这台机器根本没法重启服务，
	// 既浪费带宽，也让失败发生在更靠近替换的地方。
	if err := upgradeSupported(); err != nil {
		return err
	}
	if strings.TrimPrefix(t.Version, "v") == strings.TrimPrefix(agentVersion, "v") {
		return fmt.Errorf("当前已是 %s，无需升级", t.Version)
	}
	bin, err := u.files.self()
	if err != nil {
		return fmt.Errorf("定位自身可执行文件失败: %w", err)
	}
	// 解析软链，否则备份与替换会作用在链接而不是真实文件上。
	if real, err := filepath.EvalSymlinks(bin); err == nil {
		bin = real
	}

	if t.BaseURL == "" {
		return errors.New("未提供下载地址")
	}
	binURL, sumsURL, assetName := releaseURLs(t.BaseURL)

	u.report(c, t.ID, protocol.UpgradeStageDownloading, nil)

	// 临时文件与目标同目录：rename 要求同一文件系统，跨设备会失败。
	tmp := bin + ".new"
	defer os.Remove(tmp) // 成功路径上 tmp 已被 rename 走，Remove 返回的 NotExist 无害
	if err := downloadTo(tmp, binURL, upgradeBinCap); err != nil {
		return fmt.Errorf("下载新版本失败: %w", err)
	}
	if err := verifySum(tmp, sumsURL, assetName); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("设置可执行权限失败: %w", err)
	}
	u.report(c, t.ID, protocol.UpgradeStageVerified, nil)

	// 备份用 rename 而非 copy：rename 是原子的，且保留原 inode——
	// 正在运行的本进程不受影响，.bak 也就必然是一个完整可执行的文件。
	backup := bin + ".bak"
	os.Remove(backup) // 清掉上次升级的残留，否则 rename 在部分系统上会失败
	if err := os.Rename(bin, backup); err != nil {
		return fmt.Errorf("备份旧版本失败: %w", err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		// 替换失败必须立刻把备份换回去，否则 bin 不存在，服务再也起不来。
		if rbErr := os.Rename(backup, bin); rbErr != nil {
			return fmt.Errorf("替换失败(%v)且恢复备份失败(%v)，二进制缺失，需人工介入", err, rbErr)
		}
		return fmt.Errorf("替换新版本失败: %w", err)
	}
	u.report(c, t.ID, protocol.UpgradeStageReplaced, nil)

	grace := t.GraceSec
	if grace <= 0 {
		grace = protocol.UpgradeDefaultGrace
	}
	// 守护必须脱离本进程的进程组：它接下来要重启 moss-agent，
	// 而重启会杀掉整个进程组——守护若还在组里，会把自己一起杀在半路，
	// 二进制停在半替换状态，机器直接失联。
	if err := spawnRollbackGuard(backup, bin, grace); err != nil {
		// 守护起不来就不能重启：没有回滚兜底的重启是在赌运气。
		// 把备份换回去，回到升级前的状态。
		if rbErr := os.Rename(backup, bin); rbErr != nil {
			return fmt.Errorf("守护启动失败(%v)且恢复备份失败(%v)，需人工介入", err, rbErr)
		}
		return fmt.Errorf("回滚守护启动失败，已恢复原版本: %w", err)
	}
	u.report(c, t.ID, protocol.UpgradeStageRestarting, nil)
	log.Printf("已替换为 %s，等待守护重启（%ds 内未连回将自动回滚）", t.Version, grace)
	return nil
}

func (u *upgrader) report(c *client, id, stage string, err error) {
	res := protocol.UpgradeResult{ID: id, Stage: stage}
	if err != nil {
		res.Error = err.Error()
	}
	if sendErr := c.send(protocol.AgentMsg{Type: "upgrade_result", Upgrade: &res}); sendErr != nil {
		log.Printf("回传升级进度失败: %v", sendErr)
	}
}

/* ---------- 下载与校验 ---------- */

// releaseURLs 按本机平台拼出二进制与校验和地址，返回值第三项是 SHA256SUMS 里的文件名。
// 命名规则与 release.yml 的构建产物一致：moss-agent-<goos>-<goarch>[.exe]。
func releaseURLs(base string) (binURL, sumsURL, assetName string) {
	base = strings.TrimRight(base, "/")
	assetName = fmt.Sprintf("moss-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName += ".exe"
	}
	return base + "/" + assetName, base + "/SHA256SUMS", assetName
}

func downloadTo(dst, url string, cap int64) error {
	cli := &http.Client{Timeout: upgradeDownloadTimeout}
	resp, err := cli.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d（%s）", resp.StatusCode, url)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	// 多写 1 字节的余量用于判断是否触顶：恰好等于上限时无法区分「刚好装下」与「被截断」。
	n, err := io.Copy(f, io.LimitReader(resp.Body, cap+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > cap {
		return fmt.Errorf("文件超过 %d 字节上限，拒绝安装", cap)
	}
	if n == 0 {
		return errors.New("下载内容为空")
	}
	return nil
}

// verifySum 用 release 附带的 SHA256SUMS 校验完整性。
//
// 与 install.sh 的策略保持一致但更严格：安装脚本在拿不到 SHA256SUMS 时告警后继续
// （老版本 release 确实没有这个文件），而自升级**必须**校验通过才替换——
// 这里没有人在旁边看告警，装错一次就是机器失联。
func verifySum(file, sumsURL, name string) error {
	if sumsURL == "" {
		return errors.New("未提供校验和地址，拒绝安装")
	}
	cli := &http.Client{Timeout: 2 * time.Minute}
	resp, err := cli.Get(sumsURL)
	if err != nil {
		return fmt.Errorf("获取校验和失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("获取校验和失败: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("读取校验和失败: %w", err)
	}
	want := ""
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		// 格式为 "<sha256>  <文件名>"，文件名可能带 * 前缀（二进制模式）
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			want = strings.ToLower(fields[0])
			break
		}
	}
	if want == "" {
		return fmt.Errorf("校验和清单中没有 %s，拒绝安装", name)
	}
	got, err := fileSum(file)
	if err != nil {
		return fmt.Errorf("计算校验和失败: %w", err)
	}
	if got != want {
		return fmt.Errorf("校验和不匹配（期望 %s，实际 %s），拒绝安装", want, got)
	}
	return nil
}

func fileSum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

/* ---------- 连接标记 ---------- */

// connectedMarkerPath 连接标记文件位置。
//
// /run 是 tmpfs 且仅 root 可写，重启即清空——正合语义：标记表达的是
// 「本次开机以来 agent 连上过 server」。/run 不存在的系统回退到临时目录。
func connectedMarkerPath() string {
	if st, err := os.Stat("/run"); err == nil && st.IsDir() {
		return "/run/moss-agent.connected"
	}
	return filepath.Join(os.TempDir(), "moss-agent.connected")
}

// markConnected 每次成功连上 server 时刷新连接标记。
//
// 这是回滚守护判断「新 agent 是否真的活过来」的唯一依据。只看 systemctl is-active
// 不够：进程起来了但 token 失效、endpoint 写错时同样连不上 server，
// 而那种状态与失联无异，必须判失败并回滚。
func markConnected() {
	p := connectedMarkerPath()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		// 不中断 agent 正常工作。副作用是升级守护探测不到连接标记而回滚——
		// 偏向「回滚」而不是「假定成功」，是安全侧的取舍。
		log.Printf("写连接标记失败: %v", err)
		return
	}
	f.Close()
}

func markerModTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

/* ---------- 回滚守护 ---------- */

// runRollbackGuard 由旧二进制以独立会话运行，是升级链路上最后的安全网。
//
// 流程：重启服务 → 在 grace 秒内等待新 agent 刷新连接标记 →
// 刷新了就删掉备份收工，没刷新就把备份换回去再重启一次。
// 返回进程退出码。
func runRollbackGuard(target string, grace int) int {
	logGuard("守护启动: target=%s grace=%ds", target, grace)
	// 守护自己就是那份备份——它正是被 rename 到 <bin>.bak 的旧二进制。
	backup, err := os.Executable()
	if err != nil {
		logGuard("定位备份文件失败: %v，无法回滚", err)
		return 1
	}
	return rollbackGuard(backup, target, grace, connectedMarkerPath(), time.Second)
}

// rollbackGuard 是守护的可测主体：路径、轮询间隔与重启动作全部由外部给定。
// 真实的 systemctl 调用与 setsid 行为只能在 Linux 上验证，但「等多久、
// 何时判成功、何时恢复备份」这套判断逻辑必须能在开发机上跑到。
func rollbackGuard(backup, target string, grace int, marker string, tick time.Duration) int {
	before := markerModTime(marker)

	if err := restartAgentService(); err != nil {
		logGuard("重启服务失败: %v，立即回滚", err)
		return rollback(backup, target)
	}

	deadline := time.Now().Add(time.Duration(grace) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(tick)
		if t := markerModTime(marker); t.After(before) {
			logGuard("新版本已连回 server，升级成功")
			if err := os.Remove(backup); err != nil {
				// 备份删不掉只是留下垃圾文件，升级本身已成功，不该报失败。
				logGuard("清理备份失败（不影响升级结果）: %v", err)
			}
			return 0
		}
	}
	logGuard("%ds 内未连回 server，开始回滚", grace)
	return rollback(backup, target)
}

func rollback(backup, target string) int {
	if err := os.Rename(backup, target); err != nil {
		logGuard("回滚失败: %v，需人工介入", err)
		return 1
	}
	if err := restartAgentService(); err != nil {
		logGuard("回滚后重启失败: %v，需人工介入", err)
		return 1
	}
	logGuard("已回滚到升级前版本")
	return 1
}

// logGuard 守护脱离了 systemd，stderr 无处可去，日志必须落文件才查得到。
func logGuard(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	p := filepath.Join(os.TempDir(), "moss-agent-upgrade.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(line)
}
