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

HERMES_SOURCE="${HERMES_SOURCE:-/mnt/c/Users/yitianchen/AppData/Local/hermes/hermes-agent}"

if [ ! -f "$HERMES_SOURCE/Dockerfile" ]; then
    echo "ERROR: Hermes source not found at $HERMES_SOURCE"
    echo "Set HERMES_SOURCE to your Hermes repo path."
    exit 1
fi

cd "$HERMES_SOURCE"
echo "[build] Building Hermes Docker image from $HERMES_SOURCE ..."
docker build -t hermes-agent:latest .
echo "[build] Done. Verify: docker run --rm hermes-agent:latest hermes --version"
