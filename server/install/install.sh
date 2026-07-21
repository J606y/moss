#!/usr/bin/env bash
# Moss Agent 安装脚本（Linux / macOS）
# 用法: curl -fsSL https://your-moss/install.sh | bash -s -- --endpoint https://your-moss --token mk_xxx
set -euo pipefail

REPO="${MOSS_REPO:-j606y/moss}"   # GitHub 仓库
VERSION="${MOSS_VERSION:-latest}"
ENDPOINT=""
TOKEN=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --endpoint) ENDPOINT="$2"; shift 2 ;;
    --token)    TOKEN="$2"; shift 2 ;;
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
  # token 写入受限环境文件（600, root），不出现在 unit / 进程命令行 / ps 输出中
  $SUDO install -m 600 /dev/null /etc/moss-agent.env
  printf 'MOSS_TOKEN=%s\n' "$TOKEN" | $SUDO tee /etc/moss-agent.env >/dev/null
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
elif [[ "$OS" == "darwin" ]]; then
  PLIST="$HOME/Library/LaunchAgents/com.moss.agent.plist"
  mkdir -p "$(dirname "$PLIST")"
  # token 写入受限文件（600），plist 用 --token-file 引用，命令行不出现 token
  TOKEN_FILE="$HOME/Library/Application Support/moss-agent/token"
  mkdir -p "$(dirname "$TOKEN_FILE")"
  ( umask 077; printf '%s' "$TOKEN" > "$TOKEN_FILE" )
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.moss.agent</string>
  <key>ProgramArguments</key><array>
    <string>${BIN}</string>
    <string>--endpoint</string><string>${ENDPOINT}</string>
    <string>--token-file</string><string>${TOKEN_FILE}</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "✅ 已安装并启动 moss-agent (launchd)"
else
  echo "✅ 已安装到 ${BIN}，请自行配置开机自启："
  echo "   ${BIN} --endpoint ${ENDPOINT} --token ${TOKEN}"
fi
