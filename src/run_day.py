#!/usr/bin/env python3
"""
Run a full game day simulation for H-01 老陈.

The Mock UE connects to agenttown-mcp via WebSocket, pushes perception
snapshots, and handles tool calls. MCP forwards perception to Hermes
Gateway (HTTP) and routes Hermes tool calls back to Mock UE.

Prerequisites:
  1. Hermes Gateway running: docker compose -f docker/docker-compose.yml up -d
  2. agenttown-mcp running:  agenttown-mcp --http :8770 --ws :9091

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


def _clear_mcp_logs(log_dir: str) -> None:
    """Truncate MCP 日志文件，确保每次仿真从干净日志开始。

    覆盖 log_dir 下的 debug-mcp.log（dev）和 sim.log（stable）。
    用 'w' 模式打开即 truncate（文件存在则清空内容，不存在则忽略）。

    安全性：dev 实例 MCP 是 Windows exe + >> 追加，truncate 后 O_APPEND
    会自动 seek to end(=0)，下次 write 从头开始，无 sparse 风险。
    """
    for name in ("debug-mcp.log", "sim.log"):
        path = os.path.join(log_dir, name)
        if os.path.exists(path) and os.path.getsize(path) > 0:
            prev_size = os.path.getsize(path)
            with open(path, "w"):
                pass  # truncate to 0
            print(f"[OK] 已清空 MCP 日志: {path} (was {prev_size} bytes)")


def main():
    parser = argparse.ArgumentParser(description="AgentTown UE — Run a game day")
    parser.add_argument("--mcp-ws", default="ws://localhost:9091/ws",
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
                        help="Directory for UE day logs "
                             "(start.sh passes logs/YYYY-MM-DD)")
    parser.add_argument("--no-clear-log", action="store_true",
                        help="不清空 MCP 日志文件（WSL stable 场景用："
                             "WSL MCP 的 O_APPEND fd 跨 drvfs truncate 会产生 sparse nul）")
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
        print("Start MCP first:  agenttown-mcp --http :8770 --ws :9091")
        sys.exit(1)

    # 清空 MCP 日志文件，确保每次仿真从干净日志开始。
    #
    # dev 实例：MCP 是 Windows exe，用 >> 追加重定向。Windows 的 O_APPEND
    # 每次 write 前 seek to end，truncate 后 end=0，下次 write 从头开始 → 安全。
    #
    # stable 实例：MCP 在 WSL 跑，通过 drvfs 访问 Windows 文件。truncate 后
    # WSL 侧 fd 偏移不重置，下次 write 在旧偏移处写入，前面变成 sparse \0
    # （巨量 nul 问题根因）。stable 用 --no-clear-log 规避，清空交给 start.sh
    # 在启动 MCP 之前同侧完成。
    if not args.no_clear_log:
        _clear_mcp_logs(args.log_dir)

    asyncio.run(_run(args))


if __name__ == "__main__":
    main()
