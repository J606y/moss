// agent 一键升级：把指定机器的 agent 对齐到 server 自身版本。
//
// **只走后台管理接口，不进 MCP 工具清单。** 人点按钮是人的决定；让 AI 自己升级
// 自己脚下的 agent，等于绕过闸 2「篡改 moss 自身」的硬拦。
//
// **不做自动推送、不做批量。** 自动推送的爆炸半径是整个集群，手动逐台点坏也只坏一台。
// 但手动降低的只是爆炸半径、不是失败概率——被点的那一台照样可能起不来，
// 所以 agent 侧的自动回滚一条都不能省（见 agent/upgrade.go）。
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"moss/internal/protocol"
)

// handleUpgradeAgent 后台「更新」按钮的入口。
func (s *App) handleUpgradeAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var agentVersion string
	err := s.db.QueryRow(`SELECT agent_version FROM servers WHERE id=?`, id).Scan(&agentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "服务器不存在")
		return
	}
	if err != nil {
		log.Printf("handleUpgradeAgent query: %v", err)
		writeErr(w, 500, "内部错误")
		return
	}
	if err := s.upgrade.Start(s.hub, id, agentVersion); err != nil {
		// 校验不通过属于用户可理解、可纠正的情况（版本过旧、已是最新、机器离线），
		// 原因原样回给前端，而不是笼统的「操作失败」。
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{
		"message": "已下发升级指令，agent 替换后会自动重启；若新版本未能连回将自动回滚",
	})
}

// githubRepo 二进制来源仓库，与 install.sh 的 MOSS_REPO 保持一致的可覆盖方式。
func githubRepo() string {
	if v := strings.TrimSpace(os.Getenv("MOSS_REPO")); v != "" {
		return v
	}
	return "j606y/moss"
}

// releaseBase 返回某个 tag 的下载目录。
//
// 默认走 GitHub Releases，可用 MOSS_RELEASE_BASE 整体覆盖为镜像或自托管地址。
// 这不是为了测试方便：境内机器直连 GitHub 会超时（广州那台装 agent 时就踩过），
// 不给覆盖入口，一键升级对这类机器永远是坏的。
// 覆盖值后面会拼上 /<tag>/<文件名>，目录结构需与 GitHub Releases 一致。
func releaseBase(tag string) string {
	if v := strings.TrimSpace(os.Getenv("MOSS_RELEASE_BASE")); v != "" {
		return strings.TrimRight(v, "/") + "/" + tag
	}
	return fmt.Sprintf("https://github.com/%s/releases/download/%s", githubRepo(), tag)
}

// 升级任务的终态。进行中的阶段沿用 protocol.UpgradeStage*。
const (
	upgradeStageSuccess = "success"
	upgradeStageFailed  = "failed"
)

// upgradeOverallTimeout 单个升级任务的整体上限。
//
// **必须大于 agent 侧的 protocol 下载超时（当前 20 分钟）加上后续步骤的耗时**，
// 否则会出现 server 已判失败、agent 却马上要替换成功的错位——
// 界面显示红色，机器却真的升上去了，之后没有任何东西会来纠正这个状态。
// 30 分钟给下载留满 20 分钟，再留 10 分钟给校验、替换与拉起守护。
//
// 改这个值时务必回头看一眼 agent/upgrade.go 的 upgradeDownloadTimeout，
// 这两个数字之间有隐含契约。超时本身只是把状态判为失败，不影响目标机上实际发生的事。
const upgradeOverallTimeout = 30 * time.Minute

type upgradeJob struct {
	ID       string
	ServerID string
	Target   string // 目标版本，带 v
	Stage    string
	Err      string
	Started  time.Time
	// restartedAt 记录进入 replaced 的时刻。判超时要从这里算：
	// 下载耗时不可控，但「重启后多久该连回来」是有确定预期的。
	restartedAt time.Time
	graceSec    int
}

type upgradeManager struct {
	mu   sync.Mutex
	jobs map[string]*upgradeJob // key = serverID，一台机器同时只允许一个任务
}

func newUpgradeManager() *upgradeManager {
	return &upgradeManager{jobs: map[string]*upgradeJob{}}
}

/* ---------- 版本判定 ---------- */

// agentMinUpgradable 能接收 upgrade 消息的最低 agent 版本。
//
// upgrade 消息类型是 v2.0.0-beta.2 引入的，此前的 agent（**包括 2.0.0-beta.1**）
// 其 WS 消息分发 switch 没有 default 分支，未知类型被静默丢弃，
// 症状是干等到超时而非报错。因此必须在下发前就拦住。
//
// 判据必须精确到预发布号，不能只比主版本：2.0.0-beta.1 的主版本也是 2，
// 只看主版本会把它误判为支持，用户点了按钮就是白等一场——
// 而这正是这个按钮本该防住的事。
const agentMinUpgradable = "2.0.0-beta.2"

// agentSupportsUpgrade 判断该版本的 agent 认不认识 upgrade 消息。
//
// "dev" 视为支持：那是从源码直接构建的二进制，必然含最新协议，
// 且只可能出现在开发者自己机器上——本地端到端验证要靠它。
func agentSupportsUpgrade(v string) bool {
	v = strings.TrimSpace(strings.TrimPrefix(v, "v"))
	if v == "" {
		return false // 从未上报过版本，保守视为不支持
	}
	if v == "dev" {
		return true
	}
	return compareSemver(v, agentMinUpgradable) >= 0
}

// compareSemver 比较两个语义化版本，返回 -1 / 0 / 1。
//
// 覆盖预发布后缀的排序规则：2.0.0-beta.1 < 2.0.0-beta.2 < 2.0.0。
// 无法解析的段按 0 处理而不是报错——版本号来自 agent 上报，
// 拿不准的输入应当落到「保守判定」而非让整个判断炸掉。
func compareSemver(a, b string) int {
	an, ap := splitSemver(a)
	bn, bp := splitSemver(b)
	for i := range an {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	// 数字段相同：带预发布后缀的排在正式版之前（2.0.0-beta.1 < 2.0.0）。
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return comparePrerelease(ap, bp)
}

// splitSemver 拆出 [major, minor, patch] 与预发布后缀。
func splitSemver(v string) ([3]int, string) {
	var nums [3]int
	core, pre, _ := strings.Cut(v, "-")
	core, _, _ = strings.Cut(core, "+") // 构建元数据不参与比较
	for i, seg := range strings.SplitN(core, ".", 3) {
		if i > 2 {
			break
		}
		nums[i], _ = strconv.Atoi(seg) // 解析失败即 0
	}
	return nums, pre
}

// comparePrerelease 按点分段比较预发布标识：
// 全数字段按数值比，否则按字典序；段数少的排在前面（beta < beta.1）。
func comparePrerelease(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aErr := strconv.Atoi(as[i])
		bn, bErr := strconv.Atoi(bs[i])
		if aErr == nil && bErr == nil {
			if an < bn {
				return -1
			}
			return 1
		}
		if as[i] < bs[i] {
			return -1
		}
		return 1
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

// sameVersion 比较两个版本号，忽略 v 前缀差异。
func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// upgradeTarget 返回 server 自身版本对应的 tag 名；开发态返回空串。
func upgradeTarget() string {
	return defaultAgentVersion() // 与安装脚本钉版本用同一处判定，避免两边算出不同答案
}

// upgradeAvailability 给出某台机器的可升级性结论。
// 判定规则集中在这里一处，前端只消费结论，不自己比较版本。
//
// 返回 (可否一键升级, 提示语)。提示语在不可升级时说明原因，可升级时为空。
func upgradeAvailability(agentVersion string, online bool) (bool, string) {
	target := upgradeTarget()
	if target == "" {
		return false, "服务端为开发版本，没有对应的 release 可供安装"
	}
	if sameVersion(agentVersion, target) {
		return false, ""
	}
	// 版本判定排在在线判定之前：离线是会自行恢复的临时状态，
	// 而版本过旧决定了「必须换一种方式升级」，是更根本、需要用户采取不同行动的原因。
	// 若反过来，一台离线的旧 agent 只会显示「机器离线」，等它上线后用户点了还是失败。
	if !agentSupportsUpgrade(agentVersion) {
		return false, fmt.Sprintf("agent %s 过旧，不认识升级指令，需用安装命令手动升级一次", displayVersion(agentVersion))
	}
	if !online {
		return false, "机器离线"
	}
	return true, ""
}

func displayVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "版本未知"
	}
	return "v" + strings.TrimPrefix(v, "v")
}

/* ---------- 下发 ---------- */

var errUpgradeBusy = errors.New("该机器已有升级任务在进行")

// Start 校验并下发升级任务。校验不通过时不下发，直接返回原因。
func (m *upgradeManager) Start(hub *Hub, serverID, agentVersion string) error {
	// 并发检查必须最先做：升级中的机器**必然**会短暂离线（替换后要重启），
	// 若先判在线，用户在这个窗口里重复点击看到的是「机器离线」，
	// 而真实情况是「正在升级中」——那是两种完全不同的处置。
	if m.busy(serverID) {
		return errUpgradeBusy
	}

	target := upgradeTarget()
	online := hub.AgentConn(serverID) != nil
	if ok, why := upgradeAvailability(agentVersion, online); !ok {
		if why == "" {
			return fmt.Errorf("已是最新版本 %s", displayVersion(target))
		}
		return errors.New(why)
	}

	m.mu.Lock()
	// 上面的检查未持锁（要访问 hub），这里必须再查一次，
	// 否则两次检查之间的并发请求会各自建一个任务。
	if job, ok := m.jobs[serverID]; ok && !isTerminalStage(job.Stage) {
		m.mu.Unlock()
		return errUpgradeBusy
	}
	job := &upgradeJob{
		ID:       newExecJobID(),
		ServerID: serverID,
		Target:   target,
		Stage:    protocol.UpgradeStageDownloading,
		Started:  time.Now(),
		graceSec: protocol.UpgradeDefaultGrace,
	}
	m.jobs[serverID] = job
	m.mu.Unlock()

	conn := hub.AgentConn(serverID)
	if conn == nil {
		m.finish(serverID, job.ID, upgradeStageFailed, "机器已离线")
		return errors.New("机器已离线")
	}
	task := protocol.UpgradeTask{
		ID:       job.ID,
		Version:  target,
		BaseURL:  releaseBase(target),
		GraceSec: job.graceSec,
	}
	if err := conn.send(protocol.ServerMsg{Type: "upgrade", Upgrade: &task}); err != nil {
		m.finish(serverID, job.ID, upgradeStageFailed, "下发失败: "+err.Error())
		return fmt.Errorf("下发失败: %w", err)
	}
	log.Printf("已向 %s 下发升级任务 %s → %s", serverID, job.ID, target)
	return nil
}

func isTerminalStage(s string) bool {
	return s == upgradeStageSuccess || s == upgradeStageFailed
}

// busy 该机器是否有升级任务在途。
func (m *upgradeManager) busy(serverID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[serverID]
	return ok && !isTerminalStage(job.Stage)
}

// OnResult 接收 agent 回传的阶段进度。
//
// 收不到最终成功回执是正常的：agent 在 restarting 阶段就会被重启掐断连接。
// 真正的成功判据在 OnRegister。
func (m *upgradeManager) OnResult(serverID string, res *protocol.UpgradeResult) {
	if res == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[serverID]
	if !ok || job.ID != res.ID || isTerminalStage(job.Stage) {
		return
	}
	if res.Error != "" {
		// agent 在替换前就失败了，二进制未动，机器仍在正常运行。
		job.Stage = upgradeStageFailed
		job.Err = res.Error
		log.Printf("升级失败 %s: %s", serverID, res.Error)
		return
	}
	job.Stage = res.Stage
	if res.Stage == protocol.UpgradeStageReplaced && job.restartedAt.IsZero() {
		job.restartedAt = time.Now()
	}
}

// OnRegister 在 agent 重新注册并上报版本时判定升级结果。
//
// 这是唯一可靠的成功判据：agent 重连且上报的版本等于目标版本，说明新二进制
// 真的活过来并连回了 server。若上报的仍是旧版本，说明 agent 侧的守护已经回滚。
func (m *upgradeManager) OnRegister(serverID, reportedVersion string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[serverID]
	if !ok {
		return
	}
	if isTerminalStage(job.Stage) {
		// 失败标记应当在问题解决后自行消失：agent 上报的版本已经等于目标版本，
		// 说明人已经手动升上去了，界面再挂一个红色的失败按钮只会误导。
		if job.Stage == upgradeStageFailed && sameVersion(reportedVersion, job.Target) {
			delete(m.jobs, serverID)
		}
		return
	}
	// 替换之前的重连只是普通的网络抖动，此时版本本来就还是旧的，不能据此判失败。
	if job.restartedAt.IsZero() {
		return
	}
	if sameVersion(reportedVersion, job.Target) {
		job.Stage = upgradeStageSuccess
		job.Err = ""
		log.Printf("升级成功 %s → %s", serverID, job.Target)
		return
	}
	job.Stage = upgradeStageFailed
	job.Err = fmt.Sprintf("新版本未能连回，agent 已自动回滚到 %s", displayVersion(reportedVersion))
	log.Printf("升级失败并已回滚 %s: 仍为 %s", serverID, reportedVersion)
}

// Status 返回该机器当前的升级状态，顺带做超时收敛。
//
// 超时判定放在读取时惰性执行，不额外起清理协程：状态只在有人看的时候才需要准确。
func (m *upgradeManager) Status(serverID string) (stage, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[serverID]
	if !ok {
		return "", ""
	}
	if !isTerminalStage(job.Stage) && m.expired(job) {
		job.Stage = upgradeStageFailed
		if job.restartedAt.IsZero() {
			job.Err = "升级超时，agent 未回报进度"
		} else {
			// 已经替换过又迟迟没连回：agent 侧守护此刻应当已经回滚，
			// 但机器没连回来，无法确认，如实说明而不是断言失败。
			job.Err = "新版本未在预期时间内连回 server，agent 侧应已自动回滚，请确认机器状态"
		}
	}
	return job.Stage, job.Err
}

func (m *upgradeManager) expired(job *upgradeJob) bool {
	if !job.restartedAt.IsZero() {
		// 重启后的判据用 agent 侧的 grace 再加一倍余量：
		// 守护要先等满 grace 才回滚，回滚后还要重启一次并重新连回。
		return time.Since(job.restartedAt) > time.Duration(job.graceSec*2+60)*time.Second
	}
	return time.Since(job.Started) > upgradeOverallTimeout
}

func (m *upgradeManager) finish(serverID, jobID, stage, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job, ok := m.jobs[serverID]; ok && job.ID == jobID {
		job.Stage = stage
		job.Err = errMsg
	}
}

// OnAgentGone 在 agent 掉线时被调用。
//
// 升级过程中掉线是**预期行为**——替换后必然要重启。因此这里刻意什么都不做：
// 把正常的重启窗口误报成失败，会让每一次成功的升级都先闪一下红。
func (m *upgradeManager) OnAgentGone(serverID string) {}
