# AI 运维接口设计

> 内部设计文档，不面向终端用户。

---

# 接手须知（2026-08-04）

**当前进度：v2.0.0-beta.1 已发布上线，核心链路已在真机验证通过，正在往 beta.2 推进。**

## 环境速查

| 项 | 值 |
|---|---|
| 面板地址 | `https://jk.20051212.xyz`（**同时是 README 里的公开演示地址**） |
| MCP 端点 | `https://jk.20051212.xyz/mcp` |
| 接入方式 | 已加到 Claude Code 用户级配置（`claude mcp add --scope user`），全项目可用 |
| 密钥范围 | 全部 7 台机器，读/执行/写三项能力齐全 |

七台机器（`list_servers` 可查最新状态）：

| ID | 名称 | 系统 | agent 版本 |
|---|---|---|---|
| `uQ8B6wha` | TW NGINX SERVER | Debian 13 | **已升 beta.1 ✅ 已验证** |
| `NhjbVUHB` | United States WEB SERVER | Debian 13 / aarch64 | 升级中 |
| `TNeWg5XB` | JP NGINX SERVER | Debian 13 | 升级中 |
| `T5s5WDHB` | HK PROXY SERVER | Debian 13 | v1.4.0 待升 |
| `TMVEKaQa` | KR PROXY SERVER | Debian 13 | v1.4.0 待升 |
| `FwCxdg8F` | GUANGZHOU DNS SERVER | Debian 13 | v1.4.0 待升（**境内，GitHub 下载会超时**） |
| `AkijA4Zu` | JP SCUM SERVER | **Windows Server 2022** | v1.4.0 待升 |

**moss server 自身部署在哪台机器上尚未确认** —— 这是设计「用 moss 部署 moss」自举路径的前提，接手后需先问用户。

## 已在真机验证通过

在 `uQ8B6wha`（台湾）上实测：

- 读取：`list_servers` / `get_metrics` / `get_history` 全部正常
- 执行：异步下发 + 轮询取回，往返 152ms，退出码与 stdout 正确
- 拦截：`iptables -A INPUT -j DROP` 被服务端拦下，未下发
- 不误伤：`iptables -L INPUT -n` 正常返回规则表

## 待办（按建议优先级）

### 一、beta.2 必修的三个坑（实机踩出来的，剩余机器会重复踩）

1. **安装脚本重写 `/etc/moss-agent.env`**（`server/install/install.sh`）：
   `install -m 600 /dev/null` 先清空文件，再用 `tee`（非 `-a`）只写回 token，
   **用户自定义的 `MOSS_ALLOW_EXEC=1` 会被静默抹掉**，且无任何提示。
   修法：重写时保留非 `MOSS_TOKEN` 行，或把 token 拆到独立文件。
2. **`latest` 不含预发布**：脚本默认走 `/releases/latest/download/`，
   而 GitHub 的 `latest` 按定义跳过 Pre-release，**静默装回 v1.4.0 还提示"已安装"**。
   装 beta 必须显式 `MOSS_VERSION=v2.0.0-beta.1`。
   修法：文档写明，或脚本检测到装入版本低于当前运行版本时告警。
3. **`list_servers` 缺 `agentVersion` 字段**：库里有这个数据，工具没返回。
   导致 AI 排查执行失败时看不到最关键信息，只能靠超时时长反推。

### 二、剩余 5 台 agent 升级

正确顺序（**先装后配，反了会被脚本抹掉**）：

```bash
MOSS_VERSION=v2.0.0-beta.1 bash <(curl -fsSL https://jk.20051212.xyz/install.sh) \
  --endpoint https://jk.20051212.xyz --token <该机 token> \
  && echo 'MOSS_ALLOW_EXEC=1' >> /etc/moss-agent.env \
  && systemctl restart moss-agent && cat /etc/moss-agent.env
```

### 三、从未在真机验证的代码路径 ⚠️ 风险最高

- **Windows 执行能力**（`agent/exec_windows.go`）：Job Object 终止进程树、
  `cmd.exe` 的 `CmdLine` 转义——只跑过本地单元测试，**没在真 Windows Server 上验证过**，
  而这是整个 agent 里最容易出问题的代码。目标机 `AkijA4Zu`。
- **`write_file`**：一次真机都没跑过。
- **告警推送**：用户未配任何通知渠道，`fire()` 的 webhook 分支从未在生产触发。

### 四、文档与发布物

- `README_EN.md` 仍是旧文案，中英不一致
- 首页截图 `docs/home.png` 无「AI 接入」页签
- **beta 安装说明缺失**：必须写明装测试版要指定 `MOSS_VERSION`

### 五、功能

- **agent 自动升级**：走专用协议消息（不走 exec 通道，否则与黑名单冲突）、
  分批推送、失败自动回滚。注意升级动作必须 `setsid` 脱离 agent 进程树，
  否则 agent 重启时进程组一杀，升级到一半就断了。
- **用 moss 部署 moss**（需先确认 server 部署位置）

### 六、安全

- `/mcp` 端点挂在公开演示域名上，建议加 IP 白名单或换域名

## 不要推翻的既有决策

接手后如果想"改进"以下几点，先读对应章节的理由，这些都是权衡后定的：

| 决策 | 理由所在 |
|---|---|
| **不做人工确认闸** | 见《为什么没有人工确认闸》——能救的直接放行，救不了的硬拦，中间没有地带 |
| **不做 WebSSH** | 与命令拦截互斥，PTY 是字符流不是命令流，拦不住 |
| **黑名单不提供批准入口** | 留批准按钮意味着一次误点即灾难，而人自己上机器成本仅两分钟 |
| **只推拦截、不推正常执行** | 逐条推送会让通知沦为噪音，两天后就被关掉 |
| **MCP 基线定在 legacy（2025-11-25）** | 2026-07-28 是断代改动，现网客户端生态都在 legacy 代 |
| **agent 执行能力默认关闭** | 装了 agent 不等于同意被远程操作 |

---

## 一句话灵魂

**AI 到服务器的唯一连接中枢——手从这里伸出去，闸设在这里，痕留在这里。**

AI 有脑子没有手。给它一把 SSH 私钥，它就有了手但没有闸——模型会非常自信地在 `/` 下面执行本该在项目目录跑的清理命令。moss 同时给出手、闸，和每一次操作的完整痕迹。

**"中枢"的含义是多对多**：N 个 AI 客户端 × M 台服务器，全部连接收敛到一个点，鉴权、守门、留痕只在这一个点做。只服务单个 AI 的是工具，不是中枢——这条直接决定了 Key 体系必须从第一天就支持多客户端并发接入。

## 为什么是 moss

moss 已经具备三样别人没有的东西，这个功能是它们的自然延伸，不是另起炉灶：

1. **每台机器上已有常驻 agent 和 WS 长连接**（`server/agent_ws.go`）——下行通道现成，不用存私钥，不用目标机开 22 端口，NAT 后面的机器照样能控。
2. **已有实时指标**——AI 部署完能立刻自查 CPU/内存/磁盘是否正常，而不是盲发命令。
3. **已有通知系统**（`server/notify.go`）——高危操作需要人点头时，推送通道是现成的。

## Non-Goals（第一版明确不做）

| 不做 | 原因 |
|---|---|
| 交互式 PTY / Web 终端 | **与闸 2 互斥，不是取舍问题。** PTY 是字符流不是命令流——`rm -rf /` 可被拆成任意分片到达、可由 tab 补全拼出、可从 history 翻出，命令拦截在 PTY 上无法实现。做了 WebSSH，两道闸和审计全部作废，moss 退化为带界面的 SSH 转发器。只有一问一答的 API 才有完整、可判定、可留痕的命令 |
| 应用编排 / Docker 管理 | 不与 Portainer、Coolify、Ansible 正面竞争。AI 自己会写 `docker compose up`，不需要我们再包一层 UI |
| 真 SSH（server 当 SSH 客户端） | 要替用户保管一堆私钥，攻击面剧增。只走 agent 隧道 |
| 管理没装 agent 的机器 | 装 agent 是前提，不是障碍 |
| 文件浏览器 / SFTP | 第一版只要"写配置文件"这一个具体场景，不要通用文件管理 |

## 拓扑

**星型，不是链式。** moss 是唯一的 MCP server，所有 AI 客户端平级接入：

```
Claude Code ─┐
             ├─► moss MCP Server ─► WS 隧道 ─► agent ─► 服务器
OpenClaw   ─┘        （闸 + 审计）
```

分工按**在场与否**切，不按环节串：

- **人在场**：Claude Code 直接调 moss，写完代码自己部署、自己看日志、自己修。链路最短。
- **人不在场**：OpenClaw 值守。盯监控数据，出问题自己处置，处置不了在聊天窗口叫人。

链式（Claude Code → OpenClaw → moss）被否决：多一跳只增加信息损耗，AI 转述 AI 会让指令失真，且 OpenClaw 的真正价值是**常驻与可达**，不是当中继。

**设计推论：接口必须从第一天就支持多 key 并发接入，每个 key 独立绑定权限范围**，而不是做一个"给某个 AI 用的接口"。

## 值守闭环：告警 push，处置 pull

OpenClaw 的职责是**盯着 moss 的运行状态做维护**，这带出一个 MCP 本身解决不了的问题：**MCP 是客户端拉取模型，server 无法主动叫醒 AI。** 靠 OpenClaw 定时轮询 `get_metrics` 不可行——延迟高，且每次轮询都在烧 token，绝大多数轮询什么也没发生。

所以值守链路必须是双向的，两个方向用不同机制：

```
出事时：moss 告警检测 ──webhook push──► OpenClaw   （叫醒）
被叫醒：OpenClaw ──MCP 工具调用──► moss ──► agent  （查因 + 处置）
处置后：OpenClaw ──Telegram──► 人                  （告知）
```

**告警检测逻辑已经全部现成**（`server/notify.go`）：`OnOnline` / `OnOffline` 管上下线，`OnReport` 管 CPU、内存、磁盘、网速阈值，`checkExpiry` 管到期。不需要重写任何检测逻辑。

**缺的只有一个：通用 webhook 通道。** 现有推送只实现了 Telegram（`notify.go:330 sendTelegram`），`send()` 已经是抽象层，加一个 webhook sender 即可，工作量很小。

不做成"OpenClaw 专用推送"而做成通用 webhook 的理由：告警载荷是标准 JSON（事件类型、机器 ID、指标、时间戳），任何 AI 网关都能接。绑死单一产品会在换工具时全部推倒重来。

## 人不在场时能做什么

没有确认闸之后，夜间行为按故障类型分三种，无需任何额外授权机制：

| 故障 | 处置 |
|---|---|
| 进程挂了（服务died、容器退出） | OpenClaw 直接重启，**这类命令本就在放行区** |
| 机器整个宕了 | 走已有的 GCP Spot 自动开机守护（`gcp_autostart.go`），不经过本方案 |
| 要改配置、回滚代码才能修的 | 等人。**这是正确行为，不是缺陷**——半夜改生产配置，判断错一次通常比多挂几小时更贵 |

第三类也不是干等：告警与只读查询不受任何限制，OpenClaw 被叫醒后可以查指标、翻日志、看进程，把诊断做完再推给人。**人早上看到的不是「服务挂了」，而是「03:12 服务挂了，原因是 X，建议做 Y」——MTTR 里最耗时的诊断部分已经在夜里完成。**

## MCP 工具清单（第一版四个）

传输走 Streamable HTTP（远程接入必须，stdio 只适用本地）；认证走 Bearer API Key。

| 工具 | 作用 | 所需能力 |
|---|---|---|
| `list_servers` | 列出机器：ID、名称、在线状态、系统信息 | `read` |
| `get_metrics` | 取指定机器实时指标，部署后自查 | `read` |
| `exec` | 在指定机器执行命令，返回 stdout/stderr/exitCode | `exec` |
| `write_file` | 写入文件（内容 + 路径 + 权限 + 属主） | `write` |

`write_file` 不用 `exec` + heredoc 替代的理由：AI 写 nginx 配置、compose 文件时，shell 转义是高频出错点，且内容混在命令行里无法有效审计。独立工具让"写了什么"可以原样留痕。

读文件不单独开工具，`exec` 加 `cat` 足够，且天然受输出截断保护。

## 两道闸

> **设计变更（原为三道闸）**：原方案有第三道「人类确认闸」——高危但合法的操作挂起、推送给人、确认后执行。该闸已取消，原因见下方《为什么没有人工确认闸》。

### 闸 1 · Key 作用域（静态边界）

每个 API Key 独立绑定：

- **机器白名单**：这把 key 只能碰哪几台（部署用的 key 碰不到生产机）
- **能力集**：`read` / `exec` / `write` 三选 N，最小授予
- **有效期**：可设过期时间，过期自动失效

### 闸 2 · 命令拦截（模式黑名单，硬拒绝）

在 server 侧拦截，不下发。命中即拒绝并记审计，**不提供绕过开关**——需要执行这类命令的场景，人应该自己上机器：

- 根目录递归删除：`rm -rf /`、`rm -rf /*` 及其变形
- 磁盘裸写：`mkfs.*`、`dd of=/dev/*`
- 系统关停：`shutdown`、`halt`、`init 0`（关机走 GCP 守护，不走这条路）
- Fork 炸弹
- 篡改 moss 自身：动 agent 二进制、agent 配置、agent systemd 单元

### 为什么没有人工确认闸

判断标准不是「危险不危险」，而是**「错了之后还能不能救」**。按这个标准，命令分两类：

- **可救的**（`systemctl stop nginx`、`docker system prune -a`、`chmod -R`）——危险，但重启、重拉、改回来即可。**直接放行**，留审计。
- **救不了的**——分两种，都硬拦，且**不提供人工批准入口**：
  - *不可逆*：格盘、删根、覆写块设备、动 agent 二进制。
  - *自断手脚*：改防火墙规则、改 SSH 配置或端口、停 SSH 服务、关网卡。执行完 agent 与 SSH 一并失联，**连补救的手都伸不进去**，只能上云控制台。

两类之间没有中间地带，因此不存在需要挂起等人的场景，确认闸自然消失。

**不给批准入口是刻意的。** 真需要格盘或改防火墙时，人自己 SSH 上去执行成本是两分钟；而留一个批准按钮，意味着某天半夜误点一次就是灾难。收益极小，风险极大。

附带的好处是链路上没有任何等待状态：AI 要么直接干，要么当场被拒并拿到明确原因，不会挂在「等人批准」里空转，也不会有超时、一次性确认令牌、待办列表这些机制要维护。**一个天天弹窗的确认框，等同于没有确认框**——与其做一个必然被无脑点掉的闸，不如不做。

### 拦截告警

**只有被拦截的操作会推送通知**（复用 `notify.go` 的 Telegram 通道）。AI 正常执行的命令留在审计里供事后查阅，不推送。

理由与不做确认闸一致：逐条推送会把通知变成噪音，两天后人就把它关了。而「AI 试图修改防火墙」这类事必须当场知道——它意味着模型的判断偏离了预期，是需要人介入复盘的信号，而不是一条日常记录。

命令拦截与路径拦截收敛到 `execManager.reportBlocked` 同一处，保证两条路径的留痕与告警行为一致。

### 审计（贯穿两闸）

每次调用落库并可在前端查看：key、目标机、完整命令或文件内容、时间、退出码、输出（截断存储）。审计不可删除、不可关闭。

## 协议扩展

`internal/protocol/protocol.go` 现有 `ServerMsg` 只有 `config` 一种类型，扩展如下：

```go
// server → agent
type ExecTask struct {
    ID      string // 幂等 ID，agent 侧去重
    Cmd     string
    Dir     string // 工作目录，默认 /root
    Timeout int    // 秒，硬超时后 agent 强杀进程组
}

// agent → server，输出分片回传
type ExecResult struct {
    ID       string
    Seq      int    // 分片序号，从 0 递增
    Stream   string // stdout / stderr
    Data     string
    Done     bool
    ExitCode int    // Done=true 时有效
}
```

### 必须处理的工程约束（已从现有代码确认）

1. **单条消息 64KB 上限**（`agent_ws.go:94` `SetReadLimit(64<<10)`）——输出必须分片，单片上限定为 32KB 留足余量。
2. **总输出截断**：单次执行回传上限 1MB，超出截断并明确标注，防止 `cat` 大日志打爆 server 内存和 AI 上下文。
3. **进程组强杀**：超时后 agent 必须杀整个进程组（`setpgid`），只杀父进程会留下孤儿。
4. **下行写入已有锁保护**（`agentConn.send` 持 mutex），并发下发安全，无需额外改造。
5. **60s 读超时不受影响**：心跳 ping 在独立 goroutine（`agent_ws.go:102`），长命令执行期间连接不会被误判断开。
6. **agent 侧总开关**：agent 配置需支持完全关闭执行能力，装了 agent 不等于接受被远程操作。

## 分阶段实施

每阶段独立可验证，完成后评审再进下一阶段。

| 阶段 | 内容 | 验收 |
|---|---|---|
| 1 | 协议扩展 + agent 执行端 + 分片/超时/强杀 + 审计落库；先用 REST 接口验证 | curl 能在指定机器跑命令拿到输出，超时能杀干净，审计有记录 |
| 2 | MCP Server + Key 管理（后端 + 前端管理页）+ 闸 1 + 闸 2 | Claude Code 接入后能列机器、执行命令；越权 key 被拒 |
| ~~3~~ | ~~闸 3 人类确认~~ | **已取消**，见《为什么没有人工确认闸》 |
| 3 | 通用 webhook 告警通道 + `get_history` 工具 | 人为制造一次故障，OpenClaw 被叫醒、查因、自愈、事后告知，全程无人干预 |
| 4 | Dogfooding：用本接口部署 moss 自身 | 完整闭环跑通，写进发版流程 |

阶段 1–2 服务"人在场"链路（Claude Code 直接部署），阶段 3 服务"人不在场"链路（OpenClaw 值守）。前者先落地，因为它更简单，且能作为后者的地基验证一遍执行层是否可靠。

`get_history` 放在阶段 3 而非第一版工具清单：值守排查的第一个问题永远是"从什么时候开始的、是突发还是渐变"，只给实时指标判断不了。但"人在场"链路不需要它——人自己会看图。

## 实施进度

### 已完成：阶段 1 上半（agent 执行端）

| 文件 | 内容 |
|---|---|
| `internal/protocol/protocol.go` | `ExecTask` / `ExecResult`；`ServerMsg` 增 `exec`，`AgentMsg` 增 `exec_result`；分片与超时常量 |
| `agent/exec.go` | 准入（开关/幂等/并发）、执行、超时、分片、截断、断线处理 |
| `agent/exec_unix.go` | `/bin/sh -c` + `Setpgid`，超时向进程组发 SIGKILL |
| `agent/exec_windows.go` | COMSPEC + Job Object，超时 `TerminateJobObject` |
| `agent/exec_test.go` | 7 项测试，Windows 与 Linux 双平台实测通过 |

实现中确认并处理的坑：

1. **输出触顶后必须继续读管道**——子进程写满管道缓冲区会永久阻塞，命令再也不退出。达到 1MB 上限后继续读并丢弃，只是不再回传。
2. **`timedOut` 必须用原子量**——写在定时器协程、读在 `Wait()` 之后，「杀进程 → Wait 返回」经由操作系统传递，不构成 Go 内存模型的 happens-before。
3. **Unix 侧 `Close()` 后禁止再发信号**——进程已回收，PID 可能被系统复用，继续发信号会误杀无关进程。
4. **Windows 的 `CmdLine` 必须以程序名开头**——`cmd.exe` 会跳过第一个 token 当 argv[0]；同时须绕开 `os/exec` 的 `CommandLineToArgvW` 转义，否则命令中的引号、`&`、`|` 会被悄悄改写。
5. **不调用 `Getpgid`**——命令秒退时它返回 ESRCH，会让本可正常收尾的执行被误判为失败。直接取 pid（`Setpgid` 保证 pgid == pid）。
6. **断线不杀进程**——部署跑到一半被中断，比拿不到输出更糟。断线只停止回传。
7. **`golang.org/x/sys` 提升为直接依赖**——原为 gopsutil 间接引入，不产生新的下载或体积。

**agent 侧默认关闭远程执行**，需 `--allow-exec` 或 `MOSS_ALLOW_EXEC=1` 显式开启。存量 agent 升级后默认仍是关闭状态，需改服务单元才会生效——这是有意的安全默认，发版说明须写明。

### 已完成：阶段 1 下半（server 执行端）

| 文件 | 内容 |
|---|---|
| `server/exec.go` | 任务下发、分片按 seq 重组、收敛（正常/超时/掉线/取消）、审计读写 |
| `server/exec_guard.go` | 破坏性命令拦截 |
| `server/exec_api.go` | 执行接口 + 审计列表 / 详情接口 |
| `server/db.go` | `exec_audit` 表；审计保留期设置（默认 90 天）并入 `cleanupLoop` |
| `server/exec_test.go` | 10 项测试 |

**闸 2 提前到阶段 1 实现。** 原计划放在阶段 2，但执行接口一旦存在就是 RCE 入口，接口与拦截之间不能有裸奔的窗口期。

两条关键设计：

- **审计先于下发落库**，完成后再更新结果。只在完成后写，等于留了一个抹掉记录的窗口；被拦截的命令同样留痕——「谁试图执行什么危险命令」是审计里最有价值的记录。
- **agent 掉线立即收敛在途任务**，并如实告知「命令可能仍在目标机上继续运行」，而非谎称执行失败。

黑名单的定位在代码注释里写死了：它拦的是手滑和模型犯浑，不是定向攻击。真正的边界是 Key 作用域与 agent 侧总开关。因此测试里**防误伤的用例比防漏拦的更多**——误拦正常命令会让整个功能不可用，而这比漏拦更容易发生。

### 阶段 1 验收（已通过）

本地起 server + agent 端到端实测七项：

| # | 场景 | 结果 |
|---|---|---|
| 1 | 正常执行，stdout / stderr 分流 | 通过，稳定态往返 13ms |
| 2 | `rm -rf /` 拦截 | HTTP 403，留审计 |
| 3 | 超时强杀（60s 命令限时 2s） | 进程树终止，返回部分输出 |
| 4 | 非零退出码透传 | `exit 42` → `exitCode: 42` |
| 5 | 审计列表含被拦截命令 | 通过 |
| 6 | 执行中杀掉 agent | 3 秒内收敛返回，未干等 90 秒宽限 |
| 7 | agent 未开 `--allow-exec` | 拒绝执行 |

### 已知限制

**Windows 目标机的非 UTF-8 输出会被替换成 U+FFFD。** 中文 Windows 的 `cmd.exe` 输出为 GBK，`encoding/json` 序列化时会把无效 UTF-8 字节替换掉。agent→server 的传输链路本身是二进制安全的（`[]byte` 走 base64），损坏只发生在最终 JSON 响应。

Linux 目标机不受影响（locale 为 UTF-8），而 moss 的定位是服务器集群，Linux 是主场景，故暂不处理。若日后需要支持中文 Windows 目标机，两条路：命令前缀 `chcp 65001`（一行改动，但会改变用户命令且对老程序可能有副作用），或在 agent 侧按系统 ANSI 代码页转码（需引入 `golang.org/x/text`）。

**执行接口是同步阻塞的**，请求会挂到命令结束。阶段 1 验证足够，但接入 MCP 前需改为异步任务模式——AI 下发的部署命令跑几分钟很常见，HTTP 长连接扛不住。

### 已完成：阶段 2 后端（MCP Server + Key 体系）

| 文件 | 内容 |
|---|---|
| `server/apikey.go` | Key 体系：生成、哈希存储、Bearer 鉴权、能力集与机器白名单（闸 1）、CRUD |
| `server/mcp_types.go` | MCP 协议类型：JSON-RPC 2.0、握手、工具定义、CallToolResult |
| `server/mcp.go` | Streamable HTTP 传输层、版本协商、Origin 校验、方法分发 |
| `server/mcp_tools.go` | 五个工具及其作用域校验 |
| `server/exec.go` | 异步任务模式（`Start` / `Result`）、`SubmitWrite` |
| `agent/write.go` | agent 侧原子写入 |
| `server/mcp_test.go` | 21 项测试 |

#### 协议版本选型（关键决策）

**基线定在 legacy 时代：`2025-11-25` 为主，兼容 `2025-06-18` / `2025-03-26`。**

MCP 在 `2026-07-28` 做了断代改动——废除协议级会话与 `Mcp-Session-Id`、废除 `initialize` 握手、移除 GET 流，改为每请求自带 `_meta` 的无状态模型加 `server/discover`。选择 legacy 的依据是官方 Go SDK 的版本协商逻辑：它对任何非 `2026-07-28` 的客户端一律回落到 `2025-11-25`，现网客户端生态都在这一代。

升级路径已在代码注释中写明：规范给出的 dual-era 做法是在同一端点按「body 里有 `_meta.protocolVersion` → 走无状态路径 / method 为 `initialize` → 走会话路径」分流，届时扩展即可，不必推翻现有实现。

#### 实现中确认的要点

1. **不实现会话**。规范里 `Mcp-Session-Id` 是 MAY 而非 MUST，而 moss 的工具集与鉴权完全由 Key 决定、不随连接变化，没有需要跨请求保持的状态。这顺带对齐了 2026 的无状态方向。GET / DELETE 一律回 405。
2. **工具失败与协议错误必须分开**。规范原文要求：工具自身产生的错误要作为正常响应返回并置 `isError: true`，否则模型看不到错误内容、无法自我纠正；只有「找不到工具」「请求结构畸形」才用 JSON-RPC error。moss 的越权、拦截、agent 掉线、非零退出码全部走 `isError`。
3. **`annotations` 用指针类型**。规范给的默认值不全是 false——`destructiveHint` 与 `openWorldHint` 默认为 true，用 Go 的零值语义会把含义写反。
4. **Key 用 SHA-256 而非 bcrypt**。Key 是 160+ bit 高熵随机串，无字典攻击面；bcrypt 无法按哈希建索引，校验时要遍历全部 Key，而校验发生在每一次 API 调用上。用户密码仍走 bcrypt，两者场景不同。
5. **`list_servers` 按 Key 作用域过滤**。让模型看见它碰不到的机器毫无意义，只会诱导它尝试越权。
6. **异步任务的后台等待用 `context.Background()`**。若沿用请求 context，HTTP 一返回 context 就被取消，任务会在提交瞬间被判为「调用方取消」。
7. **新增受保护路径闸**。写文件是命令黑名单绕不过去的另一条路：不用 `rm` 也能靠覆写 `/etc/passwd`、`sshd_config` 或 agent 的 systemd 单元搞垮机器。两条路径各自设闸，含 `..` 的路径一律拒绝。

### 阶段 2 验收（已通过）

本地起 server + agent，用 curl 模拟 MCP 客户端实测九项：

| # | 场景 | 结果 |
|---|---|---|
| 1 | `initialize` 握手与版本协商 | 通过，回 `2025-11-25` + tools 能力 + instructions |
| 2 | `notifications/initialized` | HTTP 202 且无 body，符合规范 |
| 3 | `tools/list` | 五个工具，schema 与 annotations 完整 |
| 4 | `list_servers` | `structuredContent` 与 text 块双份返回 |
| 5 | `exec` 异步下发 | 立即返回 jobId + running |
| 6 | `get_result` 轮询 | 取回 stdout 与退出码 |
| 7 | `write_file` | 落盘内容逐字节正确 |
| 8 | `rm -rf /` 拦截 | `isError` + 说明这是安全拦截、重试无用 |
| 9 | 写 `/etc/ssh/sshd_config` | 被受保护路径闸拦下 |

单元测试 31 项全过（agent 7 + server 24），`go vet` 干净，Windows / Linux / macOS 三平台编译通过。

### 已完成：阶段 2 前端（AI 接入管理页）

`web/src/pages/admin/AiTab.tsx`，挂在后台「AI 接入」页签下，三个区块：

1. **接入方式**——MCP 服务地址与可直接复制的客户端配置片段。
2. **接入密钥**——列表、新建（名称/能力/机器范围/有效期）、吊销、删除。明文仅在创建时弹窗展示一次并配复制按钮，措辞明确告知关闭后无法找回。
3. **执行审计**——按时间倒序，点行看详情（完整命令、stdout、stderr）。被拦截的命令单独标红为「已拦截」，这是审计里最该被一眼看到的记录。

吊销与删除分设两个动作：吊销保留记录让历史审计仍可追溯到这把密钥，删除则不可恢复——弹窗里写明了这一差别，引导优先用吊销。

#### 布局实测

用 Playwright 驱动系统已缓存的 chromium 实测三个断点（390 / 768 / 1440），量文档与表格两级溢出：

| 断点 | 文档溢出 | 密钥表 | 审计表 |
|---|---|---|---|
| 390（手机） | 0 | 0 | 0 |
| 768（平板） | 0 | 0 | 0 |
| 1440（桌面） | 0 | 0 | 0 |

首轮实测发现窄屏下表格虽有 `overflow-x-auto` 兜底不会撑破页面，但**吊销/删除按钮和「结果」列被挤出可视区**，手机上要横滑才够得到。两处改动解决：次要列按 `md/lg` 断点隐藏；能力标签用 `whitespace-normal` + `flex-wrap` 换行堆叠（`td` 基类带 `whitespace-nowrap`，不覆盖则三个标签横排必然挤走操作列），审计时间在窄屏只给时分秒。

### 已完成：确认闸取消与自断手脚类拦截

原阶段 3（人类确认闸）取消，相关代码一并清理：Key 表的 `autonomous` 列、`apiKey.Autonomous` 字段、前端 `ApiKey.autonomous` 类型全部删除，不留死代码。

同时把「自断手脚」类命令补进硬拦截（`checkLockout`）：

| 类别 | 拦 | 放行 |
|---|---|---|
| 防火墙 | `iptables -A/-P/-F`、`ufw enable/default/delete`、`firewall-cmd --remove`、`nft add`、`iptables-restore` | `iptables -L/-S`、`ufw status`、`firewall-cmd --list-all`、`nft list` |
| SSH 配置 | 任何写入 `sshd_config` 的形态（`sed -i`、`>>`、`>`、`tee`、`cp`、`rm`） | `cat` / `grep` / `head` 读取 |
| SSH 服务 | `systemctl stop/disable/mask sshd`、`service ssh stop` | `systemctl restart sshd`、`status` |
| 网络接口 | `ip link set eth0 down`、`ifdown` | `ip link show`、`ip addr` |

**防火墙整类拦截而不做「这条规则会不会挡住 SSH」的判断**：一条规则的实际效果取决于规则顺序、默认策略与既有规则，静态看命令判断不出来——`iptables -A INPUT -j DROP` 一个端口都没提，照样把人锁在门外。查询放行保证 AI 仍能诊断网络问题。

测试期间发现的真缺陷：**`systemctl` 与 `service` 的参数顺序相反**（`systemctl stop ssh` vs `service ssh stop`），原先只写了一种形态，`service ssh stop` 与 `service moss-agent stop` 都能绕过。两处已修，测试补了对应用例。

另一处缺陷：**写文件的路径拦截原先没有落审计**——`checkProtectedPath` 命中时在工具层直接返回，`exec_audit` 里查不到任何痕迹，与「拦截同样要留痕」的原则矛盾。已把该检查下移到 `SubmitWrite`，与命令拦截共用 `reportBlocked`，两条路径现在都落审计并推送告警。补了 `TestMCPBlockedWriteIsAudited` / `TestMCPBlockedExecIsAudited` 两项测试锁住这个行为。

### 已完成：阶段 3（值守链路）

| 文件 | 内容 |
|---|---|
| `server/webhook.go` | 告警事件类型、`alertEvent` 载荷、异步推送、配置读写与测试接口 |
| `server/notify.go` | 六类告警统一收敛到 `fire()`，一份事件同时走 Telegram 与 webhook |
| `server/mcp_tools.go` | `get_history` 工具 |
| `web/src/pages/admin/NotifyTab.tsx` | Webhook 推送配置区块 |

#### 统一告警出口

原先六处告警各自调 `n.send()`。新增通道时若逐处补调用，漏一处就意味着某类故障永远不会通知到 AI——所以全部收敛到 `Notifier.fire()`：接收一个 `alertEvent`，内部同时投递 Telegram（用 `Text` 字段）与 webhook（结构化载荷）。以后再加通道只改一处。

**载荷同时携带人类文案与结构化字段**：人看 `text`，AI 读 `type` / `metric` / `value` / `threshold`。让 AI 去解析中文告警文案是脆弱的——文案一改，接收端就崩。

事件类型：`server.online` / `server.offline` / `server.load_alert` / `server.load_recovered` / `server.net_alert` / `server.net_recovered` / `server.expiring` / `exec.blocked`。命名用 `<对象>.<事件>`，接收端可按前缀路由。

#### 两个刻意的取舍

**不做重试队列。** 告警是时效性信息——一条 5 分钟前投递失败的「CPU 过高」重发到现在已无意义，而重试队列会在对端长时间不可用时无限堆积。真正需要不丢的记录在审计表里，不在这条通道上。

**投递失败只记日志，绝不阻塞。** 监控系统的告警通道故障，不能反过来拖垮监控本身。测试 `TestWebhookFailureDoesNotBlock` 锁住了这个行为。

#### get_history 的降采样

采样间隔默认 10 秒，1 小时 360 点、24 小时 8640 点，全量返回必然撑爆模型上下文。因此：

- **聚合在 SQL 里做**（`GROUP BY (time - since) / bucketMs` + `AVG`），不把上万行捞进内存。
- **桶号以查询起点为原点，不用 epoch 对齐**——epoch 对齐会让首尾各多出半个桶，点数不可控。
- **桶宽从整数阶梯里取**（10/15/30/60/300/900/3600/7200…秒），选满足 `宽度 × 120 > 跨度` 的最小值。判据是**严格大于**：桶宽恰好等于跨度÷120 时，末端那一行会落进第 120 号桶，点数变成 121 顶破上限。整数粒度也让模型一眼看懂分辨率。
- **SQL 加 `time <= untilMs`**：agent 时钟走快会写入未来时间戳，那种行桶号越界，一行脏数据就能顶破上限。

除序列外一并返回 `min`/`max`/`avg`/`first`/`last`/`trend` 摘要——多数情况下模型看摘要就够定位，不必逐点分析，这是省 token 的关键。`trend` 取首尾各 1/4 段均值比较（单点会被毛刺翻转结论），阈值用**相对**变化 5%：绝对差 5 对百分比是明显变化，对「字节每秒」则毫无意义。

#### 密钥的三种状态

后端不回传密钥明文，只回 `secretSet` 标记是否已配置。因此提交时：

| 前端行为 | 后端处理 |
|---|---|
| 密钥留空 | **不修改**已存的密钥 |
| 填入新值 | 覆盖 |
| 点「清除密钥」（`clearSecret: true`） | 清空 |

「留空 = 不修改」而非「留空 = 清空」，否则用户每次改地址都会意外把密钥清掉。

### 下一步

- 阶段 4：Dogfooding——用本接口部署 moss 自身。

## 待定项

- OpenClaw 侧接入的具体配置样例，待实际接通后补充到用户文档。
