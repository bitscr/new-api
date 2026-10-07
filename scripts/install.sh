#!/usr/bin/env bash
# new-api 一键安装 / 升级脚本（Linux 二进制 + systemd）
#
# 安装（首次部署）:
#   curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash
#
# 升级（已部署过）:
#   curl -fsSL https://raw.githubusercontent.com/bitscr/new-api/main/scripts/install.sh | sudo bash -s -- --update
#
# 可用环境变量（写在 sudo 后面，例如 sudo NEW_API_PORT=8080 bash）:
#   NEW_API_VERSION    指定版本 tag，如 v0.0.1（默认 latest）
#   NEW_API_PORT       监听端口（默认 3000；已装过时自动沿用 unit 里的 --port）
#   NEW_API_DIR        数据/日志目录（默认 /opt/new-api）
#   NEW_API_BIN        二进制路径（默认 /usr/local/bin/new-api）
#   NEW_API_USER       运行用户（默认 newapi）
#   NEW_API_SERVICE    systemd 服务名（默认 new-api）
#   NEW_API_REPO       GitHub 仓库（默认 bitscr/new-api）
#   NEW_API_ARCH       强制架构 amd64|arm64（默认按 uname -m 自动判断）
#   NEW_API_SKIP_SYSTEMD=1  只下发二进制，不创建用户/目录/unit（自测用）
#   NEW_API_SKIP_CHECKSUM=1 跳过 sha256 校验（不建议）
#
# 已存在的东西不会被覆盖：env 文件、unit 文件只在缺失时创建；
# 发现 unit 与脚本模板不一致时不会改写，而是另存为 .new 并提示 diff。

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
SKIP_SYSTEMD="${NEW_API_SKIP_SYSTEMD:-0}"
SKIP_CHECKSUM="${NEW_API_SKIP_CHECKSUM:-0}"
UNIT="/etc/systemd/system/${SERVICE}.service"

log()  { printf '\033[32m[new-api]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[new-api]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[new-api]\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    -u|--update) MODE="update" ;;
    --install)   MODE="install" ;;
    --version)   shift; [ $# -gt 0 ] || die "--version 需要一个 tag，例如 --version v0.0.1"; VERSION="$1" ;;
    -h|--help)   sed -n '2,24p' "$0" 2>/dev/null || true; exit 0 ;;
    *)           die "未知参数: $1（可用: --update / --version <tag> / --help）" ;;
  esac
  shift
done

# ---------- 前置检查 ----------
[ "$(id -u)" = "0" ] || die "需要 root 权限：请用 sudo bash 执行"
command -v curl >/dev/null 2>&1 || die "缺少 curl"
command -v tar  >/dev/null 2>&1 || die "缺少 tar"

if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64)   ARCH="amd64" ;;
    aarch64|arm64)  ARCH="arm64" ;;
    *) die "不支持的架构 $(uname -m)，只提供 amd64 / arm64 构建。可用 NEW_API_ARCH 强制指定。" ;;
  esac
fi
case "$ARCH" in
  amd64|arm64) ;;
  *) die "NEW_API_ARCH 只能是 amd64 或 arm64（当前: $ARCH）" ;;
esac

# 已装过时，端口默认沿用 unit 里已有的 --port
if [ -z "$PORT" ] && [ -f "$UNIT" ]; then
  PORT="$(sed -n 's/.*--port[= ]\([0-9][0-9]*\).*/\1/p' "$UNIT" | head -n1)"
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
log "下载 ${ASSET}（${VERSION}，${ARCH}）"
curl -fsSL -o "$WORKDIR/$ASSET" "$URL" || die "下载失败: $URL"

if [ "$SKIP_CHECKSUM" = "1" ]; then
  warn "已按 NEW_API_SKIP_CHECKSUM=1 跳过 sha256 校验"
else
  curl -fsSL -o "$WORKDIR/$ASSET.sha256" "${URL}.sha256" || die "下载校验文件失败: ${URL}.sha256"
  # Release 里的 .sha256 记录的是构建目录内的相对路径，这里只取哈希值重新拼本地文件名
  EXPECT="$(awk '{print $1; exit}' "$WORKDIR/$ASSET.sha256")"
  [ -n "$EXPECT" ] || die "校验文件内容异常: ${URL}.sha256"
  if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL="$(sha256sum "$WORKDIR/$ASSET" | awk '{print $1}')"
  else
    ACTUAL="$(shasum -a 256 "$WORKDIR/$ASSET" | awk '{print $1}')"
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

# ---------- 准备运行环境（安装模式）----------
if [ "$MODE" = "install" ] && [ "$SKIP_SYSTEMD" != "1" ]; then
  if ! id "$RUN_USER" >/dev/null 2>&1; then
    log "创建运行用户 $RUN_USER"
    useradd -r -s /usr/sbin/nologin "$RUN_USER" 2>/dev/null || useradd -r -s /sbin/nologin "$RUN_USER"
  fi
  mkdir -p "$DIR" "$DIR/logs" "$DIR/data"
  chown -R "$RUN_USER:$RUN_USER" "$DIR"

  if [ ! -f "$DIR/env" ]; then
    log "生成 $DIR/env（占位注释，按需填 MySQL/PostgreSQL/Redis）"
    cat > "$DIR/env" <<'ENVEOF'
# 需要外部数据库 / Redis 时取消注释并改成实际连接串，改完 systemctl restart new-api
# SQL_DSN=postgresql://user:password@127.0.0.1:5432/new-api
# REDIS_CONN_STRING=redis://:password@127.0.0.1:6379
# TZ=Asia/Shanghai
ENVEOF
    chown "$RUN_USER:$RUN_USER" "$DIR/env"
    chmod 600 "$DIR/env"
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
mkdir -p "$(dirname "$BIN")"
install -m 0755 "$NEW_BIN" "$BIN"
log "二进制已就位: $BIN"

if [ "$SKIP_SYSTEMD" = "1" ]; then
  log "NEW_API_SKIP_SYSTEMD=1：跳过 systemd，收工"
  exit 0
fi

# ---------- systemd unit ----------
UNIT_TMP="$WORKDIR/unit"
cat > "$UNIT_TMP" <<EOF
[Unit]
Description=New API Service
After=network.target

[Service]
User=${RUN_USER}
WorkingDirectory=${DIR}
EnvironmentFile=${DIR}/env
ExecStart=${BIN} --port ${PORT} --log-dir ${DIR}/logs
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

if [ ! -f "$UNIT" ]; then
  install -m 0644 "$UNIT_TMP" "$UNIT"
  log "已创建 $UNIT"
elif cmp -s "$UNIT_TMP" "$UNIT"; then
  log "systemd unit 与模板一致，无需改动"
else
  install -m 0644 "$UNIT_TMP" "${UNIT}.new"
  warn "$UNIT 与脚本模板不同，已保留原文件、新模板另存为 ${UNIT}.new"
  warn "确认差异后再替换: diff -u $UNIT ${UNIT}.new"
fi

command -v systemctl >/dev/null 2>&1 || die "没有 systemctl，无法管理服务（可加 NEW_API_SKIP_SYSTEMD=1 只下发二进制）"
systemctl daemon-reload
if [ "$MODE" = "update" ]; then
  log "重启服务 ..."
  systemctl restart "$SERVICE"
else
  systemctl enable --now "$SERVICE" >/dev/null 2>&1 || systemctl restart "$SERVICE"
fi

# ---------- 健康检查 ----------
i=1
while [ "$i" -le 15 ]; do
  if curl -fsS --max-time 2 "http://127.0.0.1:${PORT}/api/status" >/dev/null 2>&1; then
    log "服务已就绪: http://127.0.0.1:${PORT}/api/status"
    log "当前版本: $("$BIN" --version 2>/dev/null | head -n1 || echo "$NEW_VER")"
    if [ -n "$BAK" ]; then log "回滚方式: systemctl stop $SERVICE && mv $BAK $BIN && systemctl start $SERVICE"; fi
    exit 0
  fi
  i=$((i + 1))
  sleep 1
done

warn "服务已启动但 15 秒内没探到 127.0.0.1:${PORT}/api/status，请检查: systemctl status $SERVICE && journalctl -u $SERVICE -n 50"
exit 1
