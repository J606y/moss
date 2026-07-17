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
cur_trust(){ local TRUST_PROXY=0; [ -f "$CONF" ] && . "$CONF"; echo "${TRUST_PROXY:-0}"; }
cur_proxies(){ local TRUSTED_PROXIES=""; [ -f "$CONF" ] && . "$CONF"; echo "${TRUSTED_PROXIES:-}"; }
cur_bind(){ local BIND="0.0.0.0"; [ -f "$CONF" ] && . "$CONF"; echo "${BIND:-0.0.0.0}"; }
mode_desc(){
  if [ "$(cur_trust)" = 1 ]; then echo "反代"
  elif [ "$(cur_bind)" = "127.0.0.1" ]; then echo "直连·仅本机"
  else echo "直连"; fi
}

# start_container <port> <trust:0|1> <trusted-proxies> <bind> [额外 docker run 参数...]
# 反代模式(trust=1)：仅绑回环 127.0.0.1，并以 --trust-proxy 让 Moss 按「真实访客 IP」限流；
#   多层反代(边缘→回源)再用 --trusted-proxies 列出边缘节点公网 IP，Moss 从 XFF 最右往左
#   跳过可信代理取真实客户端，杜绝伪造 XFF 绕过限流/登录锁定。
# 直连模式(trust=0)：绑 <bind>:port（0.0.0.0 公网可达 / 127.0.0.1 仅本机），
#   沿用镜像默认参数（限流按 socket 来源 IP）。
start_container(){
  local port="$1" trust="$2" proxies="$3" bind="$4"; shift 4
  local args=( -d --name "$CONTAINER" --restart unless-stopped -v "${VOLUME}:/app/data" "$@" )
  local cmd=()
  if [ "$trust" = 1 ]; then
    args+=( -p "127.0.0.1:${port}:8787" )
    cmd=( --listen :8787 --data /app/data --trust-proxy )
    [ -n "$proxies" ] && cmd+=( --trusted-proxies "$proxies" )
  else
    args+=( -p "${bind}:${port}:8787" )
  fi
  docker run "${args[@]}" "$IMAGE" "${cmd[@]}"
}

do_install(){
  need_docker || { pause; return; }
  mkdir -p "$WORKDIR"
  local port; port="$(cur_port)"
  read -rp "对外访问端口 [默认 ${port}]: " p; [ -n "$p" ] && port="$p"
  case "$port" in ''|*[!0-9]*) err "端口必须是数字"; return;; esac

  # 反代模式：在 Nginx 等反向代理后面运行时开启 —— 限流/日志按「真实访客 IP」(最左 XFF) 生效，
  # 且端口仅绑回环，防止外部直连绕过反代伪造头部。直接用 http://IP:端口 访问的选 N。
  local trust; trust="$(cur_trust)"
  local def="N"; [ "$trust" = 1 ] && def="Y"
  read -rp "是否在 Nginx 等反向代理后面运行？(y/N) [默认 ${def}]: " tp
  case "${tp:-$def}" in [Yy]*) trust=1;; *) trust=0;; esac

  # 多层反代(边缘→回源)：须列出边缘节点公网 IP，Moss 才能从追加式 XFF 里跳过可信代理
  # 取真实访客 IP，否则伪造的最左 XFF 会绕过限流/登录锁定。单层(仅本机一台 nginx)留空即可。
  local proxies; proxies="$(cur_proxies)"
  if [ "$trust" = 1 ]; then
    echo "  多层反代(边缘CDN/前置节点 → 本机回源 → Moss)请填【边缘节点公网IP】，逗号分隔，"
    echo "  支持 CIDR(如 203.0.113.10,198.51.100.0/24)。单层反代直接回车留空。"
    read -rp "可信代理名单 --trusted-proxies [默认 ${proxies:-空}]: " pin
    [ -n "$pin" ] && proxies="$pin"
  else
    proxies=""
  fi

  # 直连模式的监听地址：0.0.0.0 公网可直接访问；127.0.0.1 仅本机可达（内网/SSH 隧道场景，
  # 不暴露公网端口也无需放行防火墙）。反代模式固定绑回环，不询问。
  local bind; bind="$(cur_bind)"
  if [ "$trust" = 0 ]; then
    local bdef="1"; [ "$bind" = "127.0.0.1" ] && bdef="2"
    echo "  1) 0.0.0.0   —— 公网可直接访问 http://IP:端口"
    echo "  2) 127.0.0.1 —— 仅本机可访问（内网/SSH 隧道场景，不暴露公网）"
    read -rp "监听地址 (1/2) [默认 ${bdef}]: " bsel
    case "${bsel:-$bdef}" in 2) bind="127.0.0.1";; *) bind="0.0.0.0";; esac
  else
    bind="127.0.0.1"
  fi

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

  local extra=()
  [ "$fresh" = 1 ] && extra+=( -e "MOSS_ADMIN_PASSWORD=$pass" )
  start_container "$port" "$trust" "$proxies" "$bind" "${extra[@]}" >/dev/null || { err "启动容器失败"; pause; return; }

  printf 'PORT=%s\nTRUST_PROXY=%s\nTRUSTED_PROXIES=%q\nBIND=%s\n' "$port" "$trust" "$proxies" "$bind" > "$CONF"
  # 反代/仅本机模式只绑回环，无需放行公网端口
  if [ "$trust" = 0 ] && [ "$bind" = "0.0.0.0" ]; then open_firewall "$port"; fi

  sleep 2
  local ip; ip="$(detect_ip)"
  echo
  c_grn "=========================================================="
  if c_running; then ok "Moss 已启动"; else warn "容器已创建但未运行，请用菜单 [5] 查看日志排查"; fi
  if [ "$trust" = 1 ]; then
    echo "  运行模式:   反代模式（仅监听 127.0.0.1:${port}，按真实访客 IP 限流）"
    echo "  访问地址:   由你的反向代理对外提供（反代目标填 http://127.0.0.1:${port}）"
  elif [ "$bind" = "127.0.0.1" ]; then
    echo "  运行模式:   直连模式·仅本机（监听 127.0.0.1:${port}，公网无法直接访问）"
    echo "  访问地址:   本机 http://127.0.0.1:${port}"
    echo "              远程访问可用 SSH 隧道: ssh -N -L ${port}:127.0.0.1:${port} 用户@${ip}"
  else
    echo "  运行模式:   直连模式"
    echo "  访问地址:   http://${ip}:${port}"
  fi
  echo "  管理员用户名: admin（可登录后在「站点设置」修改）"
  if [ "$fresh" = 1 ]; then
    printf '%s\n' "$pass" > "$CRED"; chmod 600 "$CRED" 2>/dev/null
    echo "  管理员密码: ${pass}"
    c_ylw "  ⚠ 请立即记下密码（已存于 ${CRED}）。一旦丢失，需清空数据才能重置。"
  else
    echo "  管理员密码: 沿用原数据卷中的密码（本次未重置）"
    [ -f "$CRED" ] && echo "              上次安装记录: $(cat "$CRED")"
  fi
  echo "  快捷命令:   以后在本机直接输入  moss  即可重开管理菜单"
  if [ "$trust" = 1 ]; then
    c_grn "  ✓ 已按真实访客 IP 启用应用层限流（默认 /api 600·登录 10 次/分钟，可用环境变量调整）。"
  elif [ "$bind" = "127.0.0.1" ]; then
    c_grn "  ✓ 端口仅绑定本机回环 127.0.0.1，未暴露公网。"
  else
    c_ylw "  ⚠ 当前为明文 HTTP，登录密码会明文传输；公网长期使用建议上 Nginx+TLS 并以「反代模式」重装。"
  fi
  c_grn "=========================================================="
}

do_update(){
  c_exists || { warn "尚未安装，请先选 [1] 安装"; return; }
  need_docker || return
  local port; port="$(cur_port)"
  local trust; trust="$(cur_trust)" # 沿用安装时选定的运行模式
  local proxies; proxies="$(cur_proxies)" # 沿用安装时填的可信代理名单
  local bind; bind="$(cur_bind)" # 沿用安装时选的监听地址
  info "拉取最新镜像..."
  docker pull "$IMAGE" || { err "拉取失败"; return; }
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  if start_container "$port" "$trust" "$proxies" "$bind" >/dev/null; then
    ok "已更新到最新版并重启（数据与密码保留，模式：$(mode_desc)）"
  else
    err "更新失败"
  fi
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
  if [ "$(cur_trust)" = 1 ]; then
    echo "运行模式:   反代模式（仅监听 127.0.0.1:${port}）"
    echo "访问地址:   经你的反向代理访问（反代目标 http://127.0.0.1:${port}）"
  elif [ "$(cur_bind)" = "127.0.0.1" ]; then
    echo "运行模式:   直连模式·仅本机（监听 127.0.0.1:${port}，公网无法直接访问）"
    echo "访问地址:   本机 http://127.0.0.1:${port}（远程可 SSH 隧道: ssh -N -L ${port}:127.0.0.1:${port} 用户@服务器）"
  else
    echo "运行模式:   直连模式"
    echo "访问地址:   http://$(detect_ip):${port}"
  fi
  echo "管理员用户名: admin（如已在后台修改请以新的为准）"
  if [ -f "$CRED" ]; then
    echo "管理员密码: $(cat "$CRED")"
  else
    echo "管理员密码: 安装时已显示；若复用旧数据，请用你已知道的密码"
  fi
}

# 已安装后切换监听地址（公网 0.0.0.0 / 仅本机 127.0.0.1）：端口映射写死在容器上，
# 需要以相同镜像+数据卷重建容器，数据与密码不受影响。
do_change_bind(){
  c_exists || { warn "尚未安装，请先选 [1] 安装"; return; }
  need_docker || return
  local port trust proxies bind
  port="$(cur_port)"; trust="$(cur_trust)"; proxies="$(cur_proxies)"; bind="$(cur_bind)"
  if [ "$trust" = 1 ]; then
    warn "当前为反代模式，端口固定仅绑 127.0.0.1，不存在公网直绑；"
    echo "    如需改为公网直连，请用菜单 [1] 重装并在「反向代理」一问选 N。"
    return
  fi
  echo "当前监听地址: ${bind}:${port}"
  echo "  1) 0.0.0.0   —— 公网可直接访问 http://IP:端口"
  echo "  2) 127.0.0.1 —— 仅本机可访问（内网/SSH 隧道场景，不暴露公网）"
  read -rp "切换为 (1/2，直接回车取消): " bsel
  local new=""
  case "$bsel" in
    1) new="0.0.0.0";;
    2) new="127.0.0.1";;
    *) warn "已取消，未做任何修改"; return;;
  esac
  [ "$new" = "$bind" ] && { ok "监听地址已经是 ${new}，无需修改"; return; }
  info "正在以新监听地址重建容器（镜像与数据卷复用，数据与密码保留）..."
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  if start_container "$port" "$trust" "$proxies" "$new" >/dev/null; then
    mkdir -p "$WORKDIR"
    printf 'PORT=%s\nTRUST_PROXY=%s\nTRUSTED_PROXIES=%q\nBIND=%s\n' "$port" "$trust" "$proxies" "$new" > "$CONF"
    ok "已切换为监听 ${new}:${port}"
    if [ "$new" = "0.0.0.0" ]; then
      open_firewall "$port"
      echo "访问地址: http://$(detect_ip):${port}"
    else
      echo "本机访问: http://127.0.0.1:${port}（远程可 SSH 隧道: ssh -N -L ${port}:127.0.0.1:${port} 用户@服务器）"
      warn "原先放行的防火墙/安全组 ${port} 端口规则不再需要，可自行收回"
    fi
  else
    err "重建容器失败，请用菜单 [5] 查看日志排查"
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
  printf "  状态: %s   端口: %s   模式: %s\n" "$st" "$(cur_port)" "$(mode_desc)"
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
    echo "  6) 切换监听地址（公网 0.0.0.0 / 仅本机 127.0.0.1）"
    echo "  0) 退出脚本"
    echo "----------------------------------------------------------"
    read -rp "请输入序号: " choice
    case "$choice" in
      1) do_install ;;
      2) do_update ;;
      3) do_uninstall ;;
      4) do_status ;;
      5) do_logs ;;
      6) do_change_bind ;;
      0) ok "已退出 —— 下次直接输入  moss  即可再打开本菜单"; exit 0 ;;
      *) warn "无效选择，请输入 0-6" ;;
    esac
    pause
  done
}

require_root "$@"
install_shortcut
main_menu
