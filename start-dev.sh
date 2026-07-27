#!/usr/bin/env bash
# AgentTown_v3 — 开发实例启动 wrapper
#
# 在不干扰稳定实例（d:/SmartNPC_v3-stable，跑 master，默认端口）的前提下，
# 启动开发实例（本目录，跑开发分支，偏移端口）。
#
# 端口分配：
#   MCP WS    9091 (稳定 9090)
#   MCP HTTP  8770 (稳定 8760)
#   Hermes    8643 (稳定 8642)
#   Adapter   8771 (稳定 8761)
#   CLI       52002 (稳定 52001)
#
# 用法：
#   bash start-dev.sh                # 启动开发实例全套
#   bash start-dev.sh --stop         # 停开发实例
#   bash start-dev.sh --no-rebuild   # 跳过 Hermes 镜像重建
#   bash start-dev.sh --no-hermes    # 跳过 Hermes（已手动启动时用）
#   bash start-dev.sh --no-adapter   # 跳过 Adapter（已手动启动时用）

# 端口偏移
export WS_PORT=9091
export HTTP_PORT=8770
export HERMES_PORT=8643
export ADAPTER_PORT=8771
export CLI_PORT=52002

# 开发专用 Docker compose 和 Hermes profile
export DOCKER_COMPOSE="$PWD/docker/docker-compose-dev.yml"
export HERMES_CONTAINER=agenttown-h01-dev

# 日志目录加 -dev 后缀，与稳定实例隔离
LOG_DATE=$(date +%Y-%m-%d)
export LOG_SUBDIR="$PWD/logs-dev/$LOG_DATE"

exec bash start-debug.sh "$@"
