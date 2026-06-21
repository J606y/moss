# 🌿 Moss

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
- 🔔 **通知告警**:离线告警 / 负载超阈值告警,Telegram 推送(含恢复通知)
- ⚙️ **管理后台**:服务器拖拽排序、一键安装命令、单管理员密码登录
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

装完即可用 `http://<服务器IP>:8787` 直接访问。以下为等价的手动方式:

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

前面挂一层 Nginx 终止 TLS,对外提供 HTTPS / wss。要点:容器用 `-p 127.0.0.1:8787:8787` 仅绑回环、并给 server 加 `--trust-proxy`(读取真实来源 IP + 在 HTTPS 下启用 Secure cookie);Nginx 反代到 `http://127.0.0.1:8787`,**务必带上 WebSocket 升级头**(`/api/ws`、`/api/agent/ws` 靠它,漏了实时曲线与 agent 都连不上)。完整可用配置见 [`deploy/nginx.example.conf`](deploy/nginx.example.conf),证书用 certbot / acme.sh 签发即可。

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
