#!/usr/bin/env bash
# new-api 数据迁移脚本（把已备份的数据库还原到本机）
#
# 用途：换机器 / 重装后，把之前备份的 one-api.db 放回去并让服务带上原有数据，
#       不用重新配置渠道、令牌、用户和面板设置。
#
# 用法：
#   sudo bash migrate.sh --db /path/to/one-api.db
#   sudo bash migrate.sh --db /path/to/one-api.db --dry-run
#
# 可用参数与环境变量：
#   --db <path>        要还原的数据库文件（必填）
#   --dry-run          只检查并打印计划，不写入、不重启
#   --force            目标已存在数据库时也覆盖（默认拒绝，改为备份后中止）
#   --restart / --no-restart   还原后是否重启服务（默认重启）
#   NEW_API_DIR        数据目录（默认 /opt/new-api）
#   NEW_API_USER       运行用户（默认 newapi）
#   NEW_API_SERVICE    systemd 服务名（默认 new-api）
#   NEW_API_PORT       探活端口（默认从 unit 里读 --port，读不到用 3000）
#   NEW_API_DB_NAME    库文件名（默认 one-api.db）
#
# 安全约定：
#   - 目标已有数据库时绝不静默覆盖：先备份成 <db>.bak-<时间戳>，再写入。
#   - 写入前会校验源文件是不是 SQLite 库，避免把 -wal、压缩包误当数据库。
#   - 写入前停止服务：SQLite 运行中写入会产生不一致快照。
#   - 还原后修正属主/权限并为目录补写权限：SQLite 的 journal 要在库所在目录创建文件，
#     属主或目录权限不对会以 "attempt to write a readonly database" 启动失败。

set -eu

DB_SRC=""
DRY_RUN=0
FORCE=0
DO_RESTART=1

DIR="${NEW_API_DIR:-/opt/new-api}"
RUN_USER="${NEW_API_USER:-newapi}"
SERVICE="${NEW_API_SERVICE:-new-api}"
PORT="${NEW_API_PORT:-}"
DB_NAME="${NEW_API_DB_NAME:-one-api.db}"
UNIT="/etc/systemd/system/${SERVICE}.service"

log()  { printf '\033[32m[new-api]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[new-api]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m[new-api]\033[0m %s\n' "$*" >&2; exit 1; }
step() { printf '\n\033[36m==> %s\033[0m\n' "$*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --db)         shift; [ $# -gt 0 ] || die "--db 需要一个文件路径"; DB_SRC="$1" ;;
    --dry-run)    DRY_RUN=1 ;;
    --force)      FORCE=1 ;;
    --restart)    DO_RESTART=1 ;;
    --no-restart) DO_RESTART=0 ;;
    -h|--help)    sed -n '2,30p' "$0" 2>/dev/null || true; exit 0 ;;
    *)            die "未知参数: $1（可用: --db <path> / --dry-run / --force / --restart / --no-restart / --help）" ;;
  esac
  shift
done

[ -n "$DB_SRC" ] || die "必须指定 --db <path>。用 --help 看用法。"
[ -f "$DB_SRC" ] || die "找不到文件: $DB_SRC"

# ---------- 1. 校验源文件确实是 SQLite 数据库 ----------
step "校验源数据库"

# 只读前 15 字节:SQLite magic 是 "SQLite format 3"(15 字符),第 16 字节是 NUL。
# 读满 16 字节会把 NUL 带进 shell 变量,触发 "ignored null byte" 警告。
HEAD15="$(head -c 15 "$DB_SRC" 2>/dev/null || true)"
[ "$HEAD15" = "SQLite format 3" ] || die "$DB_SRC 不是 SQLite 数据库（文件头不匹配）。
  常见原因：传进来的是 -wal/-shm 或压缩包。请确认传的是数据库文件本身。"

SRC_SIZE="$(stat -c%s "$DB_SRC" 2>/dev/null || echo 0)"
log "文件头校验通过（$SRC_SIZE 字节）"

# 有 sqlite3 或 python3 时做一次深校验，确认是 new-api 的库而不只是任意 SQLite 文件
deep_check() {
  if command -v sqlite3 >/dev/null 2>&1; then
    sqlite3 "$1" "select count(*) from sqlite_master where type='table' and name in ('options','channels','users')" 2>/dev/null || echo 0
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$1" <<'PY' 2>/dev/null || echo 0
import sqlite3, sys
try:
    c = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
    print(c.execute("select count(*) from sqlite_master where type='table' and name in ('options','channels','users')").fetchone()[0])
except Exception:
    print(0)
PY
  else
    echo "skip"
  fi
}
FOUND="$(deep_check "$DB_SRC")"
if [ "$FOUND" = "skip" ]; then
  warn "本机没有 sqlite3 / python3，跳过表结构校验（仅做了文件头校验）"
elif [ "$FOUND" -lt 3 ] 2>/dev/null; then
  die "$DB_SRC 虽然是 SQLite 库，但缺少 options/channels/users 表，不像是 new-api 的数据库。"
else
  log "表结构校验通过（options/channels/users 均存在）"
fi

# 源库旁边若有未合并的 WAL，一并带上是正确的；只复制 .db 会丢最近事务
WAL_SRC=""
if [ -f "${DB_SRC}-wal" ]; then
  if [ -s "${DB_SRC}-wal" ]; then
    WAL_SRC="${DB_SRC}-wal"
    warn "检测到非空的 ${DB_SRC}-wal，将连同 WAL/SHM 一起还原（否则最近的事务会丢）"
    warn "如果源库所在服务还在运行，强烈建议先停掉它再复制一次。"
  else
    log "源库旁有空的 -wal，忽略"
  fi
fi

# ---------- 2. 确定目标位置 ----------
TARGET_DB="${DIR}/${DB_NAME}"

step "迁移计划"
echo "  源数据库 : $DB_SRC"
echo "  目标位置 : $TARGET_DB"
echo "  数据目录 : $DIR"
echo "  运行用户 : $RUN_USER"
echo "  服务名   : $SERVICE"
echo "  模式     : $([ "$DRY_RUN" = "1" ] && echo '仅检查（--dry-run）' || echo '执行写入')"

if [ -f "$TARGET_DB" ]; then
  if [ "$FORCE" = "1" ]; then
    warn "目标已有数据库，--force 已指定，将先备份再覆盖"
  else
    CONFLICT=1
  fi
else
  CONFLICT=0
fi

if [ "$DRY_RUN" = "1" ]; then
  # --dry-run 是「报告」模式，不是「闸门」：它必须把计划连同冲突一起打印出来，
  # 并以 0 退出，这样它可以安全地嵌在 set -e 的预检里。真正的闸门是执行模式。
  if [ "${CONFLICT:-0}" = "1" ]; then
    echo
    warn "目标已存在数据库: $TARGET_DB"
    warn "按当前参数实际执行时会被拒绝（避免误删线上数据）。"
    warn "确认要覆盖请加 --force（覆盖前会自动备份成 ${TARGET_DB}.bak-<时间戳>）。"
    echo "  结论 : 会被拒绝（需要 --force）"
  else
    echo
    echo "  结论 : 可以执行，未检测到冲突"
  fi
  log "仅检查完成，未做任何改动"
  exit 0
fi

if [ "${CONFLICT:-0}" = "1" ]; then
  die "目标已存在数据库: $TARGET_DB
  拒绝直接覆盖。请先确认这是要替换的旧数据：
    - 确认要覆盖：加 --force（会先自动备份成 ${TARGET_DB}.bak-<时间戳>）
    - 只想看看：加 --dry-run"
fi

[ "$(id -u)" = "0" ] || die "需要 root 权限：请用 sudo 执行"

# ---------- 3. 停服务 ----------
SVC_MANAGED=0
if command -v systemctl >/dev/null 2>&1 && [ -f "$UNIT" ]; then
  SVC_MANAGED=1
  step "停止服务 $SERVICE"
  systemctl stop "$SERVICE" || die "停止 $SERVICE 失败"
  log "已停止（确保没有进程在写库）"
else
  warn "未找到 $UNIT，跳过 systemd 操作；如果你用的是 Docker，请改为把库放进容器的挂载目录"
fi

# ---------- 4. 备份现有库并还原 ----------
if [ -f "$TARGET_DB" ]; then
  step "备份现有数据库"
  BAK="${TARGET_DB}.bak-$(date +%Y%m%d%H%M%S)"
  cp -a "$TARGET_DB" "$BAK"
  log "已备份 → $BAK"
  # 旧的 WAL/SHM 必须清掉，否则会盖掉刚还原的数据
  for f in "${TARGET_DB}-wal" "${TARGET_DB}-shm"; do
    [ -f "$f" ] && rm -f "$f" && log "移除陈旧的 $(basename "$f")"
  done
fi

step "还原数据库"
mkdir -p "$DIR"
cp -f "$DB_SRC" "$TARGET_DB"
if [ -n "$WAL_SRC" ]; then
  cp -f "$WAL_SRC" "${TARGET_DB}-wal"
  [ -f "${DB_SRC}-shm" ] && cp -f "${DB_SRC}-shm" "${TARGET_DB}-shm"
  log "已连同 WAL/SHM 一起还原"
fi
log "已写入 $TARGET_DB"

# ---------- 5. 修正属主与权限（这是最容易踩的坑）----------
step "修正属主与权限"
if id "$RUN_USER" >/dev/null 2>&1; then
  chown "$RUN_USER:$RUN_USER" "$TARGET_DB"
  [ -f "${TARGET_DB}-wal" ] && chown "$RUN_USER:$RUN_USER" "${TARGET_DB}-wal"
  [ -f "${TARGET_DB}-shm" ] && chown "$RUN_USER:$RUN_USER" "${TARGET_DB}-shm"
  # 目录必须可写：SQLite journal 在库所在目录创建文件
  chown "$RUN_USER:$RUN_USER" "$DIR"
  mkdir -p "$DIR/logs"
  chown -R "$RUN_USER:$RUN_USER" "$DIR/logs"
  chmod 644 "$TARGET_DB"
  log "属主设为 $RUN_USER，权限 644，目录可写"
else
  warn "运行用户 $RUN_USER 不存在，跳过 chown。
  如果服务以某个用户运行，请手动执行: chown <user>:<user> $TARGET_DB 和 $DIR"
fi

# ---------- 6. 启动并验证 ----------
if [ "$SVC_MANAGED" = "1" ]; then
  step "启动服务 $SERVICE"
  systemctl start "$SERVICE" || die "启动 $SERVICE 失败，看日志: journalctl -u $SERVICE -n 50"
  if [ -z "$PORT" ]; then
    PORT="$(sed -n 's/.*--port[= ]\([0-9][0-9]*\).*/\1/p' "$UNIT" | head -n1)"
  fi
  PORT="${PORT:-3000}"
  log "等待服务就绪（端口 $PORT）"
  OK=0
  i=1
  while [ $i -le 30 ]; do
    if curl -fsS -m 3 "http://127.0.0.1:${PORT}/api/status" >/dev/null 2>&1; then OK=1; break; fi
    sleep 2
    i=$((i + 1))
  done
  if [ "$OK" = "1" ]; then
    log "服务已就绪: http://127.0.0.1:${PORT}/api/status"
  else
    warn "服务 30 次探活未成功。排查: journalctl -u $SERVICE -n 80"
    warn "最常见的两个原因：属主不对（attempt to write a readonly database），或目录不可写。"
    exit 1
  fi
fi

step "完成"
echo "  已还原: $TARGET_DB"
[ "$SVC_MANAGED" = "1" ] && echo "  服务状态: $(systemctl is-active "$SERVICE" 2>/dev/null || echo 未知)"
echo
echo "  回滚（如果数据不对）:"
if ls "${TARGET_DB}".bak-* >/dev/null 2>&1; then
  echo "    sudo systemctl stop $SERVICE"
  echo "    sudo cp -a $(ls -t "${TARGET_DB}".bak-* | head -n1) $TARGET_DB"
  echo "    sudo chown $RUN_USER:$RUN_USER $TARGET_DB"
  echo "    sudo systemctl start $SERVICE"
else
  echo "    （本次没有覆盖任何旧库，无需回滚）"
fi
