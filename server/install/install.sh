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

BIN=/usr/local/bin/moss-agent
if [[ "$VERSION" == "latest" ]]; then
  URL="https://github.com/${REPO}/releases/latest/download/moss-agent-${OS}-${ARCH}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/moss-agent-${OS}-${ARCH}"
fi

echo "下载 ${URL} ..."
curl -fsSL -o /tmp/moss-agent "$URL"
chmod +x /tmp/moss-agent
sudo mv /tmp/moss-agent "$BIN"

if [[ "$OS" == "linux" ]] && command -v systemctl >/dev/null; then
  sudo tee /etc/systemd/system/moss-agent.service >/dev/null <<EOF
[Unit]
Description=Moss Agent
After=network-online.target

[Service]
ExecStart=${BIN} --endpoint ${ENDPOINT} --token ${TOKEN}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  sudo systemctl daemon-reload
  sudo systemctl enable --now moss-agent
  echo "✅ 已安装并启动 moss-agent (systemd)"
elif [[ "$OS" == "darwin" ]]; then
  PLIST="$HOME/Library/LaunchAgents/com.moss.agent.plist"
  mkdir -p "$(dirname "$PLIST")"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.moss.agent</string>
  <key>ProgramArguments</key><array>
    <string>${BIN}</string>
    <string>--endpoint</string><string>${ENDPOINT}</string>
    <string>--token</string><string>${TOKEN}</string>
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
