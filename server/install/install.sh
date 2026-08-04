#!/usr/bin/env bash
# Moss Agent 安装脚本（Linux / macOS）
# 用法: curl -fsSL https://your-moss/install.sh | bash -s -- --endpoint https://your-moss --token mk_xxx
set -euo pipefail

REPO="${MOSS_REPO:-j606y/moss}"   # GitHub 仓库
# 默认版本由 server 下发脚本时改写为它自身的版本（见 server/static.go installScript）。
# agent 与 server 的 WS 协议随版本演进，版本错配会让新功能静默失效；而 GitHub 的
# latest 按定义跳过预发布版，beta 面板若走 latest 会装回上一个正式版还提示成功。
# 直接从仓库取用本脚本时未经改写，仍走 latest。
VERSION="${MOSS_VERSION:-latest}"
ENDPOINT=""
TOKEN=""
# 远程执行默认关闭。装了 agent 不等于同意被远程操作——
# 这个开关的控制权在机器上，面板无法远程打开它。
ALLOW_EXEC=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --endpoint)   ENDPOINT="$2"; shift 2 ;;
    --token)      TOKEN="$2"; shift 2 ;;
    --allow-exec) ALLOW_EXEC=1; shift ;;
    *) echo "未知参数: $1"; exit 1 ;;
  esac
done

[[ -z "$ENDPOINT" || -z "$TOKEN" ]] && { echo "用法: install.sh --endpoint <地址> --token <token>"; exit 1; }

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"   # linux / darwin
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "不支持的架构: $(uname -m)"; exit 1 ;;
esac

# 提权方式：已是 root 直接执行；否则用 sudo；都不满足则报错（精简系统可能无 sudo）
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"
  else
    echo "需要 root 权限，且未找到 sudo。请用 root 运行，或先安装 sudo。"; exit 1
  fi
fi

BIN=/usr/local/bin/moss-agent
if [[ "$VERSION" == "latest" ]]; then
  URL="https://github.com/${REPO}/releases/latest/download/moss-agent-${OS}-${ARCH}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/moss-agent-${OS}-${ARCH}"
fi

sha256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

echo "下载 ${URL} ..."
curl -fsSL -o /tmp/moss-agent "$URL"

# 完整性校验：release 附带 SHA256SUMS。缺失（老版本 release）则告警但继续，不匹配则终止。
EXPECT="$(curl -fsSL "${URL%/*}/SHA256SUMS" 2>/dev/null | grep "moss-agent-${OS}-${ARCH}\$" | awk '{print $1}')"
if [ -n "$EXPECT" ]; then
  ACTUAL="$(sha256 /tmp/moss-agent)"
  if [ "$EXPECT" != "$ACTUAL" ]; then
    rm -f /tmp/moss-agent
    echo "❌ 校验和不匹配，终止安装（期望 $EXPECT，实际 $ACTUAL）"; exit 1
  fi
  echo "✅ 校验和匹配"
else
  echo "⚠️  未获取到 SHA256SUMS，跳过完整性校验"
fi

chmod +x /tmp/moss-agent
$SUDO mv /tmp/moss-agent "$BIN"

if [[ "$OS" == "linux" ]] && command -v systemctl >/dev/null; then
  # token 写入受限环境文件（600, root），不出现在 unit / 进程命令行 / ps 输出中。
  #
  # 重装 / 升级必须保留用户自己加的其它变量——典型是 MOSS_ALLOW_EXEC=1。
  # 整个文件重写会把它们静默抹掉：升级完执行能力就没了，且全程无任何提示。
  ENV_FILE=/etc/moss-agent.env
  KEEP=""
  if [ -f "$ENV_FILE" ]; then
    KEEP="$($SUDO grep -v '^[[:space:]]*MOSS_TOKEN=' "$ENV_FILE" || true)"
  fi
  # 显式带了 --allow-exec 时，先滤掉旧的同名行，避免重复写出两条。
  # 没带则保持原样：已经开过的不会被关掉，这与「保留用户自定义变量」是同一条原则。
  if [ -n "$KEEP" ] && [ "$ALLOW_EXEC" = "1" ]; then
    KEEP="$(printf '%s\n' "$KEEP" | grep -v '^[[:space:]]*MOSS_ALLOW_EXEC=' || true)"
  fi
  $SUDO install -m 600 /dev/null "$ENV_FILE"
  {
    printf 'MOSS_TOKEN=%s\n' "$TOKEN"
    if [ "$ALLOW_EXEC" = "1" ]; then printf 'MOSS_ALLOW_EXEC=1\n'; fi
    # 用 if 而非 `[ -n "$KEEP" ] && printf`：后者在 KEEP 为空时返回 1，
    # 作为块内最后一条命令会让整个管道在 set -o pipefail 下失败退出。
    if [ -n "$KEEP" ]; then printf '%s\n' "$KEEP"; fi
  } | $SUDO tee "$ENV_FILE" >/dev/null
  $SUDO tee /etc/systemd/system/moss-agent.service >/dev/null <<EOF
[Unit]
Description=Moss Agent
Wants=network-online.target
After=network-online.target

[Service]
EnvironmentFile=/etc/moss-agent.env
ExecStart=${BIN} --endpoint ${ENDPOINT}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  $SUDO systemctl daemon-reload
  $SUDO systemctl enable moss-agent >/dev/null 2>&1 || true
  # 必须 restart 而非 enable --now：服务若已在运行（重装 / 换 token 场景），
  # enable --now 不会重启旧进程，会继续用旧 token 连接 →
  # “面板删号后重装一直 401 不上线”。restart 强制拉起新二进制 + 新 token。
  $SUDO systemctl restart moss-agent
  echo "✅ 已安装并启动 moss-agent (systemd)"
  if [ "$ALLOW_EXEC" = "1" ]; then
    echo "   已开启远程执行：本机接受面板下发的命令"
  else
    echo "   远程执行未开启（仅监控）"
  fi
elif [[ "$OS" == "darwin" ]]; then
  PLIST="$HOME/Library/LaunchAgents/com.moss.agent.plist"
  mkdir -p "$(dirname "$PLIST")"
  # token 写入受限文件（600），plist 用 --token-file 引用，命令行不出现 token
  TOKEN_FILE="$HOME/Library/Application Support/moss-agent/token"
  mkdir -p "$(dirname "$TOKEN_FILE")"
  ( umask 077; printf '%s' "$TOKEN" > "$TOKEN_FILE" )
  # launchd 没有 EnvironmentFile，开关只能进 ProgramArguments。
  # 换行是有意的：插进 array 后每个 <string> 各占一行，plist 保持可读。
  ALLOW_EXEC_ARG=""
  if [ "$ALLOW_EXEC" = "1" ]; then
    ALLOW_EXEC_ARG="
    <string>--allow-exec</string>"
  fi
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.moss.agent</string>
  <key>ProgramArguments</key><array>
    <string>${BIN}</string>
    <string>--endpoint</string><string>${ENDPOINT}</string>
    <string>--token-file</string><string>${TOKEN_FILE}</string>${ALLOW_EXEC_ARG}
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "✅ 已安装并启动 moss-agent (launchd)"
else
  MANUAL_ARGS="--endpoint ${ENDPOINT} --token ${TOKEN}"
  if [ "$ALLOW_EXEC" = "1" ]; then MANUAL_ARGS="${MANUAL_ARGS} --allow-exec"; fi
  echo "✅ 已安装到 ${BIN}，请自行配置开机自启："
  echo "   ${BIN} ${MANUAL_ARGS}"
fi
