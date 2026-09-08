#!/usr/bin/env bash
# AgentTown_v3 — SSH 反向隧道脚本（Windows Git Bash 端运行）
#
# 在你的 Windows 本地 Git Bash 里运行，把云端 11435 端口反向映射回 Windows
# 本地 Ollama 的 11434 端口。这样云端 MCP 通过 --ollama-url
# http://localhost:11435 就能调用你 Windows 上的 Ollama（反应层 LLM）。
#
# 为什么必须从 Windows 端发起：
#   - Ollama 跑在你 Windows 本地（:11434）
#   - 云端 MCP 需要访问 Ollama，但 Windows 通常不在公网/无 SSH 服务端
#   - 解决方案：Windows 主动 SSH 到云端，用 -R 在云端开端口反连回 Windows
#   - ssh -R <云端端口>:127.0.0.1:<Windows Ollama 端口> root@<云端 IP>
#
# 用法：
#   bash start-tunnel.sh                # 拉起反向隧道（后台保活）
#   bash start-tunnel.sh --stop         # 停止隧道
#   bash start-tunnel.sh --status       # 查看隧道状态
#   bash start-tunnel.sh --foreground   # 前台运行（方便看 SSH 日志调试）
#
# 环境变量（可在 .env 里配置覆盖默认值）：
#   SSH_TUNNEL_REMOTE_HOST    云端 SSH 地址（默认 21.214.99.100）
#   SSH_TUNNEL_REMOTE_PORT    云端 SSH 端口（默认 36000）
#   SSH_TUNNEL_REMOTE_USER    SSH 用户（默认 root）
#   OLLAMA_LOCAL_PORT         Windows 本地 Ollama 端口（默认 11434）
#   OLLAMA_TUNNEL_PORT        云端反隧道端口（默认 11435，与 MCP --ollama-url 对齐）
#
# 前置：
#   - Windows 本地 Ollama 已启动并监听 127.0.0.1:11434
#     （注意：必须用 127.0.0.1 而非 localhost，Windows 上 localhost 可能解析
#      到 IPv6 ::1 导致 SSH -R 绑定失败）
#   - 已配置 SSH 公钥免密登录云端，或会在首次连接时提示密码
#   - 云端防火墙放行 SSH_TUNNEL_REMOTE_PORT 的 SSH 入站

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

# 从 .env 加载 SSH/Ollama 相关变量（存在则覆盖默认）
load_env() {
    if [ ! -f "$ENV_FILE" ]; then
        return 0
    fi
    while IFS='=' read -r key value || [ -n "$key" ]; do
        case "$key" in
            ''|\#*) continue ;;
        esac
        case "$key" in
            SSH_TUNNEL_*|OLLAMA_LOCAL_PORT|OLLAMA_TUNNEL_PORT) export "$key=$value" ;;
        esac
    done < "$ENV_FILE"
}
load_env

# 配置默认值（与既有手动命令对齐）
SSH_TUNNEL_REMOTE_HOST="${SSH_TUNNEL_REMOTE_HOST:-21.214.99.100}"
SSH_TUNNEL_REMOTE_PORT="${SSH_TUNNEL_REMOTE_PORT:-36000}"
SSH_TUNNEL_REMOTE_USER="${SSH_TUNNEL_REMOTE_USER:-root}"
OLLAMA_LOCAL_PORT="${OLLAMA_LOCAL_PORT:-11434}"
OLLAMA_TUNNEL_PORT="${OLLAMA_TUNNEL_PORT:-11435}"

# PID 文件与日志（放在项目目录下，方便管理）
PID_DIR="$PROJECT_DIR/.run"
PID_FILE="$PID_DIR/ssh-tunnel.pid"
LOG_FILE="$PID_DIR/ssh-tunnel.log"
mkdir -p "$PID_DIR"

# ─── 参数解析 ──────────────────────────────────────────────────
ACTION="start"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --stop)      ACTION="stop"; shift ;;
        --status)    ACTION="status"; shift ;;
        --foreground) ACTION="foreground"; shift ;;
        -h|--help)
            echo "Usage: bash start-tunnel.sh [OPTIONS]"
            echo ""
            echo "在 Windows 端拉起 SSH 反向隧道，把云端 ${OLLAMA_TUNNEL_PORT} 端口"
            echo "反连回 Windows 本地 Ollama ${OLLAMA_LOCAL_PORT} 端口。"
            echo ""
            echo "Options:"
            echo "  (无参数)       后台拉起隧道（默认）"
            echo "  --foreground   前台运行（调试用，直接看 SSH 输出）"
            echo "  --stop         停止隧道"
            echo "  --status       查看隧道状态"
            echo ""
            echo "环境变量（可在 .env 配置）："
            echo "  SSH_TUNNEL_REMOTE_HOST=$SSH_TUNNEL_REMOTE_HOST"
            echo "  SSH_TUNNEL_REMOTE_PORT=$SSH_TUNNEL_REMOTE_PORT"
            echo "  SSH_TUNNEL_REMOTE_USER=$SSH_TUNNEL_REMOTE_USER"
            echo "  OLLAMA_LOCAL_PORT=$OLLAMA_LOCAL_PORT"
            echo "  OLLAMA_TUNNEL_PORT=$OLLAMA_TUNNEL_PORT"
            exit 0 ;;
        *) warn "Unknown option: $1"; shift ;;
    esac
done

# ─── 隧道命令构造 ──────────────────────────────────────────────
# SSH 选项说明：
#   -N                  不执行远程命令，仅做端口转发
#   -T                  不分配 TTY
#   -o ExitOnForwardFailure=yes   端口转发失败时 SSH 退出（而非挂着一个无用 shell）
#   -o ServerAliveInterval=30     每 30s 发 keepalive 包，防止 NAT 超时断连
#   -o ServerAliveCountMax=3      3 次 keepalive 无响应则判定断线（90s）
#   -o StrictHostKeyChecking=accept-new  首次连接自动接受 host key（方便自动化）
#   -o TCPKeepAlive=yes           启用 TCP 层 keepalive
SSH_OPTS=(
    -N -T
    -o ExitOnForwardFailure=yes
    -o ServerAliveInterval=30
    -o ServerAliveCountMax=3
    -o StrictHostKeyChecking=accept-new
    -o TCPKeepAlive=yes
    -p "$SSH_TUNNEL_REMOTE_PORT"
)

# 反向隧道：在云端开 OLLAMA_TUNNEL_PORT，转发到本地 127.0.0.1:OLLAMA_LOCAL_PORT
# 关键：用 127.0.0.1 而非 localhost（Windows 上 localhost 可能解析到 IPv6 ::1，
# 导致 SSH -R 绑定到 ::1:11434 而 Ollama 只听 127.0.0.1:11434，转发失败）
SSH_FORWARD_ARGS=(-R "${OLLAMA_TUNNEL_PORT}:127.0.0.1:${OLLAMA_LOCAL_PORT}")

SSH_TARGET="${SSH_TUNNEL_REMOTE_USER}@${SSH_TUNNEL_REMOTE_HOST}"

# ─── 辅助函数 ──────────────────────────────────────────────────

# 检测 Windows 本地 Ollama 是否在监听（避免隧道建好但 Ollama 没起）
check_local_ollama() {
    if ! command -v curl >/dev/null 2>&1; then
        return 0  # 无 curl 就跳过检测，让 SSH 自己报错
    fi
    if curl -sf "http://127.0.0.1:${OLLAMA_LOCAL_PORT}/api/tags" >/dev/null 2>&1; then
        return 0
    fi
    return 1
}

# 读取 PID 文件，返回 PID（不存在或进程已死返回空）
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
    # 检测进程是否存活（Windows Git Bash 下 kill -0 可用）
    if kill -0 "$pid" 2>/dev/null; then
        echo "$pid"
    else
        # 进程已死，清理 stale PID 文件
        rm -f "$PID_FILE"
        echo ""
    fi
}

# ─── 动作实现 ──────────────────────────────────────────────────

do_status() {
    local pid
    pid=$(read_pid)
    if [ -z "$pid" ]; then
        warn "SSH 反向隧道未运行"
        echo ""
        echo "  启动：bash start-tunnel.sh"
        echo "  目标：${SSH_TARGET}:${SSH_TUNNEL_REMOTE_PORT} → 127.0.0.1:${OLLAMA_LOCAL_PORT}（云端 ${OLLAMA_TUNNEL_PORT}）"
        return 1
    fi
    ok "SSH 反向隧道运行中（PID $pid）"
    echo ""
    echo "  目标：${SSH_TARGET}:${SSH_TUNNEL_REMOTE_PORT}"
    echo "  转发：云端 :${OLLAMA_TUNNEL_PORT} → Windows 127.0.0.1:${OLLAMA_LOCAL_PORT}"
    echo "  日志：$LOG_FILE"
    return 0
}

do_stop() {
    local pid
    pid=$(read_pid)
    if [ -z "$pid" ]; then
        warn "SSH 反向隧道未运行（无需停止）"
        return 0
    fi
    info "Stopping SSH tunnel (PID $pid)..."
    kill "$pid" 2>/dev/null
    # 等待退出（最多 5s）
    local elapsed=0
    while [ $elapsed -lt 5 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            break
        fi
        sleep 1; elapsed=$((elapsed + 1))
    done
    # 仍存活则强杀
    if kill -0 "$pid" 2>/dev/null; then
        warn "隧道未响应 SIGTERM，发送 SIGKILL"
        kill -9 "$pid" 2>/dev/null
    fi
    rm -f "$PID_FILE"
    ok "SSH 反向隧道已停止"
}

do_start() {
    # 幂等：已在运行则跳过
    local existing_pid
    existing_pid=$(read_pid)
    if [ -n "$existing_pid" ]; then
        ok "SSH 反向隧道已在运行（PID $existing_pid），跳过启动"
        do_status
        return 0
    fi

    # 前置检查：本地 Ollama 是否在监听
    if ! check_local_ollama; then
        warn "本地 Ollama 未在 127.0.0.1:${OLLAMA_LOCAL_PORT} 监听"
        warn "请先启动 Ollama：ollama serve（或检查 OLLAMA_LOCAL_PORT 配置）"
        warn "继续启动隧道——SSH 会建立但转发目标不可达，Ollama 起来后自动恢复"
        echo ""
    fi

    info "=== 启动 SSH 反向隧道 ==="
    info "  目标：${SSH_TARGET}:${SSH_TUNNEL_REMOTE_PORT}"
    info "  转发：云端 :${OLLAMA_TUNNEL_PORT} → Windows 127.0.0.1:${OLLAMA_LOCAL_PORT}"
    info "  日志：$LOG_FILE"

    # 后台启动 SSH，PID 写入文件
    # nohup + disown 让 SSH 脱离 shell（关掉 Git Bash 窗口隧道继续跑）
    nohup ssh "${SSH_OPTS[@]}" "${SSH_FORWARD_ARGS[@]}" "$SSH_TARGET" \
        > "$LOG_FILE" 2>&1 &
    local pid=$!
    disown
    echo "$pid" > "$PID_FILE"

    # 等待隧道建立（最多 10s）。SSH 连接 + 端口绑定通常 1-3s，
    # ExitOnForwardFailure=yes 保证端口被占用时 SSH 立即退出而非挂起。
    info "Waiting for tunnel to establish (max 10s)..."
    local elapsed=0
    while [ $elapsed -lt 10 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            echo ""
            fail "SSH 进程已退出（端口被占用 / 认证失败 / 网络不通）。日志："
            cat "$LOG_FILE" 2>/dev/null | tail -20
            rm -f "$PID_FILE"
        fi
        # 检测云端端口是否真的在监听——但我们在 Windows 端看不到云端端口状态，
        # 只能靠 SSH 进程存活 + 日志无错误来间接判断。SSH 进程存活 3s 即认为建立成功。
        if [ $elapsed -ge 3 ]; then
            break
        fi
        sleep 1; elapsed=$((elapsed + 1)); printf "."
    done
    echo ""

    if ! kill -0 "$pid" 2>/dev/null; then
        fail "SSH 隧道启动失败（进程已退出）"
    fi

    ok "SSH 反向隧道已建立（PID $pid）"
    echo ""
    echo -e "  ${BOLD}云端 MCP 配置${NC}"
    echo -e "    在 .env 设置：${CYAN}OLLAMA_URL=http://localhost:${OLLAMA_TUNNEL_PORT}${NC}"
    echo -e "    或启动时加 flag：${CYAN}--ollama-url http://localhost:${OLLAMA_TUNNEL_PORT}${NC}"
    echo ""
    echo -e "  ${BOLD}保活说明${NC}"
    echo -e "    隧道通过 ServerAliveInterval=30s 自动保活，关掉 Git Bash 窗口不影响"
    echo -e "    查看状态：bash start-tunnel.sh --status"
    echo -e "    停止隧道：bash start-tunnel.sh --stop"
    echo ""
    echo -e "  ${YELLOW}注意${NC}"
    echo -e "    1. 首次连接可能需要输入密码（建议配置公钥免密）"
    echo -e "    2. 若 Ollama 暂未启动，隧道会建立但转发失败；Ollama 起来后自动恢复"
    echo -e "    3. 日志在 $LOG_FILE"
}

do_foreground() {
    info "前台运行 SSH 反向隧道（Ctrl+C 退出）"
    info "  目标：${SSH_TARGET}:${SSH_TUNNEL_REMOTE_PORT}"
    info "  转发：云端 :${OLLAMA_TUNNEL_PORT} → Windows 127.0.0.1:${OLLAMA_LOCAL_PORT}"
    echo ""
    # 前台运行直接看 SSH 输出，调试友好。去掉 -N 让 SSH 保持连接但不执行命令
    # 其实 -N 已经够用，前台模式下 SSH 会阻塞直到 Ctrl+C
    exec ssh "${SSH_OPTS[@]}" "${SSH_FORWARD_ARGS[@]}" "$SSH_TARGET"
}

# ─── 入口 ──────────────────────────────────────────────────────
case "$ACTION" in
    start)     do_start ;;
    stop)      do_stop ;;
    status)    do_status ;;
    foreground) do_foreground ;;
    *)         fail "Unknown action: $ACTION" ;;
esac
