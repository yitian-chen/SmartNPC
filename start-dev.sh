#!/usr/bin/env bash
# AgentTown_v3 — 稳定实例启动 wrapper（供 stable 目录使用）
#
# 启动稳定实例（本目录，跑 master 分支，默认端口）。与 dev 目录的实例
# （/data/workspace/dev，跑 dev-working 分支，偏移端口 8770/9091/8643）
# 端口隔离，可同时运行。
#
# 端口分配（stable 默认）：
#   MCP WS    9090
#   MCP HTTP  8760
#   Hermes    8642
#   Adapter   8761
#   CLI       52001
#
# 云环境（AnyDev/纯 Linux）下 Hermes 裸金属运行，profile 用 h01-dev
# （直连 Venus，不依赖 CodeBuddy Adapter），因此默认带 --no-adapter。
# 本地 Windows+WSL 环境下若需用 h01 profile + Adapter，请改用 start.sh。
#
# 用法：
#   bash start-dev.sh                # 启动稳定实例全套
#   bash start-dev.sh --stop         # 停稳定实例
#   bash start-dev.sh --no-rebuild   # 跳过 Hermes 镜像重建
#   bash start-dev.sh --no-hermes    # 跳过 Hermes（已手动启动时用）
#   bash start-dev.sh --no-adapter   # 跳过 Adapter（已手动启动时用，默认已带）

# 端口：stable 默认端口（与 dev 偏移端口 8770/9091/8643 隔离）
export WS_PORT=9090
export HTTP_PORT=8760
export HERMES_PORT=8642
export ADAPTER_PORT=8761
export CLI_PORT=52001

# 二进制名带 -stable 后缀：避免与 dev 实例共用二进制导致编译时文件锁冲突
# Linux 下不带 .exe 后缀，Windows 下保留
if grep -qi microsoft /proc/version 2>/dev/null || command -v cmd.exe >/dev/null 2>&1; then
    export MCP_EXE_NAME=agenttown-mcp-stable.exe
else
    export MCP_EXE_NAME=agenttown-mcp-stable
fi

# Docker compose 和 Hermes 容器名带 -stable 后缀，与 dev 实例隔离
# 注意：云环境裸金属跑 Hermes 时，profile 由 start-debug.sh 读 $HERMES_PROFILE
#       stable 用 h01（直连 Venus，stable 端口 8642/8760），dev 用 h01-dev
export DOCKER_COMPOSE="$PWD/docker/docker-compose.yml"
export HERMES_CONTAINER=agenttown-h01-stable
export HERMES_PROFILE=h01

# 日志目录用 logs/（stable 默认），与 dev 的 logs-dev/ 隔离
LOG_DATE=$(date +%Y-%m-%d)
export LOG_SUBDIR="$PWD/logs/$LOG_DATE"

# h01-dev profile 直连 Venus（provider: custom:venus），不依赖 CodeBuddy Adapter。
# 默认带 --no-adapter 跳过适配层；用户额外传参追加在后（重复 --no-adapter 无害）。
exec bash start-debug.sh --no-adapter "$@"
