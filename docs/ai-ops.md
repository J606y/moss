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
| `uQ8B6wha` | TW NGINX SERVER | Debian 13 | **已升 beta.1 ✅ exec 实测可用** |
| `NhjbVUHB` | United States WEB SERVER | Debian 13 / aarch64 | **已升 beta.1 ✅ exec 实测可用**（**moss server 本体在这台**） |
| `TNeWg5XB` | JP NGINX SERVER | Debian 13 | **已升 beta.1 ✅ exec 实测可用** |
| `T5s5WDHB` | HK PROXY SERVER | Debian 13 | v1.4.0 待升 |
| `TMVEKaQa` | KR PROXY SERVER | Debian 13 | v1.4.0 待升 |
| `FwCxdg8F` | GUANGZHOU DNS SERVER | Debian 13 | v1.4.0 待升（**境内，GitHub 下载会超时**） |
| `AkijA4Zu` | JP SCUM SERVER | **Windows Server 2022** | v1.4.0 待升 |

**待升 4 台**（2026-08-04 逐台下发 `echo` 实测：前三台正常返回，后四台无响应）。

### moss server 部署位置（已确认）

跑在 **`NhjbVUHB`**（United States WEB SERVER，129.153.73.56，Debian 13 / aarch64）：

| 项 | 值 |
|---|---|
| 形态 | Docker 容器 `moss`，镜像 `ghcr.io/j606y/moss:latest` |
| 启动参数 | `--listen :8787 --data /app/data --trust-proxy`，运行用户 uid 65532 |
| 端口 | `127.0.0.1:8787`，不对外 |
| 编排 | **无 compose 文件**，是 `docker run` 起的——重建前须先 `docker inspect moss` 取回原参数与挂载 |

⚠️ **部署 beta.2 时必须把镜像从 `:latest` 改成 `:beta`。**
`docker.yml` 改掉之后，预发布版不再占用 `latest`——`:latest` 会一直停在 beta.1，
`docker pull :latest` 拉不到 beta.2，而且不会报错，只会提示已是最新。

入口链路：`jk.20051212.xyz` →（TW `35.194.160.146` / JP `35.200.29.75` 两台 nginx 反代）
→ `origin-jk.20051212.xyz` → `129.153.73.56:443` → 容器 `127.0.0.1:8787`。

**自举的核心约束：MCP 端点就在这个容器里。** 重启它等于自己切断自己的手——
下发重启命令后必然拿不到返回，只能等待重连后再确认结果。阶段 4 必须按这个前提设计。

## 已在真机验证通过

在 `uQ8B6wha`（台湾）上实测：

- 读取：`list_servers` / `get_metrics` / `get_history` 全部正常
- 执行：异步下发 + 轮询取回，往返 152ms，退出码与 stdout 正确
- 拦截：`iptables -A INPUT -j DROP` 被服务端拦下，未下发
- 不误伤：`iptables -L INPUT -n` 正常返回规则表

## 待办（按建议优先级）

### 一、beta.2 必修的坑（实机踩出来的，剩余机器会重复踩）—— 四项已全部修完

1. ~~**安装脚本重写 `/etc/moss-agent.env`**~~ **已修**（`server/install/install.sh`）：
   原先 `install -m 600 /dev/null` 先清空文件，再用 `tee`（非 `-a`）只写回 token，
   **用户自定义的 `MOSS_ALLOW_EXEC=1` 会被静默抹掉**，且无任何提示。
   现改为先读出非 `MOSS_TOKEN` 行、重写后再追加回去。
   注意块内用 `if` 而非 `[ -n "$KEEP" ] && printf`——后者在 KEEP 为空时返回 1，
   作为块内最后一条命令会让整个管道在 `set -o pipefail` 下失败退出，首装即中断。
2. ~~**`latest` 不含预发布**~~ **已修**：GitHub 的 `latest` 按定义跳过 Pre-release，
   原先**静默装回 v1.4.0 还提示"已安装"**。
   现改为 **agent 版本跟随 server 自身版本**：`server/static.go installScript` 在下发脚本时
   把默认版本行改写为 server 的 `serverVersion`（开发态 `dev` 保持 `latest`）。
   面板是什么版本，它装出来的 agent 就是什么版本，不再依赖 GitHub 的 latest 语义。
   根因不只是"装旧了"：**agent 与 server 的 WS 协议随版本演进**（`exec` / `write` 均为 v2 新增），
   版本错配不是功能旧一点，而是新功能静默失效——现网 5 台 v1.4.0 agent 接在 v2.0.0 server 上，
   `exec` 根本不工作。
3. ~~**`list_servers` 缺 `agentVersion` 字段**~~ **已修**：库里有这个数据
   （`servers.agent_version`，REST 的 `/api/servers` 与前端 `types.ts` 都有），
   只有 MCP 工具没返回，AI 排查执行失败时只能靠超时时长反推。
   除补字段外，工具描述里也写明了"低于 2.0.0 的 agent 会静默丢弃 exec / write，
   症状是干等到超时而非报错"——光有字段而模型不知道怎么用它，等于没加。
   实测佐证：2026-08-04 对 4 台 v1.4.0 下发 `echo`，全部无响应，
   server 在 `timeout` 之上加 30s 宽限后才收敛报"等待执行结果超时"。
4. ~~**Docker 的 `latest` 会被预发布版占用**~~ **已修**（`.github/workflows/docker.yml`）：
   原先是无条件 `type=raw,value=latest`，**语义与 GitHub Release 的 latest 正好相反**
   ——后者自动跳过预发布，前者不跳。后果是每发一个 beta，所有按 README 用
   `ghcr.io/j606y/moss:latest` 部署的人都会被静默升级到测试版，
   而 CHANGELOG 第一句写的是"生产环境建议等正式版"。线上容器跑着 beta.1 就是这么来的。
   现改为：带 `-` 的 tag 只打版本号 tag 与 `beta`，不动 `latest`。
   **自己的机器跟 `:beta`，外部用户跟 `:latest`。**

### 二、剩余 4 台 agent 升级（HK / KR / GZ / Win）

⚠️ **坑 1、坑 2 的修复内嵌在 server 二进制里（`go:embed`），必须先重新部署 server 才生效。**
在那之前，线上 `https://jk.20051212.xyz/install.sh` 仍是旧脚本，升级必须沿用旧命令。

**server 重新部署前**（先装后配，反了会被脚本抹掉）：

```bash
MOSS_VERSION=v2.0.0-beta.1 bash <(curl -fsSL https://jk.20051212.xyz/install.sh) \
  --endpoint https://jk.20051212.xyz --token <该机 token> \
  && echo 'MOSS_ALLOW_EXEC=1' >> /etc/moss-agent.env \
  && systemctl restart moss-agent && cat /etc/moss-agent.env
```

**server 重新部署后**（版本自动对齐，env 自动保留，顺序不再有要求）：

```bash
bash <(curl -fsSL https://jk.20051212.xyz/install.sh) \
  --endpoint https://jk.20051212.xyz --token <该机 token>
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

#### agent 更新按钮（已定形态，未实现）

后台服务器列表每台一个「更新」按钮，**版本与 server 不一致时才出现**，人选哪台点哪台。

**不做自动推送**（推翻原「分批自动推送」方案，用户决策）：手动逐台点，一次坏也只坏一台，
还剩六台是好的。自动推送的爆炸半径是全集群。

**手动降低的是爆炸半径，不是失败概率**——被点的那一台，新二进制起不来照样失联，
且 systemd `Restart=always` 会把失败进程反复拉起，agent 永远连不回来。
所以下面四条一条都不能省：

1. **不走 exec 通道**。闸 2 硬拦「动 agent 二进制 / agent 配置 / agent systemd 单元」，
   按钮若下发 install.sh 会被自己的闸拦下。必须新增 `ServerMsg.Type = "upgrade"` 专用消息。
2. **升级动作必须 `setsid` 脱离 agent 进程树**。脚本末尾要 `systemctl restart moss-agent`，
   而脚本本身是 agent 的子进程——重启时进程组一杀，脚本自己被杀在半截，
   二进制处于半替换状态，机器直接失联。
3. **必须自动回滚**：备份旧二进制 → 替换 → 启动 → 探测是否重新连回 server →
   超时未连回则换回旧二进制重启。
4. **绝不开放给 MCP / AI**。人点是人的决定；AI 自己升级自己脚下的 agent，
   等于绕过「篡改 moss 自身」的硬拦。这个按钮只走后台管理接口，不进工具清单。

版本对齐（坑 2）落地后，按钮语义是「对齐到 server 当前版本」，
不需要版本下拉框——面板是什么版本，agent 就该是什么版本。

**Windows 是另一条路径**：运行中的 exe 有文件锁，不能像 Linux 那样直接覆盖 inode，
必须先停计划任务或改名再替换。且 `install.ps1` 的 `Register-ScheduledTask -Force`
会覆盖整个任务定义，用户手动加在 `-Argument` 里的 `--allow-exec` 会被抹掉
（坑 1 的 Windows 版，Linux 侧已修，Windows 侧未修）。目标机 `AkijA4Zu` 的
执行路径本身也还没在真机验证过。

#### 其它

- **用 moss 部署 moss**（阶段 4）：部署位置已确认，见《moss server 部署位置》。
  容器化部署，重建需先 `docker inspect moss` 取回原参数；难点是重启会切断 MCP 通道本身。

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
| **agent 不做自动升级，只做手动更新按钮** | 见《agent 更新按钮》——自动推送的爆炸半径是全集群，手动逐台点坏也只坏一台 |
| **agent 版本跟随 server 版本** | 协议随版本演进，错配 = 新功能静默失效；`latest` 又跳过预发布版 |
| **一键升级不受 `--allow-exec` 约束** | 见下方《升级为什么不检查执行开关》——这是刻意的特权，不是疏漏 |

### 升级为什么不检查执行开关

`agent/main.go` 的 `case "upgrade"` 不做 `allow` 判断。曾按「安全漏洞」提出要修，
用户判定不修，理由成立并采纳：

**`--allow-exec` 管的是「允许远程执行任意命令」，升级不是任意命令。**
它只能升到 server 当前版本、从固定地址、装带 SHA256 校验的官方二进制——
属于产品自身的受限维护动作，类比 iOS 给自家应用开的特权：
明确定义、范围封闭，不等于无限授权。

实际攻击面也支持这个判断（评估于 2026-08-04）：

- `Version` 与 `BaseURL` **都不来自请求参数**，分别取自编译期注入的 `serverVersion`
  与 `releaseBase()`，调用方一个都控制不了；
- 要植入恶意二进制得改 `MOSS_RELEASE_BASE`，那是容器启动参数、需宿主机权限——
  而拿到宿主机权限的人不需要这个通道；
- 接口只在 `requireAuth` 后面，**不在 MCP 工具清单里**，MCP Key 触发不了
  （实测：持有全部 7 台读/写/执行权限的 Key 依然点不动这个按钮）。

⚠️ **这条决策的前提是「特权保持受限」。** 以下任一变更都会让它失效，届时必须补上
`allow` 检查：

- 升级支持**指定版本**（如回滚到旧版）→ 变成降级攻击通道
- 升级支持**自定义下载源**（按机器配镜像等）→ 变成任意代码分发通道

真到那一步，改的不只是加个判断，还要重新想清楚「谁有权替换机器上的二进制」。

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

### 进行中：agent 更新按钮

分四段做，每段独立可验证。**段 1 已完成**。

#### 段 1 · 协议 + agent 自升级端（已完成）

| 文件 | 内容 |
|---|---|
| `internal/protocol/protocol.go` | `UpgradeTask` / `UpgradeResult`；`ServerMsg` 增 `upgrade`，`AgentMsg` 增 `upgrade_result`；阶段常量与 `UpgradeDefaultGrace=60` |
| `agent/upgrade.go` | 幂等与串行准入、下载、SHA256 校验、原子替换、拉起守护、回滚守护本体、连接标记 |
| `agent/upgrade_unix.go` | `Setsid` 拉起守护、`systemctl restart`、平台与 root 检查 |
| `agent/upgrade_windows.go` | 如实返回不支持（原因见文件注释），不假装成功 |
| `agent/upgrade_test.go` | 7 项，集中在校验和与大小上限这两条安全关键路径 |
| `agent/main.go` | `--rollback-guard` 分流（在 token 校验**之前**）、`case "upgrade"`、连上后 `markConnected()` |

实现中确认并处理的要点：

1. **回滚必须由旧二进制守护**。新二进制若根本起不来，就没有任何进程能执行回滚，
   而 systemd 的 `Restart=always` 会把失败进程反复拉起 → 机器永久失联。
   旧二进制此刻正在运行，是唯一能确定可执行的东西，因此备份成 `<bin>.bak` 后由它当守护。
2. **守护必须用 `systemd-run` 脱离 cgroup，`Setsid` 不够**（⚠️ 本文档原先写的是 setsid，是错的）。
   守护接下来要 `systemctl restart moss-agent`，而 systemd 停止一个 service 时默认
   `KillMode=control-group`——它杀的是该 unit **cgroup 内的所有进程**。
   `Setsid` 换的是 session 不是 cgroup，守护仍留在 `moss-agent.service` 的 cgroup 里，
   会被连同旧 agent 一起杀掉：二进制已换新、服务没起来、也没有任何东西执行回滚。
   **整个设计的安全网会静默失效，且只在真机升级时才暴露。**

   这个缺陷是在 2026-08-04 手动升级真机时发现的：写安装脚本时为了保证它不被
   `systemctl restart` 连坐杀掉，不得不想清楚 cgroup 与进程组的区别，
   回头才发现 agent 自升级用的 `Setsid` 犯了同一个错。**实操把设计缺陷顶了出来**——
   这条值得记住：只在开发机上跑得通的隔离假设，不等于在 systemd 下成立。

   修法：`systemd-run --collect`（不指定 `--unit` 名，避免残留同名单元冲突），
   并把 `systemd-run` 的存在性加进 `upgradeSupported` 的前置检查——
   缺了它整条链路的安全网就是空的，与其替换完才发现没人能回滚，不如在下载前就拒绝。
3. **成功判据是「连回 server」而不是「进程活着」**。只看 `systemctl is-active` 不够：
   进程起来了但 token 失效、endpoint 写错时同样连不上，那种状态与失联无异。
   agent 每次连上刷新 `/run/moss-agent.connected` 的 mtime，守护比较 mtime 是否变新。
4. **备份用 `rename` 而非 copy**：原子，且保留原 inode——正在运行的进程不受影响，
   `.bak` 也就必然是一个完整可执行的文件。
5. **守护起不来就不重启**。没有回滚兜底的重启是在赌运气，此时把备份换回去，回到升级前状态。
6. **自升级的校验和比安装脚本更严格**。`install.sh` 拿不到 `SHA256SUMS` 时告警后继续
   （老 release 确实没有该文件），而自升级**必须**校验通过才替换——
   这里没有人在旁边看告警，装错一次就是机器失联。
7. **守护日志落文件**（`os.TempDir()/moss-agent-upgrade.log`）：它脱离了 systemd，stderr 无处可去。
8. **平台检查前置**：等下载完几十兆才发现这台机器没法重启服务，既费带宽，
   也让失败发生在更靠近替换的地方。

#### 段 2 · server 下发端（已完成）

| 文件 | 内容 |
|---|---|
| `server/upgrade.go` | `upgradeManager`、版本判定、下发、状态机、`handleUpgradeAgent`、`releaseBase` |
| `server/admin_api.go` | `adminServer` 补 `agentVersion` / `targetVersion` / `upgradable` / `upgradeHint` / `upgradeStage` / `upgradeErr` |
| `server/agent_ws.go` | `upgrade_result` 分发；register 时调 `OnRegister` 判定结果 |
| `server/main.go` | `POST /api/admin/servers/{id}/upgrade`，**只在后台管理接口下，不进 MCP 工具清单** |
| `server/upgrade_test.go` | 9 项 |

测试期间发现并修掉的两个真缺陷：

1. **并发检查必须排在在线检查之前**。升级中的机器**必然**会短暂离线（替换后重启），
   若先判在线，用户在这个窗口重复点击看到的是「机器离线」，而真实情况是「正在升级中」
   ——那是两种完全不同的处置。
2. **版本判定要排在在线判定之前**。离线是会自行恢复的临时状态，而版本过旧决定了
   「必须换一种方式升级」。反过来的话，一台离线的旧 agent 只显示「机器离线」，
   等它上线用户点了还是失败。

另外两处刻意的设计：

- **`OnAgentGone` 什么都不做**。升级过程中掉线是预期行为，把正常的重启窗口
  误报成失败，会让每一次成功的升级都先闪一下红。
- **`MOSS_RELEASE_BASE` 可覆盖下载源**。不是为了测试方便：境内机器直连 GitHub 会超时
  （广州那台装 agent 时踩过），不给覆盖入口，一键升级对这类机器永远是坏的。

#### 段 3 · 前端（已完成）

`web/src/types.ts` 补字段，`ServersTab.tsx` 桌面表格与移动卡片两处各加一个按钮，挨着 GCP 开机。

- **版本比对规则全在后端**，前端只消费 `upgradable` / `upgradeHint` 结论。
  否则「哪些 agent 认识升级指令」这条判据会散落两处，改一处漏一处。
- **离线机器不显示按钮**（整行已有离线标识，再挂个点不动的按钮只是噪音），
  但升级进行中要显示——那时机器正因重启而离线，恰恰是最该看到状态的时候。
- 三种颜色：正常可升级为默认色，需手动升级为琥珀色，升级失败为红色且可点重试。
- 有任务在途时每 3 秒轮询列表。依赖用布尔而非 list，否则每次刷新都重建定时器，永远等不满一个周期。

#### 段 4 · 端到端验证（逻辑层已完成，系统行为待真机）

`rollbackGuard` 已重构为可注入（路径、轮询间隔、重启动作全部外部给定），
在开发机上验证了三条最危险的路径：

| 场景 | 结果 |
|---|---|
| 新版本连回 server | 保留新二进制、删除备份，返回 0 |
| 新版本起来了但连不回（token 失效 / endpoint 写错） | 判失败并恢复旧二进制 |
| 连重启都失败 | 立即回滚，不干等满 grace |

⚠️ **未验证的部分**：`Setsid` 是否真的脱离了进程组、`systemctl restart` 的真实行为，
这两项在 Windows 开发机上无法验证，必须在 Linux 真机上确认。
而按钮的首次真机验证只能发生在 beta.3——beta.2 的机器要先手动装上，
按钮才有认识升级指令的 agent 可驱动。

### beta.3（2026-08-04 已发布并部署）

面板已在 `NhjbVUHB` 切到 `ghcr.io/j606y/moss:beta` 的 v2.0.0-beta.3，中断约 3 秒。
7 台 agent 全部为 beta.2，更新按钮均已亮起，等待首次真机点击验证。

**未验证仍待办**：`Setsid`→`systemd-run` 的隔离是否真的成立、`systemctl restart`
的真实行为——只有点一次按钮才测得到。首台请挑 HK 或 KR（代理机），
不要挑 `NhjbVUHB`（server 本体）或 TW / JP（面板入口，DNS 只解析到这两个 IP）。
且 beta.2 内置的守护仍是旧版，**这一次升级没有回滚网**。

#### 后续可做（用户 2026-08-04 判定不急，保持现状）

- **下载失败自动重试**：境内拉 GitHub 更常见的失败是连接重置而非速度低，
  而超时调多宽都救不了「断」——它压根没等到超时。断一次就得人工再点一次。
  真要做：只重试下载、不重试校验（校验失败说明拿到的东西本身有问题），
  每次重试回报一次进度，让界面能区分「在重试」与「卡住」。

#### beta.3 内容

#### 新增：安装命令自带执行开关

后台「安装命令」弹窗加一个切换，**默认关闭**，打开后复制出来的命令带上
`--allow-exec`（Windows 为 `-AllowExec`），装机时一步到位。

原流程是「复制命令 → 装完 → 手动往 `/etc/moss-agent.env` 补一行 → 再重启一次」。
后两步纯属多余，而且正是坑 1 的温床——安装脚本重写 env、把手加的那行抹掉，
整类问题都源于「这个开关只能装完再补」。

**安全边界一点没动**：开关只影响复制出来的命令文本，控制权仍在真正去机器上
执行它的人手里。面板**改不了**任何一台已装机器的这个设置。

曾考虑过用户提的另一版方案——「在 AI 接入的密钥里勾选了哪些机器，就自动开启那些
机器的被控」，否决了，两个理由：

1. **技术上做不到**：`--allow-exec` 是 agent 本地的启动开关，server 要能远程打开它，
   就得新增一条「远程改写 agent 配置并重启」的通道，而那条通道本身是比 exec
   更彻底的后门——用更大的口子去开一个小口子。
2. **安全上不该做**：这是面板被攻破时的最后一道墙。若面板能远程开启执行能力，
   则管理员密码泄露或 `/mcp` 被打穿 = 全部机器 root RCE 一步到位。
   现状下即使面板整个失守，攻击者能操控的也只有机器管理员主动开过 exec 的那些。

实测六种组合（首装开/关、重装带/不带开关、已有该行不重复写、其它自定义变量保留、
`KEEP` 为空的 pipefail 陷阱）全部通过。

#### 修复清单（代码已改，未发版 → 线上仍是错的）

| # | 修复 | 严重度 |
|---|---|---|
| 1 | **回滚守护改用 `systemd-run`**（原 `Setsid` 挡不住 cgroup 清理） | 高——安全网整个失效 |
| 2 | **`agentSupportsUpgrade` 改用完整 semver 比较** | 中——按钮显示为可点，点了静默丢弃 |
| 3 | **`install.ps1` 保留 `--allow-exec`** | 中——Windows 重装会静默关掉执行能力 |
| 4 | 失败回报带上真实阶段（原先一律报 `downloading`） | 低——「没动过」与「动过又恢复了」的处置不同 |
| 5 | 人工修复后清除失败标记 | 低——否则界面一直挂着已解决的红色按钮 |
| 6 | **`Modal` 改用 `createPortal` 挂到 `document.body`** | 中——弹窗跟着页面滚动，遮罩不覆盖全屏 |

第 6 条由用户在「执行审计」的详情弹窗上发现。根因不在那个弹窗，在 CSS：
**`backdrop-filter` 非 `none` 的元素会成为其后代中 `position: fixed` 元素的包含块**，
而 `.glass`（`card` / `glassPanel` 都用它）带 `backdrop-blur`。
弹窗只要被放进任何一张卡片里，它的 `fixed inset-0` 就变成相对那张卡片定位。

Playwright 实测（1280×800 视口，滚动 800px）：

| | 未滚动 top | 滚动后 top | 遮罩尺寸 |
|---|---|---|---|
| 就地渲染在 `.glass` 内 | 1500 | **700**（跟着滚） | 1280×**93**（只有卡片高度） |
| portal 到 `body` | 0 | **0** | 1280×**800** |

遮罩尺寸那一列是附带发现：`inset:0` 相对卡片算，所以背景变暗只盖住卡片范围，
点卡片外的区域关不掉弹窗。

修在 `Modal` 组件内部而非调用处：改调用处只能治一次，
挂 `body` 才能让所有使用者（含复用它的 `ConfirmDelete`）都不必关心自己被放在哪。

第 3 条是坑 1 的 Windows 版：`Register-ScheduledTask -Force` 整个覆盖任务定义，
用户加在 `-Argument` 里的 `--allow-exec` 被静默抹掉。现改为重装前先读旧任务的
参数，检测到就保留。

第 2 条原先只比主版本号，`2.0.0-beta.1` 的主版本也是 2，被误判为支持一键升级；
而 `upgrade` 消息是 beta.2 才引入的，beta.1 收到照样静默丢弃。
判据必须精确到预发布号，且 `beta.9 < beta.10` 不能按字典序比。

⚠️ **现网 agent 的现状**：已升到 beta.2 的机器，其内置守护仍是 `Setsid` 版本。
将来第一次点按钮升级它们时，**那一次是没有回滚网的**——新版本起不来就得人工上机救。
因此首次按钮验证应挑一台坏了也不影响任何东西的机器（HK / KR 这类代理机），
不要选 `NhjbVUHB`（server 本体）或 TW / JP（面板入口，DNS 只解析到这两个 IP）。

### beta.2 发版清单（已完成）

| # | 步骤 | 备注 |
|---|---|---|
| 1 | 推 `v2.0.0-beta.2` tag | `web/package.json` 与 CHANGELOG 已更新 |
| 2 | 等 Release + 镜像构建 | 本版起测试版只打 `beta` 标签，不再占用 `latest` |
| 3 | 部署 server 到 `NhjbVUHB` | **镜像必须从 `:latest` 改成 `:beta`**，否则停在 beta.1 且不报错 |
| 4 | 手动升 agent（最后一次手动） | 后台的安装命令此时已自动装对版本，直接复制粘贴 |
| 5 | 真机验证 `Setsid` 与 `systemctl restart` | 段 4 未覆盖的部分 |

第 4 步建议留 KR（`TMVEKaQa`）不升，作为旧 agent 样本，用来验证按钮的前置拦截提示。

### 已完成：阶段 4（Dogfooding——用 moss 部署 moss）

2026-08-04，v2.0.0-beta.2 的部署完全通过 MCP 完成，闭环跑通：

1. `docker inspect` 取回原容器参数——发现 `--trust-proxy` **不在镜像默认 CMD 里**，
   是 `docker run` 时显式加的，漏掉它反代后面就拿不到真实来源 IP。
2. `write_file` 写部署脚本（该工具的首次真机使用），`exec` 以 `setsid nohup` 启动。
3. 脚本自带回滚：旧容器只改名保留、不删除，新容器起不来或 40 秒内探测不通就换回去。
4. 部署后用 `list_servers` 自查，并读日志确认。

**自断手是必须正面处理的约束**：MCP 端点就在被重启的容器里，`docker stop` 之后
在途的 `jobId` 直接消失（server 重启，内存任务表清空）。因此：

- 耗时且无副作用的步骤（`docker pull`）单独一次调用，能正常拿到结果；
- 会切断连接的步骤必须自包含且自带回滚，因为发出去之后就看不到了；
- 结果落文件，重连后再读。

实测中断约 3 秒，第 2 次健康探测即通过，7 台 agent 全部自动重连。

### 下一步

- 持续打磨（用户 2026-08-04 定的节奏）：beta.2 让功能可用，接下来先攒够修复再发版，
  不连续发版——每版都要验证充分。

## 待定项

- OpenClaw 侧接入的具体配置样例，待实际接通后补充到用户文档。
