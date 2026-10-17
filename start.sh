#!/usr/bin/env bash
# 诗词雅集 —— 启动脚本（构建 / 启动 / 停止 / 重启 / 状态 / 日志）
#
#   ./start.sh start            构建并后台启动（读取同目录 .env）
#   ./start.sh start --no-build 直接启动已编译的 server
#   ./start.sh stop             停止
#   ./start.sh restart          重启
#   ./start.sh status           查看运行状态
#   ./start.sh logs             跟踪日志（Ctrl+C 退出）
#   ./start.sh build            仅编译
#
# 环境变量（可选）：PORT / SHICI_ENV_FILE / LOG_FILE 等，外部已设置的值优先于 .env
set -uo pipefail

APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${APP_DIR}"

BIN="${BIN:-${APP_DIR}/server}"
RUN_DIR="${APP_DIR}/.tmp"
PID_FILE="$RUN_DIR/shici.pid"
LOG_FILE="${LOG_FILE:-$RUN_DIR/shici.log}"
PORT_FILE="$RUN_DIR/shici.port"
GO="${GO:-go}"

mkdir -p "$RUN_DIR"

info() { printf '\033[32m%s\033[0m\n' "$*"; }
warn() { printf '\033[33m%s\033[0m\n' "$*"; }
err()  { printf '\033[31m%s\033[0m\n' "$*" >&2; }

# 定位 go：优先 PATH，其次常见安装位置
find_go() {
  if command -v "${GO}" >/dev/null 2>&1; then
    return 0
  fi
  for p in /usr/local/go/bin/go /opt/homebrew/bin/go /usr/lib/go/bin/go; do
    if [ -x "$p" ]; then GO="$p"; return 0; fi
  done
  err "未找到 go，可用 GO=/path/to/go ./start.sh build 指定"
  return 1
}

# 解析实际监听端口：环境变量 PORT 优先，其次 .env
resolve_port() {
  if [ -n "${PORT:-}" ]; then
    printf '%s' "${PORT}"
    return
  fi
  if [ -f "$APP_DIR/.env" ]; then
    grep -E '^[[:space:]]*PORT=' "$APP_DIR/.env" | tail -n 1 | cut -d= -f2 | tr -d ' "'
  fi
}

running_pid() {
  [ -f "${PID_FILE}" ] || return 1
  local pid
  pid="$(cat "${PID_FILE}" 2>/dev/null || true)"
  [ -n "${pid:-}" ] || return 1
  kill -0 "${pid}" 2>/dev/null || return 1
  echo "${pid}"
}

do_build() {
  find_go || exit 1
  info "编译中：${BIN}"
  if ! "${GO}" build -trimpath -o "${BIN}" ./cmd/server; then
    err "编译失败"
    exit 1
  fi
  info "编译完成"
}

do_start() {
  local build=1
  [ "${1:-}" = "--no-build" ] && build=0

  if pid="$(running_pid)"; then
    warn "已在运行（PID ${pid}），如需重启请执行：./start.sh restart"
    return 0
  fi
  if [ "$build" = "1" ]; then
    do_build
  elif [ ! -x "${BIN}" ]; then
    err "未找到可执行文件 ${BIN}，请先执行：./start.sh build"
    exit 1
  fi

  info "启动服务…（配置来源：环境变量 > ${SHICI_ENV_FILE:-${APP_DIR}/.env}）"
  nohup "${BIN}" >>"${LOG_FILE}" 2>&1 &
  echo $! >"${PID_FILE}"
  resolve_port >"${PORT_FILE}"

  sleep 2
  if pid="$(running_pid)"; then
    info "已启动（PID ${pid}），日志：${LOG_FILE}"
    tail -n 5 "${LOG_FILE}" 2>/dev/null | sed 's/^/  /'
  else
    err "启动失败，最近日志："
    tail -n 20 "${LOG_FILE}" 2>/dev/null | sed 's/^/  /'
    rm -f "${PID_FILE}"
    exit 1
  fi
}

do_stop() {
  if ! pid="$(running_pid)"; then
    warn "未在运行"
    rm -f "${PID_FILE}"
    return 0
  fi
  info "停止中（PID ${pid}）…"
  kill "${pid}" 2>/dev/null
  for _ in $(seq 1 20); do
    kill -0 "${pid}" 2>/dev/null || { rm -f "${PID_FILE}" "${PORT_FILE}"; info "已停止"; return 0; }
    sleep 0.5
  done
  warn "优雅退出超时，发送 SIGKILL"
  kill -9 "${pid}" 2>/dev/null
  rm -f "${PID_FILE}" "${PORT_FILE}"
  info "已强制停止"
}

do_status() {
  if pid="$(running_pid)"; then
    info "运行中（PID ${pid}）"
    ps -o pid,etime,rss,command -p "${pid}" 2>/dev/null | sed 's/^/  /'
    if [ -f "${PORT_FILE}" ]; then
      echo "  监听端口：$(cat "${PORT_FILE}")"
    fi
  else
    warn "未在运行"
    return 1
  fi
}

do_logs() {
  [ -f "${LOG_FILE}" ] || { err "日志文件不存在：${LOG_FILE}"; exit 1; }
  info "跟踪日志：${LOG_FILE}（Ctrl+C 退出）"
  tail -n 100 -f "${LOG_FILE}"
}

usage() {
  cat <<EOF
用法：./start.sh {start [--no-build]|stop|restart|status|logs|build}

  start            构建并后台启动（读取 .env）
  start --no-build 跳过构建，直接启动
  stop             停止（先 SIGTERM，20 次轮询后 SIGKILL）
  restart          重启
  status           运行状态与监听端口
  logs             跟踪日志
  build            仅编译

文件：二进制 ${BIN} / PID ${PID_FILE} / 日志 ${LOG_FILE}
EOF
}

# 默认只打印用法，避免误触启动
case "${1:-help}" in
  build)   do_build ;;
  start)   do_start "${2:-}" ;;
  stop)    do_stop ;;
  restart) do_stop; do_start "${2:-}" ;;
  status)  do_status ;;
  logs)    do_logs ;;
  help|-h|--help) usage; exit 0 ;;
  *) usage; exit 1 ;;
esac
