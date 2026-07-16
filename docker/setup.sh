#!/usr/bin/env bash
# AgentTown_v3 — 一期 Docker 一键部署
#
# 用法（在 WSL 中执行）：
#   bash docker/setup.sh
#
# 做了什么：
#   1. 构建 Hermes Docker 镜像（如果还没构建）
#   2. 启动 H-01 Gateway 容器
#   3. 等待 Gateway 就绪
#   4. 打印健康检查结果

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

cd "$PROJECT_ROOT"

# ─── 1. Build Hermes image（如果不存在）─────────────────────────
if ! docker image inspect hermes-agent:latest >/dev/null 2>&1; then
    echo "[setup] Hermes image not found, building..."
    bash docker/build-hermes.sh
    echo "[setup] Hermes image built."
else
    echo "[setup] Hermes image found, skip build."
fi

# ─── 2. Start container ─────────────────────────────────────────
echo "[setup] Starting H-01 Gateway..."
docker compose -f docker/docker-compose.yml up -d

# ─── 3. Wait for healthy ────────────────────────────────────────
echo "[setup] Waiting for gateway to be ready..."
for i in $(seq 1 30); do
    if curl -sf http://localhost:${HERMES_PORT:-8642}/health >/dev/null 2>&1; then
        echo "[setup] Gateway is healthy."
        break
    fi
    if [ "$i" = 30 ]; then
        echo "[setup] WARNING: Gateway did not become healthy within 30s."
        echo "[setup] Check logs: docker compose -f docker/docker-compose.yml logs"
        exit 1
    fi
    sleep 2
done

# ─── 4. Summary ─────────────────────────────────────────────────
echo ""
echo "=============================================="
echo "  AgentTown H-01 Gateway 已启动"
echo "  REST API: http://localhost:${HERMES_PORT:-8642}"
echo "  健康检查: curl http://localhost:${HERMES_PORT:-8642}/health"
echo ""
echo "  查看日志: docker compose -f docker/docker-compose.yml logs -f"
echo "  停止:     docker compose -f docker/docker-compose.yml down"
echo "=============================================="
