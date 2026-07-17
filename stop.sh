#!/usr/bin/env bash
# AgentTown_v3 — 停止所有组件
#
# 用法：bash stop.sh

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info()  { echo -e "${CYAN}[INFO]${NC} $*"; }
ok()    { echo -e "${GREEN}[OK]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"

# 1. 停止 Mock UE（Python 进程）
info "Stopping Mock UE..."
pkill -f "run_day.py" 2>/dev/null && ok "Mock UE stopped" || warn "Mock UE not running"

# 2. 停止 MCP（WSL 中的 Go 进程）
info "Stopping agenttown-mcp..."
MCP_PID=$(wsl bash -c "pgrep -f 'agenttown-mcp --http' 2>/dev/null | head -1" || true)
if [ -n "$MCP_PID" ]; then
    wsl bash -c "kill $MCP_PID 2>/dev/null" && ok "MCP stopped (PID $MCP_PID)"
else
    warn "MCP not running"
fi

# 3. 停止 Hermes（Docker 容器）
info "Stopping Hermes Gateway..."
wsl docker compose -f /mnt/d/SmartNPC_v3/docker/docker-compose.yml stop 2>/dev/null \
    && ok "Hermes stopped" \
    || warn "Hermes not running or already stopped"

echo ""
info "All components stopped."
info "To restart: bash start.sh"
