#!/usr/bin/env bash
# AgentTown_v3 — 一键重启全部组件
#
# 先停掉所有现有进程，再按正确顺序启动：
#   0. 停止 Mock UE + MCP + Hermes
#   1. agenttown-mcp (WSL, Go binary)  — MCP Server + WebSocket Server
#   2. Hermes Gateway (Docker)         — LLM Agent，连接 MCP 发现工具
#   3. Mock UE (Python, host)          — WebSocket 客户端，推送感知
#
# 用法：
#   bash start.sh              # 全部重启，跑完整一天 (06:00-22:00)
#   bash start.sh --quick      # 快速测试 (06:00-10:00, 高速)
#
# 前置：
#   - WSL 已安装且 Docker 可用
#   - ~/agenttown-mcp 二进制存在（Go 交叉编译产物）
#   - Python 3.10+，已安装 websockets, pyyaml
#   - d:/SmartNPC_v3/.env 存在且配置了 HERMES_AGENT_API_KEY

set -euo pipefail

# ─── 颜色输出 ──────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }

# ─── 路径与参数 ────────────────────────────────────────────────
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKER_COMPOSE="$PROJECT_DIR/docker/docker-compose.yml"
ENV_FILE="$PROJECT_DIR/.env"
MOCK_UE_SCRIPT="$PROJECT_DIR/src/run_day.py"

# ─── 环境检测 ──────────────────────────────────────────────────
# 检测是否在 WSL 内部运行。如果在 Windows Git Bash 中运行，
# `wsl` 命令可用，需要通过它调用 WSL 内的工具。如果在 WSL 内部
# 运行，`wsl` 不存在，命令直接执行。
if command -v wsl &>/dev/null; then
    # Windows Git Bash — 通过 wsl 调用 WSL 命令
    WSL="wsl"
    WSL_BASH="wsl bash -c"
    DOCKER_COMPOSE_FILE="/mnt/d/SmartNPC_v3/docker/docker-compose.yml"
    DOCKER_ENV_FILE="/mnt/d/SmartNPC_v3/.env"
else
    # 已在 WSL 内部 — 直接执行
    WSL=""
    WSL_BASH="bash -c"
    DOCKER_COMPOSE_FILE="$PROJECT_DIR/docker/docker-compose.yml"
    DOCKER_ENV_FILE="$PROJECT_DIR/.env"
fi

MOCK_START=6
MOCK_END=22
MOCK_SPEED=300
MOCK_INTERVAL=30
MOCK_SCENARIO=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --quick)
            MOCK_START=6; MOCK_END=10; MOCK_SPEED=600; MOCK_INTERVAL=30
            shift ;;
        --start)    MOCK_START="$2"; shift 2 ;;
        --end)      MOCK_END="$2"; shift 2 ;;
        --speed)    MOCK_SPEED="$2"; shift 2 ;;
        --interval) MOCK_INTERVAL="$2"; shift 2 ;;
        --scenario) MOCK_SCENARIO="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: bash start.sh [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --quick         Quick test: 06:00-10:00, speed 600x"
            echo "  --start HOUR    Start hour (default: 6)"
            echo "  --end HOUR      End hour (default: 22)"
            echo "  --speed N       Time acceleration (default: 300)"
            echo "  --interval MIN  Perception interval in game-min (default: 30)"
            echo "  --scenario FILE Scenario injection YAML file"
            exit 0 ;;
        *) warn "Unknown option: $1"; shift ;;
    esac
done

# ─── 健康检查函数 ──────────────────────────────────────────────
check_mcp_http() { $WSL curl -sf http://localhost:8760/healthz >/dev/null 2>&1; }
check_mcp_ws()   { $WSL curl -sf http://localhost:9000/healthz >/dev/null 2>&1; }
check_hermes()   { curl -sf http://localhost:8642/health >/dev/null 2>&1; }

wait_for() {
    local name="$1" check_fn="$2" max_wait="${3:-30}" elapsed=0
    info "Waiting for $name (max ${max_wait}s)..."
    while [ $elapsed -lt $max_wait ]; do
        if $check_fn; then ok "$name is up"; return 0; fi
        sleep 2; elapsed=$((elapsed + 2)); printf "."
    done
    echo ""
    fail "$name did not come up within ${max_wait}s"
}

# ─── 步骤 0: 停止所有现有进程 ──────────────────────────────────
stop_all() {
    info "=== Step 0: Stop all existing processes ==="

    # Mock UE (Python)
    info "Stopping Mock UE..."
    pkill -f "run_day.py" 2>/dev/null && ok "  Mock UE stopped" || warn "  Mock UE not running"

    # MCP (WSL Go binary) — kill by PID to avoid killing the shell itself
    info "Stopping MCP..."
    local mcp_pid
    mcp_pid=$($WSL_BASH "pgrep -f 'agenttown-mcp --http' 2>/dev/null | head -1" || true)
    if [ -n "$mcp_pid" ]; then
        $WSL_BASH "kill $mcp_pid 2>/dev/null"
        sleep 1
        ok "  MCP stopped (PID $mcp_pid)"
    else
        warn "  MCP not running"
    fi

    # Hermes (Docker)
    info "Stopping Hermes..."
    $WSL docker compose -f "$DOCKER_COMPOSE_FILE" stop 2>/dev/null \
        && ok "  Hermes stopped" \
        || warn "  Hermes not running"

    # 等待端口释放
    sleep 2
    echo ""
}

# ─── 步骤 1: 启动 MCP ─────────────────────────────────────────
start_mcp() {
    info "=== Step 1: Start agenttown-mcp ==="

    if ! $WSL_BASH 'test -f ~/agenttown-mcp'; then
        fail "MCP binary not found at ~/agenttown-mcp in WSL. Build it first:
  cd d:/SmartNPC_v3/agenttown-mcp
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agenttown-mcp-linux ./cmd/agenttown-mcp
  wsl cp /mnt/c/Users/yitianchen/AppData/Local/Temp/agenttown-mcp-linux ~/agenttown-mcp"
    fi

    # 清空旧日志
    $WSL_BASH 'echo "" > /tmp/mcp.log'

    info "Starting MCP in WSL background..."
    $WSL_BASH '~/agenttown-mcp --http :8760 --ws :9000 --hermes-url http://localhost:8642 > /tmp/mcp.log 2>&1 &'

    wait_for "MCP HTTP (:8760)" check_mcp_http 15
    wait_for "MCP WS (:9000)" check_mcp_ws 10
}

# ─── 步骤 2: 启动 Hermes ──────────────────────────────────────
start_hermes() {
    info "=== Step 2: Start Hermes Gateway ==="

    if [ ! -f "$ENV_FILE" ]; then
        fail ".env file not found at $ENV_FILE"
    fi

    info "Starting Hermes via docker compose..."
    $WSL docker compose -f "$DOCKER_COMPOSE_FILE" \
        --env-file "$DOCKER_ENV_FILE" up -d

    wait_for "Hermes Gateway (:8642)" check_hermes 40

    # 等 Hermes 连接 MCP（MCP 日志出现 session initialized）
    info "Waiting for Hermes to discover MCP tools..."
    local elapsed=0
    while [ $elapsed -lt 20 ]; do
        if $WSL_BASH 'tail -10 /tmp/mcp.log 2>/dev/null | grep -q "session initialized"'; then
            ok "Hermes connected to MCP"
            sleep 2  # 给 Hermes 额外时间完成工具注册
            return 0
        fi
        sleep 2; elapsed=$((elapsed + 2)); printf "."
    done
    echo ""
    fail "Hermes did not connect to MCP within 20s. Check:
  $WSL docker logs agenttown-h01 2>&1 | grep -i mcp
  $WSL tail -20 /tmp/mcp.log"
}

# ─── 步骤 3: 启动 Mock UE ─────────────────────────────────────
start_mock_ue() {
    info "=== Step 3: Start Mock UE ==="

    if [ ! -f "$MOCK_UE_SCRIPT" ]; then
        fail "Mock UE script not found: $MOCK_UE_SCRIPT"
    fi

    # 最终预检查
    info "Pre-flight checks:"
    check_mcp_http && ok "  MCP HTTP reachable" || fail "  MCP HTTP unreachable"
    check_mcp_ws   && ok "  MCP WS reachable"   || fail "  MCP WS unreachable"
    check_hermes   && ok "  Hermes reachable"   || fail "  Hermes unreachable"

    local args=(
        "--start" "$MOCK_START"
        "--end" "$MOCK_END"
        "--speed" "$MOCK_SPEED"
        "--interval" "$MOCK_INTERVAL"
    )
    [ -n "$MOCK_SCENARIO" ] && args+=("--scenario" "$MOCK_SCENARIO")

    echo ""
    echo "============================================================"
    echo "  AgentTown_v3 — Day Simulation"
    echo "  Time: ${MOCK_START}:00 - ${MOCK_END}:00 | Speed: ${MOCK_SPEED}x"
    echo "  Perception interval: ${MOCK_INTERVAL} game-min"
    [ -n "$MOCK_SCENARIO" ] && echo "  Scenario: $MOCK_SCENARIO"
    echo "============================================================"
    echo ""

    cd "$PROJECT_DIR"
    # 检测 python 命令（WSL 通常只有 python3）
    local py_cmd="python"
    if ! command -v python &>/dev/null; then
        if command -v python3 &>/dev/null; then
            py_cmd="python3"
        else
            fail "Python not found. Install it: sudo apt install python3 python3-pip"
        fi
    fi
    $py_cmd "$MOCK_UE_SCRIPT" "${args[@]}"
}

# ─── 主流程 ────────────────────────────────────────────────────
info "AgentTown_v3 — Full Restart & Start"
echo ""

stop_all
start_mcp
echo ""
start_hermes
echo ""
start_mock_ue
