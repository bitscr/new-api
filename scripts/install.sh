#!/usr/bin/env bash
# new-api 一键安装 / 升级脚本（Linux 二进制；服务管理按优先级自动适配:
# systemd → OpenRC → SysV init，也可以用 NEW_API_INIT 强制指定）
#
# 安装（首次部署）:
#   curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash
#   （没有 sudo 的系统：先 su - 切到 root，再 curl ... | bash）
#   （直接下载执行也可以：bash install.sh，脚本会自己申请 root）
#
# 升级（已部署过）:
#   curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash -s -- --update
#
# 导入旧库（把别处跑过的 one-api.db 带过来，相当于迁移用户/令牌/渠道）:
#   sudo bash install.sh --db /path/to/one-api.db
#   说明: 导入到 SQLITE_PATH（默认 <NEW_API_DIR>/one-api.db）；目标已有库会先备份；
#   源库若在运行请先停源或做 WAL checkpoint；配置了外部 SQL_DSN 时 SQLite 不被使用。
#
# 可用环境变量（写在 sudo 后面，例如 sudo NEW_API_PORT=8080 bash）:
#   NEW_API_VERSION    指定版本 tag，如 v0.0.1（默认 latest）
#   NEW_API_PORT       监听端口（默认 3000；已装过时自动沿用服务文件里的 --port）
#   NEW_API_DIR        数据/日志目录（默认 /opt/new-api）
#   NEW_API_BIN        二进制路径（默认 /usr/local/bin/new-api）
#   NEW_API_USER       运行用户（默认 newapi；设为 root 则不创建用户、不以低权运行）
#   NEW_API_SERVICE    服务名（默认 new-api）
#   NEW_API_REPO       GitHub 仓库（默认 bitscr/new-api）
#   NEW_API_ARCH       强制架构 amd64|arm64|386|armv7（默认按 uname -m 自动判断）
#   NEW_API_INIT       强制服务管理器 systemd|openrc|sysv|none（默认自动探测）
#   NEW_API_SKIP_SYSTEMD=1  只下发二进制，不创建用户/目录/服务（= NEW_API_INIT=none）
#   NEW_API_SKIP_CHECKSUM=1 跳过 sha256 校验（不建议）
#   NEW_API_DB         导入的 one-api.db 路径（等价 --db 参数）
#
# 兼容性说明:
#   - 非 root 时自动用 sudo 重跑（显式转发所有 NEW_API_* 变量，不依赖 env_keep）
#     没有 sudo 则给出明确提示；root 直跑不受影响。
#   - 下载优先 curl 退回 wget；装二进制优先 install 退回 cp+chmod；
#     建用户优先 useradd 退回 busybox adduser；校验支持 sha256sum/shasum。
#   - systemd 不可用（老系统/LXC/容器）: 有 OpenRC 走 OpenRC（Alpine/Gentoo），
#     否则生成 POSIX sh 的 SysV 脚本并用 update-rc.d / chkconfig 设开机自启。
#   - 服务文件只在缺失时创建；与模板不一致时另存为 .new 并提示 diff。

set -eu

MODE="install"
REPO="${NEW_API_REPO:-bitscr/new-api}"
PORT="${NEW_API_PORT:-}"
DIR="${NEW_API_DIR:-/opt/new-api}"
BIN="${NEW_API_BIN:-/usr/local/bin/new-api}"
RUN_USER="${NEW_API_USER:-newapi}"
SERVICE="${NEW_API_SERVICE:-new-api}"
VERSION="${NEW_API_VERSION:-latest}"
ARCH="${NEW_API_ARCH:-}"
INIT_OVERRIDE="${NEW_API_INIT:-}"
SKIP_SERVICE="${NEW_API_SKIP_SYSTEMD:-0}"
SKIP_CHECKSUM="${NEW_API_SKIP_CHECKSUM:-0}"
DB_IMPORT="${NEW_API_DB:-}"
[ "$INIT_OVERRIDE" = "none" ] && SKIP_SERVICE=1
UNIT="/etc/systemd/system/${SERVICE}.service"
INITD="/etc/init.d/${SERVICE}"
ENV_FILE="${DIR}/env"
LOG_FILE="${DIR}/logs/stdout.log"
PID_FILE="${DIR}/new-api.pid"
INIT=""

log()  { printf '\033[32m[new-api]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[new-api]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[new-api]\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -u|--update) MODE="update" ;;
    --install)   MODE="install" ;;
    --version)   shift; [ $# -gt 0 ] || die "--version 需要一个 tag，例如 --version v0.0.1"; VERSION="$1" ;;
    --db)        shift; [ $# -gt 0 ] || die "--db 需要一个 one-api.db 文件路径"; DB_IMPORT="$1" ;;
    -h|--help)   sed -n '2,42p' "$0" 2>/dev/null || true; exit 0 ;;
    *)           die "未知参数: $1（可用: --update / --version <tag> / --help）" ;;
  esac
  shift
done

# ---------- root / sudo ----------
# 没 sudo 的系统（精简容器/老系统）上 root 直接跑就行；非 root 且无 sudo 时
# 给出明确出路。显式转发变量而不是 sudo -E：不受 env_keep 限制。
if [ "$(id -u)" != "0" ]; then
  if command -v sudo >/dev/null 2>&1 && [ -f "$0" ]; then
    log "当前不是 root，用 sudo 重新执行（转发全部 NEW_API_* 变量）"
    exec sudo env \
      NEW_API_VERSION="$VERSION" NEW_API_PORT="$PORT" NEW_API_DIR="$DIR" NEW_API_BIN="$BIN" \
      NEW_API_USER="$RUN_USER" NEW_API_SERVICE="$SERVICE" NEW_API_REPO="$REPO" NEW_API_ARCH="$ARCH" \
      NEW_API_INIT="$INIT_OVERRIDE" NEW_API_SKIP_SYSTEMD="$SKIP_SERVICE" NEW_API_SKIP_CHECKSUM="$SKIP_CHECKSUM" \
      NEW_API_DB="$DB_IMPORT" \
      bash "$0" "$@"
  fi
  die "需要 root 权限：请用 sudo bash 执行；没有 sudo 就先 su - 切到 root 再跑。"
fi

# ---------- 依赖检查 ----------
command -v tar >/dev/null 2>&1 || die "缺少 tar"
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || die "缺少 curl 或 wget（任装一个即可）"

fetch() { # fetch <url> <输出文件>：curl 优先，wget 兜底
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL -o "$2" "$1" && return 0
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O "$2" "$1" && return 0
  fi
  return 1
}

probe() { # probe <url>：健康检查（GET，2 秒超时）
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --max-time 2 "$1" >/dev/null 2>&1
  else
    wget -q -T 2 -O /dev/null "$1" 2>/dev/null
  fi
}

# ---------- 架构 ----------
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64)               ARCH="amd64" ;;
    i386|i486|i586|i686)        ARCH="386" ;;
    aarch64|arm64)              ARCH="arm64" ;;
    armv8l|armv8b|armv7l|armv7) ARCH="armv7" ;;
    armv6*|armv5*)              die "不支持的架构 $(uname -m)：Release 提供 amd64 / arm64 / 386 / armv7，没有 ARMv5/v6 产物。" ;;
    *) die "无法识别的架构 $(uname -m)。可用 NEW_API_ARCH=amd64|arm64|386|armv7 强制指定。" ;;
  esac
fi
case "$ARCH" in
  amd64|arm64|386|armv7) ;;
  *) die "NEW_API_ARCH 只能是 amd64 / arm64 / 386 / armv7（当前: $ARCH）" ;;
esac

# ---------- 服务管理器探测 ----------
detect_init() {
  if [ "$SKIP_SERVICE" = "1" ]; then INIT="none"; return; fi
  if [ -n "$INIT_OVERRIDE" ]; then
    case "$INIT_OVERRIDE" in
      systemd|openrc|sysv) INIT="$INIT_OVERRIDE" ;;
      none) INIT="none" ;;
      *) die "NEW_API_INIT 只能是 systemd | openrc | sysv | none（当前: $INIT_OVERRIDE）" ;;
    esac
    return
  fi
  # /run/systemd/system 只在 systemd 真的当 PID1 时存在，容器里不会误判。
  if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then
    INIT="systemd"
  elif { command -v openrc-run >/dev/null 2>&1 || [ -x /sbin/openrc-run ]; } && command -v rc-update >/dev/null 2>&1; then
    INIT="openrc"
  else
    INIT="sysv"
  fi
}
detect_init

# ---------- 已装过时沿用端口（跨 systemd / OpenRC / SysV 服务文件） ----------
if [ -z "$PORT" ]; then
  for svc in "$UNIT" "$INITD"; do
    if [ -f "$svc" ]; then
      PORT="$(sed -n 's/.*--port[= ]\([0-9][0-9]*\).*/\1/p' "$svc" | head -n1)"
      [ -n "$PORT" ] && break
    fi
  done
fi
PORT="${PORT:-3000}"

if [ "$MODE" = "update" ]; then
  [ -f "$BIN" ] || die "没找到 $BIN，这台机器上还没装过，请先跑安装（去掉 --update）"
fi

# ---------- 下载 + 校验 ----------
ASSET="new-api-linux-${ARCH}.tar.gz"
if [ "$VERSION" = "latest" ]; then
  URL="https://github.com/${REPO}/releases/latest/download/${ASSET}"
else
  URL="https://github.com/${REPO}/releases/download/${VERSION}/${ASSET}"
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT
log "下载 ${ASSET}（${VERSION}，${ARCH}，服务管理器: ${INIT}）"
fetch "$URL" "$WORKDIR/$ASSET" || die "下载失败: $URL"

if [ "$SKIP_CHECKSUM" = "1" ]; then
  warn "已按 NEW_API_SKIP_CHECKSUM=1 跳过 sha256 校验"
else
  fetch "${URL}.sha256" "$WORKDIR/$ASSET.sha256" || die "下载校验文件失败: ${URL}.sha256"
  # Release 里的 .sha256 记录的是构建目录内的相对路径，这里只取哈希值重新比对
  EXPECT=""
  read -r EXPECT _ < "$WORKDIR/$ASSET.sha256" 2>/dev/null || EXPECT=""
  [ -n "$EXPECT" ] || die "校验文件内容异常: ${URL}.sha256"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$WORKDIR/$ASSET" | cut -d' ' -f1)"
  elif command -v shasum >/dev/null 2>&1; then
    ACTUAL="$(shasum -a 256 "$WORKDIR/$ASSET" | cut -d' ' -f1)"
  else
    warn "系统没有 sha256sum / shasum，跳过校验（如需强校验请安装 coreutils）"
    ACTUAL="$EXPECT"
  fi
  [ "$EXPECT" = "$ACTUAL" ] || die "sha256 不匹配，已中止（期望 $EXPECT，实际 $ACTUAL）"
  log "sha256 校验通过"
fi

tar -xzf "$WORKDIR/$ASSET" -C "$WORKDIR"
NEW_BIN="$WORKDIR/new-api-linux-${ARCH}"
[ -f "$NEW_BIN" ] || die "压缩包里没有 new-api-linux-${ARCH}"
chmod +x "$NEW_BIN"
NEW_VER="$("$NEW_BIN" --version 2>/dev/null | head -n1 || true)"
log "产物版本: ${NEW_VER:-未知}"

# ---------- 工具兜底 ----------
place_bin() { # install 命令缺失时退回 cp+chmod（busybox/精简系统）
  mkdir -p "$(dirname "$BIN")"
  if command -v install >/dev/null 2>&1; then
    install -m 0755 "$1" "$BIN"
  else
    cp "$1" "$BIN" && chmod 0755 "$BIN"
  fi
}

if [ "$MODE" = "install" ] && [ "$SKIP_SERVICE" != "1" ]; then
  if [ "$RUN_USER" != "root" ]; then
    if ! id "$RUN_USER" >/dev/null 2>&1; then
      log "创建运行用户 $RUN_USER"
      if command -v useradd >/dev/null 2>&1; then
        useradd -r -s /usr/sbin/nologin "$RUN_USER" 2>/dev/null || useradd -r -s /sbin/nologin "$RUN_USER" || die "创建用户 $RUN_USER 失败"
      elif command -v adduser >/dev/null 2>&1; then
        # busybox(Alpine): -S 系统 -D 免密 -H 不建家目录
        adduser -S -D -H -s /sbin/nologin "$RUN_USER" 2>/dev/null || adduser -S -D -H "$RUN_USER" || die "创建用户 $RUN_USER 失败"
      else
        die "系统缺少 useradd / adduser，无法创建运行用户；可设 NEW_API_USER=root 跳过"
      fi
    fi
  fi
  mkdir -p "$DIR" "$DIR/logs" "$DIR/data"
  [ "$RUN_USER" = "root" ] || chown -R "$RUN_USER" "$DIR"

  if [ ! -f "$ENV_FILE" ]; then
    log "生成 $ENV_FILE（占位注释，按需填 MySQL/PostgreSQL/Redis）"
    cat > "$ENV_FILE" <<'ENVEOF'
# 需要外部数据库 / Redis 时取消注释并改成实际连接串，改完重启服务
# （systemd: systemctl restart new-api；OpenRC: rc-service new-api restart；SysV: /etc/init.d/new-api restart）
# SQL_DSN=postgresql://user:password@127.0.0.1:5432/new-api
# REDIS_CONN_STRING=redis://:password@127.0.0.1:6379
# TZ=Asia/Shanghai
ENVEOF
    [ "$RUN_USER" = "root" ] || chown "$RUN_USER" "$ENV_FILE"
    chmod 600 "$ENV_FILE"
  fi
fi

# ---------- 备份并安装二进制 ----------
BAK=""
if [ -f "$BIN" ]; then
  OLD_VER="$("$BIN" --version 2>/dev/null | head -n1 || true)"
  BAK="${BIN}.bak-$(date +%Y%m%d%H%M%S)"
  cp -a "$BIN" "$BAK"
  log "已备份旧二进制 → $BAK（版本 ${OLD_VER:-未知}）"
fi
place_bin "$NEW_BIN"
log "二进制已就位: $BIN"

# ---------- 导入 SQLite 数据库（可选:--db / NEW_API_DB）----------
# 场景:从旧的 one-api / new-api 实例迁移用户、令牌、渠道、配置。
# 只在 SQLite 模式下有意义:env 里配置了外部 SQL_DSN 时程序不会读这个库。
if [ -n "$DB_IMPORT" ]; then
  [ -f "$DB_IMPORT" ] || die "--db 指定的文件不存在: $DB_IMPORT"
  header="$(head -c 15 "$DB_IMPORT" 2>/dev/null || true)"
  [ "$header" = "SQLite format 3" ] || die "$DB_IMPORT 不是 SQLite 数据库文件（头部校验失败）"

  # 目标路径:env 里的 SQLITE_PATH 优先（相对路径相对工作目录 $DIR），默认 $DIR/one-api.db。
  # 值可能带 DSN 参数（one-api.db?_busy_timeout=...），去掉 ? 之后的部分。
  TARGET_DB="$(sed -n 's/^[[:space:]]*SQLITE_PATH=//p' "$ENV_FILE" 2>/dev/null | tail -n1)"
  # 值可能带引号或 DSN 参数(如 one-api.db?_busy_timeout=...),一并清理
  TARGET_DB="$(echo "$TARGET_DB" | tr -d "\"" | sed -e "s/'//g" -e 's/?.*//')"
  [ -n "$TARGET_DB" ] || TARGET_DB="one-api.db"
  case "$TARGET_DB" in
    /*) ;;
    *) TARGET_DB="$DIR/$TARGET_DB" ;;
  esac

  if grep -Eq '^[[:space:]]*SQL_DSN=.' "$ENV_FILE" 2>/dev/null; then
    dsn="$(sed -n 's/^[[:space:]]*SQL_DSN=//p' "$ENV_FILE" | tail -n1)"
    case "$dsn" in
      local) ;;
      postgres*|postgresql*) warn "$ENV_FILE 配置了 PostgreSQL（SQL_DSN），导入的 SQLite 不会被程序使用" ;;
      *) warn "$ENV_FILE 配置了 MySQL（SQL_DSN），导入的 SQLite 不会被程序使用" ;;
    esac
  fi

  # 更新模式下服务正在运行就先停:覆盖一个被打开的 SQLite 会损坏数据。
  # 停掉后由后面既有的 update 重启逻辑统一拉起。
  if [ "$MODE" = "update" ] && [ "$SKIP_SERVICE" != "1" ]; then
    case "$INIT" in
      systemd) systemctl is-active --quiet "$SERVICE" 2>/dev/null && { systemctl stop "$SERVICE"; log "已停止服务以安全导入数据库（稍后统一重启）"; } ;;
      openrc)  rc-service "$SERVICE" status >/dev/null 2>&1 && { rc-service "$SERVICE" stop; log "已停止服务以安全导入数据库（稍后统一重启）"; } ;;
      sysv)    [ -x "$INITD" ] && "$INITD" status >/dev/null 2>&1 && { "$INITD" stop; log "已停止服务以安全导入数据库（稍后统一重启）"; } ;;
    esac
  fi

  DB_BAK=""
  if [ -f "$TARGET_DB" ] && [ "$DB_IMPORT" != "$TARGET_DB" ]; then
    DB_BAK="${TARGET_DB}.bak-$(date +%Y%m%d%H%M%S)"
    cp -a "$TARGET_DB" "$DB_BAK"
    log "目标库已存在，先备份 → $DB_BAK"
  fi
  if [ "$DB_IMPORT" != "$TARGET_DB" ]; then
    mkdir -p "$(dirname "$TARGET_DB")"
    cp -f "$DB_IMPORT" "$TARGET_DB"
    # WAL 伴生文件:源有就一起带过来，源没有就清掉目标残留（旧 WAL 会污染新库）
    for ext in -wal -shm; do
      if [ -f "${DB_IMPORT}${ext}" ]; then
        cp -f "${DB_IMPORT}${ext}" "${TARGET_DB}${ext}"
        log "已随库导入伴生文件 ${ext}"
      elif [ -f "${TARGET_DB}${ext}" ]; then
        rm -f "${TARGET_DB}${ext}"
      fi
    done
    [ "$RUN_USER" = "root" ] || chown "$RUN_USER" "$TARGET_DB"
    chmod 600 "$TARGET_DB"
    log "数据库已导入: $DB_IMPORT → $TARGET_DB"
    [ -z "$DB_BAK" ] || log "回滚方式: mv $DB_BAK $TARGET_DB"
    if [ -f "${DB_IMPORT}-wal" ]; then
      warn "源库带有 -wal 文件：若源实例还在运行，未 checkpoint 的数据可能不完整，建议停源后重新导入"
    fi
  else
    log "源与目标是同一个文件，跳过覆盖"
  fi
fi

if [ "$SKIP_SERVICE" = "1" ]; then
  log "已按 NEW_API_SKIP_SYSTEMD=1 / NEW_API_INIT=none 跳过服务配置，收工"
  exit 0
fi

rollback_hint() {
  case "$INIT" in
    systemd) echo "systemctl stop $SERVICE && mv $BAK $BIN && systemctl start $SERVICE" ;;
    openrc)  echo "rc-service $SERVICE stop && mv $BAK $BIN && rc-service $SERVICE start" ;;
    sysv)    echo "$INITD stop && mv $BAK $BIN && $INITD start" ;;
  esac
}

# 写服务文件:缺失才创建;与模板不一致时另存 .new 并提示 diff(不悄悄改用户定制)。
install_service_file() { # $1=目标路径 $2=临时模板 $3=权限
  if [ ! -f "$1" ]; then
    install -m "$3" "$2" "$1"
    log "已创建 $1"
  elif cmp -s "$2" "$1" 2>/dev/null || { ! command -v cmp >/dev/null 2>&1 && diff -q "$2" "$1" >/dev/null 2>&1; }; then
    log "$1 与脚本模板一致，无需改动"
  else
    install -m "$3" "$2" "${1}.new"
    warn "$1 与脚本模板不同，已保留原文件、新模板另存为 ${1}.new"
    warn "确认差异后再替换: diff -u $1 ${1}.new"
  fi
}

# ============================================================
# systemd
# ============================================================
if [ "$INIT" = "systemd" ]; then
  UNIT_TMP="$WORKDIR/unit"
  cat > "$UNIT_TMP" <<EOF
[Unit]
Description=New API Service
After=network.target

[Service]
User=${RUN_USER}
WorkingDirectory=${DIR}
EnvironmentFile=${ENV_FILE}
ExecStart=${BIN} --port ${PORT} --log-dir ${DIR}/logs
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  install_service_file "$UNIT" "$UNIT_TMP" 0644
  systemctl daemon-reload
  if [ "$MODE" = "update" ]; then
    log "重启服务 ..."
    systemctl restart "$SERVICE"
  else
    systemctl enable --now "$SERVICE" >/dev/null 2>&1 || systemctl restart "$SERVICE"
  fi
fi

# ============================================================
# OpenRC（Alpine / Gentoo）
# ============================================================
if [ "$INIT" = "openrc" ]; then
  RC_TMP="$WORKDIR/newapi-openrc"
  cat > "$RC_TMP" <<'RCEOF'
#!/sbin/openrc-run

name="__SERVICE__"
description="New API gateway"
command="__BIN__"
command_args="--port __PORT__ --log-dir __DIR__/logs"
directory="__DIR__"
command_background="yes"
pidfile="__PID_FILE__"
output_log="__LOG_FILE__"
error_log="__DIR__/logs/stderr.log"

depend() {
    need net
}

start_pre() {
    checkpath -d -o "__RUN_USER__" "__DIR__" "__DIR__/logs"
    set -a
    [ -f "__ENV_FILE__" ] && . "__ENV_FILE__"
    set +a
    return 0
}
RCEOF
  sed -i \
    -e "s|__SERVICE__|$SERVICE|g" -e "s|__BIN__|$BIN|g" -e "s|__PORT__|$PORT|g" \
    -e "s|__DIR__|$DIR|g" -e "s|__PID_FILE__|$PID_FILE|g" -e "s|__LOG_FILE__|$LOG_FILE|g" \
    -e "s|__RUN_USER__|$RUN_USER|g" -e "s|__ENV_FILE__|$ENV_FILE|g" \
    "$RC_TMP"
  install_service_file "$INITD" "$RC_TMP" 0755
  rc-update add "$SERVICE" default >/dev/null 2>&1 || warn "rc-update add 失败，开机自启未设置（服务仍会启动）"
  if [ "$MODE" = "update" ]; then
    log "重启服务 ..."
    rc-service "$SERVICE" restart || "$INITD" restart
  else
    rc-service "$SERVICE" start || "$INITD" start
  fi
fi

# ============================================================
# SysV(没有 systemd / OpenRC 的兜底:Debian 系 update-rc.d、RHEL 系 chkconfig)。
# POSIX sh,nohup + PID 文件,不依赖 start-stop-daemon;低权运行经 su 丢权,
# RUN_USER=root 时直接跑。
# ============================================================
if [ "$INIT" = "sysv" ]; then
  SYSV_TMP="$WORKDIR/newapi-sysv"
  cat > "$SYSV_TMP" <<'SYSEOF'
#!/bin/sh
# 由 new-api install.sh 生成。systemd/OpenRC 不可用时的兜底服务脚本。
### BEGIN INIT INFO
# Provides:          __SERVICE__
# Required-Start:    $network $remote_fs
# Required-Stop:     $network $remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: new-api gateway
### END INIT INFO
# chkconfig: 2345 90 10
# description: new-api AI gateway

NAME="__SERVICE__"
BIN="__BIN__"
DIR="__DIR__"
RUN_USER="__RUN_USER__"
ARGS="--port __PORT__ --log-dir __DIR__/logs"
ENV_FILE="__ENV_FILE__"
PID_FILE="__PID_FILE__"
LOG_FILE="__LOG_FILE__"

is_running() {
    [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null
}

start() {
    if is_running; then
        echo "$NAME 已在运行 (pid $(cat "$PID_FILE"))"
        return 0
    fi
    mkdir -p "$DIR/logs"
    touch "$LOG_FILE"
    [ "$RUN_USER" = "root" ] || chown "$RUN_USER" "$LOG_FILE" "$DIR/logs" 2>/dev/null || true
    echo "启动 $NAME ..."
    # 变量在这里展开成字面路径,只有 $! 留给子 shell(后台进程的 PID)
    DAEMON_CMD="set -a; [ -f '$ENV_FILE' ] && . '$ENV_FILE'; set +a; cd '$DIR' && nohup '$BIN' $ARGS >> '$LOG_FILE' 2>&1 & echo \$! > '$PID_FILE'"
    if [ "$RUN_USER" = "root" ]; then
        sh -c "$DAEMON_CMD"
    else
        su -s /bin/sh -c "$DAEMON_CMD" "$RUN_USER"
    fi
    sleep 1
    if is_running; then
        echo "已启动 (pid $(cat "$PID_FILE"))"
        return 0
    fi
    echo "启动失败，见 $LOG_FILE"
    return 1
}

stop() {
    if is_running; then
        pid="$(cat "$PID_FILE")"
        echo "停止 $NAME (pid $pid)"
        kill "$pid" 2>/dev/null || true
        i=0
        while [ "$i" -lt 10 ] && kill -0 "$pid" 2>/dev/null; do
            sleep 1
            i=$((i + 1))
        done
        kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null
    fi
    rm -f "$PID_FILE"
    return 0
}

restart() { stop; start; }

status() {
    if is_running; then
        echo "$NAME 运行中 (pid $(cat "$PID_FILE"))"
        return 0
    fi
    echo "$NAME 未运行"
    return 3
}

case "$1" in
    start)   start ;;
    stop)    stop ;;
    restart) restart ;;
    status)  status ;;
    *) echo "用法: $0 {start|stop|restart|status}"; exit 1 ;;
esac
SYSEOF
  sed -i \
    -e "s|__SERVICE__|$SERVICE|g" -e "s|__BIN__|$BIN|g" -e "s|__PORT__|$PORT|g" \
    -e "s|__DIR__|$DIR|g" -e "s|__PID_FILE__|$PID_FILE|g" -e "s|__LOG_FILE__|$LOG_FILE|g" \
    -e "s|__RUN_USER__|$RUN_USER|g" -e "s|__ENV_FILE__|$ENV_FILE|g" \
    "$SYSV_TMP"
  install_service_file "$INITD" "$SYSV_TMP" 0755
  if [ "$MODE" = "update" ]; then
    log "重启服务 ..."
    "$INITD" restart
  else
    if command -v update-rc.d >/dev/null 2>&1; then
      update-rc.d "$SERVICE" defaults >/dev/null 2>&1 || warn "update-rc.d 失败，开机自启未设置"
    elif command -v chkconfig >/dev/null 2>&1; then
      chkconfig --add "$SERVICE" >/dev/null 2>&1 && chkconfig "$SERVICE" on >/dev/null 2>&1 || warn "chkconfig 失败，开机自启未设置"
    else
      warn "没有 update-rc.d / chkconfig，开机自启请手动设置"
    fi
    "$INITD" start
  fi
fi

# ---------- 健康检查 ----------
i=1
while [ "$i" -le 15 ]; do
  if probe "http://127.0.0.1:${PORT}/api/status"; then
    log "服务已就绪: http://127.0.0.1:${PORT}/api/status"
    log "当前版本: $("$BIN" --version 2>/dev/null | head -n1 || echo "$NEW_VER")"
    if [ -n "$BAK" ]; then log "回滚方式: $(rollback_hint)"; fi
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done

case "$INIT" in
  systemd) warn "服务已启动但 15 秒内没探到 127.0.0.1:${PORT}/api/status，请检查: systemctl status $SERVICE && journalctl -u $SERVICE -n 50" ;;
  openrc)  warn "服务已启动但 15 秒内没探到 127.0.0.1:${PORT}/api/status，请检查: rc-service $SERVICE status; tail -n 50 $DIR/logs/*.log" ;;
  *)       warn "服务已启动但 15 秒内没探到 127.0.0.1:${PORT}/api/status，请检查: $INITD status; tail -n 50 $LOG_FILE" ;;
esac
exit 1