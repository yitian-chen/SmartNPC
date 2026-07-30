#!/usr/bin/env bash
# Build the Hermes Docker image from the local Hermes source repo.
# Run this once (or after `git pull` in the Hermes source) before
# starting docker-compose.
#
# Prerequisites:
#   - Hermes source repo cloned at the path below (or set HERMES_SOURCE)
#   - Docker running in WSL
#
# Usage:
#   bash docker/build-hermes.sh

set -euo pipefail

# Hermes source repo path (read-only dependency for `docker build`).
# Default points to /data/workspace/hermes-agent — the convention used in
# AnyDev/remote Linux setups where the source is cloned alongside the
# project repos (dev/stable). Override with HERMES_SOURCE if your layout
# differs (e.g. WSL+Windows: /mnt/c/Users/.../hermes-agent).
HERMES_SOURCE="${HERMES_SOURCE:-/data/workspace/hermes-agent}"

if [ ! -f "$HERMES_SOURCE/Dockerfile" ]; then
    echo "ERROR: Hermes source not found at $HERMES_SOURCE"
    echo "Set HERMES_SOURCE to your Hermes repo path."
    exit 1
fi

cd "$HERMES_SOURCE"
echo "[build] Building Hermes Docker image from $HERMES_SOURCE ..."
docker build -t hermes-agent:latest .
echo "[build] Done. Verify: docker run --rm hermes-agent:latest hermes --version"
