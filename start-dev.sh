#!/usr/bin/env bash
# AgentTown_v3 — 开发实例启动 wrapper（供 dev 目录使用）
#
# 启动开发实例（本目录 /data/workspace/dev，跑 dev-working 分支，偏移端口
# 8770/9091/8643）。与 stable 目录的实例（/data/workspace/stable，跑 master
# 分支，默认端口 8760/9090/8642）端口隔离，可同时运行。
#
# 端口分配（dev 偏移端口）：
#   MCP WS    9091
#   MCP HTTP  8770
#   Hermes    8643
#   Adapter   8771
#   CLI       52002
#
# 云环境（AnyDev/纯 Linux）下 Hermes 裸金属运行，profile 用 h01-dev
# （直连 Venus，不依赖已弃用的 CodeBuddy Adapter）。
# 本地 Windows+WSL 环境下若需用 h01-dev profile + Adapter，请改用 start.sh。
#
# 用法：
#   bash start-dev.sh                # 启动开发实例全套
#   bash start-dev.sh --stop         # 停开发实例
#   bash start-dev.sh --no-rebuild   # 跳过 Hermes 镜像重建
#   bash start-dev.sh --no-hermes    # 跳过 Hermes（已手动启动时用）
#   bash start-dev.sh --with-adapter # 临时启动已弃用的 Adapter

# 端口：dev 偏移端口（与 stable 默认端口 8760/9090/8642 隔离）
export WS_PORT=9091
export HTTP_PORT=8770
export HERMES_PORT=8643
export ADAPTER_PORT=8771
export CLI_PORT=52002

# 二进制名带 -dev 后缀：避免与 stable 实例共用二进制导致编译时文件锁冲突
# Linux 下不带 .exe 后缀，Windows 下保留
if grep -qi microsoft /proc/version 2>/dev/null || command -v cmd.exe >/dev/null 2>&1; then
    export MCP_EXE_NAME=agenttown-mcp-dev.exe
else
    export MCP_EXE_NAME=agenttown-mcp-dev
fi

# Docker compose 和 Hermes 容器名带 -dev 后缀，与 stable 实例隔离
# 注意：云环境裸金属跑 Hermes 时，profile 由 start-debug.sh 读 $HERMES_PROFILE
#       stable 用 h01（直连 Venus，stable 端口 8642/8760），dev 用 h01-dev
export DOCKER_COMPOSE="$PWD/docker/docker-compose-dev.yml"
export HERMES_CONTAINER=agenttown-h01-dev
export HERMES_PROFILE=h01-dev

# 日志目录用 logs-dev/（dev 实例），与 stable 的 logs/ 隔离
LOG_DATE=$(date +%Y-%m-%d)
export LOG_SUBDIR="$PWD/logs-dev/$LOG_DATE"

# Adapter 已弃用（h01-dev 直连 Venus），start-debug.sh 默认不启动 adapter。
# 这里仍传 --no-adapter 保持向后兼容；如需临时调试 adapter，改用 --with-adapter。
exec bash start-debug.sh "$@"
