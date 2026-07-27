#!/usr/bin/env bash
# AgentTown_v3 — 一键重启全部组件
#
# 先停掉所有现有进程，再按正确顺序启动：
#   0. 停止 Mock UE + MCP + Hermes + CodeBuddy Adapter
#   1. agenttown-mcp (WSL, Go binary)         — MCP Server + WebSocket Server
#   2. CodeBuddy Adapter (Python, Windows)    — OpenAI ↔ CLI Run API 适配层
#   3. Hermes Gateway (Docker)                — LLM Agent，连接 MCP 发现工具，
#                                                连接 Adapter 调用公司 GLM-5.2
#   4. Mock UE (Python, host)                 — WebSocket 客户端，推送感知
#
# 用法：
#   bash start.sh              # 全部重启，跑完整一天 (06:00-22:00)
#   bash start.sh --quick      # 快速测试 (06:00-10:00, 高速)
#
# 前置：
#   - WSL 已安装且 Docker 可用
#   - Go 编译器可访问（PATH 中有 go，或位于 /d/Go/bin/go）
#   - Hermes 源码在默认位置或通过 HERMES_SOURCE 环境变量指定
#   - Python 3.10+，已安装 websockets, pyyaml, httpx, fastapi, uvicorn
#   - d:/SmartNPC_v3/.env 存在且配置了 HERMES_AGENT_API_KEY
#   - CodeBuddy CLI 已在 Windows 登录（适配层复用其 OAuth 认证）
#
# 每次 start.sh 都会强制重建以下组件（不再支持跳过）：
#   - MCP Go 二进制：交叉编译 linux/amd64 + 跑 cmd/agenttown-mcp 单元测试
#   - Hermes Docker 镜像：从本地 Hermes 源码 docker build
# Mock UE 是 Python 脚本，每次执行即加载最新代码，无需编译。

set -uo pipefail

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
ADAPTER_SCRIPT="$PROJECT_DIR/src/agenttown/codebuddy_adapter.py"
ADAPTER_PORT=8761

# ─── 日志目录（按测试日期归档到 logs/YYYY-MM-DD/）─────────────
# 单一日期来源：start.sh 启动时计算一次，传给 MCP 启动器和 Mock UE，
# 避免跨进程时钟漂移导致同一次仿真的日志被分到不同目录。
LOG_DATE=$(date +%Y-%m-%d)
LOG_SUBDIR="$PROJECT_DIR/logs/$LOG_DATE"

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

MODE="normal"
if [[ $# -gt 0 ]]; then
    case "$1" in
        normal|behavior|quick-smoke) MODE="$1"; shift ;;
    esac
fi

apply_mode() {
    case "$MODE" in
        normal)
            MOCK_START=6; MOCK_END=22; MOCK_SPEED=150; MOCK_INTERVAL=60; MOCK_SCENARIO="" ;;
        behavior)
            MOCK_START=6; MOCK_END=18; MOCK_SPEED=60; MOCK_INTERVAL=15
            MOCK_SCENARIO="$PROJECT_DIR/assets/scenarios_sample.yaml" ;;
        quick-smoke)
            MOCK_START=6; MOCK_END=10; MOCK_SPEED=600; MOCK_INTERVAL=30; MOCK_SCENARIO="" ;;
        *) fail "Unknown mode: $MODE" ;;
    esac
}
apply_mode

while [[ $# -gt 0 ]]; do
    case "$1" in
        --quick)
            MODE="quick-smoke"; apply_mode; shift ;;
        --start)    MOCK_START="$2"; shift 2 ;;
        --end)      MOCK_END="$2"; shift 2 ;;
        --speed)    MOCK_SPEED="$2"; shift 2 ;;
        --interval) MOCK_INTERVAL="$2"; shift 2 ;;
        --scenario) MOCK_SCENARIO="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: bash start.sh [normal|behavior|quick-smoke] [OPTIONS]"
            echo ""
            echo "Modes:"
            echo "  normal       06:00-22:00, speed 150x, interval 60"
            echo "  behavior     06:00-18:00, speed 60x, interval 15, sample scenario"
            echo "  quick-smoke  06:00-10:00, speed 600x, interval 30"
            echo ""
            echo "Options (override mode defaults):"
            echo "  --quick         Compatibility alias for quick-smoke"
            echo "  --start HOUR    Start hour"
            echo "  --end HOUR      End hour"
            echo "  --speed N       Time acceleration"
            echo "  --interval MIN  Perception interval in game-min"
            echo "  --scenario FILE Scenario injection YAML file"
            exit 0 ;;
        *) warn "Unknown option: $1"; shift ;;
    esac
done

# ─── 健康检查函数 ──────────────────────────────────────────────
# 适配层运行在 Windows，其他服务运行在 WSL/Docker。
# - 从 Git Bash 运行：localhost 即 Windows localhost，可直接访问适配层
# - 从 WSL 运行：localhost 是 WSL VM 的 localhost，访问 Windows 适配层需要用
#   Windows 宿主 IP（WSL2 的默认网关，即 vEthernet 网卡 IP）
ADAPTER_HOST="localhost"
if [ -z "$WSL" ]; then
    # 在 WSL 内 — 解析 Windows 宿主 IP（默认网关）
    ADAPTER_HOST=$(ip route show default 2>/dev/null | awk '{print $3}' | head -1)
    [ -z "$ADAPTER_HOST" ] && ADAPTER_HOST=$(grep nameserver /etc/resolv.conf 2>/dev/null | head -1 | awk '{print $2}')
    [ -z "$ADAPTER_HOST" ] && ADAPTER_HOST="172.18.16.1"  # 兜底：常见 vEthernet IP
fi

check_mcp_http() { $WSL curl -sf http://localhost:8760/healthz >/dev/null 2>&1; }
check_mcp_ws()   { $WSL curl -sf http://localhost:9090/healthz >/dev/null 2>&1; }
check_hermes()   { curl -sf http://localhost:8642/health >/dev/null 2>&1; }
check_adapter()  { curl -sf http://$ADAPTER_HOST:$ADAPTER_PORT/health >/dev/null 2>&1; }

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

    # MCP (WSL Go binary) — use pkill -x to match exact process name,
    # avoiding accidentally killing the wsl bash shell itself.
    info "Stopping MCP..."
    $WSL_BASH 'pkill -x agenttown-mcp 2>/dev/null' && ok "  MCP stopped" || warn "  MCP not running"
    sleep 1

    # CodeBuddy Adapter (Python, Windows) — 适配层是 Windows 进程，
    # MSYS pkill -f 看不到 Python 脚本名，所以用 Windows netstat.exe 找到
    # 监听 :8761 的 PID，再用 Windows taskkill.exe 终止。
    # 必须显式用 .exe 后缀：Git Bash 和 WSL 都能通过该后缀调到 Windows 工具。
    # /F /PID 用单斜杠（//F 会被 taskkill 拒绝）；MSYS_NO_PATHCONV=1 防止
    # Git Bash 把 /F 误转成 Windows 路径（WSL 不受影响，该变量是无害空操作）。
    # tr -d '\r' 剥离 netstat.exe 输出的 CRLF 行尾，否则 PID 带 \r 会导致
    # taskkill 报"无效查询"。
    info "Stopping CodeBuddy Adapter..."
    local adapter_pid
    adapter_pid=$(netstat.exe -ano 2>/dev/null | grep "0.0.0.0:$ADAPTER_PORT" | grep LISTENING | awk '{print $NF}' | tr -d '\r' | head -1)
    if [ -n "$adapter_pid" ]; then
        if MSYS_NO_PATHCONV=1 taskkill.exe /F /PID "$adapter_pid" >/dev/null 2>&1; then
            ok "  Adapter stopped (PID $adapter_pid)"
        else
            warn "  Failed to kill adapter PID $adapter_pid (taskkill returned non-zero)"
        fi
    else
        warn "  Adapter not running"
    fi
    sleep 1

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

    # 强制交叉编译 + 部署最新 MCP 二进制到 WSL 的 ~/agenttown-mcp。
    # 编译失败或 Go 编译器找不到时直接 fail 退出，避免误用旧二进制
    # 跑出新行为与代码不一致的日志。
    #
    # Locate the Go compiler. Search order:
    #   1. $GO_BIN env var (explicit user override — recommended when
    #      launching from PowerShell, where PATH may not include Go)
    #   2. `command -v go` (PATH lookup)
    #   3. $GOROOT/bin/go
    #   4. Common MINGW mount paths (/d/Go/bin/go etc.) — also try the
    #      .exe variant because Windows Go installs ship as go.exe and
    #      WSL/bash needs the explicit suffix to exec it.
    #   5. User-local SDK dirs (~/go/bin/go, ~/sdk/*/bin/go)
    # NOTE: use ${GO_BIN:-} (not $GO_BIN) because `set -u` is in effect;
    # assigning `GO_BIN="${GO_BIN:-}"` preserves a user-supplied value
    # while ensuring the variable is bound when no override was given.
    GO_BIN="${GO_BIN:-}"
    if [ -n "$GO_BIN" ] && [ -x "$GO_BIN" ]; then
        : # explicit override, use as-is
    elif command -v go &>/dev/null; then
        GO_BIN="$(command -v go)"
    else
        # Fallback paths cover three layouts:
        #   - Linux Go installed via package manager or tarball: /usr/local/go/bin/go
        #   - Linux Go installed in user home: ~/go-sdk/bin/go, ~/go/bin/go
        #   - Windows Go accessed from Git Bash: /d/Go/bin/go(.exe)
        #   - Windows Go accessed from WSL bash: /mnt/d/Go/bin/go.exe
        # The .exe variants are required when a Windows Go is invoked from
        # WSL bash (Linux needs the explicit suffix to exec a Windows binary).
        for p in \
            "${GOROOT:+$GOROOT/bin/go}" "${GOROOT:+$GOROOT/bin/go.exe}" \
            /usr/local/go/bin/go \
            "$HOME/go-sdk/bin/go" \
            "$HOME/go/bin/go" "$HOME/go/bin/go.exe" \
            "$HOME/sdk/"*/bin/go "$HOME/sdk/"*/bin/go.exe \
            /d/Go/bin/go /d/Go/bin/go.exe \
            /c/Go/bin/go /c/Go/bin/go.exe \
            /e/Go/bin/go /e/Go/bin/go.exe \
            /mnt/d/Go/bin/go.exe /mnt/c/Go/bin/go.exe /mnt/e/Go/bin/go.exe; do
            if [ -x "$p" ]; then GO_BIN="$p"; break; fi
        done
    fi

    if [ -z "$GO_BIN" ] || [ ! -x "$GO_BIN" ]; then
        fail "Go compiler not found. Install Go or set GO_BIN env var.
  Recommended (WSL bash):    sudo apt install golang-go  (or download from go.dev)
  Recommended (PowerShell):  \$env:GO_BIN = 'D:\Go\bin\go.exe'
  Recommended (Git Bash):    GO_BIN=/d/Go/bin/go bash start.sh
  Tried: \$GO_BIN, PATH, \${GOROOT}/bin/go, /usr/local/go/bin/go,
         ~/go-sdk/bin/go, ~/go/bin/go, ~/sdk/*/bin/go,
         /d/Go/bin/go, /c/Go/bin/go, /e/Go/bin/go,
         /mnt/d/Go/bin/go.exe, /mnt/c/Go/bin/go.exe (and .exe variants)"
    fi

    info "Building MCP (linux/amd64, CGO disabled)..."
    "$GO_BIN" version

    mkdir -p "$PROJECT_DIR/agenttown-mcp/tmp"
    (cd "$PROJECT_DIR/agenttown-mcp" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -o tmp/agenttown-mcp-linux ./cmd/agenttown-mcp) \
        || fail "Go cross-compile failed"

    info "Running MCP unit tests..."
    (cd "$PROJECT_DIR/agenttown-mcp" && "$GO_BIN" test ./cmd/agenttown-mcp/ -count=1) \
        || fail "MCP unit tests failed; refusing to deploy broken binary"

    info "Deploying to WSL ~/agenttown-mcp..."
    # Remove stale binary first (avoids "text file busy").
    MSYS_NO_PATHCONV=1 $WSL rm -f /home/yitianchen/agenttown-mcp
    MSYS_NO_PATHCONV=1 $WSL cp /mnt/d/SmartNPC_v3/agenttown-mcp/tmp/agenttown-mcp-linux /home/yitianchen/agenttown-mcp \
        || fail "Failed to copy binary to WSL ~/agenttown-mcp"
    MSYS_NO_PATHCONV=1 $WSL chmod +x /home/yitianchen/agenttown-mcp
    ok "MCP binary deployed"

    # 清空 sim.log：必须在 MCP 启动前由 WSL 端清空，避免 MCP 已打开
    # O_APPEND fd 后再截断文件导致 WSL/Windows 文件系统缓存不一致产生
    # \0 字节（Windows 端 Python truncate 后 WSL 端 fd 偏移仍停留在旧
    # 位置，下次 write 在旧偏移处写入，前面变 sparse \0）。
    mkdir -p "$LOG_SUBDIR"
    : > "$LOG_SUBDIR/sim.log"

    # 在 WSL 内创建启动脚本（setsid + disown 确保 MCP 进程在 WSL 会话
    # 结束后仍能存活——直接 `wsl bash -c "cmd &"` 会在 wsl 返回时杀掉子进程）
    # 重要：使用 >> (追加模式) 而非 > (截断模式)，这样 Mock UE (Python, Windows)
    # 也能以 append 模式写入同一个文件，两个进程的日志不会互相覆盖。
    # heredoc 不加引号，让 $LOG_DATE 在 host shell 展开后传入 WSL。
    $WSL_BASH "cat > ~/start_mcp.sh << LAUNCHER
#!/bin/bash
pkill -x agenttown-mcp 2>/dev/null
sleep 1
setsid /home/yitianchen/agenttown-mcp --http :8760 --ws :9090 --hermes-url http://localhost:8642 >> /mnt/d/SmartNPC_v3/logs/$LOG_DATE/sim.log 2>&1 &
disown
sleep 2
LAUNCHER
chmod +x ~/start_mcp.sh"

    info "Starting MCP in WSL background..."
    $WSL_BASH 'bash ~/start_mcp.sh'

    wait_for "MCP HTTP (:8760)" check_mcp_http 20
    wait_for "MCP WS (:9090)" check_mcp_ws 10
}

# ─── 步骤 2: 启动 CodeBuddy Adapter ───────────────────────────
# 适配层把 OpenAI /v1/chat/completions 转成 CLI Run API 请求，
# 复用 CLI 已有的 OAuth 认证调用公司 GLM-5.2 模型。
#
# 硬约束：适配层必须在 Windows 运行（CLI 本地服务绑 127.0.0.1，
# WSL 访问不到）。所以这里用 Windows Python 启动，不能用 WSL Python。
#
# start.sh 可从两种环境运行，此函数都需兼容：
#   - Git Bash (Windows): `python` 是 Windows Python，路径用 cygpath -w 转换
#     （$PROJECT_DIR 是 /d/SmartNPC_v3 风格，cygpath 理解 /d/ → D:\）
#   - WSL bash: `python.exe` 通过互操作调用 Windows Python，路径用 wslpath -w
#     （$PROJECT_DIR 是 /mnt/d/SmartNPC_v3 风格，cygpath 不理解 /mnt/d/，
#      会错误转成 D:\...\mnt\d\...；wslpath 才能正确转成 D:\SmartNPC_v3\...）
start_adapter() {
    info "=== Step 2: Start CodeBuddy CLI Adapter ==="

    if [ ! -f "$ADAPTER_SCRIPT" ]; then
        fail "Adapter script not found: $ADAPTER_SCRIPT"
    fi

    # 定位 Windows Python。适配层必须在 Windows 跑（CLI 绑 127.0.0.1）。
    local py_cmd=""
    local script_arg="$ADAPTER_SCRIPT"

    if [ -n "$WSL" ]; then
        # Git Bash (Windows) — `python` 通常就是 Windows Python
        if ! command -v python &>/dev/null; then
            fail "Python not found. Install Python 3.10+ on Windows and add to PATH."
        fi
        py_cmd="python"
        # MSYS 路径 /d/... → Windows 路径 D:\...
        if command -v cygpath &>/dev/null; then
            script_arg="$(cygpath -w "$ADAPTER_SCRIPT")"
        fi
    else
        # WSL — 需要 Windows Python 互操作
        if ! $WSL_BASH 'command -v python.exe &>/dev/null' 2>/dev/null; then
            fail "Adapter must run on Windows Python, but python.exe not found in WSL PATH.
  Options:
    1. Run start.sh from Git Bash (recommended): bash start.sh
    2. Install Python on Windows and ensure it's in Windows PATH (python.exe)"
        fi
        py_cmd="python.exe"
        # WSL 路径 /mnt/d/... → Windows 路径 D:\...
        # 必须用 wslpath，不能用 cygpath（cygpath 不理解 /mnt/d/ 挂载约定，
        # 会把 /mnt/d/SmartNPC_v3/... 错误转成 D:\...\mnt\d\SmartNPC_v3\...）
        if $WSL_BASH 'command -v wslpath &>/dev/null' 2>/dev/null; then
            script_arg=$($WSL_BASH "wslpath -w '$ADAPTER_SCRIPT'" 2>/dev/null)
            [ -z "$script_arg" ] && script_arg="$ADAPTER_SCRIPT"
        fi
    fi
    ok "  Python: $py_cmd"
    ok "  Script: $script_arg"

    local adapter_log="$LOG_SUBDIR/adapter.log"

    # 启动适配层到后台。nohup + disown 确保进程在 start.sh 退出后存活。
    # 日志写入 logs/$LOG_DATE/adapter.log（与 sim.log 同目录，但独立文件，
    # 因为适配层是基础设施，不属于仿真协议链路）。
    info "Starting adapter on :$ADAPTER_PORT (log: logs/$LOG_DATE/adapter.log)..."
    : > "$adapter_log"  # 清空旧日志，方便本次排查
    nohup "$py_cmd" "$script_arg" --port "$ADAPTER_PORT" > "$adapter_log" 2>&1 &
    disown

    # 自定义等待循环：失败时打印 adapter.log 末尾方便排查（wait_for 会直接
    # exit 1，无法在失败后追加诊断信息）
    info "Waiting for CodeBuddy Adapter (:$ADAPTER_PORT) (max 20s)..."
    local elapsed=0
    while [ $elapsed -lt 20 ]; do
        if check_adapter; then
            ok "CodeBuddy Adapter (:$ADAPTER_PORT) is up"
            break
        fi
        sleep 2; elapsed=$((elapsed + 2)); printf "."
    done
    echo ""
    if [ $elapsed -ge 20 ]; then
        warn "  Adapter did not come up within 20s. Last 25 lines of adapter.log:"
        tail -25 "$adapter_log" 2>/dev/null | sed 's/^/    /'
        fail "  Adapter failed to start. See logs/$LOG_DATE/adapter.log for full details."
    fi

    # 验证适配层能连通 CLI（CLI 必须已登录）
    local health
    health=$(curl -sS http://$ADAPTER_HOST:$ADAPTER_PORT/health 2>/dev/null)
    if echo "$health" | grep -q '"status":"ok"'; then
        local cli_url
        cli_url=$(echo "$health" | sed -n 's/.*"cli_url":"\([^"]*\)".*/\1/p')
        ok "  Adapter connected to CLI ($cli_url)"
    else
        warn "  Adapter is up but CLI not reachable (health: $health)"
        fail "  CodeBuddy CLI not running or not logged in.
  Start CLI in a separate terminal: codebuddy
  Log in with your Tencent account, then re-run start.sh"
    fi
}

# ─── 步骤 3: 启动 Hermes ──────────────────────────────────────
start_hermes() {
    info "=== Step 3: Start Hermes Gateway ==="

    if [ ! -f "$ENV_FILE" ]; then
        fail ".env file not found at $ENV_FILE"
    fi

    # 每次启动都从本地 Hermes 源码强制重建 Docker 镜像，确保 Hermes
    # 仓库的任何改动（Python 代码、依赖、Dockerfile）立即生效。SOUL.md
    # 与 SKILL.md 通过 volume 挂载（见 docker-compose.yml），改这两个
    # 文件本身不需要重建镜像，但镜像重建不会跳过。
    info "Rebuilding Hermes Docker image from source..."
    HERMES_BUILD_SCRIPT="$PROJECT_DIR/docker/build-hermes.sh"
    if [ ! -f "$HERMES_BUILD_SCRIPT" ]; then
        fail "Hermes build script not found: $HERMES_BUILD_SCRIPT"
    fi
    # build-hermes.sh 通过 HERMES_SOURCE 环境变量定位 Hermes 源码
    # （默认 /mnt/c/Users/yitianchen/AppData/Local/hermes/hermes-agent）。
    # Docker 在 WSL 内运行，所以脚本要在 WSL 里执行。
    #
    # 路径处理：脚本可能在两种环境下被调用，路径风格不同：
    #   - Git Bash (MINGW): $PROJECT_DIR 是 /d/SmartNPC_v3 风格，需要
    #     wslpath 转成 /mnt/d/SmartNPC_v3 才能给 WSL 用
    #   - WSL bash: $PROJECT_DIR 已经是 /mnt/d/SmartNPC_v3 风格，直接用
    # 通过检测开头是否 /mnt/ 来判断，避免重复加前缀。
    case "$HERMES_BUILD_SCRIPT" in
        /mnt/*)
            HERMES_BUILD_SCRIPT_WSL="$HERMES_BUILD_SCRIPT" ;;
        /?/*)
            # MSYS/Git-Bash 风格路径 /d/... → /mnt/d/...
            # wslpath -u 对 MSYS 路径会错误剥离前导 / 产生相对路径，
            # 导致 WSL 以 /mnt/d/ 为 CWD 拼出 /mnt/d/d/... 找不到文件。
            HERMES_BUILD_SCRIPT_WSL="/mnt${HERMES_BUILD_SCRIPT}" ;;
        *)
            HERMES_BUILD_SCRIPT_WSL=$(MSYS_NO_PATHCONV=1 $WSL wslpath -u "$HERMES_BUILD_SCRIPT" 2>/dev/null \
                || echo "/mnt/d/SmartNPC_v3/docker/build-hermes.sh") ;;
    esac
    MSYS_NO_PATHCONV=1 $WSL_BASH "bash '$HERMES_BUILD_SCRIPT_WSL'" \
        || fail "Hermes Docker image build failed"

    info "Starting Hermes via docker compose..."
    # MSYS_NO_PATHCONV=1 prevents Git Bash from mangling WSL paths
    # --force-recreate ensures Hermes restarts and reconnects to MCP
    MSYS_NO_PATHCONV=1 $WSL docker compose -f "$DOCKER_COMPOSE_FILE" \
        --env-file "$DOCKER_ENV_FILE" up -d --force-recreate

    wait_for "Hermes Gateway (:8642)" check_hermes 40

    # 等 Hermes 连接 MCP（MCP 日志出现 session initialized）
    info "Waiting for Hermes to discover MCP tools..."
    local elapsed=0
    while [ $elapsed -lt 40 ]; do
        if $WSL_BASH "tail -10 /mnt/d/SmartNPC_v3/logs/$LOG_DATE/sim.log 2>/dev/null | grep -q 'session initialized'"; then
            ok "Hermes connected to MCP"
            sleep 2  # 给 Hermes 额外时间完成工具注册
            return 0
        fi
        sleep 2; elapsed=$((elapsed + 2)); printf "."
    done
    echo ""
    fail "Hermes did not connect to MCP within 40s. Check:
  $WSL docker logs agenttown-h01 2>&1 | grep -i mcp
  $WSL tail -20 /mnt/d/SmartNPC_v3/logs/$LOG_DATE/sim.log"
}

# ─── 步骤 4: 启动 Mock UE ─────────────────────────────────────
start_mock_ue() {
    info "=== Step 4: Start Mock UE ==="

    if [ ! -f "$MOCK_UE_SCRIPT" ]; then
        fail "Mock UE script not found: $MOCK_UE_SCRIPT"
    fi

    # 最终预检查
    info "Pre-flight checks:"
    check_mcp_http && ok "  MCP HTTP reachable" || fail "  MCP HTTP unreachable"
    check_mcp_ws   && ok "  MCP WS reachable"   || fail "  MCP WS unreachable"
    check_adapter  && ok "  Adapter reachable"  || fail "  Adapter unreachable"
    check_hermes   && ok "  Hermes reachable"   || fail "  Hermes unreachable"

    local args=(
        "--mode" "$MODE"
        "--start" "$MOCK_START"
        "--end" "$MOCK_END"
        "--speed" "$MOCK_SPEED"
        "--interval" "$MOCK_INTERVAL"
        "--log-dir" "$LOG_SUBDIR"
    )
    [ -n "$MOCK_SCENARIO" ] && args+=("--scenario" "$MOCK_SCENARIO")

    echo ""
    echo "============================================================"
    echo "  AgentTown_v3 — Day Simulation"
    echo "  Mode: ${MODE}"
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

    # Windows Python does not understand MSYS paths reliably when bash was
    # launched from PowerShell (it may turn /d/... into D:\d\...). Convert
    # script/scenario/log-dir paths explicitly before invoking it.
    # --log-dir must be converted too: otherwise os.path.join("/d/SmartNPC_v3/...",
    # "sim.log") resolves to D:\d\SmartNPC_v3\...\sim.log (note the extra "d\"),
    # and run_day.py truncates a stale file at the wrong path while the actual
    # sim.log (written by MCP via WSL) accumulates across runs.
    local script_arg="$MOCK_UE_SCRIPT"
    if command -v cygpath &>/dev/null; then
        script_arg="$(cygpath -w "$MOCK_UE_SCRIPT")"
        local log_dir_win
        log_dir_win="$(cygpath -w "$LOG_SUBDIR")"
        for i in "${!args[@]}"; do
            [ "${args[$i]}" = "$LOG_SUBDIR" ] && args[$i]="$log_dir_win"
        done
        if [ -n "$MOCK_SCENARIO" ]; then
            local scenario_win
            scenario_win="$(cygpath -w "$MOCK_SCENARIO")"
            for i in "${!args[@]}"; do
                [ "${args[$i]}" = "$MOCK_SCENARIO" ] && args[$i]="$scenario_win"
            done
        fi
    fi
    if ! "$py_cmd" "$script_arg" "${args[@]}"; then
        fail "Mock UE simulation failed"
    fi

    # ── Unified log ──────────────────────────────────────────────
    # MCP is the sole writer of logs/YYYY-MM-DD/sim.log (JSON Lines).
    # Mock UE no longer writes its own day1_*.log file — its events are
    # captured by MCP's [UE→MCP] / [MCP→UE] logs, and human-readable
    # summaries ([PERCEPTION]/[STATE]/[SPEAK]) go to Mock UE's console
    # only. No post-run merge needed.
    ok "Unified log at logs/$LOG_DATE/sim.log (UE + MCP + Hermes)"
}

# ─── 主流程 ────────────────────────────────────────────────────
info "AgentTown_v3 — Full Restart & Start"
echo ""

stop_all
start_mcp
echo ""
start_adapter
echo ""
start_hermes
echo ""
start_mock_ue
