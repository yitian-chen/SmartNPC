"""Analyze game time vs real time progression in sim.log.

Extracts `time_of_day` from perception_update events' payload and compares
against real timestamps to detect uneven time flow.
"""
import json
import re
import sys
from datetime import datetime

log_path = sys.argv[1] if len(sys.argv) > 1 else "d:/SmartNPC_v3/logs/2026-07-22/sim.log"
start_line = int(sys.argv[2]) if len(sys.argv) > 2 else 1

with open(log_path, encoding="utf-8") as f:
    lines = f.readlines()

events = []
for line in lines[start_line - 1:]:
    line = line.strip()
    if not line:
        continue
    try:
        r = json.loads(line)
    except Exception:
        continue
    msg = r.get("msg", "")
    t = r.get("time", "")
    # perception_update from UE, or [MCP→Hermes/PERCEPTION] (carries time_of_day in text)
    if msg == "[UE→MCP]" and r.get("type") == "perception_update":
        try:
            payload = json.loads(r["payload"]) if isinstance(r.get("payload"), str) else r.get("payload", {})
            tod = payload.get("environment", {}).get("time_of_day")
            if tod:
                events.append((t, tod, "perception"))
        except Exception:
            pass
    elif "[MCP→Hermes/PERCEPTION]" in msg:
        # text contains "时间06:00"
        m = re.search(r"时间(\d{2}:\d{2})", msg)
        if m:
            events.append((t, m.group(1), "decision"))

print(f"Total perception events with time_of_day: {len(events)}")
print()
print(f"{'#':>3} {'real_time':<32} {'game_t':<8} {'real_gap_s':>10} {'game_gap_m':>10} {'ratio':>8}  type")
print("-" * 100)

prev_real = None
prev_game_min = None
ratios = []

for i, (t, game_t, etype) in enumerate(events):
    try:
        gh, gm = map(int, game_t.split(":"))
    except Exception:
        continue
    game_total_min = gh * 60 + gm
    try:
        real_t = datetime.fromisoformat(t.replace("Z", "+00:00"))
    except Exception:
        continue

    if prev_real is not None and prev_game_min is not None:
        real_gap = (real_t - prev_real).total_seconds()
        game_gap = game_total_min - prev_game_min
        if game_gap < 0:
            game_gap += 24 * 60
        ratio = (game_gap * 60 / real_gap) if real_gap > 0 else 0
        ratios.append(ratio)
        marker = "  <<<" if (real_gap > 3.0 and abs(ratio - 300) > 100) else ""
        print(f"{i+1:>3} {t[:23]:<32} {game_t:<8} {real_gap:>10.2f} {game_gap:>10} {ratio:>8.1f}  {etype}{marker}")
    else:
        print(f"{i+1:>3} {t[:23]:<32} {game_t:<8} {'-':>10} {'-':>10} {'-':>8}  {etype}")

    prev_real = real_t
    prev_game_min = game_total_min

if ratios:
    print()
    print(f"Ratio stats: min={min(ratios):.1f} max={max(ratios):.1f} avg={sum(ratios)/len(ratios):.1f}")
    print(f"Expected ratio: 300.0 (speed=300x)")
    outliers = [r for r in ratios if abs(r - 300) > 100]
    print(f"Outliers (|ratio-300|>100): {len(outliers)} / {len(ratios)}")
