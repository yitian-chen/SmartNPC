"""
Mock UE Bridge — M-5 of Phase 1 work checklist.

Simulates the UE game world for a single NPC (H-01 老陈).
Drives the Hermes Gateway through the full perception → decision → action loop.

Tasks covered:
  5.1  Mock message receiver    — parse Hermes responses, extract actions
  5.2  Mock execution timing    — simulate action duration by type
  5.3  Mock perception push     — timed perception_update to Gateway
  5.4  Zone simulation          — auto-detect zone from coordinates
  5.5  Time acceleration        — configurable game-time speed
  5.6  Scenario injection       — load preset events from YAML
"""

import json
import os
import time
import uuid
import logging
from dataclasses import dataclass, field
from datetime import datetime, timedelta
from typing import Any, Dict, List, Optional, Tuple

import requests
import yaml

logger = logging.getLogger("agenttown.mock_ue")

# ─── Constants ──────────────────────────────────────────────────

# Action durations in game-minutes
ACTION_DURATIONS: Dict[str, float] = {
    "move_to": 2,           # 2 game-min per zone transition
    "turn_to": 0.1,
    "work_assemble": 0,     # uses explicit duration_min param
    "work_inspect": 5,      # 5 game-min default
    "charge_at": 0,
    "self_check": 1,
    "speak": 0.5,
    "emote": 0.1,
    "wait": 0,
}

# Default zone boundaries (x_min, x_max, y_min, y_max)
ZONE_BOUNDS: Dict[str, Tuple[float, float, float, float]] = {
    "main_workshop": (150, 250, 50, 150),
    "central_plaza": (80, 150, 80, 150),
    "charging_station": (200, 250, 50, 100),
}

# Zone entry points
ZONE_ENTRIES: Dict[str, List[float]] = {
    "main_workshop": [160, 100, 0],
    "central_plaza": [100, 100, 0],
    "charging_station": [215, 85, 0],
}

# Location interaction points
LOCATION_POINTS: Dict[str, List[float]] = {
    "workbench_01": [195, 105, 0],
    "charging_station_01": [215, 85, 0],
}


@dataclass
class NPCState:
    """Tracks a single NPC's world state."""
    agent_id: str = "H-01"
    name: str = "老陈"
    position: List[float] = field(default_factory=lambda: [200, 100, 0])
    current_zone: str = "main_workshop"
    energy: float = 100.0
    fatigue: float = 0.0
    holding: Optional[str] = None
    current_action: Optional[str] = None


@dataclass
class GameTime:
    """Simulated game time with acceleration."""
    speed: float = 60.0    # 1 real-sec = 60 game-sec (1 game-min)
    day: int = 1
    hour: int = 6
    minute: int = 0

    @property
    def total_minutes(self) -> int:
        return self.hour * 60 + self.minute

    @property
    def display(self) -> str:
        return f"Day{self.day} {self.hour:02d}:{self.minute:02d}"

    def advance(self, game_minutes: float):
        total = self.total_minutes + game_minutes
        total = int(total)
        self.hour = (total // 60) % 24
        self.minute = total % 60
        if self.hour == 0 and self.minute == 0 and total > 0:
            self.day += 1

    def sleep(self, real_seconds: float):
        """Sleep real_seconds, advancing game time accordingly."""
        time.sleep(real_seconds)
        game_seconds = real_seconds * self.speed
        self.advance(game_seconds / 60.0)


class MockUE:
    """
    Mock UE Bridge — drives a single NPC through a day.

    Usage:
        mock = MockUE(gateway_url="http://localhost:8642")
        mock.run_day()
    """

    def __init__(
        self,
        gateway_url: str = "http://localhost:8642",
        api_key: str = "agenttown-test-key",
        model: str = "deepseek-v4-flash",
        time_speed: float = 60.0,       # 1 real-sec = 1 game-min
        perception_interval: int = 3,    # game-min between perception pushes
        scenario_file: Optional[str] = None,
        log_dir: str = "logs",
    ):
        self.gateway_url = gateway_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.perception_interval = perception_interval
        self.log_dir = log_dir

        # World state
        self.npc = NPCState()
        self.time = GameTime(speed=time_speed)
        self.last_perception_at = 0
        self.action_log: List[Dict] = []

        # Scenario injection
        self.scenarios: List[Dict] = []
        if scenario_file:
            self._load_scenarios(scenario_file)

        # Setup logging
        os.makedirs(log_dir, exist_ok=True)
        self._setup_logging()

    # ─── 5.1 Message Receiver ─────────────────────────────────

    def send_message(self, message: str) -> Dict[str, Any]:
        """
        POST a message to Hermes Gateway /v1/responses.
        Returns the parsed JSON response.
        """
        url = f"{self.gateway_url}/v1/responses"
        payload = {
            "model": self.model,
            "input": message,
        }
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.api_key}",
        }

        logger.debug(f"[MOCK→Hermes] {message[:100]}...")
        try:
            resp = requests.post(url, json=payload, headers=headers, timeout=120)
            resp.raise_for_status()
            data = resp.json()
            self._log_response(data)
            return data
        except requests.RequestException as e:
            logger.error(f"[MOCK→Hermes] Request failed: {e}")
            return {"error": str(e)}

    def extract_text(self, response: Dict) -> str:
        """Extract assistant text from Hermes response."""
        try:
            for item in response.get("output", []):
                if item.get("type") == "message" and item.get("role") == "assistant":
                    for content in item.get("content", []):
                        if content.get("type") == "output_text":
                            return content.get("text", "")
        except (TypeError, KeyError):
            pass
        return ""

    # ─── 5.3 Perception Push ──────────────────────────────────

    def push_perception(self) -> Optional[Dict[str, Any]]:
        """
        Push a perception snapshot to Hermes.
        Called periodically (based on perception_interval).
        """
        perception_text = self._build_perception()
        logger.info(f"[PERCEPTION] {self.time.display} | {self.npc.current_zone}")
        return self.send_message(perception_text)

    def _build_perception(self) -> str:
        """Build a natural-language perception string for the LLM."""
        t = self.time
        npc = self.npc
        nearby = self._get_nearby_objects()

        lines = [
            f"[PERCEPTION] 游戏时间: {t.display}",
            f"你的位置: {npc.current_zone} ({npc.position[0]:.0f}, {npc.position[1]:.0f})",
            f"电池: {npc.energy:.0f}% | 疲劳: {npc.fatigue:.0f}%",
        ]
        if npc.current_action:
            lines.append(f"当前动作: {npc.current_action}")
        if nearby:
            lines.append(f"附近可用: {', '.join(nearby)}")
        if npc.current_action is None:
            lines.append("你处于空闲状态。")

        # 5.6 Scenario injection
        self._inject_scenarios(lines)

        return "\n".join(lines)

    def _get_nearby_objects(self) -> List[str]:
        """Determine nearby objects based on zone."""
        zone = self.npc.current_zone
        objects = {
            "main_workshop": ["workbench_01 (工作台)", "charging_station_01 (充电桩)"],
            "central_plaza": ["充电站"],
            "charging_station": ["charging_station_01 (充电桩)"],
        }
        return objects.get(zone, [])

    # ─── 5.2 Action Execution ─────────────────────────────────

    def execute_action(self, action_cmd: str, params: dict) -> float:
        """
        Simulate action execution. Returns game-minutes consumed.

        Moves NPC position, updates zone, consumes energy.
        """
        duration = ACTION_DURATIONS.get(action_cmd, 1.0)

        if action_cmd == "move_to":
            target = params.get("target", "")
            self._simulate_move(target)
        elif action_cmd == "work_assemble":
            duration = params.get("duration_min", 30)
            self.npc.fatigue = min(100, self.npc.fatigue + duration * 0.3)
        elif action_cmd == "charge_at":
            duration = params.get("duration_min", 30)
            self.npc.energy = min(100, self.npc.energy + duration * 2)
        elif action_cmd == "self_check":
            pass  # just consumes time
        elif action_cmd == "speak":
            pass
        elif action_cmd == "emote":
            pass
        elif action_cmd == "wait":
            duration = params.get("seconds", 5) / 60.0

        # Universal energy consumption
        self.npc.energy = max(0, self.npc.energy - duration * 0.1)
        self.npc.current_action = f"{action_cmd}({params.get('target', '')})"

        # Log
        entry = {
            "time": self.time.display,
            "action": action_cmd,
            "params": params,
            "duration_game_min": duration,
            "result": "success",
        }
        self.action_log.append(entry)
        logger.info(
            f"[ACTION] {self.time.display} | {action_cmd} | "
            f"{duration:.0f}min | zone={self.npc.current_zone}"
        )
        return duration

    def _simulate_move(self, target: str):
        """Move NPC to a zone entry or location point."""
        dest = ZONE_ENTRIES.get(target) or LOCATION_POINTS.get(target)
        if dest:
            self.npc.position = list(dest)
            new_zone = self._resolve_zone(self.npc.position)
            if new_zone != self.npc.current_zone:
                old = self.npc.current_zone
                self.npc.current_zone = new_zone
                logger.info(f"[ZONE] {old} → {new_zone}")

    # ─── 5.4 Zone Resolution ──────────────────────────────────

    def _resolve_zone(self, pos: List[float]) -> str:
        """Find which zone contains the given position."""
        x, y = pos[0], pos[1]
        for name, (xmin, xmax, ymin, ymax) in ZONE_BOUNDS.items():
            if xmin <= x <= xmax and ymin <= y <= ymax:
                return name
        return self.npc.current_zone

    # ─── 5.6 Scenario Injection ───────────────────────────────

    def _load_scenarios(self, filepath: str):
        """Load preset events from YAML."""
        with open(filepath, "r", encoding="utf-8") as f:
            data = yaml.safe_load(f) or {}
        self.scenarios = data.get("events", [])
        logger.info(f"Loaded {len(self.scenarios)} scenarios from {filepath}")

    def _inject_scenarios(self, lines: List[str]):
        """Inject events whose trigger time matches current game time."""
        current = (self.time.hour, self.time.minute)
        remaining = []
        for evt in self.scenarios:
            h = int(evt.get("hour", 0))
            m = int(evt.get("minute", 0))
            if (h, m) == current and not evt.get("_fired"):
                lines.append(f"\n[EVENT] {evt['description']}")
                evt["_fired"] = True
                logger.info(f"[EVENT] {self.time.display} | {evt['description']}")
            if not evt.get("_fired") or (h, m) > current:
                remaining.append(evt)
        self.scenarios = remaining

    # ─── Main Loop ─────────────────────────────────────────────

    def run_day(self, start_hour: int = 6, end_hour: int = 22):
        """
        Run one full game day from start_hour to end_hour.

        The loop:
          1. Push perception → Hermes decides what to do
          2. Parse actions from Hermes response
          3. Simulate action execution
          4. Advance game time
          5. Repeat
        """
        self.time.hour = start_hour
        self.time.minute = 0

        logger.info(f"=== Day {self.time.day} start ===")
        print(f"\n{'='*60}")
        print(f"  Mock UE — Day {self.time.day} ({start_hour:02d}:00 - {end_hour:02d}:00)")
        print(f"  NPC: {self.npc.name} ({self.npc.agent_id})")
        print(f"  Gateway: {self.gateway_url}")
        print(f"  Time speed: {self.time.speed}x")
        print(f"{'='*60}\n")

        # Initial perception: let Hermes know the day started
        start_msg = (
            f"[SYSTEM] 新的一天开始了。游戏时间: {self.time.display}。"
            f"你在{self.npc.current_zone}。"
            f"请规划你今天要做的事，并按顺序执行。"
        )
        self.send_message(start_msg)

        while self.time.hour < end_hour:
            # Push perception
            response = self.push_perception()
            text = self.extract_text(response)

            if text:
                print(f"\n[{self.time.display}] 老陈: {text[:120]}...")

            # Parse action from response and execute
            action, params = self._parse_action(text)
            if action:
                duration = self.execute_action(action, params)
                self.time.advance(duration)
            else:
                # No action — idle, just advance time
                self.time.advance(self.perception_interval)

            # Real-time delay (with time acceleration)
            self.time.sleep(0.5)

        # End of day
        end_msg = (
            f"[SYSTEM] 一天结束了。游戏时间: {self.time.display}。"
            f"你完成了今天的工作。请准备充电休息。"
        )
        self.send_message(end_msg)

        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} complete")
        print(f"  Actions executed: {len(self.action_log)}")
        print(f"  Final zone: {self.npc.current_zone}")
        print(f"  Energy: {self.npc.energy:.0f}%")
        print(f"{'='*60}\n")

    def _parse_action(self, text: str) -> Tuple[Optional[str], dict]:
        """
        Parse an action from Hermes output text.
        Looks for patterns like:
          - "move_to main_workshop"
          - "work_assemble at workbench_01 for 120 minutes"
          - "charge_at charging_station_01 for 60 minutes"
        Returns (action_name, params_dict) or (None, {}).
        """
        if not text:
            return None, {}

        import re

        # Try JSON format first
        json_match = re.search(r'\{[^{}]*"action"[^{}]*\}', text)
        if json_match:
            try:
                action_obj = json.loads(json_match.group())
                return action_obj.get("action"), action_obj.get("params", {})
            except json.JSONDecodeError:
                pass

        # Keyword-based action parsing
        known_actions = list(ACTION_DURATIONS.keys()) + ["work_assemble", "self_check"]
        for action in known_actions:
            if action in text.lower():
                # Extract target
                target_match = re.search(rf'{action}\s+(\w+)', text)
                target = target_match.group(1) if target_match else ""
                # Extract duration
                dur_match = re.search(r'(\d+)\s*(min|分钟)', text)
                duration = int(dur_match.group(1)) if dur_match else None
                params = {"target": target}
                if duration:
                    params["duration_min"] = duration
                return action, params

        return None, {}

    # ─── Logging ──────────────────────────────────────────────

    def _setup_logging(self):
        """Configure file logger."""
        log_file = os.path.join(
            self.log_dir,
            f"day{self.time.day}_{datetime.now().strftime('%Y%m%d_%H%M%S')}.log"
        )
        handler = logging.FileHandler(log_file, encoding="utf-8")
        handler.setFormatter(logging.Formatter(
            "%(asctime)s [%(levelname)s] %(message)s"
        ))
        logger.addHandler(handler)
        logger.setLevel(logging.DEBUG)
        # Also log to console with simpler format
        console = logging.StreamHandler()
        console.setFormatter(logging.Formatter("[%(levelname)s] %(message)s"))
        console.setLevel(logging.INFO)
        logger.addHandler(console)
        logger.info(f"Logging to {log_file}")

    def _log_response(self, data: dict):
        """Log Hermes response summary."""
        text = self.extract_text(data)
        usage = data.get("usage", {})
        logger.debug(f"Tokens: {usage.get('total_tokens', '?')} | Text: {text[:100]}")

    # ─── Report ───────────────────────────────────────────────

    def print_report(self):
        """Print end-of-day summary report."""
        print(f"\n{'='*60}")
        print(f"  Day {self.time.day} — Action Report")
        print(f"{'='*60}")
        print(f"  {'Time':<10} {'Action':<20} {'Target':<20} {'Dur(min)':<10}")
        print(f"  {'-'*60}")
        for entry in self.action_log:
            print(
                f"  {entry['time']:<10} "
                f"{entry['action']:<20} "
                f"{str(entry['params'].get('target', '')):<20} "
                f"{entry['duration_game_min']:<10.0f}"
            )
        print(f"  {'-'*60}")
        print(f"  Total actions: {len(self.action_log)}")
        print(f"{'='*60}\n")
