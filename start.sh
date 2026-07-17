#!/usr/bin/env bash
# AgentTown_v3 — 一键启动脚本
#
# 按正确顺序启动所有组件，每步验证连接后才继续下一步：
#   1. agenttown-mcp (WSL, Go binary)  — MCP Server + WebSocket Server
#   2. Hermes Gateway (Docker)         — LLM Agent，连接 MCP 发现工具
#   3. Mock UE (Python, host)          — WebSocket 客户端，推送感知
#
# 用法：
#   bash start.sh              # 启动全部，跑完整一天 (06:00-22:00)
#   bash start.sh --quick      # 快速测试 (06:00-10:00, 高速)
#   bash start.sh --mock-only  # 只启动 Mock UE（假设 MCP + Hermes 已在运行）
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
NC='\033[0m' # No Color

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
fail()  { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }

# ─── 路径与参数 ────────────────────────────────────────────────
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
DOCKER_COMPOSE="$PROJECT_DIR/docker/docker-compose.yml"
ENV_FILE="$PROJECT_DIR/.env"
MOCK_UE_SCRIPT="$PROJECT_DIR/src/run_day.py"

# Mock UE 参数（可通过命令行覆盖）
MOCK_START=6
MOCK_END=22
MOCK_SPEED=300
MOCK_INTERVAL=30
MOCK_SCENARIO=""

# 解析命令行参数
MODE="full"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --quick)
            MODE="quick"
            MOCK_START=6
            MOCK_END=10
            MOCK_SPEED=600
            MOCK_INTERVAL=30
            shift
            ;;
        --mock-only)
            MODE="mock-only"
            shift
            ;;
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
            echo "  --mock-only     Only start Mock UE (MCP + Hermes already running)"
            echo "  --start HOUR    Start hour (default: 6)"
            echo "  --end HOUR      End hour (default: 22)"
            echo "  --speed N       Time acceleration (default: 300)"
            echo "  --interval MIN  Perception interval in game-min (default: 30)"
            echo "  --scenario FILE Scenario injection YAML file"
            exit 0
            ;;
        *)
            warn "Unknown option: $1"
            shift
            ;;
    esac
done

# ─── 健康检查函数 ──────────────────────────────────────────────

check_mcp_http() {
    wsl curl -sf http://localhost:8760/healthz >/dev/null 2>&1
}

check_mcp_ws() {
    wsl curl -sf http://localhost:9000/healthz >/dev/null 2>&1
}

check_hermes() {
    curl -sf http://localhost:8642/health >/dev/null 2>&1
}

check_hermes_mcp_connected() {
    # Check Hermes logs for successful MCP tool discovery
    wsl docker logs agenttown-h01 2>&1 | tail -50 | grep -q "session initialized" 2>/dev/null
}

wait_for() {
    local name="$1"
    local check_fn="$2"
    local max_wait="${3:-30}"
    local elapsed=0
    info "Waiting for $name (max ${max_wait}s)..."
    while [ $elapsed -lt $max_wait ]; do
        if $check_fn; then
            ok "$name is up"
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
        printf "."
    done
    echo ""
    fail "$name did not come up within ${max_wait}s"
}

# ─── 步骤 1: 启动 MCP ─────────────────────────────────────────

start_mcp() {
    info "=== Step 1: Start agenttown-mcp ==="

    if check_mcp_http; then
        warn "MCP already running, skipping start"
        return 0
    fi

    # 检查二进制是否存在
    if ! wsl bash -c 'test -f ~/agenttown-mcp'; then
        fail "MCP binary not found at ~/agenttown-mcp in WSL. Build it first:
  cd d:/SmartNPC_v3/agenttown-mcp
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agenttown-mcp-linux ./cmd/agenttown-mcp
  wsl cp /mnt/c/Users/yitianchen/AppData/Local/Temp/agenttown-mcp-linux ~/agenttown-mcp"
    fi

    info "Starting MCP in WSL background..."
    wsl bash -c '~/agenttown-mcp --http :8760 --ws :9000 --hermes-url http://localhost:8642 > /tmp/mcp.log 2>&1 &'

    wait_for "MCP HTTP (:8760)" check_mcp_http 15
    wait_for "MCP WS (:9000)" check_mcp_ws 10
}

# ─── 步骤 2: 启动/重启 Hermes ─────────────────────────────────

start_hermes() {
    info "=== Step 2: Start Hermes Gateway ==="

    # 如果 Hermes 已在运行，检查 MCP 工具是否已注册
    if check_hermes; then
        # 检查 Hermes 是否已连接 MCP（日志中有 session initialized）
        if wsl docker logs agenttown-h01 2>&1 | tail -30 | grep -q "session initialized" 2>/dev/null; then
            # 进一步验证：MCP 日志中是否有 Hermes 的连接记录
            if wsl bash -c 'grep -q "session initialized" /tmp/mcp.log 2>/dev/null'; then
                ok "Hermes already running with MCP connected"
                return 0
            fi
        fi
        # Hermes 在运行但 MCP 没连上 → 需要重启
        warn "Hermes running but MCP not connected. Restarting Hermes..."
    fi

    if [ ! -f "$ENV_FILE" ]; then
        fail ".env file not found at $ENV_FILE"
    fi

    info "Starting Hermes via docker compose..."
    wsl docker compose --env-file "$(wsl wslpath -a "$ENV_FILE" 2>/dev/null || echo "$ENV_FILE")" \
        -f "$(wsl wslpath -a "$DOCKER_COMPOSE" 2>/dev/null || echo "$DOCKER_COMPOSE")" \
        up -d 2>/dev/null || \
    wsl docker compose -f /mnt/d/SmartNPC_v3/docker/docker-compose.yml --env-file /mnt/d/SmartNPC_v3/.env up -d

    wait_for "Hermes Gateway (:8642)" check_hermes 40

    # 等 Hermes 连接 MCP（MCP 日志出现 session initialized）
    info "Waiting for Hermes to discover MCP tools..."
    local elapsed=0
    while [ $elapsed -lt 20 ]; do
        if wsl bash -c 'tail -10 /tmp/mcp.log 2>/dev/null | grep -q "session initialized"'; then
            ok "Hermes connected to MCP"
            # 给 Hermes 额外 2 秒完成工具注册
            sleep 2
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
        printf "."
    done
    echo ""
    fail "Hermes did not connect to MCP within 20s. Check:
  wsl docker logs agenttown-h01 2>&1 | grep -i mcp
  wsl tail -20 /tmp/mcp.log"
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

    # 构建参数
    local args=(
        "--start" "$MOCK_START"
        "--end" "$MOCK_END"
        "--speed" "$MOCK_SPEED"
        "--interval" "$MOCK_INTERVAL"
    )
    if [ -n "$MOCK_SCENARIO" ]; then
        args+=("--scenario" "$MOCK_SCENARIO")
    fi

    info "Starting Mock UE: python $MOCK_UE_SCRIPT ${args[*]}"
    echo ""
    echo "============================================================"
    echo "  AgentTown_v3 — Day Simulation"
    echo "  Time: ${MOCK_START}:00 - ${MOCK_END}:00 | Speed: ${MOCK_SPEED}x"
    echo "  Perception interval: ${MOCK_INTERVAL} game-min"
    [ -n "$MOCK_SCENARIO" ] && echo "  Scenario: $MOCK_SCENARIO"
    echo "============================================================"
    echo ""

    cd "$PROJECT_DIR"
    python "$MOCK_UE_SCRIPT" "${args[@]}"
}

# ─── 主流程 ────────────────────────────────────────────────────

info "AgentTown_v3 Startup Script"
echo ""

if [ "$MODE" != "mock-only" ]; then
    start_mcp
    echo ""
    start_hermes
    echo ""
fi

start_mock_ue
