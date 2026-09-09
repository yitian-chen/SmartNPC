#!/usr/bin/env bash
# AgentTown_v3 — vLLM 正向隧道脚本（Windows Git Bash 端运行）
#
# 把 AutoDL 容器里的 vLLM 推理服务（容器内 127.0.0.1:8000）通过 SSH 本地
# 正向隧道（-L）映射到本机 Windows 的 localhost:${VLLM_LOCAL_PORT}。这样
# Windows 上跑的 MCP 进程经 --llm-config=assets/llm_backend.yaml 把战略/战术/
# 对话三层都指向 http://127.0.0.1:${VLLM_LOCAL_PORT}，即可调用自部署 vLLM。
#
# 与 start-tunnel.sh（反向隧道 -R，供 Ollama 反应层用）的区别：
#   - 本脚本是「正向」：Windows 主动连 AutoDL，把远端 8000 拉到本地。
#   - 前提：AutoDL 实例已开机，且容器内已跑 vllm serve（监听 127.0.0.1:8000）。
#
# 用法：
#   bash start-vllm-tunnel.sh              # 后台拉起隧道（保活）
#   bash start-vllm-tunnel.sh --foreground # 前台运行（看 SSH 日志调试）
#   bash start-vllm-tunnel.sh --stop       # 停止隧道
#   bash start-vllm-tunnel.sh --status     # 查看状态
#
# 环境变量（可在 .env 配置覆盖默认值）：
#   VLLM_REMOTE_HOST       AutoDL SSH 地址（默认用 ssh 别名 autodl，见 ~/.ssh/config）
#   VLLM_REMOTE_PORT       AutoDL SSH 端口（默认 18705）
#   VLLM_REMOTE_USER       SSH 用户（默认 root）
#   VLLM_REMOTE_PORT_SSH   容器内 vLLM 监听端口（默认 8000）
#   VLLM_LOCAL_PORT        Windows 本地监听端口（默认 8000，与 llm_backend.yaml 对齐）
#
# 前置：
#   - ~/.ssh/config 已配 autodl 别名（HostName/Port/User/IdentityFile）
#   - AutoDL 实例已开机（关机时网关返回 502，隧道无法建立）
#   - 容器内 vLLM 已启动：vllm serve <model> --port 8000

set -uo pipefail

# ─── 颜色输出 ──────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }

# ─── 路径与配置 ────────────────────────────────────────────────
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="$PROJECT_DIR/.env"

# 从 .env 加载 VLLM_ 相关变量（存在则覆盖默认）
load_env() {
    if [ ! -f "$ENV_FILE" ]; then
        return 0
    fi
    while IFS='=' read -r key value || [ -n "$key" ]; do
        case "$key" in
            ''|\#*) continue ;;
        esac
        case "$key" in
            VLLM_*) export "$key=$value" ;;
        esac
    done < "$ENV_FILE"
}
load_env

# ─── 配置 ──────────────────────────────────────────────────────
# 远端 SSH 目标：默认用 ~/.ssh/config 的 autodl 别名（host/user/port/identity
# 都在别名里）；VLLM_REMOTE_HOST 显式给 IP 时用 IP 覆盖别名。
VLLM_REMOTE_HOST="${VLLM_REMOTE_HOST:-autodl}"
VLLM_REMOTE_PORT="${VLLM_REMOTE_PORT:-18705}"
VLLM_REMOTE_USER="${VLLM_REMOTE_USER:-root}"
VLLM_REMOTE_PORT_SSH="${VLLM_REMOTE_PORT_SSH:-8000}"
VLLM_LOCAL_PORT="${VLLM_LOCAL_PORT:-8000}"

# 显式给了 IP 才拆 host+user，否则整体走别名（别名已含 User root）
if [ "$VLLM_REMOTE_HOST" != "autodl" ]; then
    SSH_TARGET="${VLLM_REMOTE_USER}@${VLLM_REMOTE_HOST}"
    SSH_PORT_ARG=(-p "$VLLM_REMOTE_PORT")
else
    SSH_TARGET="autodl"
    SSH_PORT_ARG=()
fi

# PID 文件与日志
PID_DIR="$PROJECT_DIR/.run"
PID_FILE="$PID_DIR/vllm-tunnel.pid"
LOG_FILE="$PID_DIR/vllm-tunnel.log"
mkdir -p "$PID_DIR"

# ─── 参数解析 ──────────────────────────────────────────────────
ACTION="start"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --stop)       ACTION="stop"; shift ;;
        --status)     ACTION="status"; shift ;;
        --foreground) ACTION="foreground"; shift ;;
        -h|--help)
            echo "Usage: bash start-vllm-tunnel.sh [OPTIONS]"
            echo ""
            echo "把 AutoDL 容器 vLLM（127.0.0.1:${VLLM_REMOTE_PORT_SSH}）正向隧道到"
            echo "本机 Windows localhost:${VLLM_LOCAL_PORT}。"
            echo ""
            echo "Options:"
            echo "  (无参数)       后台拉起隧道（默认）"
            echo "  --foreground   前台运行（调试用）"
            echo "  --stop         停止隧道"
            echo "  --status       查看状态"
            echo ""
            echo "环境变量（可在 .env 配置）："
            echo "  VLLM_REMOTE_HOST=$VLLM_REMOTE_HOST"
            echo "  VLLM_REMOTE_PORT=$VLLM_REMOTE_PORT"
            echo "  VLLM_REMOTE_USER=$VLLM_REMOTE_USER"
            echo "  VLLM_REMOTE_PORT_SSH=$VLLM_REMOTE_PORT_SSH"
            echo "  VLLM_LOCAL_PORT=$VLLM_LOCAL_PORT"
            exit 0 ;;
        *) warn "Unknown option: $1"; shift ;;
    esac
done

# ─── SSH 选项 ──────────────────────────────────────────────────
# -N 不执行远程命令；-T 不分配 TTY
# -L 本地端口:远端主机:远端端口 → 把容器内 127.0.0.1:8000 映射到本地
# ExitOnForwardFailure 保证端口被占/远端不可达时 SSH 立即退出而非挂起
# ServerAliveInterval 30s keepalive，防 NAT 超时断连
SSH_OPTS=(
    -N -T
    -o ExitOnForwardFailure=yes
    -o ServerAliveInterval=30
    -o ServerAliveCountMax=3
    -o StrictHostKeyChecking=accept-new
    -o TCPKeepAlive=yes
)
if [ ${#SSH_PORT_ARG[@]} -gt 0 ]; then
    SSH_OPTS+=("${SSH_PORT_ARG[@]}")
fi

# 正向隧道：本地 :VLLM_LOCAL_PORT → 容器内 127.0.0.1:VLLM_REMOTE_PORT_SSH
# 关键用 127.0.0.1：vLLM 默认只绑定容器内回环地址。
SSH_FORWARD_ARGS=(-L "${VLLM_LOCAL_PORT}:127.0.0.1:${VLLM_REMOTE_PORT_SSH}")

# ─── 辅助函数 ──────────────────────────────────────────────────

# 检测本地端口是否已被占用（含 vLLM 是否可达）
check_local_port() {
    if ! command -v curl >/dev/null 2>&1; then
        return 1
    fi
    curl -sf "http://127.0.0.1:${VLLM_LOCAL_PORT}/health" >/dev/null 2>&1
}

read_pid() {
    if [ ! -f "$PID_FILE" ]; then
        echo ""
        return
    fi
    local pid
    pid=$(cat "$PID_FILE" 2>/dev/null)
    if [ -z "$pid" ]; then
        echo ""
        return
    fi
    if kill -0 "$pid" 2>/dev/null; then
        echo "$pid"
    else
        rm -f "$PID_FILE"
        echo ""
    fi
}

do_status() {
    local pid
    pid=$(read_pid)
    if [ -z "$pid" ]; then
        warn "vLLM 正向隧道未运行"
        echo ""
        echo "  启动：bash start-vllm-tunnel.sh"
        echo "  转发：本地 :${VLLM_LOCAL_PORT} → ${SSH_TARGET} 容器内 127.0.0.1:${VLLM_REMOTE_PORT_SSH}"
        return 1
    fi
    ok "vLLM 正向隧道运行中（PID $pid）"
    echo ""
    echo "  转发：本地 :${VLLM_LOCAL_PORT} → ${SSH_TARGET} 容器内 127.0.0.1:${VLLM_REMOTE_PORT_SSH}"
    if check_local_port; then
        echo -e "  vLLM 可达：${GREEN}http://127.0.0.1:${VLLM_LOCAL_PORT}/health 正常${NC}"
    else
        echo -e "  vLLM 可达：${YELLOW}隧道已建，但 /health 无响应（容器内 vllm serve 可能未启动）${NC}"
    fi
    echo "  日志：$LOG_FILE"
    return 0
}

do_stop() {
    local pid
    pid=$(read_pid)
    if [ -z "$pid" ]; then
        warn "vLLM 正向隧道未运行（无需停止）"
        return 0
    fi
    info "Stopping vLLM tunnel (PID $pid)..."
    kill "$pid" 2>/dev/null
    local elapsed=0
    while [ $elapsed -lt 5 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        sleep 1; elapsed=$((elapsed + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        warn "隧道未响应 SIGTERM，发送 SIGKILL"
        kill -9 "$pid" 2>/dev/null
    fi
    rm -f "$PID_FILE"
    ok "vLLM 正向隧道已停止"
}

do_start() {
    local existing_pid
    existing_pid=$(read_pid)
    if [ -n "$existing_pid" ]; then
        ok "vLLM 正向隧道已在运行（PID $existing_pid），跳过启动"
        do_status
        return 0
    fi

    # 前置检查：本地端口是否被占用（其他程序抢了 8000）
    if check_local_port; then
        ok "本地 :${VLLM_LOCAL_PORT} 已有服务在响应 /health（可能 vLLM 直连或已有隧道）"
        warn "若这是本机自跑的 vLLM，无需隧道；若想走 AutoDL，请先 --stop 再排查端口占用"
        echo ""
    fi

    info "=== 启动 vLLM 正向隧道 ==="
    info "  目标：${SSH_TARGET}"
    info "  转发：本地 :${VLLM_LOCAL_PORT} → 容器内 127.0.0.1:${VLLM_REMOTE_PORT_SSH}"
    info "  日志：$LOG_FILE"

    nohup ssh "${SSH_OPTS[@]}" "${SSH_FORWARD_ARGS[@]}" "$SSH_TARGET" \
        > "$LOG_FILE" 2>&1 &
    local pid=$!
    disown
    echo "$pid" > "$PID_FILE"

    info "Waiting for tunnel to establish (max 10s)..."
    local elapsed=0
    while [ $elapsed -lt 10 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            echo ""
            fail "SSH 进程已退出（端口被占 / 认证失败 / AutoDL 实例关机返回 502）。日志："
            cat "$LOG_FILE" 2>/dev/null | tail -20
            rm -f "$PID_FILE"
        fi
        if [ $elapsed -ge 3 ]; then
            break
        fi
        sleep 1; elapsed=$((elapsed + 1)); printf "."
    done
    echo ""

    if ! kill -0 "$pid" 2>/dev/null; then
        fail "vLLM 隧道启动失败（进程已退出）"
    fi

    ok "vLLM 正向隧道已建立（PID $pid）"
    echo ""
    echo -e "  ${BOLD}MCP 配置${NC}"
    echo -e "    LLM 后端指向：${CYAN}http://127.0.0.1:${VLLM_LOCAL_PORT}${NC}"
    echo -e "    验证：${CYAN}curl http://127.0.0.1:${VLLM_LOCAL_PORT}/v1/models${NC}"
    echo ""
    echo -e "  ${BOLD}保活说明${NC}"
    echo -e "    ServerAliveInterval=30s 自动保活，关掉 Git Bash 窗口不影响"
    echo -e "    查看状态：bash start-vllm-tunnel.sh --status"
    echo -e "    停止隧道：bash start-vllm-tunnel.sh --stop"
    echo ""
    echo -e "  ${YELLOW}注意${NC}"
    echo -e "    1. 确保 AutoDL 实例已开机（关机时网关回 502，隧道建不起来）"
    echo -e "    2. 确保容器内 vllm serve 已启动（vllm serve <model> --port ${VLLM_REMOTE_PORT_SSH}）"
    echo -e "    3. 日志在 $LOG_FILE"
}

do_foreground() {
    info "前台运行 vLLM 正向隧道（Ctrl+C 退出）"
    info "  目标：${SSH_TARGET}"
    info "  转发：本地 :${VLLM_LOCAL_PORT} → 容器内 127.0.0.1:${VLLM_REMOTE_PORT_SSH}"
    echo ""
    exec ssh "${SSH_OPTS[@]}" "${SSH_FORWARD_ARGS[@]}" "$SSH_TARGET"
}

# ─── 入口 ──────────────────────────────────────────────────────
case "$ACTION" in
    start)      do_start ;;
    stop)       do_stop ;;
    status)     do_status ;;
    foreground) do_foreground ;;
    *)          fail "Unknown action: $ACTION" ;;
esac
