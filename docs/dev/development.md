# 开发说明

## 架构

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

## 真实访客 IP 的识别

开启 `--trust-proxy` 后才读取 `X-Forwarded-For`,否则按 socket 来源 IP——多层反代下那会是回源 Nginx 的 IP,所有访客共用一个限流桶。各层 Nginx 用 `$proxy_add_x_forwarded_for` 追加转发,Moss 不信任 XFF 最左段(客户端可任意伪造,会绕过限流与登录锁定),而是按可信代理名单从右往左取第一个非可信地址:

- **单层**:默认取 XFF 最右段,即该 Nginx 追加的对端 IP,客户端伪造不到。
- **多层**:`--trusted-proxies` 列出边缘节点公网 IP。Moss 从右往左跳过名单内地址与环回地址,第一个非可信地址即真实访客;伪造的最左段够不到该位置。

## 本地开发

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
