#!/usr/bin/env python3
"""
Run a full game day simulation for H-01 老陈.

The Mock UE drives the Hermes Gateway through perception → decision → action loop.
Hermes Gateway must be running: docker compose up -d

Usage:
    python src/run_day.py
    python src/run_day.py --speed 120 --scenario scenarios/test_day.yaml
"""

import argparse
import sys
import os

# Add project root to path
sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from agenttown.mock_ue import MockUE


def main():
    parser = argparse.ArgumentParser(description="AgentTown Mock UE — Run a game day")
    parser.add_argument("--gateway", default="http://localhost:8642",
                        help="Hermes Gateway URL")
    parser.add_argument("--api-key", default="agenttown-test-key",
                        help="Gateway API key")
    parser.add_argument("--model", default="deepseek-v4-flash",
                        help="LLM model name")
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

    args = parser.parse_args()

    # Health check
    import requests
    try:
        r = requests.get(f"{args.gateway}/health", timeout=5)
        r.raise_for_status()
        print(f"[OK] Gateway healthy: {r.json()}")
    except requests.RequestException as e:
        print(f"[ERROR] Gateway not reachable at {args.gateway}: {e}")
        print("Start with: docker compose -f docker/docker-compose.yml up -d")
        sys.exit(1)

    # Create and run Mock UE
    mock = MockUE(
        gateway_url=args.gateway,
        api_key=args.api_key,
        model=args.model,
        time_speed=args.speed,
        perception_interval=args.interval,
        scenario_file=args.scenario,
        log_dir="logs",
    )

    try:
        mock.run_day(start_hour=args.start, end_hour=args.end)
    except KeyboardInterrupt:
        print("\n[STOP] Simulation interrupted.")
    finally:
        mock.print_report()


if __name__ == "__main__":
    main()
