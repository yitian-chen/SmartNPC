#!/usr/bin/env python3
"""combat-detach 冒烟：无 UE 单侧验证 让位→归还 全闭环。

模拟一个最小 UE 客户端（注册 H-01 + 推一条 perception + 回 action_started），
然后经 /debug/event 注入：

  1. detach 攻击事件（force+detach，event_type 用 UE 实测短名 attacked、无
     category——顺带验证协议容错）→ 断言：stop 在途 action + 让位静默期零
     action_command + /debug/tactical 显示 detached
  2. combat_exit → 断言：重规划（真实 Venus 战术层调用）产出战后新动作

使用方法（dev 实例需已启动，端口 8770/9093）：
  python3 scripts/smoke_combat_detach.py

前置：.env 的 VENUS_API_KEY 已由 start-dev.sh 读取；会触发 2-3 次真实
LLM 调用（战略规划 + 战术分解 ×2）。
"""
import asyncio
import json
import time
import urllib.request
import uuid

import websockets

HTTP_URL = "http://127.0.0.1:8770"
WS_URL = "ws://127.0.0.1:9093/ws"
AGENT = "H-01"

# perception 载荷按真实 UE 推送结构构造（logs-dev 实测样本）。
PERCEPTION = {
    "location": {
        "position": [-8950, -12630, -22709.5],
        "rotation": [0, 0, 0],
        "current_zone": "residential_quarters",
        "current_location": None,
    },
    "nearby_objects": [],
    "object_status_summary": {},
    "environment": {
        "game_time_sec": 30000.0,   # D1 08:20
        "time_of_day_sec": 30000.0,
        "day_count": 0,
        "time_scale": 90,
    },
    "visible_agents": [],
    "physical_state_delta": {"energy": 100, "fatigue": 0, "joint_wear": 0, "money": 200},
}


def envelope(seq, msg_type, agent_id, payload):
    return {
        "version": "1.0",
        "msg_id": str(uuid.uuid4()),
        "seq": seq,
        "timestamp": int(time.time() * 1000),
        "type": msg_type,
        "agent_id": agent_id,
        "payload": payload,
    }


def post_debug_event(event):
    body = json.dumps({"agent_id": AGENT, "event": event}).encode()
    req = urllib.request.Request(
        f"{HTTP_URL}/debug/event", data=body,
        headers={"Content-Type": "application/json"}, method="POST",
    )
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.loads(resp.read())


def get_debug_tactical():
    with urllib.request.urlopen(f"{HTTP_URL}/debug/tactical", timeout=10) as resp:
        return json.loads(resp.read())


class RecvLog:
    """记录 MCP 推来的消息，供断言（action_command / stop_action）。"""

    def __init__(self):
        self.actions = []   # [(cmd, action_id, params)]
        self.stops = []     # [action_id]

    def feed(self, msg):
        t = msg.get("type")
        p = msg.get("payload") or {}
        if t == "action_command":
            self.actions.append((p.get("cmd"), p.get("action_id"), p.get("params")))
            print(f"  [UE<-MCP] action_command: {p.get('cmd')} {json.dumps(p.get('params'), ensure_ascii=False)[:80]}")
        elif t == "stop_action":
            self.stops.append(p.get("action_id"))
            print(f"  [UE<-MCP] stop_action: {p.get('action_id')}")


async def recv_task(ws, log):
    async for raw in ws:
        try:
            log.feed(json.loads(raw))
        except json.JSONDecodeError:
            pass


async def heartbeat_task(ws, state):
    """UE 侧心跳：每 4s 主动发一条（MCP 按「收到心跳」计活，15s 无收包断连）。"""
    uptime = 0
    while True:
        await asyncio.sleep(4)
        uptime += 4
        state["seq"] += 1
        try:
            await ws.send(json.dumps(envelope(
                state["seq"], "heartbeat", "system", {"uptime_sec": uptime})))
        except websockets.ConnectionClosed:
            return


async def wait_for(cond, timeout, what):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if cond():
            return
        await asyncio.sleep(0.3)
    raise SystemExit(f"[FAIL] 等待超时：{what}")


async def main():
    log = RecvLog()
    state = {"seq": 0}

    async with websockets.connect(WS_URL, max_size=2**22) as ws:
        asyncio.create_task(recv_task(ws, log))
        asyncio.create_task(heartbeat_task(ws, state))

        def next_seq():
            state["seq"] += 1
            return state["seq"]

        # 1. 注册 + 感知（worker 启动 → 战略规划 + 战术分解，真实 LLM）。
        await ws.send(json.dumps(envelope(next_seq(), "agent_registered", AGENT, {
            "agent_type": "Human", "ue5_ref": "BP_SmokeCombat_C",
            "initial_position": PERCEPTION["location"]["position"],
            "initial_zone": "residential_quarters",
        })))
        await asyncio.sleep(1)
        await ws.send(json.dumps(envelope(next_seq(), "perception_update", AGENT, PERCEPTION)))
        print("[1] 已注册 + 推送 perception，等首个 action_command（战略+战术 LLM）...")
        await wait_for(lambda: log.actions, 60, "首个 action_command（战略+战术层 LLM）")

        # 回 action_started 保持 in-flight（不回 completed）。
        first_cmd, first_id, _ = log.actions[-1]
        await ws.send(json.dumps(envelope(next_seq(), "action_started", AGENT, {
            "action_id": first_id, "accepted": True,
        })))
        print(f"[2] in-flight 建立：{first_cmd} ({first_id})")

        # 2. 注入 detach 攻击（UE 实测形态：短名 attacked、无 category）。
        print("[3] POST /debug/event：被攻击（force + detach）...")
        resp = post_debug_event({
            "event_type": "attacked", "force": True, "detach": True,
            "severity": 10, "subject": AGENT,
            "data": {"attacker": "player_1", "damage": 20, "damage_type": "physical"},
        })
        print(f"    note: {resp.get('note')}")
        await wait_for(lambda: log.stops, 5, "让位 stop 在途动作")
        if log.stops[-1] != first_id:
            raise SystemExit(f"[FAIL] stop 目标错误：{log.stops[-1]} != {first_id}")

        # 让位静默期：8 秒内零新 action_command。
        silence_start = len(log.actions)
        await asyncio.sleep(8)
        if len(log.actions) != silence_start:
            raise SystemExit(f"[FAIL] 让位期间下发了动作：{log.actions[silence_start:]}")
        entries = [e for e in get_debug_tactical() if e.get("agent_id") == AGENT]
        if not entries or not entries[0].get("detached"):
            raise SystemExit(f"[FAIL] /debug/tactical 应显示 detached=true：{entries}")
        print(f"[4] 让位验证通过：stop×1、8s 静默零下发、detached=true（situations={entries[0].get('situations', '')!r}）")

        # 3. 注入 combat_exit → 归还重规划（真实 LLM）。
        print("[5] POST /debug/event：combat_exit（归还控制权）...")
        resp = post_debug_event({
            "event_type": "combat_exit", "severity": 6, "subject": AGENT,
            "data": {"attacker": "player_1", "outcome": "escaped"},
        })
        print(f"    note: {resp.get('note')}")
        await wait_for(lambda: len(log.actions) > silence_start, 45, "战后重规划产出新 action_command")
        post_cmds = [a[0] for a in log.actions[silence_start:]]
        print(f"[6] 归还验证通过：战后新动作 {post_cmds}")

    print("\n[PASS] combat-detach 冒烟全链路通过：让位（stop+静默+detached）→ combat_exit 归还（重规划新动作）")


if __name__ == "__main__":
    asyncio.run(main())
