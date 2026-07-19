# Moss

> 面向个人自托管的轻量级服务器监控 —— 单二进制、内嵌前端、SQLite 落库,无需 MySQL/Redis,5 分钟装好。只做真正用得上的核心功能,不堆砌花哨特性。

<p>
  <a href="https://github.com/J606y/moss/stargazers"><img src="https://img.shields.io/github/stars/J606y/moss?style=flat&logo=github" alt="Stars"></a>
  <a href="https://github.com/J606y/moss/releases"><img src="https://img.shields.io/github/v/release/J606y/moss?logo=github" alt="Release"></a>
  <a href="./LICENSE"><img src="https://img.shields.io/github/license/J606y/moss" alt="License"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/J606y/moss?logo=go" alt="Go">
  <a href="https://ghcr.io/j606y/moss"><img src="https://img.shields.io/badge/ghcr.io-moss-2496ED?logo=docker&logoColor=white" alt="Docker"></a>
</p>

**🔗 [在线演示 Live Demo](https://jk.20051212.xyz)** · **📖 [English](./README_EN.md)**

![Moss 首页](docs/home.png)

---

## ✨ 特性

- 📊 **服务器概览**:卡片 / 列表双视图,CPU、内存、硬盘、实时网速、流量一目了然
- 📈 **实时监控**:WebSocket 实时上报(默认 2s),曲线实时滚动,网速数字逐位平滑跳动
- 🕐 **历史记录**:1 小时 ~ 数天负载历史,秒级可调采样,SQLite 存储
- 🛰️ **延迟探测**:ICMP / TCP / HTTP 探测任务,延迟曲线 + 丢包率
- 🔔 **通知告警**:离线 / 负载超阈值 / 网速超阈值 / 服务器到期提醒,Telegram 推送(含恢复通知)
- ☁️ **GCP Spot 守护**:Spot 实例被抢占关机后,面板自动调用 GCP API 重新开机(带确认延迟 / 冷却重试 / 次数上限)
- ⚙️ **管理后台**:服务器与探测任务拖拽排序、一键安装命令、单管理员密码登录
- 🚀 **极简部署**:server 单二进制(已内嵌前端)+ agent 单二进制,三端(Linux/macOS/Windows)通用

> 明确不做:OAuth / 2FA / 多用户、WebSSH、主题市场、多语言。保持轻、保持简单。

## 📸 截图

| 负载监控 · 实时指标与历史曲线 | 延迟探测 · ICMP/TCP/HTTP |
| :---: | :---: |
| ![服务器详情](docs/detail.png) | ![延迟探测](docs/probe.png) |

## 🤔 为什么是 Moss?

自托管监控不缺方案(哪吒 Nezha、Komari 等都很好),Moss 的取舍很明确:

- **零外部依赖**:后端用 Go + `modernc` 纯 Go SQLite 驱动,落库就是一个文件,不用单独跑 MySQL / Redis,1C512M 小鸡也能带。
- **单文件交付**:`moss-server` 把前端构建产物一并内嵌,`./moss-server` 直接裸跑;不想折腾也有 Docker 镜像和一键脚本。
- **够用就好**:只做服务器监控 + 延迟探测 + 告警这三件事,UI 干净、配置项少,装上就能用。

适合:有几台到十几台 VPS / 小鸡、想要一个好看又省心的监控面板、不需要企业级多租户的个人玩家。

## 🚀 快速开始

### server(Docker,推荐)

**最简单 —— 一键脚本**(菜单式:安装 / 更新 / 卸载,自动生成并打印管理员密码):

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/J606y/moss/main/deploy/moss.sh)
```

装完即可用 `http://<服务器IP>:8787` 直接访问。脚本还会注册全局命令 **`moss`** —— 以后在服务器上直接输入 `moss` 就能重开管理菜单(安装 / 更新 / 卸载 / 查看状态密码 / 日志 / 切换监听地址),不必再记那串 curl。

> 安装时会询问**是否在反向代理(Nginx)后面运行**:选「是」则自动以 `--trust-proxy` 启动并仅绑回环 `127.0.0.1`,让应用层限流按**真实访客 IP**生效;选「否」为直连模式(限流按 socket 来源 IP)。直连模式还会再问**监听地址**:默认 `0.0.0.0`(公网可直接访问),也可选 `127.0.0.1` 仅本机可达(内网/SSH 隧道场景,不暴露公网端口、不放行防火墙)。这些选择会被记住,`moss` 更新时自动沿用。多层反代(边缘→回源)还需手动补 `--trusted-proxies`,详见下方[反向代理小节](#反向代理--tlsnginx可选但生产推荐)。

以下为等价的手动方式:

```bash
# 方式一:用预构建镜像(GitHub Release / GHCR 发布后可用)
mkdir -p moss && cd moss
curl -fsSL -o docker-compose.yml https://raw.githubusercontent.com/J606y/moss/main/deploy/docker-compose.yml
echo 'MOSS_ADMIN_PASSWORD=你的强密码' > .env   # 仅首次初始化生效,之后忽略
docker compose up -d

# 方式二:克隆源码本地构建
git clone https://github.com/J606y/moss.git && cd moss/deploy
echo 'MOSS_ADMIN_PASSWORD=你的强密码' > .env
docker compose up -d --build
```

或单条 `docker run`:

```bash
docker run -d --name moss -p 8787:8787 \
  -e MOSS_ADMIN_PASSWORD=你的强密码 \
  -v moss-data:/app/data \
  ghcr.io/j606y/moss:latest
```

数据库存于命名卷 `moss-data`(镜像内 `/app/data` 已归属 nonroot,无需手动 chown)。浏览器访问 `http://<服务器IP>:8787`。

> Release 同时提供自包含的 `moss-server-*` 二进制(已内嵌前端),可不依赖 Docker 直接 `./moss-server-linux-amd64 --data ./data` 裸跑。

### 反向代理 + TLS(Nginx,可选但生产推荐)

前面挂一层 Nginx 终止 TLS、对外提供 HTTPS / wss。先让 Moss 容器**仅绑回环并开启 `--trust-proxy`**(`-p 127.0.0.1:8787:8787` + 启动参数加 `--trust-proxy`,用于读取真实来源 IP、在 HTTPS 下启用 Secure cookie),再用 Nginx 反代到 `http://127.0.0.1:8787`。用一键脚本安装时选「反代模式」即自动完成这两步。

> **应用层限流 + 登录锁定(默认开启)**:Moss 按真实访客 IP 对 `/api` 限流(默认 **600** 次/IP/分钟)、对登录等敏感端点更严(默认 **10** 次/IP/分钟,并对失败登录按 IP 锁定),超限返回 `429`。阈值用环境变量 `MOSS_RATELIMIT_PER_MIN` / `MOSS_RATELIMIT_AUTH_PER_MIN` 调整,设 `0` 关闭对应层。
>
> **如何识别真实访客 IP(安全要点)**:须开启 `--trust-proxy` 才会读取 `X-Forwarded-For`(否则按 socket 来源 IP——多层反代下那会是回源 Nginx 的 IP,所有访客共用一个限流桶)。各层 Nginx 用 `$proxy_add_x_forwarded_for`**追加**转发,Moss **不再信任 XFF 最左段**(那段客户端可任意伪造,会绕过限流/登录锁定),而是按**可信代理名单从右往左**取第一个非可信地址作为真实访客:
> - **单层**(仅一台 Nginx 直连 Moss):无需额外配置,Moss 默认取 XFF 最右段(＝该 Nginx 追加的对端 IP,客户端伪造不到)。
> - **多层**(访客 → 边缘 Nginx → 回源 Nginx → Moss):用 `--trusted-proxies` 列出你自己的边缘节点公网 IP(逗号分隔的 CIDR 或裸 IP,如 `--trusted-proxies 203.0.113.10,198.51.100.0/24`)。Moss 从 XFF 最右往左跳过名单内地址与环回地址,取到的第一个非可信地址即真实访客;攻击者伪造的最左段够不到该位置,无法绕过。

**省事 —— 一键反代脚本 [`nginx-rp`](https://github.com/J606y/nginx-rp)**(自动装 Nginx + acme.sh 签发并续期证书,支持 HTTP-01 / DNS API / 泛域名):

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/J606y/nginx-rp/main/nginx-rp.sh)
```

按提示:**反代目标**填 `http://127.0.0.1:8787`,**缓存模式务必选「无缓存 none」**(Moss 源站自管缓存,选普通/分片缓存会导致发版后卡旧版),域名与证书方式按引导走即可。脚本生成的配置已自带 WebSocket 升级头,`/api/ws`、`/api/agent/ws`(实时曲线 + agent 上报)开箱即用。

**想手工配** → 见 [`deploy/nginx.example.conf`](deploy/nginx.example.conf):一份 Moss 定制的完整示例,核心是 `location /` 带 WebSocket 升级头、传 `X-Forwarded-Proto`、并**切勿对 HTML 加缓存**。

### agent(install 脚本)

在每台被监控主机上,用后台「添加服务器」拿到的 `mk_` token 安装探针:

```bash
# Linux / macOS
curl -fsSL https://<你的moss>/install.sh | bash -s -- --endpoint https://<你的moss> --token mk_xxx
```

```powershell
# Windows(管理员 PowerShell)
powershell -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((iwr -useb https://<你的moss>/install.ps1))) -Endpoint 'https://<你的moss>' -Token 'mk_xxx'"
```

脚本会从 GitHub Release(`J606y/moss`)下载对应平台的 agent 二进制,并注册为开机自启服务(systemd / launchd / 计划任务)。

> ICMP 探测在 Windows 下需要管理员权限、Linux 下需要 root 或 `CAP_NET_RAW`;TCP / HTTP 探测无此要求。

### GCP Spot 自动开机(可选)

GCP 的 Spot 实例便宜但随时可能被抢占(实例变为 `TERMINATED`,数据不丢)。Moss 可以充当看门狗:节点确认离线后自动调用 Compute Engine API `instances.start` 把它拉起来,成功 / 失败 / 放弃都会走 Telegram 通知。

**1. 创建最小权限的 Service Account**(推荐,只给「查状态 + 开机」两个权限):

```bash
PROJECT=你的项目ID
gcloud iam service-accounts create moss-starter --project=$PROJECT
gcloud iam roles create mossSpotStarter --project=$PROJECT \
  --permissions=compute.instances.get,compute.instances.start
gcloud projects add-iam-policy-binding $PROJECT \
  --member="serviceAccount:moss-starter@$PROJECT.iam.gserviceaccount.com" \
  --role="projects/$PROJECT/roles/mossSpotStarter"
gcloud iam service-accounts keys create moss-sa.json \
  --iam-account=moss-starter@$PROJECT.iam.gserviceaccount.com
```

(偷懒可以直接用预定义角色 `roles/compute.instanceAdmin.v1`,但权限过宽,不推荐。)

**2. 面板配置**:后台「GCP 守护」页粘贴 `moss-sa.json` 内容 → 保存并测试连接 → 打开自动开机总开关;再到「服务器」页编辑对应节点,开启「GCP 自动开机」并填写 zone(如 `us-central1-a`)与实例名。节点行会出现 ▶ 按钮,可随时手动立即开机。

**工作方式**:节点离线超过「确认延迟」(默认 120s)后查询实例状态——仅 `TERMINATED` 才开机;`RUNNING` 但离线说明是 agent / 网络问题,只提醒不动实例。开机失败(如 Spot 容量不足)按「冷却」(默认 300s)自动重试,达到「最大尝试次数」(默认 3 次)后停止并通知,节点重新上线自动复位计数。

**注意事项**:

- **面板不要部署在被守护的 Spot 实例上**——面板和实例一起被抢占就没人拉起了。
- **人为关机前先关掉该节点的自动开机开关**(或总开关),否则会被自动拉起。
- Spot 实例的「终止操作」需保持默认的 **停止(STOP)**;若设为「删除(DELETE)」,被抢占后实例直接消失,无法拉起。
- 凭证以明文存于面板数据库,请务必只授予上述两个最小权限、只绑定必要项目。
- 实例 `SUSPENDED`(挂起)状态暂不支持自动恢复。

## 🏗 架构

```
moss/
├── web/       # 前端 React + TS + Tailwind + Recharts
├── server/    # 后端 Go + SQLite(modernc 纯 Go 驱动),内嵌前端,单二进制
├── agent/     # 探针 Go + gopsutil,单二进制,WebSocket 上报
├── internal/  # server / agent 共享的协议类型
└── deploy/    # Dockerfile + docker-compose + nginx 反代示例 + 一键脚本 moss.sh
```

- Agent 通过 `ws(s)://<server>/api/agent/ws?token=mk_xxx` 连接,上报系统指标与探测结果;连接信息 Windows / Linux / macOS 三端通用,仅安装命令不同(`install.sh` / `install.ps1`)。
- 浏览器通过 `/api/ws` 订阅实时推送;历史数据按采样间隔聚合入库。

## 🛠 本地开发

```powershell
# 1. 构建前端(server 会托管 web/dist)
cd web; npm install; npm run build

# 2. 构建并启动 server(首次启动会打印随机管理密码,或用环境变量指定)
cd ..
go build -o bin/moss-server.exe ./server
$env:MOSS_ADMIN_PASSWORD='你的密码'
.\bin\moss-server.exe --listen :8787 --data .\data --web .\web\dist

# 3. 登录 http://localhost:8787 后台添加服务器,拿到 token 启动本机 agent
go build -o bin/moss-agent.exe ./agent
.\bin\moss-agent.exe --endpoint http://localhost:8787 --token mk_xxx
```

前端热更新开发:`cd web; npm run dev`(Vite 已配置 `/api` 代理到 `localhost:8787`,打开 http://localhost:5173)。

## 🗺 路线 & 进展

核心功能均已上线并跑在生产环境:服务器监控、延迟探测、Telegram 告警、Docker / 一键脚本部署、Nginx 反代 + TLS/wss。详细变更见 [CHANGELOG](./CHANGELOG.md)。

欢迎 Issue / PR / Star ⭐ —— 觉得好用就点个星,这是对项目最大的鼓励。

## 📄 License

[MIT](./LICENSE) © j606y
