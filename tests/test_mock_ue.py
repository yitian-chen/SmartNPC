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


class AgentRoutingTests(unittest.IsolatedAsyncioTestCase):
    async def test_command_for_other_agent_is_rejected(self):
        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        await ue._handle_envelope({
            "type": "action_command",
            "agent_id": "H-99",
            "seq": 1,
            "payload": {"action_id": "act_wrong", "cmd": "Wait", "params": {"duration_sec": 1}},
        })
        self.assertEqual(len(ue._ws.sent), 1)
        import json
        response = json.loads(ue._ws.sent[0])
        self.assertEqual(response["type"], "error")
        self.assertEqual(response["payload"]["error_code"], "UNKNOWN_AGENT")
        self.assertEqual(ue.action_log, [])


if __name__ == "__main__":
    unittest.main()
