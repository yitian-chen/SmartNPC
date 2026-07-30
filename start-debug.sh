#!/usr/bin/env bash
# AgentTown_v3 — 联调启动脚本（给 UE 端同事连接用）
#
# 与 start.sh 的区别：
#   - MCP 在 Windows 原生跑（监听 0.0.0.0），局域网可达
#   - 不启动 Mock UE（避免抢 WS 连接）
#   - 启动后打印局域网地址给 UE 同事
#
# 启动组件：
#   1. agenttown-mcp.exe (Windows, WS :9090 + HTTP :8760)  ← UE 连这个
#   2. CodeBuddy Adapter (Windows, :8761, localhost only)
#   3. Hermes Gateway (Docker/WSL, :8642, localhost only)
#
# 用法：
#   bash start-debug.sh                # 默认全启
#   bash start-debug.sh --no-rebuild   # 跳过 Hermes 镜像重建（快速重启）
#   bash start-debug.sh --no-hermes    # 跳过 Hermes（已手动启动时用）
#   bash start-debug.sh --no-adapter   # 跳过 Adapter（已手动启动时用）
#
# 前置：
#   - Go 编译器可访问（PATH 中有 go，或设置 GO_BIN）
#   - Docker Desktop 运行中（Hermes 跑在 Docker）
#   - CodeBuddy CLI 已登录（适配层复用其 OAuth）
#   - d:/SmartNPC_v3/.env 存在且配置了 HERMES_AGENT_API_KEY

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

# ─── 路径 ──────────────────────────────────────────────────────
# 所有端口/路径支持环境变量覆盖，使同一脚本可服务稳定实例和开发实例。
# 开发实例用 start-dev.sh wrapper 设置偏移端口后调本脚本。
PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
MCP_DIR="$PROJECT_DIR/agenttown-mcp"
# 二进制名可环境变量覆盖：stable 用 agenttown-mcp.exe，dev 用 agenttown-mcp-dev.exe
# 避免两实例共用同一 exe 导致 stable 运行时 dev 无法重新编译覆盖
MCP_EXE_NAME="${MCP_EXE_NAME:-agenttown-mcp.exe}"
MCP_EXE="$MCP_DIR/$MCP_EXE_NAME"
DOCKER_COMPOSE="${DOCKER_COMPOSE:-$PROJECT_DIR/docker/docker-compose.yml}"
ENV_FILE="$PROJECT_DIR/.env"
ADAPTER_SCRIPT="$PROJECT_DIR/src/agenttown/codebuddy_adapter.py"
ADAPTER_PORT="${ADAPTER_PORT:-8761}"
WS_PORT="${WS_PORT:-9090}"
HTTP_PORT="${HTTP_PORT:-8760}"
HERMES_PORT="${HERMES_PORT:-8642}"
CLI_PORT="${CLI_PORT:-52001}"
HERMES_CONTAINER="${HERMES_CONTAINER:-agenttown-h01}"

LOG_DATE=$(date +%Y-%m-%d)
LOG_SUBDIR_BASE="$PROJECT_DIR/logs/$LOG_DATE"
LOG_SUBDIR="${LOG_SUBDIR:-$LOG_SUBDIR_BASE}"
MCP_LOG="$LOG_SUBDIR/debug-mcp.log"
ADAPTER_LOG="$LOG_SUBDIR/debug-adapter.log"

# ─── 加载 .env 到 shell 环境 ──────────────────────────────────
# MCP 是 Windows exe，通过 .bat 启动；bash export 不会自动传给 cmd.exe，
# 因此需要在生成 .bat 时显式 set 环境变量。这里先把 .env 的变量 source
# 到当前 shell，后续 generate .bat 时再注入。
# 默认架构是 MCP → Hermes → Venus：MCP 走 hermes backend，Venus 凭据由
# Hermes 容器通过 docker-compose env_file 直接读取，不经过 MCP。
# 这里仍加载 VENUS_ 前缀变量，便于临时切回 --llm-backend venus 直连调试。
if [ -f "$ENV_FILE" ]; then
    while IFS='=' read -r key value || [ -n "$key" ]; do
        # 跳过空行和注释
        case "$key" in
            ''|\#*) continue ;;
        esac
        # 只 export VENUS_ 前缀变量
        case "$key" in
            VENUS_*) export "$key=$value" ;;
        esac
    done < "$ENV_FILE"
fi

# Venus URL/model 默认值（与 Go flag 默认值一致）。
# 仅在临时切回 --llm-backend venus 直连调试时使用；默认走 Hermes 时不读取。
VENUS_URL="${VENUS_URL:-http://v2.open.venus.oa.com/llmproxy}"
VENUS_MODEL="${VENUS_MODEL:-qwen3.6-35b-a3b}"

# ─── 参数 ──────────────────────────────────────────────────────
REBUILD_HERMES=true
START_HERMES=true
START_ADAPTER=true
STOP_ONLY=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --no-rebuild) REBUILD_HERMES=false; shift ;;
        --no-hermes)  START_HERMES=false; shift ;;
        --no-adapter) START_ADAPTER=false; shift ;;
        --stop)       STOP_ONLY=true; shift ;;
        -h|--help)
            echo "Usage: bash start-debug.sh [OPTIONS]"
            echo ""
            echo "UE 联调启动脚本：MCP 跑在 Windows 原生（监听 0.0.0.0），局域网可达。"
            echo ""
            echo "Options:"
            echo "  --stop         仅停止所有服务，不重启"
            echo "  --no-rebuild   跳过 Hermes 镜像重建（快速重启）"
            echo "  --no-hermes    跳过 Hermes 启动（已手动启动时用）"
            echo "  --no-adapter   跳过 Adapter 启动（已手动启动时用）"
            echo ""
            echo "UE 端连接地址：ws://<本机局域网IP>:$WS_PORT/ws"
            exit 0 ;;
        *) warn "Unknown option: $1"; shift ;;
    esac
done

# ─── 环境检测 ──────────────────────────────────────────────────
# 脚本可能在三种环境运行：
#   - Git Bash (Windows): localhost = Windows localhost，用 wsl 调 WSL 命令、cygpath 转路径
#   - WSL bash: localhost = WSL VM localhost，访问 Windows 服务需用宿主 IP（wslpath 转路径）
#   - 纯 Linux (AnyDev/远程): 无 Windows 工具，无需路径转换，docker compose 直接可用
# 检测方法：WSL 里 /proc/version 含 "microsoft"；纯 Linux 无 cmd.exe；其余视为 Git Bash。
IN_WSL=false
IN_LINUX=false
if grep -qi microsoft /proc/version 2>/dev/null; then
    IN_WSL=true
elif ! command -v cmd.exe >/dev/null 2>&1; then
    IN_LINUX=true
fi

# Windows 宿主 IP（WSL 访问 Windows 服务用）；Git Bash 和纯 Linux 里不用
WIN_HOST="localhost"
if $IN_WSL; then
    WIN_HOST=$(ip route show default 2>/dev/null | awk '{print $3}' | head -1)
    [ -z "$WIN_HOST" ] && WIN_HOST=$(grep nameserver /etc/resolv.conf 2>/dev/null | head -1 | awk '{print $2}')
    [ -z "$WIN_HOST" ] && WIN_HOST="172.18.16.1"  # 兜底：常见 vEthernet IP
fi

# WSL 调用前缀与路径转换工具：
#   - WSL: WSL_CMD=""（直接执行），PATH_CONV=wslpath
#   - 纯 Linux: WSL_CMD=""，PATH_CONV=""（无需转换）
#   - Git Bash: WSL_CMD=wsl，PATH_CONV=cygpath
WSL_CMD=""
PATH_CONV=""
if $IN_WSL; then
    WSL_CMD=""
    PATH_CONV="wslpath"
elif $IN_LINUX; then
    WSL_CMD=""
    PATH_CONV=""
else
    WSL_CMD="wsl"
    PATH_CONV="cygpath"
fi

# ─── 健康检查 ──────────────────────────────────────────────────
# MCP 跑在 Windows（本脚本的核心设计），WSL 里访问要用 WIN_HOST。
# Hermes 跑在 Docker（WSL2），localhost 即可达（WSL2 localhost forwarding）。
# Adapter 跑在 Windows，WSL 里访问要用 WIN_HOST。
check_mcp_http() { curl -sf http://$WIN_HOST:$HTTP_PORT/healthz >/dev/null 2>&1; }
check_mcp_ws()   { curl -sf http://$WIN_HOST:$WS_PORT/healthz >/dev/null 2>&1; }
check_hermes()   { curl -sf http://localhost:$HERMES_PORT/health >/dev/null 2>&1; }
check_adapter()  { curl -sf http://$WIN_HOST:$ADAPTER_PORT/health >/dev/null 2>&1; }

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

# ─── 局域网 IP 探测 ────────────────────────────────────────────
# 排除 WSL vEthernet (172.18.x.x) 和 VPN 虚拟网卡 (192.168.255.x)，
# 优先返回公司内网 IP。
detect_lan_ip() {
    if $IN_LINUX; then
        # 纯 Linux：hostname -I 输出空格分隔的 IP 列表，取第一个
        local ip
        ip=$(hostname -I 2>/dev/null | awk '{print $1}')
        if [ -z "$ip" ]; then
            # 兜底：从 ip addr 提取第一个非 loopback 的 IPv4
            ip=$(ip -4 addr show 2>/dev/null | grep -oE "inet [0-9.]+" | grep -v "127.0.0.1" | head -1 | awk '{print $2}')
        fi
        echo "$ip"
        return
    fi
    # Windows/WSL：用 ipconfig.exe
    local ip
    # ipconfig 输出可能因语言不同而字段名不同，用 grep 抓所有 IPv4 行
    ip=$(ipconfig.exe 2>/dev/null | grep -E "IPv4|IPv4 Address" \
        | grep -oE "[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+" \
        | grep -v "^127\." \
        | grep -v "^172\.18\." \
        | grep -v "^192\.168\.255\." \
        | head -1)
    if [ -z "$ip" ]; then
        # 兜底：取第一个非 localhost 的 IP
        ip=$(ipconfig.exe 2>/dev/null | grep -E "IPv4|IPv4 Address" \
            | grep -oE "[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+" \
            | grep -v "^127\." | head -1)
    fi
    echo "$ip"
}

# ─── Step 0: 停止现有进程 ──────────────────────────────────────
# kill_port_listeners <port> <label>
# 杀掉指定端口上所有监听进程（IPv4 + IPv6，任意地址）。
# 关键：不能只抓 0.0.0.0，否则会漏掉 WSL wslrelay 镜像在 [::1] 上的幽灵监听，
# 导致新启动的 MCP 被 wslrelay 抢占端口（curl 命中 wslrelay 返回 404）。
kill_port_listeners() {
    local port="$1" label="$2"
    if $IN_LINUX; then
        # 纯 Linux：用 ss 找监听 PID + kill。awk 提取 "pid=1234" 中的数字。
        local pids
        pids=$(ss -ltnp 2>/dev/null | awk -v p=":$port" '$4 ~ p"$" {match($0, /pid=([0-9]+)/, m); print m[1]}' | sort -u)
        if [ -z "$pids" ]; then
            warn "  No listener on :$port"
            return 0
        fi
        local pid
        for pid in $pids; do
            kill -9 "$pid" >/dev/null 2>&1 \
                && ok "  $label on :$port stopped (PID $pid)" \
                || warn "  Failed to kill $label PID $pid on :$port"
        done
        return
    fi
    # Windows/WSL：netstat.exe + taskkill.exe
    local pids
    # netstat 本地地址列 ($2) 形如 0.0.0.0:8760 / [::1]:8760 / [::]:8760 / 127.0.0.1:8760
    # 用 awk 匹配 ":<port>$" 结尾，避免误伤 87600 等端口
    pids=$(netstat.exe -ano 2>/dev/null \
        | awk -v p=":$port" '$2 ~ p"$" && $4=="LISTENING" {gsub(/\r/,""); print $NF}' \
        | sort -u)
    if [ -z "$pids" ]; then
        warn "  No listener on :$port"
        return 0
    fi
    local pid
    for pid in $pids; do
        MSYS_NO_PATHCONV=1 taskkill.exe /F /PID "$pid" >/dev/null 2>&1 \
            && ok "  $label on :$port stopped (PID $pid)" \
            || warn "  Failed to kill $label PID $pid on :$port"
    done
}

stop_all() {
    info "=== Step 0: Stop existing processes ==="

    # Mock UE（联调不应跑，但兜底杀）
    info "Stopping Mock UE..."
    pkill -f "run_day.py" 2>/dev/null && ok "  Mock UE stopped" || warn "  Mock UE not running"

    # MCP（Windows exe）— 杀掉端口上所有监听者（含 WSL wslrelay 幽灵）
    info "Stopping existing MCP..."
    local port
    for port in $WS_PORT $HTTP_PORT; do
        kill_port_listeners "$port" "MCP"
    done

    # Adapter
    if $START_ADAPTER; then
        info "Stopping existing Adapter..."
        kill_port_listeners "$ADAPTER_PORT" "Adapter"
    fi

    # CodeBuddy CLI 子进程
    kill_port_listeners "$CLI_PORT" "CLI subprocess"

    # Hermes（可选停止）
    if $START_HERMES; then
        info "Stopping Hermes..."
        if $IN_LINUX; then
            # 裸金属：pkill hermes gateway 进程 + 兜底杀端口
            pkill -f "hermes.*gateway run" 2>/dev/null && ok "  Hermes process stopped" || warn "  Hermes process not running"
            kill_port_listeners "$HERMES_PORT" "Hermes"
        else
            local compose_path
            if $IN_WSL; then
                compose_path="$DOCKER_COMPOSE"
            else
                compose_path=$(MSYS_NO_PATHCONV=1 $WSL_CMD wslpath -u "$DOCKER_COMPOSE" 2>/dev/null || echo "/mnt/d/SmartNPC_v3/docker/docker-compose.yml")
            fi
            $WSL_CMD docker compose -f "$compose_path" stop 2>/dev/null \
                && ok "  Hermes stopped" \
                || warn "  Hermes not running"
        fi
    fi

    sleep 2
    echo ""
}

# ─── Step 1: 编译 MCP Windows exe ──────────────────────────────
build_mcp() {
    info "=== Step 1: Build agenttown-mcp.exe (Windows) ==="

    # 定位 Go 编译器（与 start.sh 保持一致的查找逻辑）
    GO_BIN="${GO_BIN:-}"
    if [ -n "$GO_BIN" ] && [ -x "$GO_BIN" ]; then
        : # explicit override, use as-is
    elif command -v go &>/dev/null; then
        GO_BIN="$(command -v go)"
    else
        # Fallback paths cover common layouts (与 start.sh 同步):
        #   - Linux Go: /usr/local/go/bin/go
        #   - Windows Go from Git Bash: /d/Go/bin/go (无 .exe 后缀也能执行)
        #   - Windows Go from WSL: /mnt/d/Go/bin/go.exe
        #   - User-local SDK: ~/go/bin/go, ~/sdk/*/bin/go
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
        fail "Go compiler not found. Set GO_BIN env var, e.g.:
  GO_BIN=/d/Go/bin/go bash start-debug.sh"
    fi

    # 删除旧二进制，强制 go build 重新链接产物。
    # Go 的包缓存是内容哈希的（源码改了必定重编包），但若输出文件存在且 mtime 较新，
    # go build 可能直接跳过链接步骤。删掉旧 exe 保证最终二进制永远反映当前源码。
    rm -f "$MCP_EXE"
    if $IN_LINUX; then
        info "Building MCP (linux/amd64)..."
        (cd "$MCP_DIR" && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -o "$MCP_EXE_NAME" ./cmd/agenttown-mcp) \
            || fail "Go build failed"
    else
        info "Building MCP (windows/amd64)..."
        (cd "$MCP_DIR" && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 "$GO_BIN" build -o "$MCP_EXE_NAME" ./cmd/agenttown-mcp) \
            || fail "Go build failed"
    fi
    "$GO_BIN" version

    info "Running MCP unit tests..."
    (cd "$MCP_DIR" && "$GO_BIN" test ./cmd/agenttown-mcp/ -count=1) \
        || fail "MCP unit tests failed; refusing to deploy broken binary"

    ok "MCP binary built: $MCP_EXE"
    echo ""
}

# ─── Step 2: 启动 Adapter ─────────────────────────────────────
start_adapter() {
    info "=== Step 2: Start CodeBuddy Adapter (localhost:$ADAPTER_PORT) ==="

    if [ ! -f "$ADAPTER_SCRIPT" ]; then
        fail "Adapter script not found: $ADAPTER_SCRIPT"
    fi

    # 定位 Python（adapter 需要 httpx, pyyaml, fastapi, uvicorn）
    # 优先用 Hermes venv（依赖齐全），其次 PATH 里的 python/python3/py
    # Windows venv 里的可执行文件是 python.exe（无后缀的 python 不存在），
    # 所以显式加 .exe 后缀检测。
    local py_cmd=""
    local hermes_venv_py="/c/Users/yitianchen/AppData/Local/hermes/hermes-agent/venv/Scripts/python.exe"
    if [ -x "$hermes_venv_py" ]; then
        if "$hermes_venv_py" -c "import httpx, yaml, fastapi, uvicorn" 2>/dev/null; then
            py_cmd="$hermes_venv_py"
            ok "  Using Hermes venv Python: $py_cmd"
        fi
    fi
    if [ -z "$py_cmd" ]; then
        for cmd in python python.exe python3 py; do
            # command -v 返回路径后，再验证不是 Windows Store 的 stub
            # （Store stub 执行会重定向到商店，import 必失败）
            local resolved
            resolved=$(command -v "$cmd" 2>/dev/null) || continue
            case "$resolved" in
                *WindowsApps*) continue ;;  # 跳过 Store stub
            esac
            if "$resolved" -c "import httpx, yaml, fastapi, uvicorn" 2>/dev/null; then
                py_cmd="$resolved"
                ok "  Using Python: $py_cmd"
                break
            fi
        done
    fi
    if [ -z "$py_cmd" ]; then
        fail "Python with deps (httpx, pyyaml, fastapi, uvicorn) not found.
  Options:
    1. Ensure Hermes venv exists: $hermes_venv_py
    2. pip install httpx pyyaml fastapi uvicorn into your Python
    3. Set PATH to include a Python that has these deps"
    fi

    mkdir -p "$LOG_SUBDIR"
    : > "$ADAPTER_LOG"

    # 路径转换：脚本可能在 Git Bash（用 cygpath）或 WSL（用 wslpath）运行。
    # Windows Python 不认 /d/... 或 /mnt/d/... 风格路径，必须转成 D:\... 风格。
    # cygpath 不理解 /mnt/d/ 挂载约定，会错误转成 D:\mnt\d\...；wslpath 才对。
    local script_arg="$ADAPTER_SCRIPT"
    if command -v wslpath &>/dev/null; then
        # WSL 环境
        script_arg=$(wslpath -w "$ADAPTER_SCRIPT" 2>/dev/null) || script_arg="$ADAPTER_SCRIPT"
    elif command -v cygpath &>/dev/null; then
        # Git Bash (MSYS) 环境
        script_arg="$(cygpath -w "$ADAPTER_SCRIPT")"
    fi

    info "Starting adapter (log: logs/$LOG_DATE/debug-adapter.log)..."
    info "  Python: $py_cmd"
    info "  Script: $script_arg"
    nohup "$py_cmd" "$script_arg" --port "$ADAPTER_PORT" --cli-port "$CLI_PORT" > "$ADAPTER_LOG" 2>&1 &
    disown

    info "Waiting for Adapter (max 20s)..."
    local elapsed=0
    while [ $elapsed -lt 20 ]; do
        if check_adapter; then
            ok "Adapter is up (localhost:$ADAPTER_PORT)"
            local health
            health=$(curl -sS http://localhost:$ADAPTER_PORT/health 2>/dev/null)
            if echo "$health" | grep -q '"status":"ok"'; then
                ok "  Adapter connected to CLI"
            else
                warn "  Adapter up but CLI not reachable: $health"
                warn "  Start CodeBuddy CLI in a separate terminal: codebuddy"
            fi
            return 0
        fi
        sleep 2; elapsed=$((elapsed + 2)); printf "."
    done
    echo ""
    warn "  Adapter failed. Last 15 lines:"
    tail -15 "$ADAPTER_LOG" 2>/dev/null | sed 's/^/    /'
    fail "  Adapter failed to start."
}

# ─── Step 3: 启动 Hermes ──────────────────────────────────────
start_hermes() {
    info "=== Step 3: Start Hermes Gateway (localhost:$HERMES_PORT) ==="

    if [ ! -f "$ENV_FILE" ]; then
        fail ".env file not found at $ENV_FILE"
    fi

    # 加载 .env 中的 VENUS_API_KEY（h01-dev 直连 Venus 必需）
    local venus_key=""
    if grep -q "^VENUS_API_KEY=" "$ENV_FILE" 2>/dev/null; then
        venus_key=$(grep "^VENUS_API_KEY=" "$ENV_FILE" | cut -d= -f2-)
    fi

    if $IN_LINUX; then
        # 裸金属：直接在本机跑 hermes 命令，不走 Docker
        # 依赖：pip install -e /data/workspace/hermes-agent + pip install aiohttp
        # profile 路径通过 HERMES_HOME 指向 $PROJECT_DIR/hermes，
        # Hermes 自动解析到 $HERMES_HOME/profiles/h01-dev
        if ! command -v hermes >/dev/null 2>&1; then
            fail "hermes command not found. Install dependencies:
  cd /data/workspace/hermes-agent && pip3 install -e . && pip3 install aiohttp"
        fi

        # 裸金属下 host.docker.internal 不存在（那是 Docker 容器内才能解析的宿主别名）。
        # h01-dev config.yaml 里 MCP URL 写的是 http://host.docker.internal:8770/mcp，
        # 这里在 /etc/hosts 加一行让该域名解析到 127.0.0.1，MCP 和 Hermes 同机运行。
        if ! getent hosts host.docker.internal >/dev/null 2>&1; then
            echo "127.0.0.1 host.docker.internal" | sudo tee -a /etc/hosts >/dev/null 2>&1 \
                && ok "  Added host.docker.internal → 127.0.0.1 to /etc/hosts" \
                || warn "  Failed to add host.docker.internal to /etc/hosts (MCP connection may fail)"
        fi

        local hermes_log="$LOG_SUBDIR/debug-hermes.log"
        mkdir -p "$LOG_SUBDIR"
        : > "$hermes_log"

        info "Starting Hermes (bare-metal, log: $LOG_SUBDIR/debug-hermes.log)..."
        HERMES_HOME="$PROJECT_DIR/hermes" \
        TERMINAL_CWD="$PROJECT_DIR/hermes" \
        VENUS_API_KEY="$venus_key" \
        GATEWAY_ALLOW_ALL_USERS=true \
        nohup hermes -p h01-dev gateway run --accept-hooks >> "$hermes_log" 2>&1 &
        disown

        wait_for "Hermes Gateway (:$HERMES_PORT)" check_hermes 40
        ok "Hermes is up (bare-metal)"
        echo ""
        return
    fi

    # Windows/WSL：走 Docker compose
    if $REBUILD_HERMES; then
        info "Rebuilding Hermes Docker image..."
        HERMES_BUILD_SCRIPT="$PROJECT_DIR/docker/build-hermes.sh"
        if [ ! -f "$HERMES_BUILD_SCRIPT" ]; then
            fail "Hermes build script not found: $HERMES_BUILD_SCRIPT"
        fi
        # 转成 WSL 路径（/mnt/d/... 风格），build-hermes.sh 在 WSL/Docker 里跑
        # WSL 直接用原路径；仅 Git Bash 需要 wslpath 转换
        local script_wsl
        if $IN_WSL; then
            script_wsl="$HERMES_BUILD_SCRIPT"
        else
            case "$HERMES_BUILD_SCRIPT" in
                /mnt/*) script_wsl="$HERMES_BUILD_SCRIPT" ;;
                /?/*)   script_wsl="/mnt${HERMES_BUILD_SCRIPT}" ;;
                *)      script_wsl=$(MSYS_NO_PATHCONV=1 $WSL_CMD wslpath -u "$HERMES_BUILD_SCRIPT" 2>/dev/null \
                            || echo "/mnt/d/SmartNPC_v3/docker/build-hermes.sh") ;;
            esac
        fi
        MSYS_NO_PATHCONV=1 $WSL_CMD bash -c "bash '$script_wsl'" \
            || fail "Hermes Docker image build failed"
    else
        info "Skipping Hermes image rebuild (--no-rebuild)"
    fi

    info "Starting Hermes via docker compose..."
    local compose_wsl env_wsl
    if $IN_WSL; then
        compose_wsl="$DOCKER_COMPOSE"
        env_wsl="$ENV_FILE"
    else
        compose_wsl=$(MSYS_NO_PATHCONV=1 $WSL_CMD wslpath -u "$DOCKER_COMPOSE" 2>/dev/null || echo "/mnt/d/SmartNPC_v3/docker/docker-compose.yml")
        env_wsl=$(MSYS_NO_PATHCONV=1 $WSL_CMD wslpath -u "$ENV_FILE" 2>/dev/null || echo "/mnt/d/SmartNPC_v3/.env")
    fi
    MSYS_NO_PATHCONV=1 $WSL_CMD docker compose -f "$compose_wsl" --env-file "$env_wsl" up -d --force-recreate \
        || fail "docker compose up failed"

    wait_for "Hermes Gateway (:$HERMES_PORT)" check_hermes 40

    # 等 Hermes 连接 MCP（MCP 还没起，这里只等 Hermes 自身健康）
    ok "Hermes is up"
    echo ""
}

# ─── Step 4: 启动 MCP（监听 0.0.0.0，局域网可达）────────────
start_mcp() {
    info "=== Step 4: Start agenttown-mcp (0.0.0.0:$WS_PORT + :$HTTP_PORT) ==="

    if [ ! -f "$MCP_EXE" ]; then
        fail "MCP binary not found: $MCP_EXE. Run build step first."
    fi

    mkdir -p "$LOG_SUBDIR"
    : > "$MCP_LOG"

    # 在启动前确认 Hermes 可达，否则 MCP 连不上会一直重试
    if ! check_hermes; then
        warn "Hermes not reachable on :$HERMES_PORT — MCP may fail to discover tools"
    fi

    # MCP 是 Windows exe，传给它的路径必须是 Windows 风格（D:\...）。
    # WSL 里用 wslpath -w 转换；Git Bash 里用 cygpath -w。
    # 纯 Linux 无需转换，直接用原路径。
    # cwd 也要是 Windows 路径，否则 Windows 进程看不到 assets/ 等 相对路径。
    local mcp_exe_win="$MCP_EXE"
    local world_kb_win="$PROJECT_DIR/assets/world_kb.yaml"
    local mcp_log_win="$MCP_LOG"
    local cwd_win="$PROJECT_DIR"
    if $IN_LINUX; then
        : # 纯 Linux 直接用原路径，无需转换
    elif $IN_WSL; then
        mcp_exe_win=$(wslpath -w "$MCP_EXE" 2>/dev/null) || mcp_exe_win="$MCP_EXE"
        world_kb_win=$(wslpath -w "$PROJECT_DIR/assets/world_kb.yaml" 2>/dev/null) || world_kb_win="$PROJECT_DIR/assets/world_kb.yaml"
        mcp_log_win=$(wslpath -w "$MCP_LOG" 2>/dev/null) || mcp_log_win="$MCP_LOG"
        cwd_win=$(wslpath -w "$PROJECT_DIR" 2>/dev/null) || cwd_win="$PROJECT_DIR"
    elif command -v cygpath &>/dev/null; then
        mcp_exe_win="$(cygpath -w "$MCP_EXE")"
        world_kb_win="$(cygpath -w "$PROJECT_DIR/assets/world_kb.yaml")"
        mcp_log_win="$(cygpath -w "$MCP_LOG")"
        cwd_win="$(cygpath -w "$PROJECT_DIR")"
    fi

    info "Starting MCP (log: logs/$LOG_DATE/debug-mcp.log)..."
    # --ws :9090 在 Windows 上监听 0.0.0.0:9090，局域网可达
    # --http :8760 同理
    # --llm-backend hermes 走 MCP → Hermes → Venus 架构：MCP 把战略/战术层
    #   LLM 调用发给 Hermes Gateway，由 Hermes 配置 (hermes/profiles/h01-dev/
    #   config.yaml) 决定后端模型（当前为 Venus qwen3.6-35b-a3b）。
    #   Venus client 仍保留在代码中，需要时切回 --llm-backend venus 直连即可。
    # --world-kb 用绝对路径，避免 cwd 不对找不到 assets/world_kb.yaml
    #
    # 注意：MCP 走 Hermes 时不需要 VENUS_API_KEY，Venus 凭据由 Hermes 容器
    # 通过 docker-compose 的 env_file: ../.env 读取，不透传到 MCP 进程。
    if $IN_LINUX; then
        # 纯 Linux：直接 nohup 启动 Linux 二进制，无需 .bat/cmd.exe
        nohup "$MCP_EXE" --http ":$HTTP_PORT" --ws ":$WS_PORT" \
            --llm-backend hermes --hermes-url "http://localhost:$HERMES_PORT" \
            --world-kb "$PROJECT_DIR/assets/world_kb.yaml" \
            --log-level debug >> "$MCP_LOG" 2>&1 &
        disown
    else
        # Windows/WSL：写一个 .bat 临时文件用 cmd.exe 启动，
        # 避免在 bash 里嵌套 cmd.exe /C 时的多层引号转义问题（反斜杠+引号
        # 在 bash 双引号里会被部分解释，导致路径破损）。
        local bat_file="$LOG_SUBDIR/start_mcp.bat"
        cat > "$bat_file" << EOF
@echo off
pushd "$cwd_win"
"$mcp_exe_win" --http ":$HTTP_PORT" --ws ":$WS_PORT" --llm-backend hermes --hermes-url "http://localhost:$HERMES_PORT" --world-kb "$world_kb_win" --log-level debug >> "$mcp_log_win" 2>&1
EOF
        if $IN_WSL; then
            local bat_win
            bat_win=$(wslpath -w "$bat_file" 2>/dev/null) || bat_win="$bat_file"
            MSYS_NO_PATHCONV=1 cmd.exe /C "$bat_win" >/dev/null 2>&1 &
        else
            MSYS_NO_PATHCONV=1 cmd.exe /C "$(cygpath -w "$bat_file" 2>/dev/null || echo "$bat_file")" >/dev/null 2>&1 &
        fi
    fi

    wait_for "MCP HTTP (:$HTTP_PORT)" check_mcp_http 20
    wait_for "MCP WS (:$WS_PORT)" check_mcp_ws 10

    ok "MCP is up (listening on 0.0.0.0)"
    echo ""
}

# ─── Step 5: 打印联调信息 ─────────────────────────────────────
print_summary() {
    info "=== Step 5: Summary ==="

    local lan_ip
    lan_ip=$(detect_lan_ip)

    echo ""
    echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${GREEN}  AgentTown_v3 — UE 联调就绪${NC}"
    echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
    echo ""
    echo -e "  ${BOLD}UE 端连接地址${NC}"
    echo -e "    WebSocket:  ${CYAN}ws://$lan_ip:$WS_PORT/ws${NC}"
    echo -e "    HTTP (MCP): ${CYAN}http://$lan_ip:$HTTP_PORT${NC}  (可选，MCP 工具调用)"
    echo ""
    echo -e "  ${BOLD}本机服务${NC}"
    echo -e "    MCP:        0.0.0.0:$WS_PORT (WS) + :$HTTP_PORT (HTTP)"
    echo -e "    Hermes:     localhost:$HERMES_PORT"
    echo -e "    Adapter:    localhost:$ADAPTER_PORT"
    echo ""
    echo -e "  ${BOLD}日志${NC}"
    local log_rel="${LOG_SUBDIR#$PROJECT_DIR/}"
    echo -e "    MCP:     $log_rel/debug-mcp.log"
    if $START_ADAPTER; then
        echo -e "    Adapter: $log_rel/debug-adapter.log"
    fi
    if $IN_LINUX; then
        echo -e "    Hermes:  $log_rel/debug-hermes.log"
    else
        echo -e "    Hermes:  wsl docker logs -f $HERMES_CONTAINER"
    fi
    echo ""
    echo -e "  ${BOLD}协议文档${NC}"
    echo -e "    docs/AgentTown_CommProtocol_Values.md"
    echo -e "    docs/AgentTown_Core_DeepDive.md"
    echo ""
    echo -e "  ${YELLOW}注意${NC}"
    if $IN_LINUX; then
        echo -e "    1. 确保防火墙放行 :$WS_PORT 端口："
        echo -e "       sudo ufw allow $WS_PORT/tcp  (或 firewalld: sudo firewall-cmd --add-port=$WS_PORT/tcp --permanent)"
        echo -e "    2. 本脚本未启动 Mock UE，UE 同事自己连 WS 发感知即可"
        echo -e "    3. 停止服务：bash start-dev.sh --stop 或 kill 占用端口的进程"
    else
        echo -e "    1. 确保 Windows 防火墙放行 :$WS_PORT 端口（管理员 PowerShell）："
        echo -e "       New-NetFirewallRule -DisplayName \"AgentTown WS\" -Direction Inbound -LocalPort $WS_PORT -Protocol TCP -Action Allow"
        echo -e "    2. 本脚本未启动 Mock UE，UE 同事自己连 WS 发感知即可"
        echo -e "    3. 停止服务：bash start-debug.sh --stop 或手动 taskkill"
    fi
    echo -e ""
    echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
}

# ─── --stop 选项 ──────────────────────────────────────────────
# stop_all 内部用 START_HERMES / START_ADAPTER 控制 是否停该组件，
# 因此 --stop 默认停全部；--stop --no-hermes 则只跳过 Hermes，以此类推。
if $STOP_ONLY; then
    stop_all
    ok "All services stopped."
    exit 0
fi

# ─── 主流程 ────────────────────────────────────────────────────
info "AgentTown_v3 — UE Debug Startup"
if $IN_WSL; then
    warn "检测到在 WSL 里运行。建议从 Git Bash 运行本脚本："
    warn "  1. 打开 Git Bash（不是 WSL）"
    warn "  2. cd /d/SmartNPC_v3 && bash start-debug.sh"
    warn "  WSL 里跑也能用，但 Windows 进程管理更麻烦。"
    warn "  继续用 WSL 模式（WIN_HOST=$WIN_HOST）..."
fi
echo ""

mkdir -p "$LOG_SUBDIR"

stop_all
build_mcp
$START_ADAPTER && start_adapter
$START_HERMES  && start_hermes
start_mcp
print_summary
