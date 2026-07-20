import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from agenttown.mock_ue import MockUE


class ScenarioInjectionTests(unittest.TestCase):
    def make_ue(self):
        ue = MockUE(log_dir="logs")
        ue.scenarios = [
            {"hour": 7, "minute": 30, "description": "传送带异常"},
            {"hour": 10, "minute": 45, "description": "库存不足"},
        ]
        return ue

    def test_crossed_event_is_queued_once(self):
        ue = self.make_ue()
        ue._queue_crossed_scenario_events(7 * 60, 8 * 60)
        self.assertEqual(len(ue._pending_audible_events), 1)
        self.assertEqual(ue._pending_audible_events[0]["content"], "传送带异常")

        ue._queue_crossed_scenario_events(7 * 60, 8 * 60)
        self.assertEqual(len(ue._pending_audible_events), 1)

    def test_event_does_not_require_exact_tick(self):
        ue = self.make_ue()
        ue._queue_crossed_scenario_events(10 * 60 + 30, 11 * 60)
        self.assertEqual([e["content"] for e in ue._pending_audible_events], ["库存不足"])

    def test_pending_event_is_consumed_by_one_perception(self):
        ue = self.make_ue()
        ue._queue_crossed_scenario_events(7 * 60, 8 * 60)
        first = ue._build_perception()
        second = ue._build_perception()
        self.assertEqual(len(first["audible_events"]), 1)
        self.assertEqual(second["audible_events"], [])


if __name__ == "__main__":
    unittest.main()
