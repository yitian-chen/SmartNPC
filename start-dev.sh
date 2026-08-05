#!/usr/bin/env bash
# AgentTown_v3 — 开发实例启动 wrapper（供 dev 目录使用）
#
# 启动开发实例（本目录 /data/workspace/dev，跑 dev-working 分支，偏移端口
# 8770/9091）。与 stable 目录的实例（/data/workspace/stable，跑 master
# 分支，默认端口 8760/9090）端口隔离，可同时运行。
#
# 端口分配（dev 偏移端口）：
#   MCP WS    9091
#   MCP HTTP  8770
#   Adapter   8771
#   CLI       52002
#
# LLM 后端：MCP 直连 Venus（OpenAI Chat Completions 协议），
# VENUS_API_KEY 从 .env 读取，由 start-debug.sh 透传给 MCP 进程。
#
# 用法：
#   bash start-dev.sh                # 启动开发实例全套
#   bash start-dev.sh --stop         # 停开发实例
#   bash start-dev.sh --with-adapter # 临时启动已弃用的 Adapter

# 端口：dev 偏移端口（与 stable 默认端口 8760/9090 隔离）
export WS_PORT=9091
export HTTP_PORT=8770
export ADAPTER_PORT=8771
export CLI_PORT=52002

# 二进制名带 -dev 后缀：避免与 stable 实例共用二进制导致编译时文件锁冲突
# Linux 下不带 .exe 后缀，Windows 下保留
if grep -qi microsoft /proc/version 2>/dev/null || command -v cmd.exe >/dev/null 2>&1; then
    export MCP_EXE_NAME=agenttown-mcp-dev.exe
else
    export MCP_EXE_NAME=agenttown-mcp-dev
fi

# 日志目录用 logs-dev/（dev 实例），与 stable 的 logs/ 隔离
LOG_DATE=$(date +%Y-%m-%d)
export LOG_SUBDIR="$PWD/logs-dev/$LOG_DATE"

# Adapter 已弃用（MCP 直连 Venus），start-debug.sh 默认不启动 adapter。
# 如需临时调试 adapter，改用 --with-adapter。
exec bash start-debug.sh "$@"
