# 云端会话接入 moss MCP

> 内部文档，不面向终端用户。

目标：在 claude.ai/code 的云端会话里直接调用 moss 的 MCP 工具，**仓库里不放任何 MCP 配置，也不放密钥**。

## 为什么要单独配

云端会话跑在 Anthropic 的临时虚拟机里，每次新开都是一份全新环境，读不到本机的 `~/.claude.json`。本机用 `claude mcp add --scope user` 配的 moss 在云端不生效。

云端有两处可配：

| 配置 | 作用 |
|---|---|
| Setup script | 虚拟机启动后、Claude Code 启动前执行，用来注册 MCP 服务器 |
| API credentials | Anthropic 代理在请求离开虚拟机后，按域名给请求补上 `Authorization` 头 |

两项都挂在**云端环境**（默认叫 Default）上。用该环境开的会话，不论选哪个仓库都生效。

密钥由代理在请求途中注入，Claude 和会话里的命令都读不到它。这比放环境变量安全：环境变量对所有使用该环境的人可见。

## 前置条件

- Pro 或 Max 套餐，且是自己组织的管理员。Team / Enterprise 暂无 API credentials，只能退回环境变量方案。
- 面板里给云端**单独建一把接入密钥**（建议名 `claude-cloud`），不要复用本机那把：出问题可单独吊销，执行审计也能按密钥筛出云端操作。

## 配置步骤

1. 打开 claude.ai/code，点输入框上方的云图标 → **Cloud** → 悬停在 Default 那一行，点右侧**齿轮**，进入 Edit environment。
2. **API credentials** → Add credential：

   | 字段 | 值 |
   |---|---|
   | Name | `moss` |
   | Credential type | Bearer |
   | Allowed websites | `jk.20051212.xyz`（只填域名） |
   | Path prefixes | `/mcp`（末尾不加斜杠） |
   | Custom headers | Name `Authorization`，Prefix `Bearer`，Value 为上面那把密钥本身（不要再带 `Bearer`） |

   点 Connect。保存后值不可再查看，也不可编辑，改动只能删了重建。
3. **Setup script** 填：

   ```bash
   #!/bin/bash
   set -e
   claude mcp add --transport http --scope user moss \
     https://jk.20051212.xyz/mcp
   ```

   `set -e` 让脚本在 `claude` 命令不存在时直接报错，而不是静默跳过。
4. 点 Save changes。

## 验证

用 Default 环境开一个云端会话，仓库选**没有 `.mcp.json` 的**（如本仓库），发：

```
先运行 claude mcp list，把输出原样贴出来；再调用 moss 的 list_servers 工具。
```

期望：`moss: https://jk.20051212.xyz/mcp (HTTP) - ✓ Connected`，且 `list_servers` 返回机器列表。2026-10-06 实测通过。

## 注意事项

- **网页版输入 `/mcp` 不能用来验证**：它打开的是 claude.ai 的 Connectors 设置页，不是虚拟机里的 MCP 列表。
- **仓库自带 `.mcp.json` 且定义了 `moss` 时**，项目级配置优先于用户级，会盖住 Setup script 注册的那份。若它引用了 `${MOSS_API_KEY}` 而环境里没设，`claude mcp list` 会报 Missing environment variables 警告；连接本身仍可用，因为代理照样注入密钥。处理办法：删掉仓库里那条定义。
- **Path prefix 限定 `/mcp`**：密钥只会附在发往 MCP 端点的请求上，会话访问面板的其它路径既带不上密钥，也过不了网络白名单。
- **不要把 `.mcp.json` 提交进本仓库**：仓库公开，且与本机用户级同名 `moss` 冲突时，项目级优先，本机在该仓库内可能变成不带密钥而 401。

## 撤销

- 删除 API credentials 里的 `moss` 条目，并在面板里吊销 `claude-cloud` 密钥。
- 清空 Setup script 中的 `claude mcp add` 行。
