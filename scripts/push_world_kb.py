#!/usr/bin/env python3
"""向 MCP 推送一份新的 world_kb，注册到内存 KB 并落盘。

通过 WebSocket 发送 type=world_kb 的 7 字段信封（agent_id=system），
MCP 收到后执行 merge + 落盘 + swap 内存 KB。必须在首个 agent_registered
之前发送（MCP 启动窗口），否则会被拒绝。

默认推送"最早 3 zone / 2 object"版本（main_workshop / central_plaza /
charging_station + workbench_01 / charging_station_01），用新 schema 格式
重写自 089b357 提交的旧 schema 版本。charging_station_01 的 zone 归属
按物理坐标修正为 charging_station（旧版 zones.locations 与 locations.zone
不一致的 bug 已在此版本修正）。

使用方法：
  # 1. 启动 MCP（dev 端口 :9091）
  ./mcp --llm-backend=venus --http :8770 --ws :9091 \
    --venus-api-key "$VENUS_API_KEY" --log-level debug

  # 2. 在 Mock UE 连接之前，运行本脚本推送新 KB
  python3 scripts/push_world_kb.py
  # 或指定 WS 地址
  python3 scripts/push_world_kb.py --ws ws://localhost:9091/ws

  # 3. 启动 Mock UE（需用同一份 KB 或跳过 world_kb 推送，否则会覆盖）
  python3 src/run_day.py

可选参数：
  --ws URL    MCP WebSocket 地址（默认 ws://localhost:9091/ws，dev 端口）
  --yaml PATH 自定义 KB yaml 文件路径（新 schema 格式）；省略则用内置 3zone 版本

推送成功后 MCP 会：
  1. merge generated + authored → 合并 KB
  2. 写回 --world-kb 指定的路径（assets/world_kb.yaml 或 --world-kb flag 指定）
  3. swap 内存 KB（worker 启动时捕获新 KB）
  4. 日志输出 "world_kb merged and persisted"
"""
import argparse
import asyncio
import json
import sys
import time
import uuid
from pathlib import Path

import websockets

PROTOCOL_VERSION = "1.0"
SYSTEM_AGENT_ID = "system"
TYPE_WORLD_KB = "world_kb"

GENERATOR_DEFAULT = {"name": "MockUE", "version": "0.1.0"}
SOURCE_DEFAULT = {"map_package": "", "map_name": ""}
COORD_SYSTEM_DEFAULT = {
    "space": "UE5_world", "distance_unit": "centimeter",
    "rotation_unit": "degree", "rotation_order": "pitch_yaw_roll",
}

# ─── 内置 3 zone / 2 object KB（新 schema，重写自 089b357 旧 schema 版本） ───
#
# 与当前 assets/world_kb.yaml（7 zone/3 object）的差异：
#   - 删除 archive_station / logistics_hub / recycling_yard / repair_bay /
#     residential_quarters 五个 zone
#   - 删除 rest_bench_01 object
#   - charging_station zone 重新出现（当前 KB 已合并进 central_plaza）
#   - charging_station_01 归属修正为 charging_station（按物理坐标
#     [21500,8500] 在 charging_station bounds 内判定，修正旧版
#     zones.locations 与 locations.zone 不一致 bug）
#   - agent H-01 initial_zone 改回 main_workshop（旧版 default_zone）
#
# YAML 格式（合并后形态），split_yaml_to_generated_authored 会自动拆成
# generated + authored 两半发给 MCP。
BUILTIN_KB_YAML = """
version: "1.0"
narrative:
    setting: 工业机器人园区
    theme: 一座由机器人维持生产的封闭工业园区。
zones:
    - id: main_workshop
      display_name: 主生产车间
      description: 机器人们的主要工作场所。
      aliases:
        - 车间
      bounds:
        center:
            - 20000
            - 10000
            - 0
        extent:
            - 5000
            - 5000
            - 5000
      entry_point:
        - 16000
        - 10000
        - 100
      entry_facing:
        - 1
        - 0
        - 0
      connections:
        - to: central_plaza
          type: road
          bidirectional: true
    - id: central_plaza
      display_name: 中央广场
      description: 园区中央的开放广场，机器人休息社交的场所。
      aliases:
        - 广场
      bounds:
        center:
            - 11500
            - 11500
            - 0
        extent:
            - 3500
            - 3500
            - 5000
      entry_point:
        - 10000
        - 10000
        - 100
      entry_facing:
        - 0
        - -1
        - 0
      connections:
        - to: main_workshop
          type: road
          bidirectional: true
        - to: charging_station
          type: road
          bidirectional: true
    - id: charging_station
      display_name: 充电站
      description: 园区充电设施所在区域。
      aliases:
        - 充电区
      bounds:
        center:
            - 22500
            - 7500
            - 0
        extent:
            - 2500
            - 2500
            - 5000
      entry_point:
        - 21500
        - 8500
        - 100
      entry_facing:
        - -1
        - 0
        - 0
      connections:
        - to: central_plaza
          type: road
          bidirectional: true
objects:
    - id: workbench_01
      display_name: 工作台一号
      description: 装配工作台，支持组装和检查。
      category: workbench
      zone_id: main_workshop
      actor_class: /Game/AgentTown/Objects/BP_Workbench.BP_Workbench_C
      actor_position:
        - 20000
        - 10000
        - 100
      interaction_point:
        - 19500
        - 10500
        - 100
      interaction_facing:
        - 1
        - 0
        - 0
      interaction_radius: 1500
      available_interactions:
        - assemble
        - inspect
      default_state: idle
      tags:
        - crafting
        - assembly
    - id: charging_station_01
      display_name: 充电桩一号
      description: 园区主充电桩，支持充电和检查。
      category: charging_station
      zone_id: charging_station
      actor_class: /Game/AgentTown/Objects/BP_ChargingStation.BP_ChargingStation_C
      actor_position:
        - 21500
        - 8500
        - 100
      interaction_point:
        - 21500
        - 8500
        - 100
      interaction_facing:
        - 1
        - 0
        - 0
      interaction_radius: 1500
      available_interactions:
        - charge
        - inspect
      default_state: idle
      tags:
        - charging
agents:
    - id: H-01
      display_name: 老陈
      description: 车间主管机器人，沉稳念旧，重视工艺。
      type: humanoid
      profession: 车间主管
      personality:
        traits:
            - 沉稳
            - 念旧
            - 重视工艺
        speech_style: 简洁，偶尔念叨老物件
      initial_zone: main_workshop
      initial_position:
        - 20000
        - 10000
        - 100
      actor_class: /Game/AgentTown/Agents/BP_LaoChen.BP_LaoChen_C
      action_table: /Game/AgentTown/AI/DT_ActionBTMap.DT_ActionBTMap
      main_behavior_tree: /Game/AgentTown/AI/BT_AgentTownMain.BT_AgentTownMain
"""


def split_yaml_to_generated_authored(yaml_data: dict) -> tuple[dict, dict]:
    """拆分合并后的 KB dict → generated + authored 两半。

    复用 mock_ue.split_yaml_to_generated_authored 的字段归属规则：
    空间/事实字段 → generated，叙事字段 → authored。
    """
    import yaml  # 延迟 import，--help 不需要

    gen_zones = []
    for z in yaml_data.get("zones", []) or []:
        bounds = z.get("bounds", {}) or {}
        gen_zones.append({
            "id": z.get("id", ""),
            "editor_label": "",
            "actor_path": "",
            "bounds": {
                "center": bounds.get("center", [0, 0, 0]),
                "extent": bounds.get("extent", [0, 0, 0]),
                "rotation": bounds.get("rotation", [0, 0, 0]),
            },
            "entry_point": z.get("entry_point", [0, 0, 0]),
            "entry_facing": z.get("entry_facing", [0, 0, 0]),
        })

    gen_objects = []
    for o in yaml_data.get("objects", []) or []:
        gen_objects.append({
            "id": o.get("id", ""),
            "category": o.get("category", ""),
            "zone_id": o.get("zone_id", ""),
            "editor_label": "",
            "actor_class": o.get("actor_class", ""),
            "actor_position": o.get("actor_position", [0, 0, 0]),
            "interaction_point": o.get("interaction_point", [0, 0, 0]),
            "interaction_facing": o.get("interaction_facing", [0, 0, 0]),
            "available_interactions": o.get("available_interactions", []) or [],
            "default_state": o.get("default_state", ""),
        })

    gen_agents = []
    for a in yaml_data.get("agents", []) or []:
        gen_agents.append({
            "id": a.get("id", ""),
            "type": a.get("type", ""),
            "initial_zone": a.get("initial_zone", ""),
            "editor_label": "",
            "actor_class": a.get("actor_class", ""),
            "initial_position": a.get("initial_position", [0, 0, 0]),
            "action_table": a.get("action_table", ""),
            "main_behavior_tree": a.get("main_behavior_tree", ""),
        })

    generated = {
        "$schema": "agenttown-world-generated/v1",
        "schema_version": yaml_data.get("version", "1.0"),
        "generated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "generator": dict(GENERATOR_DEFAULT),
        "source": dict(SOURCE_DEFAULT),
        "coordinate_system": dict(COORD_SYSTEM_DEFAULT),
        "zones": gen_zones,
        "objects": gen_objects,
        "agents": gen_agents,
        "validation_summary": {"errors": 0, "warnings": 0},
    }

    auth_zones = {}
    for z in yaml_data.get("zones", []) or []:
        zid = z.get("id", "")
        if not zid:
            continue
        auth_zones[zid] = {
            "display_name": z.get("display_name", zid),
            "description": z.get("description", ""),
            "aliases": z.get("aliases", []) or [],
            "connections": z.get("connections", []) or [],
        }

    auth_objects = {}
    for o in yaml_data.get("objects", []) or []:
        oid = o.get("id", "")
        if not oid:
            continue
        auth_objects[oid] = {
            "display_name": o.get("display_name", oid),
            "description": o.get("description", ""),
            "tags": o.get("tags", []) or [],
        }

    auth_agents = {}
    for a in yaml_data.get("agents", []) or []:
        aid = a.get("id", "")
        if not aid:
            continue
        auth_agents[aid] = {
            "display_name": a.get("display_name", aid),
            "description": a.get("description", ""),
            "profession": a.get("profession", ""),
            "personality": a.get("personality", {}) or {},
            "initial_zone": a.get("initial_zone", ""),
            "relationships": a.get("relationships", []) or [],
        }

    authored = {
        "version": yaml_data.get("version", "1.0"),
        "narrative": yaml_data.get("narrative", {}) or {},
        "zones": auth_zones,
        "objects": auth_objects,
        "agents": auth_agents,
    }

    return generated, authored


def build_envelope(payload: dict) -> dict:
    """构造 7 字段信封。"""
    return {
        "version": PROTOCOL_VERSION,
        "msg_id": str(uuid.uuid4()),
        "seq": 1,
        "timestamp": int(time.time() * 1000),
        "type": TYPE_WORLD_KB,
        "agent_id": SYSTEM_AGENT_ID,
        "payload": payload,
    }


def load_kb_yaml(path: str | None) -> dict:
    """加载 KB yaml 文件，或返回内置 3zone 版本。"""
    import yaml
    if path:
        with open(path, "r", encoding="utf-8") as f:
            return yaml.safe_load(f)
    return yaml.safe_load(BUILTIN_KB_YAML)


async def push_world_kb(ws_url: str, kb_yaml_path: str | None):
    """连接 MCP WS，发送 world_kb 消息，等待响应。"""
    kb_data = load_kb_yaml(kb_yaml_path)
    generated, authored = split_yaml_to_generated_authored(kb_data)

    zone_ids = [z["id"] for z in kb_data.get("zones", [])]
    obj_ids = [o["id"] for o in kb_data.get("objects", [])]
    agent_ids = [a["id"] for a in kb_data.get("agents", [])]
    print(f"[KB] zones={zone_ids}")
    print(f"[KB] objects={obj_ids}")
    print(f"[KB] agents={agent_ids}")
    print(f"[KB] narrative.setting={kb_data.get('narrative', {}).get('setting', '')}")

    payload = {
        "pushed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "generated": generated,
        "authored": authored,
    }
    envelope = build_envelope(payload)
    frame = json.dumps(envelope, ensure_ascii=False)

    print(f"\n[WS] connecting to {ws_url} ...")
    try:
        async with websockets.connect(ws_url, open_timeout=5) as ws:
            await ws.send(frame)
            print("[WS] world_kb message sent, waiting for response ...")
            # 等待 MCP 响应（world_kb 无显式 ACK，等一条任意消息或超时）
            try:
                resp = await asyncio.wait_for(ws.recv(), timeout=3)
                print(f"[WS] response: {resp}")
            except asyncio.TimeoutError:
                print("[WS] no response within 3s (world_kb has no explicit ACK; "
                      "check MCP log for 'world_kb merged and persisted')")
            except websockets.ConnectionClosed:
                print("[WS] connection closed by MCP (check MCP log for errors)")
    except ConnectionRefusedError:
        print(f"[ERROR] cannot connect to {ws_url} — is MCP running?", file=sys.stderr)
        sys.exit(1)
    except Exception as e:
        print(f"[ERROR] {type(e).__name__}: {e}", file=sys.stderr)
        sys.exit(1)

    print("\n[DONE] world_kb push completed.")
    print("Next steps:")
    print("  1. Check MCP log for 'world_kb merged and persisted'")
    print("  2. GET http://localhost:8770/debug/kb to verify the new KB")
    print("  3. Start Mock UE (must use same KB or skip its world_kb push)")


def main():
    parser = argparse.ArgumentParser(
        description="Push a new world_kb to MCP via WebSocket (startup window only).",
    )
    parser.add_argument(
        "--ws", default="ws://localhost:9091/ws",
        help="MCP WebSocket URL (default: ws://localhost:9091/ws, dev port)",
    )
    parser.add_argument(
        "--yaml", default=None,
        help="custom KB yaml file (new schema); omit to use built-in 3zone/2object KB",
    )
    args = parser.parse_args()

    asyncio.run(push_world_kb(args.ws, args.yaml))


if __name__ == "__main__":
    main()
