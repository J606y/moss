#!/usr/bin/env bash
# ==========================================================================
#  Moss 监控 · server 一键部署 / 管理脚本
#  用法:  bash <(curl -fsSL https://raw.githubusercontent.com/J606y/moss/main/deploy/moss.sh)
#  结果:  http://<服务器IP>:<端口> 直接访问监控面板（首次安装自动生成管理员密码）
#  环境:  Linux + Docker（无 Docker 会询问是否自动安装）
# ==========================================================================
set -o pipefail

IMAGE="ghcr.io/j606y/moss:latest"
CONTAINER="moss"
VOLUME="moss-data"
WORKDIR="/opt/moss"
CONF="$WORKDIR/moss.conf"
CRED="$WORKDIR/admin_password.txt"
DEFAULT_PORT="8787"
SELF_URL="https://raw.githubusercontent.com/J606y/moss/main/deploy/moss.sh"
BIN_PATH="/usr/local/bin/moss"

c_red(){ printf '\033[31m%s\033[0m\n' "$*"; }
c_grn(){ printf '\033[32m%s\033[0m\n' "$*"; }
c_ylw(){ printf '\033[33m%s\033[0m\n' "$*"; }
c_cyn(){ printf '\033[36m%s\033[0m\n' "$*"; }
info(){ c_cyn "[*] $*"; }
ok(){   c_grn "[✓] $*"; }
warn(){ c_ylw "[!] $*"; }
err(){  c_red "[✗] $*"; }

# 兼容 curl|bash 运行：把交互输入接回控制终端，否则 read 会读到 EOF 卡死
[ -t 0 ] || { [ -r /dev/tty ] && exec </dev/tty; }
pause(){ read -rp "按回车返回菜单..." _; }

require_root(){
  if [ "$(id -u)" -ne 0 ]; then
    # 若以已安装的固定文件（moss 命令）运行，自动 sudo 提权
    local self; self="$(readlink -f "$0" 2>/dev/null || echo "$0")"
    if [ -f "$self" ]; then exec sudo bash "$self" "$@"; fi
    err "请用 root 运行，例如："
    echo "  sudo bash <(curl -fsSL $SELF_URL)"
    exit 1
  fi
}

# 把脚本本体安装/刷新到 /usr/local/bin/moss，使「moss」命令可随时打开本菜单。
install_shortcut(){
  local self; self="$(readlink -f "$0" 2>/dev/null || echo "$0")"
  local existed=1; [ -f "$BIN_PATH" ] || existed=0
  if [ -f "$self" ] && [ "$self" != "$BIN_PATH" ]; then
    # 以真实文件运行（bash moss.sh）：拷贝到固定路径
    cp -f "$self" "$BIN_PATH" 2>/dev/null && chmod +x "$BIN_PATH"
  elif [ ! -f "$self" ]; then
    # curl|bash / 进程替换运行：从 GitHub 拉最新到固定路径（顺带自更新命令本体）
    local tmp; tmp="$(mktemp 2>/dev/null || echo /tmp/moss.$$)"
    if curl -fsSL "$SELF_URL" -o "$tmp" 2>/dev/null && bash -n "$tmp" 2>/dev/null; then
      cp -f "$tmp" "$BIN_PATH" 2>/dev/null && chmod +x "$BIN_PATH"
    fi
    rm -f "$tmp" 2>/dev/null
  fi
  if [ -f "$BIN_PATH" ] && [ "$existed" = 0 ]; then
    ok "已安装命令「moss」——以后直接输入  moss  即可重新打开本菜单"
    sleep 1
  fi
}

need_docker(){
  if ! command -v docker >/dev/null 2>&1; then
    warn "未检测到 Docker"
    read -rp "是否自动安装 Docker？(Y/n): " yn
    case "$yn" in [Nn]*) err "请先安装 Docker 再运行本脚本"; return 1;; esac
    info "正在安装 Docker（get.docker.com）..."
    curl -fsSL https://get.docker.com | sh || { err "Docker 安装失败，请手动安装"; return 1; }
    systemctl enable --now docker 2>/dev/null
  fi
  if ! docker info >/dev/null 2>&1; then
    info "尝试启动 Docker 服务..."
    systemctl start docker 2>/dev/null
    docker info >/dev/null 2>&1 || { err "Docker 守护进程未运行"; return 1; }
  fi
  return 0
}

gen_password(){
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -base64 24 | tr -dc 'A-Za-z0-9' | head -c 20
  else
    tr -dc 'A-Za-z0-9' </dev/urandom | head -c 20
  fi
}

detect_ip(){
  local ip
  ip="$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null)"
  [ -z "$ip" ] && ip="$(curl -fsS4 --max-time 5 https://ifconfig.me 2>/dev/null)"
  [ -z "$ip" ] && ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  echo "${ip:-<服务器IP>}"
}

open_firewall(){
  local port="$1"
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q "Status: active"; then
    ufw allow "${port}/tcp" >/dev/null 2>&1 && ok "ufw 已放行 ${port}/tcp"
  elif command -v firewall-cmd >/dev/null 2>&1 && systemctl is-active --quiet firewalld 2>/dev/null; then
    firewall-cmd --permanent --add-port="${port}/tcp" >/dev/null 2>&1
    firewall-cmd --reload >/dev/null 2>&1 && ok "firewalld 已放行 ${port}/tcp"
  fi
  warn "云服务器还需在【云控制台安全组】手动放行 ${port} 端口"
}

c_exists(){ docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; }
c_running(){ docker ps    --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER"; }
v_exists(){ docker volume inspect "$VOLUME" >/dev/null 2>&1; }
cur_port(){ local PORT="$DEFAULT_PORT"; [ -f "$CONF" ] && . "$CONF"; echo "$PORT"; }

do_install(){
  need_docker || { pause; return; }
  mkdir -p "$WORKDIR"
  local port; port="$(cur_port)"
  read -rp "对外访问端口 [默认 ${port}]: " p; [ -n "$p" ] && port="$p"
  case "$port" in ''|*[!0-9]*) err "端口必须是数字"; return;; esac

  if c_exists; then
    warn "已存在 Moss 容器"
    read -rp "重新创建容器？(数据保留) (y/N): " yn
    case "$yn" in [Yy]*) docker rm -f "$CONTAINER" >/dev/null 2>&1;; *) return;; esac
  fi

  # MOSS_ADMIN_PASSWORD 仅在「数据卷为空（首次）」时生效；复用旧卷则沿用原密码
  local fresh=1; v_exists && fresh=0
  local pass=""
  [ "$fresh" = 1 ] && pass="$(gen_password)"

  info "拉取镜像 $IMAGE ..."
  docker pull "$IMAGE" || { err "拉取镜像失败（检查网络）"; pause; return; }

  local args=( -d --name "$CONTAINER" --restart unless-stopped -p "${port}:8787" -v "${VOLUME}:/app/data" )
  [ "$fresh" = 1 ] && args+=( -e "MOSS_ADMIN_PASSWORD=$pass" )
  docker run "${args[@]}" "$IMAGE" >/dev/null || { err "启动容器失败"; pause; return; }

  echo "PORT=$port" > "$CONF"
  open_firewall "$port"

  sleep 2
  local ip; ip="$(detect_ip)"
  echo
  c_grn "=========================================================="
  if c_running; then ok "Moss 已启动"; else warn "容器已创建但未运行，请用菜单 [5] 查看日志排查"; fi
  echo "  访问地址:   http://${ip}:${port}"
  if [ "$fresh" = 1 ]; then
    printf '%s\n' "$pass" > "$CRED"; chmod 600 "$CRED" 2>/dev/null
    echo "  管理员密码: ${pass}"
    c_ylw "  ⚠ 请立即记下密码（已存于 ${CRED}）。一旦丢失，需清空数据才能重置。"
  else
    echo "  管理员密码: 沿用原数据卷中的密码（本次未重置）"
    [ -f "$CRED" ] && echo "              上次安装记录: $(cat "$CRED")"
  fi
  c_ylw "  ⚠ 当前为明文 HTTP，登录密码会明文传输；公网长期使用建议再上 nginx+TLS。"
  c_grn "=========================================================="
}

do_update(){
  c_exists || { warn "尚未安装，请先选 [1] 安装"; return; }
  need_docker || return
  local port; port="$(cur_port)"
  info "拉取最新镜像..."
  docker pull "$IMAGE" || { err "拉取失败"; return; }
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  docker run -d --name "$CONTAINER" --restart unless-stopped -p "${port}:8787" -v "${VOLUME}:/app/data" "$IMAGE" >/dev/null \
    && ok "已更新到最新版并重启（数据与密码保留）" || err "更新失败"
}

do_uninstall(){
  c_exists || warn "未检测到 Moss 容器"
  read -rp "确认卸载 Moss？将删除容器 (y/N): " yn
  case "$yn" in [Yy]*) ;; *) return;; esac
  docker rm -f "$CONTAINER" >/dev/null 2>&1 && ok "容器已删除"
  read -rp "是否同时删除数据卷「${VOLUME}」？永久删除数据库/密码/历史，不可恢复 (y/N): " yn2
  case "$yn2" in
    [Yy]*) docker volume rm "$VOLUME" >/dev/null 2>&1 && ok "数据卷已删除"; rm -f "$CRED" "$CONF" 2>/dev/null;;
    *)     warn "已保留数据卷，下次安装会沿用原数据与密码";;
  esac
  read -rp "是否删除镜像「${IMAGE}」？(y/N): " yn3
  case "$yn3" in [Yy]*) docker rmi "$IMAGE" >/dev/null 2>&1 && ok "镜像已删除";; esac
}

do_status(){
  if c_running; then
    ok "运行中"
    docker ps --filter "name=^/${CONTAINER}$" --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'
  elif c_exists; then
    warn "容器已安装但未运行（可执行: docker start ${CONTAINER}）"
  else
    warn "尚未安装"; return
  fi
  local port; port="$(cur_port)"
  echo "访问地址:   http://$(detect_ip):${port}"
  if [ -f "$CRED" ]; then
    echo "管理员密码: $(cat "$CRED")"
  else
    echo "管理员密码: 安装时已显示；若复用旧数据，请用你已知道的密码"
  fi
}

do_logs(){
  c_exists || { warn "尚未安装"; return; }
  echo "----- 最近 120 行日志（实时跟踪可另跑: docker logs -f ${CONTAINER}）-----"
  docker logs --tail 120 "$CONTAINER" 2>&1
}

banner(){
  local st
  if c_running; then st=$'\033[32m运行中\033[0m'
  elif c_exists; then st=$'\033[33m已停止\033[0m'
  else st=$'\033[31m未安装\033[0m'; fi
  cat <<'EOF'
   __  __  ___  ___ ___
  |  \/  |/ _ \/ __/ __|    Moss 监控 · 一键部署 / 管理
  | |\/| | (_) \__ \__ \    https://github.com/J606y/moss
  |_|  |_|\___/|___/___/
EOF
  echo "  镜像: $IMAGE"
  printf "  状态: %s     端口: %s\n" "$st" "$(cur_port)"
  echo "----------------------------------------------------------"
}

main_menu(){
  while true; do
    clear 2>/dev/null
    banner
    echo "  1) 安装 / 启动 Moss"
    echo "  2) 更新到最新版"
    echo "  3) 卸载 Moss"
    echo "  4) 查看状态 / 访问地址 / 管理员密码"
    echo "  5) 查看运行日志"
    echo "  0) 退出脚本"
    echo "----------------------------------------------------------"
    read -rp "请输入序号: " choice
    case "$choice" in
      1) do_install ;;
      2) do_update ;;
      3) do_uninstall ;;
      4) do_status ;;
      5) do_logs ;;
      0) exit 0 ;;
      *) warn "无效选择，请输入 0-5" ;;
    esac
    pause
  done
}

require_root "$@"
install_shortcut
main_menu
