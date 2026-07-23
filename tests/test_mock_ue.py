import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from agenttown.mock_ue import MockUE, CMD_EXECUTE_COMPOSITE, PHYS_RATES, PHYS_RATES_PASSIVE


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

    async def test_unknown_route_is_action_failed(self):
        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        await ue._handle_envelope({
            "type": "action_command",
            "agent_id": "H-01",
            "seq": 1,
            "payload": {"action_id": "act_patrol", "cmd": "ExecuteComposite",
                        "params": {"name": "patrol_route", "route_id": "morning_patrol"}},
        })
        import json
        self.assertGreaterEqual(len(ue._ws.sent), 1)
        ack = json.loads(ue._ws.sent[0])
        self.assertEqual(ack["type"], "action_started")
        self.assertEqual(ack["payload"]["accepted"], False)
        self.assertIn("unknown patrol route", ack["payload"]["reject_reason"])

    async def test_unknown_move_target_is_rejected(self):
        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        await ue._handle_envelope({
            "type": "action_command",
            "agent_id": "H-01",
            "seq": 1,
            "payload": {"action_id": "act_move", "cmd": "MoveTo",
                        "params": {"target": "narnia"}},
        })
        import json
        ack = json.loads(ue._ws.sent[0])
        self.assertEqual(ack["type"], "action_started")
        self.assertEqual(ack["payload"]["accepted"], False)

    async def test_scan_area_echoes_scan_id_once(self):
        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        await ue._handle_envelope({
            "type": "scan_area",
            "agent_id": "H-01",
            "seq": 1,
            "payload": {"scan_id": "scan_123"},
        })
        import json
        response = json.loads(ue._ws.sent[-1])
        self.assertEqual(response["type"], "perception_update")
        self.assertEqual(response["payload"]["scan_id"], "scan_123")

    async def test_periodic_perception_has_no_scan_id(self):
        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        await ue._send_perception()
        import json
        response = json.loads(ue._ws.sent[-1])
        self.assertNotIn("scan_id", response["payload"])


class PhysicalEvolutionTests(unittest.TestCase):
    """Tests for _evolve_physical rate branching by composite action name.

    Regression guard for the bug where charge_at drained energy instead of
    restoring it because _evolve_physical treated all composite actions as
    work-like (the composite name wasn't tracked on the NPC state).
    """

    def make_ue(self):
        return MockUE(log_dir="logs")

    def _set_busy_composite(self, ue, name):
        """Put the NPC into a busy composite action with the given name."""
        ue.npc.busy_cmd = CMD_EXECUTE_COMPOSITE
        ue.npc.busy_action_id = "act_test"
        ue.npc.busy_composite_name = name

    def test_charge_at_restores_energy_and_reduces_fatigue(self):
        ue = self.make_ue()
        ue.npc.physical.energy = 50.0
        ue.npc.physical.fatigue = 60.0
        ue.npc.physical.joint_wear = 10.0
        self._set_busy_composite(ue, "charge_at")

        energy_before = ue.npc.physical.energy
        fatigue_before = ue.npc.physical.fatigue
        joint_before = ue.npc.physical.joint_wear

        ue._evolve_physical()

        # Energy must go UP — this is the core regression. The pre-fix code
        # applied the work-like drain (-0.05/min) to charge_at.
        self.assertGreater(ue.npc.physical.energy, energy_before,
                           "charge_at must restore energy, not drain it")
        # Fatigue must go DOWN.
        self.assertLess(ue.npc.physical.fatigue, fatigue_before,
                        "charge_at must relieve fatigue, not accrue it")
        # Joint wear must not increase while docked.
        self.assertEqual(ue.npc.physical.joint_wear, joint_before,
                         "charge_at must not add joint wear")

    def test_charge_at_rate_matches_phys_rates_constant(self):
        ue = self.make_ue()
        interval = ue.perception_interval
        ue.npc.physical.energy = 50.0
        ue.npc.physical.fatigue = 60.0
        self._set_busy_composite(ue, "charge_at")
        ue._evolve_physical()
        rate = PHYS_RATES["charge_at"]
        self.assertAlmostEqual(ue.npc.physical.energy, 50.0 + interval * rate["energy"])
        self.assertAlmostEqual(ue.npc.physical.fatigue, 60.0 + interval * rate["fatigue"])

    def test_rest_idle_reduces_fatigue_with_minimal_energy_drain(self):
        ue = self.make_ue()
        ue.npc.physical.energy = 80.0
        ue.npc.physical.fatigue = 70.0
        ue.npc.physical.joint_wear = 5.0
        self._set_busy_composite(ue, "rest_idle")

        energy_before = ue.npc.physical.energy
        fatigue_before = ue.npc.physical.fatigue
        joint_before = ue.npc.physical.joint_wear

        ue._evolve_physical()

        # Fatigue goes down — rest_idle was broken by the same root cause.
        self.assertLess(ue.npc.physical.fatigue, fatigue_before,
                        "rest_idle must relieve fatigue")
        # Energy drain is minimal (much less than the work rate).
        drain = energy_before - ue.npc.physical.energy
        self.assertLess(drain, 1.0,
                        f"rest_idle energy drain should be minimal, got {drain}")
        # Joint wear unchanged.
        self.assertEqual(ue.npc.physical.joint_wear, joint_before)

    def test_work_assemble_keeps_work_like_drain(self):
        """The default composite rate (work_assemble etc.) must keep the
        pre-fix drain behavior — this guards against accidentally flipping
        the sign or dropping the rate during the refactor."""
        ue = self.make_ue()
        ue.npc.physical.energy = 80.0
        ue.npc.physical.fatigue = 30.0
        ue.npc.physical.joint_wear = 5.0
        self._set_busy_composite(ue, "work_assemble")

        energy_before = ue.npc.physical.energy
        fatigue_before = ue.npc.physical.fatigue
        joint_before = ue.npc.physical.joint_wear

        ue._evolve_physical()

        self.assertLess(ue.npc.physical.energy, energy_before,
                        "work_assemble must drain energy")
        self.assertGreater(ue.npc.physical.fatigue, fatigue_before,
                           "work_assemble must accrue fatigue")
        self.assertGreater(ue.npc.physical.joint_wear, joint_before,
                           "work_assemble must accrue joint wear")
        # And the delta must match the _default rate exactly.
        interval = ue.perception_interval
        rate = PHYS_RATES["_default"]
        self.assertAlmostEqual(ue.npc.physical.energy, energy_before + interval * rate["energy"])
        self.assertAlmostEqual(ue.npc.physical.fatigue, fatigue_before + interval * rate["fatigue"])
        self.assertAlmostEqual(ue.npc.physical.joint_wear, joint_before + interval * rate["joint_wear"])

    def test_unknown_composite_name_falls_back_to_default_drain(self):
        """An unregistered composite name must fall back to the work-like
        default rate rather than crashing or silently no-oping."""
        ue = self.make_ue()
        ue.npc.physical.energy = 80.0
        ue.npc.physical.fatigue = 30.0
        self._set_busy_composite(ue, "some_future_composite")
        ue._evolve_physical()
        # Should drain like work, not raise.
        self.assertLess(ue.npc.physical.energy, 80.0)
        self.assertGreater(ue.npc.physical.fatigue, 30.0)

    def test_passive_drain_when_not_busy(self):
        ue = self.make_ue()
        ue.npc.physical.energy = 80.0
        ue.npc.physical.fatigue = 30.0
        ue.npc.physical.joint_wear = 5.0
        # No busy state set.
        energy_before = ue.npc.physical.energy
        joint_before = ue.npc.physical.joint_wear
        ue._evolve_physical()
        self.assertLess(ue.npc.physical.energy, energy_before,
                        "passive must drain energy")
        self.assertGreater(ue.npc.physical.fatigue, 30.0,
                           "passive must accrue fatigue")
        self.assertEqual(ue.npc.physical.joint_wear, joint_before,
                         "passive must not add joint wear")
        interval = ue.perception_interval
        rate = PHYS_RATES_PASSIVE
        self.assertAlmostEqual(ue.npc.physical.energy, energy_before + interval * rate["energy"])

    def test_charge_at_clamps_energy_at_100(self):
        ue = self.make_ue()
        ue.npc.physical.energy = 99.0
        ue.npc.physical.fatigue = 10.0
        self._set_busy_composite(ue, "charge_at")
        # One tick would push energy past 100; must clamp.
        ue._evolve_physical()
        self.assertLessEqual(ue.npc.physical.energy, 100.0,
                             "energy must not exceed 100")
        self.assertEqual(ue.npc.physical.energy, 100.0)

    def test_charge_at_clamps_fatigue_at_0(self):
        ue = self.make_ue()
        ue.npc.physical.energy = 50.0
        ue.npc.physical.fatigue = 1.0
        self._set_busy_composite(ue, "charge_at")
        ue._evolve_physical()
        self.assertGreaterEqual(ue.npc.physical.fatigue, 0.0,
                                "fatigue must not go below 0")
        self.assertEqual(ue.npc.physical.fatigue, 0.0)

    def test_busy_composite_name_set_on_composite_start(self):
        """When a composite action starts, busy_composite_name must be
        populated from params['name'] so _evolve_physical can branch on it."""
        import json

        class FakeWS:
            def __init__(self):
                self.sent = []

            async def send(self, frame):
                self.sent.append(frame)

        ue = MockUE(log_dir="logs")
        ue._ws = FakeWS()
        ue.time.speed = 300.0
        ue.time.day = 1
        ue.time.hour = 8  # 08:00
        ue.time.minute = 0
        ue.npc.current_zone = "main_workshop"
        ue.npc.physical.energy = 50.0
        ue.npc.physical.fatigue = 60.0

        import asyncio

        async def _run():
            await ue._handle_envelope({
                "type": "action_command",
                "agent_id": "H-01",
                "seq": 1,
                "payload": {
                    "action_id": "act_charge",
                    "cmd": "ExecuteComposite",
                    "params": {
                        "name": "charge_at",
                        "station_id": "charging_station_01",
                        "duration_sec": 1800,
                    },
                },
            })

        asyncio.run(_run())

        self.assertEqual(ue.npc.busy_composite_name, "charge_at",
                         "busy_composite_name must be set to 'charge_at'")
        self.assertEqual(ue.npc.busy_cmd, CMD_EXECUTE_COMPOSITE)
        self.assertIsNotNone(ue.npc.busy_action_id)

        # Now evolve — energy must go up (the whole point of the fix).
        energy_before = ue.npc.physical.energy
        ue._evolve_physical()
        self.assertGreater(ue.npc.physical.energy, energy_before,
                           "charge_at started via _handle_envelope must restore energy")

    def test_busy_composite_name_cleared_on_stop(self):
        """_clear_busy must reset busy_composite_name so a subsequent
        passive tick doesn't keep applying the composite rate."""
        ue = self.make_ue()
        self._set_busy_composite(ue, "charge_at")
        self.assertEqual(ue.npc.busy_composite_name, "charge_at")
        ue._clear_busy()
        self.assertIsNone(ue.npc.busy_composite_name)
        self.assertIsNone(ue.npc.busy_cmd)
        self.assertIsNone(ue.npc.busy_action_id)

    def test_non_composite_busy_does_not_set_composite_name(self):
        """A busy non-composite action (e.g. none here) must leave
        busy_composite_name as None so the passive rate applies."""
        ue = self.make_ue()
        # Simulate a busy non-composite state manually.
        ue.npc.busy_cmd = "MoveTo"  # not CMD_EXECUTE_COMPOSITE
        ue.npc.busy_action_id = "act_move"
        ue.npc.busy_composite_name = None
        ue.npc.physical.energy = 80.0
        ue.npc.physical.fatigue = 30.0
        ue._evolve_physical()
        # MoveTo is not CMD_EXECUTE_COMPOSITE, so passive rate applies.
        interval = ue.perception_interval
        self.assertAlmostEqual(ue.npc.physical.energy, 80.0 + interval * PHYS_RATES_PASSIVE["energy"])


if __name__ == "__main__":
    unittest.main()
