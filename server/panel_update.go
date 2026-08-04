// 面板自更新：查询 GitHub Releases，并通过面板所在机器的 agent 执行部署。
//
// **为什么绕一圈走 agent**
// 面板跑在容器里，容器内的进程改不了自己的镜像、也重建不了自己的容器——那需要宿主机
// 的 docker 控制权。给容器挂 docker.sock 能解决，但等于把宿主机 root 交给一个
// 已经握有全部机器 agent 隧道的进程，闸 2 与 --allow-exec 那套边界会一并作废。
//
// 容器内替换自己的二进制（sub2api 那种做法）也不行：moss 以 nonroot 运行、
// 二进制属 root，写不了；就算改 Dockerfile 放开权限，更新也只活在容器可写层里——
// 下次 pull 重建容器会悄悄退回镜像版本，且没有任何提示。
//
// 走 agent 更新的是**镜像本身**，重建不会退版本，且复用了已经真机验证过的部署流程。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"moss/internal/protocol"
)

// 更新通道。正式版跳过预发布，测试版取最新的一个（含预发布）。
const (
	channelStable = "stable"
	channelBeta   = "beta"
)

// releaseCheckTTL 版本查询结果的缓存时长。
//
// GitHub 对未认证请求限速 60 次/小时，而前端每次打开更新页都会查一次。
// 15 分钟足够及时，又不会把配额烧光——真要立刻看最新版，界面上的
// 「检查更新」按钮会强制绕过缓存。
const releaseCheckTTL = 15 * time.Minute

// githubRelease 只取用得上的字段。
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Name       string `json:"name"`
	Body       string `json:"body"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Published  string `json:"published_at"`
}

// releaseInfo 回给前端的版本信息。
type releaseInfo struct {
	Version   string `json:"version"`   // 带 v 前缀的 tag 名
	Name      string `json:"name"`      // release 标题
	Notes     string `json:"notes"`     // 更新说明（Markdown 原文）
	Published string `json:"published"` // ISO 时间
	// Prerelease 让界面能标出「测试版」；正式版通道拿到的这一项恒为 false。
	Prerelease bool `json:"prerelease"`
}

type releaseCache struct {
	mu      sync.Mutex
	byChan  map[string]releaseInfo
	fetched map[string]time.Time
	lastErr map[string]string
}

func newReleaseCache() *releaseCache {
	return &releaseCache{
		byChan:  map[string]releaseInfo{},
		fetched: map[string]time.Time{},
		lastErr: map[string]string{},
	}
}

// githubAPIBase 版本查询的 API 地址。
//
// 与 MOSS_RELEASE_BASE 同样留镜像入口：境内机器直连 api.github.com 常常拉不动，
// 而版本查询失败会让整个更新页变成一块死砖。
func githubAPIBase() string {
	if v := strings.TrimSpace(os.Getenv("MOSS_GITHUB_API")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://api.github.com"
}

var releaseHTTP = &http.Client{Timeout: 20 * time.Second}

// fetch 查询指定通道的最新版本。force 为 true 时绕过缓存。
func (c *releaseCache) fetch(channel string, force bool) (releaseInfo, error) {
	c.mu.Lock()
	if !force {
		if t, ok := c.fetched[channel]; ok && time.Since(t) < releaseCheckTTL {
			info := c.byChan[channel]
			errMsg := c.lastErr[channel]
			c.mu.Unlock()
			if errMsg != "" {
				return releaseInfo{}, fmt.Errorf("%s", errMsg)
			}
			return info, nil
		}
	}
	c.mu.Unlock()

	info, err := fetchLatestRelease(channel)

	c.mu.Lock()
	c.fetched[channel] = time.Now()
	if err != nil {
		// 失败也记入缓存：GitHub 不可达时，前端每次刷新都重试只会让页面卡 20 秒。
		c.lastErr[channel] = err.Error()
	} else {
		delete(c.lastErr, channel)
		c.byChan[channel] = info
	}
	c.mu.Unlock()
	return info, err
}

// fetchLatestRelease 拉取某通道的最新 release。
//
// 正式版走 /releases/latest —— GitHub 对该端点的定义就是「跳过预发布」，
// 与本项目 docker.yml 里 latest 标签的语义一致。
// 测试版走 /releases 取第一个非草稿项，它按发布时间倒序，因此含预发布。
func fetchLatestRelease(channel string) (releaseInfo, error) {
	repo := githubRepo()
	url := githubAPIBase() + "/repos/" + repo + "/releases"
	if channel == channelStable {
		url += "/latest"
	} else {
		url += "?per_page=10"
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return releaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "moss/"+serverVersion)

	resp, err := releaseHTTP.Do(req)
	if err != nil {
		return releaseInfo{}, fmt.Errorf("连接 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return releaseInfo{}, fmt.Errorf("读取响应失败: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return releaseInfo{}, fmt.Errorf("GitHub 接口限流（HTTP %d），请稍后再试", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return releaseInfo{}, fmt.Errorf("GitHub 返回 HTTP %d", resp.StatusCode)
	}

	pick := func(r githubRelease) releaseInfo {
		return releaseInfo{
			Version:    r.TagName,
			Name:       r.Name,
			Notes:      r.Body,
			Published:  r.Published,
			Prerelease: r.Prerelease,
		}
	}

	if channel == channelStable {
		var r githubRelease
		if err := json.Unmarshal(body, &r); err != nil {
			return releaseInfo{}, fmt.Errorf("解析响应失败: %w", err)
		}
		if r.TagName == "" {
			return releaseInfo{}, fmt.Errorf("尚无正式版发布")
		}
		return pick(r), nil
	}

	var list []githubRelease
	if err := json.Unmarshal(body, &list); err != nil {
		return releaseInfo{}, fmt.Errorf("解析响应失败: %w", err)
	}
	for _, r := range list {
		if r.Draft {
			continue // 草稿对外不可见，装不了
		}
		return pick(r), nil
	}
	return releaseInfo{}, fmt.Errorf("尚无可用发布")
}

/* ---------- 配置 ---------- */

// defaultChannel 未显式设置时的更新通道。
//
// 按当前运行的版本推断：正在跑测试版的人默认留在测试版通道。
// 一律默认 stable 的话，测试版用户一进页面就看到「降级到某个旧正式版」，
// 那不是他想要的，也不像个默认值该有的样子。
func defaultChannel() string {
	if strings.Contains(serverVersion, "-") {
		return channelBeta
	}
	return channelStable
}

type panelUpdateConfig struct {
	Channel    string `json:"channel"`
	Auto       bool   `json:"auto"`
	HostServer string `json:"hostServer"`
}

func loadPanelUpdateConfig(s *App) panelUpdateConfig {
	return panelUpdateConfig{
		Channel:    getSetting(s.db, keyPanelChannel, defaultChannel()),
		Auto:       getSetting(s.db, keyPanelAutoUpdate, "0") == "1",
		HostServer: getSetting(s.db, keyPanelHostServer, ""),
	}
}

// panelUpdateView 更新页需要的全部信息，一次取回。
type panelUpdateView struct {
	Current    string             `json:"current"` // 当前运行版本，不带 v
	Config     panelUpdateConfig  `json:"config"`
	Latest     *releaseInfo       `json:"latest,omitempty"`
	CheckError string             `json:"checkError,omitempty"`
	Avail      updateAvailability `json:"avail"`
	// HostReady 面板所在机器当前能否执行更新：已指定、在线、且 agent 认识执行指令。
	HostReady bool   `json:"hostReady"`
	HostHint  string `json:"hostHint,omitempty"`
	Stage     string `json:"stage,omitempty"`
	StageErr  string `json:"stageErr,omitempty"`
}

func (s *App) handleGetPanelUpdate(w http.ResponseWriter, r *http.Request) {
	s.writePanelUpdateView(w, r.URL.Query().Get("check") == "1")
}

func (s *App) writePanelUpdateView(w http.ResponseWriter, force bool) {
	cfg := loadPanelUpdateConfig(s)
	view := panelUpdateView{
		Current: strings.TrimPrefix(serverVersion, "v"),
		Config:  cfg,
	}
	if info, err := s.releases.fetch(cfg.Channel, force); err != nil {
		view.CheckError = err.Error()
		view.Avail = updateAvailability{Action: "unknown", Reason: "版本查询失败"}
	} else {
		view.Latest = &info
		view.Avail = compareToCurrent(info.Version)
	}
	view.HostReady, view.HostHint = s.panelHostReady(cfg.HostServer)
	view.Stage, view.StageErr = s.panelUpd.status()
	writeJSON(w, 200, view)
}

// panelHostReady 判断面板所在机器现在能不能执行更新。
func (s *App) panelHostReady(id string) (bool, string) {
	if id == "" {
		return false, "尚未指定面板所在的服务器"
	}
	var name, agentVer string
	if err := s.db.QueryRow(`SELECT name, agent_version FROM servers WHERE id=?`, id).
		Scan(&name, &agentVer); err != nil {
		return false, "指定的服务器不存在，请重新选择"
	}
	if s.hub.AgentConn(id) == nil {
		return false, fmt.Sprintf("「%s」当前离线", name)
	}
	// 更新经由 exec 通道下发，需要该机器显式开启远程执行——
	// 这道闸不因为「更新的是面板自己」就放开。
	if !agentSupportsUpgrade(agentVer) {
		return false, fmt.Sprintf("「%s」的 agent 版本过旧，请先手动升级", name)
	}
	return true, ""
}

func (s *App) handlePutPanelUpdate(w http.ResponseWriter, r *http.Request) {
	var f panelUpdateConfig
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if f.Channel != channelStable && f.Channel != channelBeta {
		f.Channel = defaultChannel()
	}
	if f.HostServer != "" {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE id=?`, f.HostServer).Scan(&n)
		if n == 0 {
			writeErr(w, 400, "选择的服务器不存在")
			return
		}
	}
	setSetting(s.db, keyPanelChannel, f.Channel)
	setSetting(s.db, keyPanelAutoUpdate, boolSetting(f.Auto))
	setSetting(s.db, keyPanelHostServer, f.HostServer)
	// 切换通道后原缓存对应的是另一条通道，强制重查一次，避免界面停在旧结果上
	s.writePanelUpdateView(w, true)
}

func boolSetting(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

/* ---------- 更新执行 ---------- */

// panelUpdater 跟踪面板自更新的执行状态。
//
// 状态只放内存，不落库，因为**成功的更新会重启面板、把状态一并抹掉**——
// 这恰好是对的：重启后 serverVersion 已经是新版本，前端比对版本号就知道成了，
// 不需要一条"我刚才在更新"的记录。只有失败时面板不会重启，那条错误才有人读。
type panelUpdater struct {
	mu     sync.Mutex
	stage  string // "" 空闲 / running / failed
	err    string
	jobID  string
	target string
}

// panelUpdateTimeout 判失超时。
//
// 拉镜像 + 重建容器 + 健康探测，正常两三分钟内结束；给到 5 分钟是留给慢链路。
const panelUpdateTimeout = 5 * time.Minute

func newPanelUpdater() *panelUpdater { return &panelUpdater{} }

func (p *panelUpdater) status() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage, p.err
}

func (p *panelUpdater) begin(target, jobID string) {
	p.mu.Lock()
	p.stage, p.err, p.target, p.jobID = "running", "", target, jobID
	p.mu.Unlock()

	// 判失靠的是「这个协程居然跑到头了」：更新成功的话面板早已重启，
	// 它会随进程一起消失，根本执行不到 fail。反过来，能执行到就说明面板没重启，
	// 也就是没成功。内存态的状态机不需要别的收敛手段。
	go func() {
		time.Sleep(panelUpdateTimeout)
		p.fail(fmt.Sprintf(
			"更新到 %s 超时：面板未在 %d 分钟内重启。请到目标机器查看 /root/moss-panel-update.log，"+
				"或在执行审计中检索任务 %s",
			target, int(panelUpdateTimeout.Minutes()), jobID))
	}()
}

// fail 记录失败原因。已是终态时不覆盖——先到的那个原因更接近现场。
func (p *panelUpdater) fail(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stage != "running" {
		return
	}
	p.stage, p.err = "failed", msg
}

func (p *panelUpdater) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stage == "running"
}

// panelUpdateScript 生成部署脚本。
//
// 参数不写死，全部从运行中的容器 inspect 出来再复刻——今天手动部署时就踩过：
// --trust-proxy 不在镜像默认 CMD 里，是 docker run 时加的，漏掉它反代后面
// 就拿不到真实来源 IP。凡是"我以为默认值是什么"的地方，都该改成"去读实际值"。
//
// 旧容器只改名保留、不删除：面板重启期间发起更新的那个连接必然断开，
// 出了问题没有任何人能补救，只能靠脚本自己换回去。
func panelUpdateScript(imageRepo, image string) string {
	return `#!/usr/bin/env bash
set -u
exec > /root/moss-panel-update.log 2>&1

REPO=` + shellQuote(imageRepo) + `
IMG=` + shellQuote(image) + `
log() { echo "[$(date '+%F %T')] $*"; }

log "目标镜像: $IMG"

# 按镜像名探测容器，而不是让用户配一个容器名——配错了就是把更新打到别的容器上。
# 排除 -old 后缀：那是上一次更新留下的回滚备份，不是正在跑的面板。
C=$(docker ps -a --format '{{.Names}} {{.Image}}' \
      | awk -v r="^${REPO}(:|@|$)" '$2 ~ r {print $1}' \
      | grep -v -- '-old$' | head -1)
if [ -z "$C" ]; then
  log "RESULT: FAILED (未找到基于 ${REPO} 的容器)"
  exit 1
fi
log "面板容器: $C"

# 逐项复刻运行参数，不假设任何默认值
PORTS=$(docker inspect "$C" --format '{{range $p,$b := .HostConfig.PortBindings}}{{range $b}}-p {{if .HostIp}}{{.HostIp}}:{{end}}{{.HostPort}}:{{$p}} {{end}}{{end}}')
VOLS=$(docker inspect "$C" --format '{{range .Mounts}}-v {{if eq .Type "volume"}}{{.Name}}{{else}}{{.Source}}{{end}}:{{.Destination}} {{end}}')
ENVS=$(docker inspect "$C" --format '{{range .Config.Env}}--env={{.}} {{end}}')
ARGS=$(docker inspect "$C" --format '{{range .Args}}{{.}} {{end}}')
RESTART=$(docker inspect "$C" --format '{{.HostConfig.RestartPolicy.Name}}')
NET=$(docker inspect "$C" --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' | awk '{print $1}')
[ -z "$RESTART" ] && RESTART=unless-stopped

log "拉取镜像"
if ! docker pull "$IMG"; then
  log "RESULT: FAILED (拉取镜像失败)"
  exit 1
fi

log "停止并保留旧容器"
docker stop "$C" >/dev/null
docker rm -f "${C}-old" >/dev/null 2>&1 || true
docker rename "$C" "${C}-old"

log "启动新容器"
# shellcheck disable=SC2086
if ! docker run -d --name "$C" --restart "$RESTART" ${NET:+--network "$NET"} $PORTS $VOLS $ENVS "$IMG" $ARGS; then
  log "启动失败，回滚"
  docker rm -f "$C" >/dev/null 2>&1 || true
  docker rename "${C}-old" "$C" && docker start "$C"
  log "RESULT: ROLLED_BACK (docker run 失败)"
  exit 1
fi

log "等待服务就绪"
PORT=$(docker inspect "$C" --format '{{range $p,$b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}}{{end}}{{end}}' | head -c 10)
[ -z "$PORT" ] && PORT=8787
for i in $(seq 1 20); do
  if curl -fsS -o /dev/null "http://127.0.0.1:${PORT}/"; then
    log "服务已就绪（第 ${i} 次探测）"
    docker rm "${C}-old" >/dev/null 2>&1 || true
    log "RESULT: OK"
    exit 0
  fi
  sleep 2
done

log "40 秒内未就绪，回滚。新容器日志："
docker logs --tail 30 "$C" 2>&1 | sed 's/^/    /'
docker rm -f "$C" >/dev/null 2>&1 || true
docker rename "${C}-old" "$C" && docker start "$C"
log "RESULT: ROLLED_BACK (健康探测失败)"
exit 1
`
}

// panelUpdateCaller 审计里的调用方标识。面板自更新同样留痕——
// 「谁在什么时候把面板换成了哪个版本」和执行一条命令一样值得记录。
const panelUpdateCaller = "panel-update"

func (s *App) handleStartPanelUpdate(w http.ResponseWriter, r *http.Request) {
	if s.panelUpd.busy() {
		writeErr(w, 400, "已有更新正在进行")
		return
	}
	cfg := loadPanelUpdateConfig(s)
	if ready, hint := s.panelHostReady(cfg.HostServer); !ready {
		writeErr(w, 400, hint)
		return
	}
	info, err := s.releases.fetch(cfg.Channel, false)
	if err != nil {
		writeErr(w, 400, "版本查询失败: "+err.Error())
		return
	}
	// 只许往前走。前端也会挡，但不能只靠前端——绕过界面直接调接口同样要被拒。
	// 除了「往回走很怪」，更实际的原因是数据库迁移是单向的：
	// 旧版本读不懂新版本写下的结构。
	if a := compareToCurrent(info.Version); a.Action != "update" {
		writeErr(w, 400, "只能更新到比当前更高的版本（当前 v"+strings.TrimPrefix(serverVersion, "v")+"）")
		return
	}

	repo := "ghcr.io/" + githubRepo()
	image := repo + ":" + strings.TrimPrefix(info.Version, "v")
	script := panelUpdateScript(repo, image)

	// 脚本用 write_file 下发而不是塞进命令行：几 KB 的 shell 拼进 exec 既难读，
	// 转义也容易出错，而 write_file 会把内容原样记进审计。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := s.exec.SubmitWrite(ctx, s.hub, cfg.HostServer, panelUpdateCaller, protocol.WriteTask{
		Path: "/root/moss-panel-update.sh",
		Data: []byte(script),
		Mode: 0o755,
	})
	if err != nil || out.Error != "" {
		msg := out.Error
		if msg == "" && err != nil {
			msg = err.Error()
		}
		writeErr(w, 400, "下发更新脚本失败: "+msg)
		return
	}

	// setsid 让脚本脱离 agent 的进程树：它接下来要停掉面板容器，
	// 而面板一停，这条 exec 的回传通道就断了——脚本必须活得比连接久。
	jobID, err := s.exec.Start(s.hub, cfg.HostServer, panelUpdateCaller, protocol.ExecTask{
		Cmd:     "setsid nohup bash /root/moss-panel-update.sh >/dev/null 2>&1 < /dev/null & echo started",
		Timeout: 60,
	})
	if err != nil {
		writeErr(w, 400, "启动更新失败: "+err.Error())
		return
	}
	s.panelUpd.begin(info.Version, jobID)
	log.Printf("面板更新已下发: %s → %s（机器 %s）", serverVersion, info.Version, cfg.HostServer)

	writeJSON(w, 200, map[string]string{
		"message": "更新已开始，面板将在替换后重启；若新版本未能启动会自动回滚到当前版本",
		"target":  info.Version,
	})
}

// shellQuote 把字符串包成单引号字面量，内部单引号按 '\” 转义。
// 容器名与镜像名来自配置，不该有奇怪字符，但拼进 shell 的东西一律不裸奔。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

/* ---------- 版本比较与更新判定 ---------- */

// updateAvailability 描述当前版本与目标版本的关系。
type updateAvailability struct {
	// Action 取值：none（已是最新）/ update（升级）/ downgrade（降级）/ unknown（无法比较）
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
}

// compareToCurrent 判断目标版本相对当前版本是升是降。
//
// 降级必须与升级分开说：切到正式版通道时，最新正式版可能比当前的测试版更旧，
// 那时点下去是**回退**。都叫「更新」会让人以为在往前走，实际在往回退。
func compareToCurrent(target string) updateAvailability {
	cur := strings.TrimSpace(serverVersion)
	if cur == "" || cur == "dev" {
		return updateAvailability{Action: "unknown", Reason: "当前为开发版本，无法与发布版本比较"}
	}
	if target == "" {
		return updateAvailability{Action: "unknown", Reason: "未取到目标版本"}
	}
	switch c := compareSemver(strings.TrimPrefix(target, "v"), strings.TrimPrefix(cur, "v")); {
	case c > 0:
		return updateAvailability{Action: "update"}
	case c == 0:
		return updateAvailability{Action: "none"}
	default:
		return updateAvailability{Action: "downgrade", Reason: "该通道的最新版本比当前运行的版本更旧"}
	}
}
