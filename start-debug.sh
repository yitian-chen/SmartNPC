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
#
# LLM 后端：MCP 直连 Venus（OpenAI Chat Completions 协议），
# 凭据 VENUS_API_KEY 从 .env 读取，启动时透传给 MCP 进程。
#
# 用法：
#   bash start-debug.sh                # 启动
#   bash start-debug.sh --stop         # 仅停止所有服务
#
# 前置：
#   - Go 编译器可访问（PATH 中有 go，或设置 GO_BIN）
#   - d:/SmartNPC_v3/.env 存在且配置了 VENUS_API_KEY

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
ENV_FILE="$PROJECT_DIR/.env"
WS_PORT="${WS_PORT:-9090}"
HTTP_PORT="${HTTP_PORT:-8760}"

LOG_DATE=$(date +%Y-%m-%d)
LOG_SUBDIR_BASE="$PROJECT_DIR/logs/$LOG_DATE"
LOG_SUBDIR="${LOG_SUBDIR:-$LOG_SUBDIR_BASE}"
MCP_LOG="$LOG_SUBDIR/debug-mcp.log"

# ─── 加载 .env 到 shell 环境 ──────────────────────────────────
# MCP 是 Windows exe，通过 .bat 启动；bash export 不会自动传给 cmd.exe，
# 因此需要在生成 .bat 时显式 set 环境变量。这里先把 .env 的变量 source
# 到当前 shell，后续 generate .bat 时再注入。
# 架构是 MCP 直连 Venus：MCP 进程需要 VENUS_API_KEY 才能调用 LLM。
if [ -f "$ENV_FILE" ]; then
    while IFS='=' read -r key value || [ -n "$key" ]; do
        # 跳过空行和注释
        case "$key" in
            ''|\#*) continue ;;
        esac
        # 只 export VENUS_ / AGENTTOWN_MCP_ 前缀变量
        case "$key" in
            VENUS_*) export "$key=$value" ;;
            AGENTTOWN_MCP_*) export "$key=$value" ;;
        esac
    done < "$ENV_FILE"
fi

# Venus URL/model 默认值（与 Go flag 默认值一致）。
VENUS_URL="${VENUS_URL:-http://v2.open.venus.oa.com/llmproxy}"
VENUS_MODEL="${VENUS_MODEL:-qwen3.6-35b-a3b}"

# ─── 参数 ──────────────────────────────────────────────────────
STOP_ONLY=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --stop)        STOP_ONLY=true; shift ;;
        # 向后兼容：旧版有 Adapter/Hermes 相关 flag，现均已废弃，识别后忽略
        --no-adapter|--with-adapter|--no-hermes|--no-rebuild) warn "$1 已废弃（Hermes/Adapter 已移除），忽略"; shift ;;
        -h|--help)
            echo "Usage: bash start-debug.sh [OPTIONS]"
            echo ""
            echo "UE 联调启动脚本：MCP 跑在 Windows 原生（监听 0.0.0.0），局域网可达。"
            echo ""
            echo "Options:"
            echo "  --stop          仅停止所有服务，不重启"
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
#   - 纯 Linux (AnyDev/远程): 无 Windows 工具，无需路径转换
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
check_mcp_http() { curl -sf http://$WIN_HOST:$HTTP_PORT/healthz >/dev/null 2>&1; }
check_mcp_ws()   { curl -sf http://$WIN_HOST:$WS_PORT/healthz >/dev/null 2>&1; }

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
    info "Stopping UE..."
    pkill -f "run_day.py" 2>/dev/null && ok "  UE stopped" || warn "  UE not running"

    # MCP（Windows exe）— 杀掉端口上所有监听者（含 WSL wslrelay 幽灵）
    info "Stopping existing MCP..."
    local port
    for port in $WS_PORT $HTTP_PORT; do
        kill_port_listeners "$port" "MCP"
    done

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

    ok "MCP binary built: $MCP_EXE"
    echo ""
}

# ─── Step 2: 启动 MCP（监听 0.0.0.0，局域网可达）────────────
start_mcp() {
    info "=== Step 2: Start agenttown-mcp (0.0.0.0:$WS_PORT + :$HTTP_PORT) ==="

    if [ ! -f "$MCP_EXE" ]; then
        fail "MCP binary not found: $MCP_EXE. Run build step first."
    fi

    # 从 .env 读取 VENUS_API_KEY（MCP 直连 Venus 必需）
    local venus_key=""
    if [ -f "$ENV_FILE" ]; then
        if grep -q "^VENUS_API_KEY=" "$ENV_FILE" 2>/dev/null; then
            venus_key=$(grep "^VENUS_API_KEY=" "$ENV_FILE" | cut -d= -f2-)
        fi
    fi
    if [ -z "$venus_key" ]; then
        fail "VENUS_API_KEY not found in $ENV_FILE. MCP 直连 Venus 必需此凭据。"
    fi

    mkdir -p "$LOG_SUBDIR"
    : > "$MCP_LOG"

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
    # MCP 直连 Venus（OpenAI Chat Completions 协议），--venus-api-key 透传凭据。
    # --world-kb 用绝对路径，避免 cwd 不对找不到 assets/world_kb.yaml
    if $IN_LINUX; then
        # 纯 Linux：直接 nohup 启动 Linux 二进制，无需 .bat/cmd.exe
        # --auto-plan 从 .env 的 AGENTTOWN_MCP_AUTO_PLAN 读取（默认 true）
        nohup "$MCP_EXE" --http ":$HTTP_PORT" --ws ":$WS_PORT" \
            --venus-api-key "$venus_key" \
            --world-kb "$PROJECT_DIR/assets/world_kb.yaml" \
            --auto-plan "${AGENTTOWN_MCP_AUTO_PLAN:-true}" \
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
"$mcp_exe_win" --http ":$HTTP_PORT" --ws ":$WS_PORT" --venus-api-key "$venus_key" --world-kb "$world_kb_win" --auto-plan "${AGENTTOWN_MCP_AUTO_PLAN:-true}" --log-level debug >> "$mcp_log_win" 2>&1
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

# ─── Step 3: 打印联调信息 ─────────────────────────────────────
print_summary() {
    info "=== Step 3: Summary ==="

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
    echo -e "    LLM 后端:   Venus 直连（$VENUS_URL, model=$VENUS_MODEL）"
    echo ""
    echo -e "  ${BOLD}日志${NC}"
    local log_rel="${LOG_SUBDIR#$PROJECT_DIR/}"
    echo -e "    MCP:     $log_rel/debug-mcp.log"
    echo ""
    echo -e "  ${BOLD}协议文档${NC}"
    echo -e "    docs/AgentTown_CommProtocol_Values.md"
    echo -e "    docs/AgentTown_Core_DeepDive.md"
    echo ""
    echo -e "  ${YELLOW}注意${NC}"
    if $IN_LINUX; then
        echo -e "    1. 确保防火墙放行 :$WS_PORT 端口："
        echo -e "       sudo ufw allow $WS_PORT/tcp  (或 firewalld: sudo firewall-cmd --add-port=$WS_PORT/tcp --permanent)"
        echo -e "    2. 本脚本未启动 UE，UE 同事自己连 WS 发感知即可"
        echo -e "    3. 停止服务：bash start-dev.sh --stop 或 kill 占用端口的进程"
    else
        echo -e "    1. 确保 Windows 防火墙放行 :$WS_PORT 端口（管理员 PowerShell）："
        echo -e "       New-NetFirewallRule -DisplayName \"AgentTown WS\" -Direction Inbound -LocalPort $WS_PORT -Protocol TCP -Action Allow"
        echo -e "    2. 本脚本未启动 UE，UE 同事自己连 WS 发感知即可"
        echo -e "    3. 停止服务：bash start-debug.sh --stop 或手动 taskkill"
    fi
    echo -e ""
    echo -e "${BOLD}${GREEN}═══════════════════════════════════════════════════════════════${NC}"
}

# ─── --stop 选项 ──────────────────────────────────────────────
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
start_mcp
print_summary
