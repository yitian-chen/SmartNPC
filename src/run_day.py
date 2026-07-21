#!/usr/bin/env python3
"""
Run a full game day simulation for H-01 老陈.

The Mock UE connects to agenttown-mcp via WebSocket, pushes perception
snapshots, and handles tool calls. MCP forwards perception to Hermes
Gateway (HTTP) and routes Hermes tool calls back to Mock UE.

Prerequisites:
  1. Hermes Gateway running: docker compose -f docker/docker-compose.yml up -d
  2. agenttown-mcp running:  agenttown-mcp --http :8760 --ws :9090

Usage:
    python src/run_day.py
    python src/run_day.py --speed 120 --scenario scenarios/test_day.yaml
"""

import argparse
import asyncio
import os
import sys

# Add project root to path
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from agenttown.mock_ue import MockUE


async def _run(args):
    mock = MockUE(
        mcp_ws_url=args.mcp_ws,
        mode=args.mode,
        time_speed=args.speed,
        perception_interval=args.interval,
        scenario_file=args.scenario,
        log_dir=args.log_dir,
        world_kb_path=args.world_kb,
    )
    try:
        await mock.run_day(start_hour=args.start, end_hour=args.end)
    except KeyboardInterrupt:
        print("\n[STOP] Simulation interrupted.")
    finally:
        mock.print_report()


def main():
    parser = argparse.ArgumentParser(description="AgentTown Mock UE — Run a game day")
    parser.add_argument("--mcp-ws", default="ws://localhost:9090/ws",
                        help="agenttown-mcp WebSocket URL")
    parser.add_argument("--mode", choices=("normal", "behavior", "quick-smoke"), default="normal",
                        help="simulation mode label (parameters are supplied separately)")
    parser.add_argument("--speed", type=float, default=60.0,
                        help="Time acceleration (1 real-sec = N game-sec)")
    parser.add_argument("--start", type=int, default=6,
                        help="Start hour (0-23)")
    parser.add_argument("--end", type=int, default=22,
                        help="End hour (0-23)")
    parser.add_argument("--scenario", default=None,
                        help="YAML file with preset events (scenario injection)")
    parser.add_argument("--interval", type=int, default=15,
                        help="Perception push interval (game-minutes)")
    parser.add_argument("--log-dir", default="logs",
                        help="Directory for Mock UE day logs "
                             "(start.sh passes logs/YYYY-MM-DD)")
    parser.add_argument("--world-kb", default="assets/world_kb.yaml",
                        help="Path to world_kb.yaml (single source of truth for "
                             "zone/location/object data, shared with agenttown-mcp)")

    args = parser.parse_args()

    # Health check: can we reach the MCP WS endpoint's HTTP health?
    import urllib.request
    health_url = args.mcp_ws.replace("/ws", "/healthz").replace("ws://", "http://")
    try:
        r = urllib.request.urlopen(health_url, timeout=3)
        if r.status != 200:
            raise RuntimeError(f"status {r.status}")
        print(f"[OK] MCP reachable: {health_url}")
    except Exception as e:
        print(f"[ERROR] MCP not reachable at {health_url}: {e}")
        print("Start MCP first:  agenttown-mcp --http :8760 --ws :9090")
        sys.exit(1)

    asyncio.run(_run(args))


if __name__ == "__main__":
    main()
