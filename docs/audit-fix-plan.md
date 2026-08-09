# 全项目审查修复规划书

> 内部设计文档，不面向终端用户。
> 依据：2026-08-08 对 v2.0.0-beta.5（commit `f43bf5a`）的全项目审查。

---

## 接手须知（2026-08-08）

**当前状态：审查已完成，一行代码未改。工作区只有 `PanelUpdateTab.tsx` 一处无关的布局改动（纯样式，已确认无问题）。**

审查覆盖：server 全部 22 个 go 文件、agent 全部 13 个、web/src 全部、`deploy/` 四个文件、工具链。

基线（本机 go1.26.4 实测）：

| 项 | 结果 |
|---|---|
| `go build` / `go vet ./...` | 干净 |
| `go test ./...` | 全过（agent 7.2s / server 3.7s） |
| `tsc --noEmit` | 干净 |
| `go test -race` | **本机跑不了**，缺 gcc（`-race` 需 CGO） |
| 覆盖率 | 35.8%；`auth.go` / `collect.go` / `gcp_autostart.go` 均为 **0%** |
| `govulncheck` | 1 项 stdlib（GO-2026-5856，升 go1.26.5 即修） |

**结论：没有会导致进程崩溃的缺陷。** WebSocket 并发写全部串行化、channel 纪律干净、SQLite 配置正确（WAL + busy_timeout + 事务有回滚）、无 SQL 注入、无 panic 面、时间单位自洽。问题全部集中在**边界与失败路径**。

---

## 一句话灵魂

**把已经击穿的闸门补上，把静默失败变成会叫的失败。** 不重构，不加功能。

---

## Non-Goals（明确不做，避免范围蔓延）

| 不做 | 理由 |
|---|---|
| 不重构 exec 链路，不做 job 持久化 | 服务端重启丢 job 只改「错误提示指向正确方向」（回退查审计表），持久化收益不抵复杂度 |
| 不把命令黑名单改成白名单 | `exec_guard.go` 头注释的定位是对的——它拦手滑，不是安全边界。真边界是 Key 作用域 + agent 侧总开关 |
| 不对抗定向攻击 | 变量间接（`X=/dev/sda; dd of=$X`）、base64 解码执行、`$IFS` 花招一律不管。要那种强度就得上白名单，与上一条冲突 |
| 不回填历史指标 | 网速口径修正后数字会变小，历史数据留一道台阶，不迁移 |
| 不改 API Key「`servers` 空 = 全部机器」语义 | 已确认是有意设计，UI 明示（`AiTab.tsx:255,275`、列表页显示「全部机器」）。审查中曾被误报为提权漏洞 |
| 不改「网速数字观感」 | 合计卡直接跳变 + 单台 Ticker 补间是已定稿的前端动画，与本次采集口径修正无关 |
| 不动首页卡片布局 | — |
| 不在本机装 gcc 跑 `-race` | 只加 CI job，本地不折腾工具链 |

---

## 批次划分

按「独立可发布 + 可独立回滚 + 一次只碰一侧」切分。**每批次跑完 G2/G3 闸门才进下一批。**

| 批次 | 主题 | 侧 | 提交数 | 依赖 |
|---|---|---|---|---|
| **1** | exec 闸门补漏 | server | 1 | 无 |
| **2** | agent 修复（含一次 agent 发版） | agent | 3 | 无 |
| **3** | 重连时序 + 离线告警重建 | server | 2 | 无 |
| **4** | shell 脚本 | deploy + panel_update | 2 | 无 |
| **5** | 通知链路 | server | 1 | 无 |
| **6** | P2 收尾 | server + web | 若干 | 前五批 |

各批次无相互依赖，但**建议按序**：1 是安全边界，2 是可用性杀手，3 是告警可信度，之后才是打磨。

---

## 批次 1 — exec 闸门补漏

**问题**：四条缺陷同根同源，全部已用一次性测试实测放行。

| 位置 | 实测放行输入 |
|---|---|
| `exec_guard.go:60-66` | `iptables -L; iptables -F`、`nft list ruleset; nft flush ruleset`、`ufw status \| grep x; ufw disable`、`sudo iptables -S; sudo iptables -P INPUT DROP`、`iptables -L -n && iptables -P INPUT DROP` |
| `exec_guard.go:26,27,44` | `dd if=/dev/zero of="/dev/sda"`、`dd if=/dev/zero of='/dev/nvme0n1'`、`cat /tmp/x > "/dev/sda"`、`wipefs -a "/dev/sda"` |
| `exec_guard.go:22` | `cd / && rm -rf *`、`rm -rf --no-preserve-root ///` |
| `server/exec.go:223` | `{"cmd":"rm -rf *","dir":"/"}` |

**根因**：三个各自独立的实现疏漏，不是取舍。

1. `firewallReadOnlyRe` 有 `^` 无 `$` → 以只读查询开头即整串放行。同文件 `sshdConfigReadOnlyRe:71-72` 带 `$`，行为正确——**同源对照证明这是漏写**（实测 `cat sshd_config; echo x >> sshd_config` 正确拦截）。
2. `dd`/重定向/`wipefs` 三条规则的 `/` 前后没有 `['"]?`，而同文件 `:22` 的 rm 规则**已经写了** `['"]?/['"]?`。
3. `prepare` 只把 `task.Cmd` 送进 `checkDestructive`，`task.Dir` 一路直达 `agent/exec.go:123` 的 `cmd.Dir`，不过任何闸。

### 设计

不去逐条加锚点、逐条补引号——那是打补丁，下一个变体照样漏。改成三层结构，从根上消掉整类绕过：

**第一层：归一化。** 匹配前先做一次预处理：剥掉路径 token 外围的 `'` `"`、折叠重复斜杠（`///` → `/`）、把换行归一成 `;`。这一层同时消掉引号类和 `///` 类，且让后面所有正则不必再自己操心引号。

**第二层：按段判定。** 把命令按 `;` `&&` `||` `|` 切成段，**每段独立**过全部规则。这样：
- 只读白名单天然变成「每段都必须是只读」，缺 `$` 锚点的问题从结构上消失，不靠加 `$` 打补丁。
- `iptables -L; iptables -F` 的第二段命中 `firewallToolRe` 且不命中只读白名单 → 拦。

**第三层：工作目录过闸。** `prepare` 里补两件事：
- `checkProtectedPath(task.Dir)` —— 复用现成函数，`dir` 指向 `/etc/ssh/` 之类直接拒。
- 新增「危险 cwd + 通配删除」规则：`task.Dir` 规范化后等于 `/` 或命中受保护前缀时，拒绝 `rm`/`find -delete`/`chmod`/`chown` 这类作用于相对路径的命令。这一条同时覆盖 `cd / && rm -rf *`（第二层切段后，`cd /` 段会被识别为 cwd 变更）。

**取舍记录**：切段会带来误伤面上升。`grep halt /var/log/syslog | wc -l` 这类无害命令，第二段 `wc -l` 不含危险 token，不受影响；但 `exec_guard.go:29` 的 `\b(shutdown|poweroff|halt)\b` 本来就误伤 `grep halt`，切段不会让它更糟也不会更好——**这条误伤单独在批次 6 处理**（给它加上「必须是命令首 token」的约束）。

### 验收（G3）

`server/exec_test.go` 新增三组用例，缺一不可：

- **链式组**：上表 5 条防火墙绕过 + `cat sshd_config; echo x >> sshd_config`（回归保护，本来就该拦）。
- **引号组**：上表 4 条 + 反向用例 `dd if=/dev/zero of=/tmp/x`（不该拦）。
- **cwd 组**：`{"cmd":"rm -rf *","dir":"/"}`、`{"cmd":"find . -delete","dir":"/"}`、`{"cmd":"rm -rf *","dir":"/tmp/build"}`（**不该拦**——这是正常用法，必须保住）。

现有 `TestCheckLockoutAllowsReadOnly` / `TestCheckDestructiveAllowsNormalCommands` 全部必须继续通过。**这批次的价值一半在测试**——现有测试只喂单条命令，所以这四个洞在 CI 上完全不可见。

---

## 批次 2 — agent 修复（一次 agent 发版）

三个提交，一次发版。agent 发版要升 7 台机器，所以合批。

### 2a（P0）exec 永久瘫死

**问题**：`agent/exec.go:91-92` 的 `defer r.done()` + `r.run(c, task)`。`run` 的出口是 `:174` 的 `for ck := range ch`，`ch` 只在两个 `pumpPipe` 都拿到 EOF 后关闭；而 stdout 管道写端要等**所有**继承它的进程退出。超时走 `KillTree()`，Unix 侧只对原进程组发信号：

```go
// exec_unix.go:43
syscall.Kill(-k.pgid, syscall.SIGKILL)   // 负号 = 仅原进程组
```

已 `setsid` 的子孙不在该组，不被杀，继续持有管道写端 → `ch` 永不关闭 → `run` 永久挂起 → `r.running` 永不回落。`execMaxConcurrent = 4`（`exec.go:20`），**累计 4 次后这台机器的 exec 永久返回「并发执行数已达上限」，只能重启 agent**。服务端只看到宽限超时，没有线索指向真因。

触发输入普通到不需要恶意：`setsid sleep 3600 &`、自行 `fork+setsid` 守护化的程序、部分 `xxx start` 脚本。`server/panel_update.go:503` 刻意写了 `>/dev/null 2>&1 < /dev/null` —— 说明这个坑被识别过，但 agent 侧没兜底。

**设计**：给排空加截止时间。`KillTree()` 之后启动一个 drain deadline（建议 5s）；到点仍未 `close(ch)`，就**主动关闭父进程侧的管道读端**，强制 `pumpPipe` 的 `Read` 返回错误而退出 → `ch` 关闭 → `run` 正常返回、释放并发槽。

选这个方案而不是「不等 ch、直接 return」，是因为后者会泄漏两个 `pumpPipe` goroutine 和管道 fd；关读端让它们干净退出。

**取舍**：这只治「槽不释放」，治不了「逃逸的子孙进程还在跑」。彻底的做法是每条命令一个 cgroup（`systemd-run --scope`），像 `upgrade_unix.go:14-24` 给回滚守护做的那样。**列为批次 6 的候选，本批次不做**——理由是它把 agent 绑死在 systemd 上，而 agent 要支持非 systemd 环境。

**验收**：`agent/exec_test.go` 新增用例，起一个 `setsid` 的长命子进程后超时，断言 (1) `run` 在 deadline + 余量内返回、(2) `r.running` 回落到 0、(3) 连续 5 次后第 5 次仍能受理。现有 `appendLoopCmd` 用例（同进程组孙进程）必须继续通过。

### 2b（P0）自升级下载源

**问题**：`agent/upgrade.go:199-206` 的 `releaseURLs` 从同一个 `base` 拼出二进制和 `SHA256SUMS`；`downloadTo:208` / `verifySum:244` 只判非空，**不强制 https、无 host 白名单**。`base` 源头是 `MOSS_RELEASE_BASE`（`server/upgrade.go:65-70`）。

校验和与二进制同源 → 只能证明「传输没坏」，证不了「二进制是官方的」。而 `agent/main.go:196-204` 的注释是这条的自我否证：

> 刻意不检查 allow：……升级只能升到 server 当前版本、从**固定地址**装带校验和的**官方**二进制……⚠️ 这条成立的前提是「特权保持受限」。一旦升级支持**自定义下载源**，它就变成了任意代码分发通道，那时必须补上 allow 检查

`BaseURL` 早已是自定义下载源，前提不成立，allow 检查没补。现实路径：为境内机器把 base 指向 http 镜像（ghfast 那类，记忆里确实用过）→ 中间人同时换二进制与 SHA256SUMS → agent 以 root 装上并重启 → **覆盖所有机器，包括没开 `--allow-exec` 的**。

**设计（分两步，本批次只做第一步）**：

- **本批次**：agent 侧强制 `https://`，拒绝一切其他 scheme，无例外开关。理由是「无例外」——任何 `MOSS_RELEASE_ALLOW_INSECURE` 之类的逃生门都会被人打开然后忘掉。境内镜像走 https 是能做到的，做不到的场景应该手动装。
- **批次 6 候选**：release.yml 用 ed25519 给 `SHA256SUMS` 签名，公钥编进 agent，`verifySum` 先验签再比对。这才是真正让「敌意镜像也无法伪造」的解。不放进本批次是因为它要改 CI 与发版流程，风险面与代码修复不同源，该单独走一遍。

**不做**：不给升级加 `--allow-exec` 检查。那会让没开 exec 的机器再也无法一键升级，是用户能感知的功能退化；正确做法是恢复「来源可信」这个前提，而不是收紧权限。

**验收**：`agent/upgrade_test.go` 新增 `http://`、`ftp://`、无 scheme 三种 base 均被拒的用例；`https://` 正常路径回归。

### 2c（P1）采集口径

三处，都在 `agent/collect.go`：

**网速把虚拟网卡全算进去**（`:209`）：

```go
counters, err := gnet.IOCounters(false)   // false = 不分网卡，聚合全部接口
```

`pernic=false` 让 gopsutil 对所有接口求和，含 `lo`、`docker0`、`veth*`、`tun*`/`wg*`。容器出网流量在 `eth0` 和 `veth` 上各算一次，本机回环调用也算「网速」。**装了 Docker 的机器必现**，包括跑面板那台。

改 `IOCounters(true)` 后按名字过滤：`lo`、`docker*`、`br-*`、`veth*`、`virbr*`、`tun*`、`tap*`、`wg*`、`zt*`，Windows 再加 `Loopback*`、`vEthernet*`。

**取舍**：纯 VPN 出网的机器主网卡可能就叫 `tun0`，过滤后网速变 0。兜底：**过滤后如果一张网卡都不剩，退回聚合全部**。另外 `TotalUp`/`TotalDown` 口径变了，历史曲线会有一道台阶——按 Non-Goals 不回填，但要写进 CHANGELOG（面向用户的说法：「网速不再把本机回环与容器内部流量算进去，数字会比之前小，是修正而非下降」）。

**挂起的文件系统会冻结全部指标**（`:23-27,40`）：`disk.Usage` 是纯阻塞 `statfs(2)`，在上报主循环 goroutine 里同步调用（`main.go:226-230`），无超时、不可中断。

**这条的范围要说清**：审查初稿说「失联 NFS 会卡死」，我查了 `gopsutil@v4.26.5` 源码后**推翻了 NFS 部分**——`parseFieldsOnMountinfo:373` 用 `!strings.HasPrefix(mntSrc, "/")` 滤掉 `host:/export`，`parseFieldsOnMounts:316` 用 `/proc/filesystems` 非 `nodev` 白名单，NFS 和 sshfs 都进不来。**但 CIFS/SMB 的挂载源是 `//server/share`，以 `/` 开头，能穿过 mountinfo 那道过滤**，而 `skipFs` 没列 `cifs`。所以真实触发面是 CIFS/SMB 断连，比初稿窄得多，级别从高降到中。

改两处：`skipFs` 补 `cifs`/`smbfs`/`fuse.sshfs`；`diskTotals` 挪进带超时的独立 goroutine，超时则沿用上一拍的值并记日志。

**config 消息丢新留旧**（`main.go:175-184`）：`intervalCh`/`tasksCh` 容量都是 1，`select` + `default` 是「发不进去就丢弃本次」——丢的是新值，留的是旧值，语义反了。改成先排空再塞入。触发条件是连续改配置或重连补发，后果是配置静默不生效且无日志。

### 发版风险（G4）

本批次改的正是升级链路本身。**先在一台机器上手动装新 agent 并验证，确认无误再批量一键升。** 若 2b 的 https 强制有 bug，会导致所有 agent 再也升不上去——回滚手段是手动跑 install 脚本装回，需在规划确认后先演练一次。

---

## 批次 3 — 重连时序 + 离线告警重建

### 3a（建议提到 P0）面板重启后已离线的节点永远不告警

**问题**：`notify.go:325-341` 的 `Run()` 只遍历 `n.states`，而条目只能由 `OnOnline`/`OnOffline`/`OnReport` 创建，三者全由 WS 连接事件触发（`hub.go:117-145`）。`main.go:88-100` 启动时不做状态重建，`servers.last_seen` **全库唯一的读取点不存在**（只在 `hub.go:142` 写）。

节点 A 已离线 → 面板重启（**含面板自更新成功后的那次重启**）→ A 从未在新进程注册过 → 「🔴 离线」永远不发；A 恢复后 `wasAlerted` 为 false → 「🟢 恢复」也不发。**一台死机彻底静默。**

`gcp_autostart.go:57-58` 的注释证明这个缺陷被识别过，但只在 GCP 那条路上绕开了：

> 不挂在离线告警块内：那里受 OfflineOn 开关控制，且依赖 WS 断连事件，**面板重启后会漏掉已死节点**。

告警引擎自己没有这层兜底。

**为什么建议提到 P0**：这让「机器死了」这件事彻底静默，而这是监控系统存在的唯一理由。其余 P0 是「闸门能被绕过」，这条是「核心功能在常见场景下不工作」。

**设计**：启动时按 `servers.last_seen` 播种 `n.states`，`offlineSince = max(last_seen, 进程启动时间)`。

取 `max` 是关键决策：
- 不用 `last_seen` 裸值——否则面板自己停机一周后重启，会为所有机器立刻补发一大堆离线告警。
- 不跳过播种——那就是现状（永远静默）。
- 取 `max` 的效果是：面板启动后给 `OfflineDelay` 的重连窗口，仍没连上的才告警一次。**不刷屏、不静默。**

**验收**：`notify_recover_test.go` 新增三例：(1) 播种后 `OfflineDelay` 内重连 → 不告警；(2) 播种后一直不连 → `OfflineDelay` 后告警一次且只有一次；(3) 面板停机远长于 `OfflineDelay` → 仍只在启动后延迟一个窗口才告警，不立即刷屏。

### 3b（P1）三处重连时序

- **`agent_ws.go:87-94` `OnAgentGone` 不校验连接身份**：相邻的 `UnregisterAgent(serverID, ac)` 内部有 `h.agents[id] != c` 判断，`OnAgentGone(serverID)` 没有，按 serverID 扫全表收敛。agent 抖动重连后立刻下发的新 exec 会被旧连接的 defer 打成「执行期间 agent 掉线」，而命令实际在正常执行。**改**：给 `OnAgentGone` 传入连接身份，与 `UnregisterAgent` 用同一个判断。

- **`hub.go:131-145` `UnregisterAgent` 的副作用在锁外无条件执行**：锁内有「仍是活跃连接才继续」的判断，但 `h.mu.Unlock()` 之后的 `db.Exec`、`OnOffline`、`broadcast` 三个副作用没被覆盖。旧连接的 `OnOffline` 会把已被 `OnOnline` 清掉的 `offlineSince` 重新写上，而 `Notifier.Run:335-340` 只看 `offlineSince` 不看 hub 在线状态 → **推一条假的「🔴 离线」，且一直挂着直到下次重连再补一条假的「🟢 恢复」**。**改**：把「是否仍是活跃连接」的结论带出锁，三个副作用一并受它保护。

- **`exec.go:255-256` + `:300-303` `remember` 晚于 `unregister`**：`await` 的 `defer m.unregister(job.id)` 先执行，`remember` 在其后。窗口内两张表都查不到 → `Result` 回 `found=false` → `toolGetResult:704` 回措辞很确定的「jobId 有误，或结果已超过 30 分钟保留期」。**命令已成功执行**，模型据此放弃或重跑，对非幂等操作是真实危害。**改**：先 `remember` 再 `unregister`，或两者同一把锁内完成。

**验收**：3b 三条各补一个时序测试。异步 jobId 生命周期目前**零覆盖**（`Start`/`Result`/`remember`/`get_result` 全无测试），这批要把这块空白填上。

---

## 批次 4 — shell 脚本

### 4a `panel_update.go` 内嵌脚本

**`$ARGS` 不加引号削弱 `--trusted-proxies`**（`:404` 抓、`:422` 展开）：

```bash
ARGS=$(docker inspect "$C" --format '{{range .Args}}{{.}} {{end}}')
# shellcheck disable=SC2086
if ! docker run -d --name "$C" ... "$IMG" $ARGS; then
```

`shellcheck disable` 说明分词是刻意的（`$PORTS`/`$VOLS` 需要），但同一次分词会拆散任何含空格的**单个** argv。`deploy/moss.sh:162` 用 `read -rp` 让用户自由填可信代理名单，`:135` 以 `--trusted-proxies "$proxies"` 作为单个 argv 传入。用户填 `1.2.3.4, 5.6.7.8`（逗号后带空格，最自然的写法）→ 新容器拿到 `--trusted-proxies 1.2.3.4,` 加一个被 flag 丢弃的游离参数 → 边缘节点从可信名单消失 → XFF 取值退化 → **限流与登录锁定可被伪造头绕过**。`parseTrustedProxies`（`main.go:47-59`）对空项静默跳过，**无任何日志**。且不可自愈——下次更新读到的 `.Args` 已是拆散版。

**改**：用 NUL 分隔读成 bash 数组。不引 jq——脚本跑在**宿主机**（`panel_update.go:505` 经 exec 下发 `bash /root/moss-panel-update.sh`），不能假设装了 jq：

```bash
ARGS=()
while IFS= read -r -d '' a; do ARGS+=("$a"); done \
  < <(docker inspect "$C" --format '{{range .Args}}{{.}}{{printf "\x00"}}{{end}}')
docker run ... "$IMG" "${ARGS[@]}"
```

用 `while read -d ''` 而不是更短的 `mapfile -d '' -t`：后者要 bash 4.4+（2016），而 CentOS 7 一类宿主机是 bash 4.2。脚本头是 `#!/usr/bin/env bash` 且由 `bash` 显式调用，所以 bash 有保证，但版本没有。

必须用进程替换 `< <(...)` 而非命令替换 `$(...)`——后者会吞掉 NUL 字节。

`$ENVS`（`:403`）与 `$VOLS`（`:402`）同类问题（含空格的 env 值或 bind 路径会让 `docker run` 失败并触发回滚），一并改成数组。

**多端口容器误回滚一次成功的更新**（`:431`）：

```bash
PORT=$(docker inspect "$C" --format '{{range $p,$b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}}{{end}}{{end}}' | head -c 10)
```

模板对所有绑定**无分隔符拼接**。两个端口（`-p 8787:8787 -p 9090:9090`，或同端口绑两个 HostIp）→ `PORT=87879090` → 非空所以不走 8787 兜底 → `curl http://127.0.0.1:87879090/` 必失败 → 20 次探测后回滚。新镜像其实是好的，日志只说「40 秒内未就绪」。**改**：模板加空格分隔 + `awk '{print $1}'`。

**`busy()` 与 `begin()` 非原子**（`:456` 检查、`:511` 占位）：中间隔着 GitHub 查询与 `SubmitWrite`（秒级窗口）。`panelUpdater.begin:337` 没有持锁复查——对比 `upgrade.go:318-324` 就补了这道。两个管理员同时点，各起一份脚本；交错时序下脚本 B 会 `docker rm -f "${C}-old"` **删掉 A 刚做的唯一备份**。**改**：`begin` 内持锁复查并返回是否抢到，抢不到就退出。

### 4b `deploy/moss.sh`

- **`:181-193` 安装失败会把运行中的面板搞下线**：删容器在拉镜像**之前**。用户选「重新创建」后网络抖动导致 pull 失败 → 容器已删、新的没起来，从「运行中」直接变「未安装」。`do_update:247-248` 顺序是对的（先 pull 后 rm），说明这是 `do_install` 单独写错。**改**：对齐 `do_update` 的顺序。
- **`:224-227` 会显示一个错误的管理员密码**：用「数据卷是否存在」判断是否首次安装，但卷可能存在而库里没密码（上次 `docker run` 失败已创建卷）。这时走 `fresh=0` 分支打印上次记录的旧密码，而服务端此刻会自己生成新随机密码（`auth.go:84-87`，只打到 docker 日志）。用户拿屏幕上的密码登不进去。**改**：判据换成「库里有没有 password hash」，或首次启动后从 `docker logs` 抓生成的密码。
- **`:248` / `:322` 无回滚**：`docker rm -f` 后若 `start_container` 失败，只打印「更新失败」，容器已消失。**改**：先 `docker rename` 保留旧容器，起新的成功后再删，失败则改名回来——与 `panel_update.go` 已有的回滚策略对齐。
- **文档与代码矛盾**：`moss.sh:149` 和 `docker-compose.yml:24` 都写「按**最左** XFF 取真实访客 IP」，而代码是从**最右**往左跳过可信代理（`main.go:27`、`ratelimit.go:30-53`）。最左 XFF 是客户端可伪造的。`nginx.example.conf` 和 `moss.sh:124-125` 写的是对的——同一个文件里两处注释自相矛盾。**改**：删掉错的两处。

---

## 批次 5 — 通知链路

- **`gcp_autostart.go` 五处告警绕过统一出口**（`:107`/`:156`/`:164`/`:169`/`:180` 直接调 `n.send`）。`notify.go:95-98` 的约定写得很清楚：「`fire` 是所有告警的唯一出口……统一出口而非在每个告警点分别调两次，是为了避免新增通道时漏改某个分支」。**注释预言的事真的发生了**：只配 webhook 没填 TG token 的用户，GCP 守护告警完全静默——Spot 被抢占后连开机失败都无人知晓，而这恰是该链路最该覆盖的场景。**改**：五处全部走 `fire`。
- **`notify.go:455` TG bot token 随传输错误进日志**：token 拼在 URL 里，`PostForm` 失败返回的 `*url.Error` 的 `Error()` 含完整 URL，`:448-450` 直接 `log.Printf("%v", err)`。境内机器连 api.telegram.org 超时是常态，**这是常见路径不是边缘路径**。`handleTestNotify:517` 还把 err 原文写进 HTTP 响应体。同类：`webhook.go:96` 会打印完整 webhook URL——钉钉/企微/飞书的 token 就在 query string 里。**改**：加一个错误脱敏函数，把 URL 里的 token 段与 query string 替换掉，两处共用。
- **`notify.go:196-200` 关掉再打开告警开关留下 stale `highAlerted`**：`Reload():124-135` 只换 `cfg` 不重置 `states`。已触发过网速告警 → 关掉 NetOn → 再打开 → 新的超阈值**不再告警**，直到跌破回差线凭空发一条「✅ 网速恢复」。改阈值同理。**改**：`Reload` 时按开关与阈值变化清理受影响的 `highAlerted` 项。

---

## 批次 6 — P2 收尾

不逐条展开设计，列清单与判据：

**服务端**：`/mcp` 纳入限流（`ratelimit.go:159` 的 `/api/` 前缀判断）· `get_result` 补作用域校验（`exec.go:119` 的 `finishedJob` 要存 `serverID`）· `OnResult` 校验回传方身份（`job.serverID` 字段已存在，比对是一行）· 审计写失败改 fail-closed · 浏览器 WS 补 ReadDeadline + PongHandler（对齐 `agent_ws.go:97-102`）· 登录锁定改成单临界区（`auth.go:122-141` 的 check-then-act）· 锁定期结束后清零 `fails`（`auth.go:42-53` 的自锁死循环）· `secret.go:50-64` 加密失败改 fail-closed · TG token 与 webhook secret 补 `encryptSecret`（`decryptSecret` 对无前缀值透传，不需要迁移脚本）· `exec.go:186` `Result` 判过期 · `exec_guard.go:29` 的 `halt` 误伤加「命令首 token」约束 · `mcp_types.go:70` 的 `id,omitempty` 改成显式 `null` · 批量请求回 `-32600` · 服务端重启后 `get_result` 回退查审计表 · `upgrade.go:336` 的永久红标 · `gcp.go:149` 的 `expires_in` 缺失 · `agent/write.go:89` 覆盖写把 0600 放宽到 0644

**agent**：`collect.go:63` 按字节切首字符 · `collect.go:251-268` 采集失败用零值代替「不可用」 · `main.go:145` geoip 同步阻塞首次 register 最坏 15 秒

**前端**：`AuditTab.tsx:57-71` 筛选竞态（加 AbortController 或请求序号）· `store.ts:106-114` 重连加指数退避 + jitter · `ServerDetail.tsx:112-128` 隐藏页签停止轮询 · 四处保存按钮补 busy 门控（`NotifyTab:161`、`SettingsTab:107/146`、`GcpTab:133`）· `AiTab.tsx:273` 有效期天数加上限

**工具链**：升 go1.26.5（清 GO-2026-5856）· CI 加 Linux `go test -race`

**候选（要单独评估，不默认做）**：
- agent exec 改 cgroup 隔离（`systemd-run --scope`）——彻底解决进程逃逸，但把 agent 绑死 systemd
- release 加 ed25519 签名——让敌意镜像也无法伪造，但要改 CI 与发版流程

---

## 每批次的闸门（G2 / G3）

复用全局交付闸门，逐批执行，任一 NO 即阻断：

**G2 准出**：`go vet ./...` 零告警 · `tsc --noEmit` 零错误 · 零 TODO · 零吞异常 · 本批次新增代码无资源泄漏

**G3 准出**：本批次列出的验收用例全部通过 · `go test ./...` 全过 · 覆盖率不低于改动前 · 本批次触及的失败路径有显式降级行为（不是静默返回零值）

**发版前（G4）**：CI 的 `-race` 通过 · 批次 2 先单机验证再批量升 · 批次 4 的回滚路径实际演练一次

---

## 覆盖率现状（作为批次进展的度量）

危险的不是 35.8% 这个数字，是它的分布：

```
  0.0%  server/auth.go          ← 整站的门
  0.0%  server/gcp_autostart.go
  0.0%  agent/collect.go        ← 指标算错等于监控没用
  0.0%  agent/probe.go
 10.0%  server/agent_ws.go
 17.5%  server/admin_api.go
 26.2%  server/ratelimit.go
```

`exec_guard.go` 96%、`mcp_tools.go` 82% 说明关键路径是被有意识地测过的——但 `exec_guard.go` 的 96% 覆盖率下藏着 11 个实测可绕过的洞，**因为用例只喂单条命令**。这是覆盖率作为指标的局限，也是为什么每批次的验收都要写明「必须覆盖哪个具体的绕过用例」，而不是只看百分比。

异步 jobId 生命周期零覆盖，无任何测试跨越「agent 重连」时序——批次 2、3 要把这两块空白填上。

---

## 待拍板

1. **批次 3a（面板重启后离线永不告警）是否提到 P0？** 建议提——它让核心功能在常见场景下彻底静默。
2. **批次顺序是否按 1→6 执行？** 六个批次之间无技术依赖，可调整。
3. **批次 6 的两个「候选」是否纳入本轮？** 都是从根上解决问题的方案，但一个绑 systemd、一个改发版流程，风险面与其余修复不同源。
