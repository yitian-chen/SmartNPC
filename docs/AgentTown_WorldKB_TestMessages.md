# AgentTown `world_kb` 测试消息集

## 文档目的

本文提供三份不同规模的 `world_kb` 测试消息，格式与 Mock UE 通过 WebSocket 推送给 MCP 的真实消息完全一致（7 字段信封 + `WorldKBPayload`）。可用于：

- 端到端联调：手工通过 `wscat` / Python `websockets` 推送到 MCP 验证合并管线
- 单元测试 fixture：直接复制粘贴为 Go / Python 测试的 payload 输入
- 协议演示：理解 `world_kb` 消息的完整结构

## 消息格式约定

每条消息是符合 `pkg/protocol/envelope.go` 的 7 字段信封，`type="world_kb"`，`agent_id="system"`（系统级消息），`payload` 为 `WorldKBPayload`（见 `pkg/protocol/messages.go:238`）：

```jsonc
{
  "version": "1.0",                    // 协议版本，固定 "1.0"
  "msg_id": "<UUID v4>",               // 每条消息唯一 ID，发送时由 uuid4 生成
  "seq": <int>,                        // per-sender 单调递增，UE 侧从 1 开始
  "timestamp": <unix_ms>,              // Unix 毫秒，发送时由 int(time.time()*1000) 填充
  "type": "world_kb",
  "agent_id": "system",                // 系统级消息保留 ID
  "payload": {
    "pushed_at": "<RFC3339 UTC>",      // 可选诊断字段，UE 侧用 strftime("%Y-%m-%dT%H:%M:%SZ", gmtime) 生成
    "generated": { ... },              // world.generated.json blob（UE 自动导出的空间事实）
    "authored":  { ... }               // world.authored.json blob（人工维护的叙事层）
  }
}
```

**动态字段说明**：`msg_id`、`seq`、`timestamp`、`pushed_at` 在真实发送时由发送方实时填充，本文用 `<placeholder>` 标注。`generated` 与 `authored` 是消息的核心内容，结构必须符合 `pkg/worldkb/schema.go` 定义的 `GeneratedDoc` / `AuthoredDoc`（NEW schema, 2026-07）。

**校验约束**（摘自 `pkg/worldkb/validator.go`，使用前必读）：

| 约束 | 说明 |
|------|------|
| ID 长度 ≥ 3 | zone / object / agent 的 `id` 字段至少 3 个字符 |
| 引用闭合 | `object.zone_id`、`agent.initial_zone`、`zone.connections[].to` 必须引用存在的 zone ID |
| 版本一致 | `generated.schema_version` 必须等于 `authored.version` |
| 必填字段 | zone: `bounds.center/extent`、`entry_point`；object: `actor_position`、`interaction_point/facing`、`available_interactions`、`default_state`；agent: `type`、`initial_zone`、`actor_class`、`initial_position` |

**启动窗口约束**：MCP 仅在「首个 `agent_registered` 之前」接受 `world_kb` 消息；之后到达的会被拒绝并告警（见 `main.go` 的 `worldKBSwap` + `errAgentWindowClosed`）。手工测试时必须在 `agent_registered` 之前推送。

---

## 测试消息一：最小可用 KB（单 zone / 单 object / 单 agent）

**用途**：烟测、协议最小闭合验证、单元测试默认 fixture。

**特点**：
- 仅 1 个 zone、1 个 object、1 个 agent
- 无 connections、无 relationships
- 叙事字段最简

```json
{
  "version": "1.0",
  "msg_id": "00000000-0000-4000-8000-000000000001",
  "seq": 1,
  "timestamp": 1722470400000,
  "type": "world_kb",
  "agent_id": "system",
  "payload": {
    "pushed_at": "2026-08-01T00:00:00Z",
    "generated": {
      "$schema": "agenttown-world-generated/v1",
      "schema_version": "1.0",
      "generated_at": "2026-08-01T00:00:00Z",
      "generator": {"name": "MockUE-Test", "version": "0.1"},
      "source": {
        "map_package": "/Game/Test/Maps/L_Minimal",
        "map_name": "L_Minimal"
      },
      "coordinate_system": {
        "space": "UE5_world",
        "distance_unit": "centimeter",
        "rotation_unit": "degree",
        "rotation_order": "pitch_yaw_roll"
      },
      "zones": [
        {
          "id": "test_zone",
          "editor_label": "Zone_TestZone",
          "actor_path": "/Game/Test/Maps/L_Minimal.Zone_TestZone",
          "bounds": {"center": [0, 0, 0], "extent": [2000, 2000, 500], "rotation": [0, 0, 0]},
          "entry_point": [0, 1500, 100],
          "entry_facing": [0, -1, 0]
        }
      ],
      "objects": [
        {
          "id": "test_object",
          "category": "test_fixture",
          "zone_id": "test_zone",
          "editor_label": "BP_TestObject",
          "actor_class": "/Game/Test/Objects/BP_TestObject.BP_TestObject_C",
          "actor_position": [0, 0, 100],
          "interaction_point": [0, 500, 100],
          "interaction_facing": [0, -1, 0],
          "available_interactions": ["inspect"],
          "default_state": "idle"
        }
      ],
      "agents": [
        {
          "id": "T-01",
          "type": "humanoid",
          "initial_zone": "test_zone",
          "editor_label": "BP_TestBot",
          "actor_class": "/Game/Test/Agents/BP_TestBot.BP_TestBot_C",
          "initial_position": [0, 1000, 100],
          "action_table": "/Game/Test/AI/DT_ActionBTMap.DT_ActionBTMap",
          "main_behavior_tree": "/Game/Test/AI/BT_TestMain.BT_TestMain"
        }
      ],
      "validation_summary": {"errors": 0, "warnings": 0}
    },
    "authored": {
      "version": "1.0",
      "narrative": {
        "setting": "最小测试场景",
        "theme": "用于烟测的最小化 KB，仅验证合并管线主路径。"
      },
      "zones": {
        "test_zone": {
          "display_name": "测试区域",
          "description": "仅包含一个交互对象的最小测试空间。",
          "aliases": ["测试区"],
          "connections": []
        }
      },
      "objects": {
        "test_object": {
          "display_name": "测试对象",
          "description": "一个仅供 inspect 的测试用智能对象。",
          "tags": ["test"]
        }
      },
      "agents": {
        "T-01": {
          "display_name": "测试机器人",
          "description": "最小化场景的测试 NPC。",
          "profession": "测试员",
          "personality": {
            "traits": ["冷静", "服从"],
            "speech_style": "简短确认"
          },
          "initial_zone": "test_zone",
          "relationships": []
        }
      }
    }
  }
}
```

---

## 测试消息二：双 zone 车间（带 connections，单 agent）

**用途**：多 zone 场景、zone 连接拓扑验证、单 Agent 跨 zone 行动测试。

**特点**：
- 2 个 zone（`workshop_hall`、`break_room`），双向 connection
- 2 个 object（`assemble_station`、`coffee_machine`），分布在两个 zone
- 1 个 agent（`H-01` 老陈），可在两个 zone 间往返
- 第一期单 Agent 场景，`relationships` 留空
- 验证 merger 的 zone connections 合并、deterministic 排序

```json
{
  "version": "1.0",
  "msg_id": "00000000-0000-4000-8000-000000000002",
  "seq": 1,
  "timestamp": 1722470400000,
  "type": "world_kb",
  "agent_id": "system",
  "payload": {
    "pushed_at": "2026-08-01T00:00:00Z",
    "generated": {
      "$schema": "agenttown-world-generated/v1",
      "schema_version": "1.0",
      "generated_at": "2026-08-01T00:00:00Z",
      "generator": {"name": "MockUE-Test", "version": "0.1"},
      "source": {
        "map_package": "/Game/Workshop/Maps/L_Workshop",
        "map_name": "L_Workshop"
      },
      "coordinate_system": {
        "space": "UE5_world",
        "distance_unit": "centimeter",
        "rotation_unit": "degree",
        "rotation_order": "pitch_yaw_roll"
      },
      "zones": [
        {
          "id": "workshop_hall",
          "editor_label": "Zone_WorkshopHall",
          "actor_path": "/Game/Workshop/Maps/L_Workshop.Zone_WorkshopHall",
          "bounds": {"center": [10000, 10000, 0], "extent": [5000, 4000, 500], "rotation": [0, 0, 0]},
          "entry_point": [6000, 10000, 100],
          "entry_facing": [1, 0, 0]
        },
        {
          "id": "break_room",
          "editor_label": "Zone_BreakRoom",
          "actor_path": "/Game/Workshop/Maps/L_Workshop.Zone_BreakRoom",
          "bounds": {"center": [10000, 3000, 0], "extent": [3000, 2000, 500], "rotation": [0, 0, 0]},
          "entry_point": [10000, 5000, 100],
          "entry_facing": [0, 1, 0]
        }
      ],
      "objects": [
        {
          "id": "assemble_station",
          "category": "workbench",
          "zone_id": "workshop_hall",
          "editor_label": "BP_AssembleStation",
          "actor_class": "/Game/Workshop/Objects/BP_AssembleStation.BP_AssembleStation_C",
          "actor_position": [10000, 10000, 100],
          "interaction_point": [9500, 10500, 100],
          "interaction_facing": [1, 0, 0],
          "available_interactions": ["assemble", "inspect"],
          "default_state": "idle"
        },
        {
          "id": "coffee_machine",
          "category": "vending",
          "zone_id": "break_room",
          "editor_label": "BP_CoffeeMachine",
          "actor_class": "/Game/Workshop/Objects/BP_CoffeeMachine.BP_CoffeeMachine_C",
          "actor_position": [10000, 3000, 100],
          "interaction_point": [10000, 3500, 100],
          "interaction_facing": [0, -1, 0],
          "available_interactions": ["brew", "clean"],
          "default_state": "idle"
        }
      ],
      "agents": [
        {
          "id": "H-01",
          "type": "humanoid",
          "initial_zone": "workshop_hall",
          "editor_label": "BP_LaoChen",
          "actor_class": "/Game/Workshop/Agents/BP_LaoChen.BP_LaoChen_C",
          "initial_position": [10000, 11000, 100],
          "action_table": "/Game/Workshop/AI/DT_ActionBTMap.DT_ActionBTMap",
          "main_behavior_tree": "/Game/Workshop/AI/BT_WorkshopMain.BT_WorkshopMain"
        }
      ],
      "validation_summary": {"errors": 0, "warnings": 0}
    },
    "authored": {
      "version": "1.0",
      "narrative": {
        "setting": "双 Agent 小型车间",
        "theme": "师徒二人共同维护一座小型装配车间，验证多 Agent 与关系数据通路。"
      },
      "zones": {
        "workshop_hall": {
          "display_name": "装配车间大厅",
          "description": "主要装配与质检作业区。",
          "aliases": ["车间", "大厅"],
          "connections": [
            {"to": "break_room", "type": "door", "bidirectional": true}
          ]
        },
        "break_room": {
          "display_name": "休息茶水间",
          "description": "工人短暂休息和补充饮水的区域。",
          "aliases": ["茶水间"],
          "connections": [
            {"to": "workshop_hall", "type": "door", "bidirectional": true}
          ]
        }
      },
      "objects": {
        "assemble_station": {
          "display_name": "一号装配台",
          "description": "车间主力装配工作台。",
          "tags": ["crafting", "assembly"]
        },
        "coffee_machine": {
          "display_name": "茶水间咖啡机",
          "description": "提供咖啡和热水的自动售卖机。",
          "tags": ["vending", "drink"]
        }
      },
      "agents": {
        "H-01": {
          "display_name": "老陈",
          "description": "车间老师傅，技艺纯熟，话少。",
          "profession": "装配技师",
          "personality": {
            "traits": ["沉稳", "严谨", "惜字如金"],
            "speech_style": "短句为主，偶尔点拨"
          },
          "initial_zone": "workshop_hall",
          "relationships": []
        }
      }
    }
  }
}
```

---

## 测试消息三：多区域研究园区（4 zones / 3 objects / 单 agent，复杂拓扑）

**用途**：复杂拓扑压力测试、多 zone 连接图遍历、单 Agent 跨多 zone 行动规划、不同 profession / personality 的 LLM prompt 生成验证。

**特点**：
- 4 个 zone（`lab`、`library`、`cafeteria`、`dorm`），构成四边形拓扑（lab↔library、library↔cafeteria、cafeteria↔dorm、lab↔cafeteria）
- 3 个 object，分布在三个不同 zone
- 1 个 agent（`R-01` 研究员），可在四个 zone 间行动
- 第一期单 Agent 场景，`relationships` 留空
- 验证 merger 的 deterministic 排序、复杂 connections 合并、per-agent personality struct 正确填充

```json
{
  "version": "1.0",
  "msg_id": "00000000-0000-4000-8000-000000000003",
  "seq": 1,
  "timestamp": 1722470400000,
  "type": "world_kb",
  "agent_id": "system",
  "payload": {
    "pushed_at": "2026-08-01T00:00:00Z",
    "generated": {
      "$schema": "agenttown-world-generated/v1",
      "schema_version": "1.0",
      "generated_at": "2026-08-01T00:00:00Z",
      "generator": {"name": "MockUE-Test", "version": "0.1"},
      "source": {
        "map_package": "/Game/Campus/Maps/L_ResearchPark",
        "map_name": "L_ResearchPark"
      },
      "coordinate_system": {
        "space": "UE5_world",
        "distance_unit": "centimeter",
        "rotation_unit": "degree",
        "rotation_order": "pitch_yaw_roll"
      },
      "zones": [
        {
          "id": "lab",
          "editor_label": "Zone_Lab",
          "actor_path": "/Game/Campus/Maps/L_ResearchPark.Zone_Lab",
          "bounds": {"center": [0, 20000, 0], "extent": [6000, 5000, 500], "rotation": [0, 0, 0]},
          "entry_point": [0, 15000, 100],
          "entry_facing": [0, 1, 0]
        },
        {
          "id": "library",
          "editor_label": "Zone_Library",
          "actor_path": "/Game/Campus/Maps/L_ResearchPark.Zone_Library",
          "bounds": {"center": [15000, 20000, 0], "extent": [5000, 5000, 500], "rotation": [0, 0, 0]},
          "entry_point": [10000, 20000, 100],
          "entry_facing": [1, 0, 0]
        },
        {
          "id": "cafeteria",
          "editor_label": "Zone_Cafeteria",
          "actor_path": "/Game/Campus/Maps/L_ResearchPark.Zone_Cafeteria",
          "bounds": {"center": [15000, 5000, 0], "extent": [5000, 4000, 500], "rotation": [0, 0, 0]},
          "entry_point": [15000, 9000, 100],
          "entry_facing": [0, 1, 0]
        },
        {
          "id": "dorm",
          "editor_label": "Zone_Dorm",
          "actor_path": "/Game/Campus/Maps/L_ResearchPark.Zone_Dorm",
          "bounds": {"center": [0, 5000, 0], "extent": [4000, 4000, 500], "rotation": [0, 0, 0]},
          "entry_point": [0, 9000, 100],
          "entry_facing": [0, 1, 0]
        }
      ],
      "objects": [
        {
          "id": "experiment_bench",
          "category": "lab_equipment",
          "zone_id": "lab",
          "editor_label": "BP_ExperimentBench",
          "actor_class": "/Game/Campus/Objects/BP_ExperimentBench.BP_ExperimentBench_C",
          "actor_position": [0, 20000, 100],
          "interaction_point": [-500, 19500, 100],
          "interaction_facing": [1, 0, 0],
          "available_interactions": ["experiment", "calibrate", "inspect"],
          "default_state": "idle"
        },
        {
          "id": "book_shelf_01",
          "category": "furniture",
          "zone_id": "library",
          "editor_label": "BP_BookShelf01",
          "actor_class": "/Game/Campus/Objects/BP_BookShelf.BP_BookShelf_C",
          "actor_position": [15000, 20000, 100],
          "interaction_point": [14500, 20000, 100],
          "interaction_facing": [1, 0, 0],
          "available_interactions": ["browse", "borrow"],
          "default_state": "stocked"
        },
        {
          "id": "dining_table_01",
          "category": "dining",
          "zone_id": "cafeteria",
          "editor_label": "BP_DiningTable01",
          "actor_class": "/Game/Campus/Objects/BP_DiningTable.BP_DiningTable_C",
          "actor_position": [15000, 5000, 100],
          "interaction_point": [15000, 5500, 100],
          "interaction_facing": [0, -1, 0],
          "available_interactions": ["dine", "clean"],
          "default_state": "clean"
        }
      ],
      "agents": [
        {
          "id": "R-01",
          "type": "humanoid",
          "initial_zone": "lab",
          "editor_label": "BP_Researcher",
          "actor_class": "/Game/Campus/Agents/BP_Researcher.BP_Researcher_C",
          "initial_position": [0, 19000, 100],
          "action_table": "/Game/Campus/AI/DT_ActionBTMap.DT_ActionBTMap",
          "main_behavior_tree": "/Game/Campus/AI/BT_CampusMain.BT_CampusMain"
        }
      ],
      "validation_summary": {"errors": 0, "warnings": 0}
    },
    "authored": {
      "version": "1.0",
      "narrative": {
        "setting": "多区域研究园区",
        "theme": "由实验室、图书馆、餐厅和宿舍四个区域构成的小型封闭研究社区。"
      },
      "zones": {
        "lab": {
          "display_name": "综合实验室",
          "description": "园区核心实验区，配备通用实验台和仪器。",
          "aliases": ["实验室"],
          "connections": [
            {"to": "library", "type": "corridor", "bidirectional": true},
            {"to": "cafeteria", "type": "corridor", "bidirectional": true}
          ]
        },
        "library": {
          "display_name": "园区图书馆",
          "description": "收藏文献与档案的安静区域，附设阅读位。",
          "aliases": ["图书馆", "书库"],
          "connections": [
            {"to": "lab", "type": "corridor", "bidirectional": true},
            {"to": "cafeteria", "type": "corridor", "bidirectional": true}
          ]
        },
        "cafeteria": {
          "display_name": "公共餐厅",
          "description": "供应三餐和茶歇的公共用餐区。",
          "aliases": ["餐厅", "食堂"],
          "connections": [
            {"to": "library", "type": "corridor", "bidirectional": true},
            {"to": "dorm", "type": "road", "bidirectional": true},
            {"to": "lab", "type": "corridor", "bidirectional": true}
          ]
        },
        "dorm": {
          "display_name": "员工宿舍",
          "description": "园区常驻人员的居住区，配休息舱。",
          "aliases": ["宿舍", "居住区"],
          "connections": [
            {"to": "cafeteria", "type": "road", "bidirectional": true}
          ]
        }
      },
      "objects": {
        "experiment_bench": {
          "display_name": "通用实验台",
          "description": "支持多类实验的标准化实验台。",
          "tags": ["research", "equipment"]
        },
        "book_shelf_01": {
          "display_name": "主书架",
          "description": "图书馆中央书架，收录核心文献。",
          "tags": ["furniture", "books"]
        },
        "dining_table_01": {
          "display_name": "一号用餐桌",
          "description": "餐厅靠窗的双人用餐桌。",
          "tags": ["dining", "furniture"]
        }
      },
      "agents": {
        "R-01": {
          "display_name": "研究员阿明",
          "description": "园区常驻研究员，专注实验设计与数据分析。",
          "profession": "研究员",
          "personality": {
            "traits": ["专注", "理性", "偶尔健谈"],
            "speech_style": "术语较多，逻辑清晰"
          },
          "initial_zone": "lab",
          "relationships": []
        }
      }
    }
  }
}
```

---

## 三份消息对比速查

| 维度 | 消息一（最小） | 消息二（车间） | 消息三（园区） |
|------|---------------|---------------|---------------|
| zones 数 | 1 | 2 | 4 |
| objects 数 | 1 | 2 | 3 |
| agents 数 | 1 | 1 | 1 |
| connections 边数 | 0 | 2（双向 door） | 7（四边形 + 食堂中枢） |
| relationships 数 | 0 | 0 | 0 |
| connection `type` 取值 | — | `door` | `corridor` / `road` |
| relationship `type` 取值 | — | — | — |
| 验证场景 | 烟测、协议最小闭合 | 多 zone 拓扑、单 Agent 跨 zone 行动 | 复杂拓扑、多 zone 排序、不同 profession 的 prompt 生成 |

> **第一期单 Agent 约束**：三份消息均只含 1 个 agent（`T-01` / `H-01` / `R-01`），`relationships` 一律留空。多 Agent 与关系数据通路留待后续里程碑启用。

## 使用方式

### 手工推送到运行中的 MCP（启动窗口内）

```bash
# 假设 MCP 已启动但 Mock UE 尚未连接（启动窗口未关闭）
# 用 wscat 或 Python websockets 推送消息一：
wscat -c ws://localhost:9091/ws < docs/AgentTown_WorldKB_TestMessages.jsonl
```

或用 Python 一行推送（从本文档解析 JSON 后发送）：

```python
import asyncio, json, websockets

async def push():
    msg = json.load(open("docs/AgentTown_WorldKB_TestMessages.jsonl"))  # 选一条
    async with websockets.connect("ws://localhost:9091/ws") as ws:
        await ws.send(json.dumps(msg, ensure_ascii=False))

asyncio.run(push())
```

### 作为 Go 测试 fixture

`payload` 字段可直接 unmarshal 进 `protocol.WorldKBPayload`，再调 `worldkb.MergeAndWriteBytes(payload.Generated, payload.Authored, ...)` 验证合并管线：

```go
var env protocol.Envelope
json.Unmarshal(rawMsg, &env)
var wkb protocol.WorldKBPayload
json.Unmarshal(env.Payload, &wkb)
kb, err := worldkb.MergeAndWriteBytes(wkb.Generated, wkb.Authored, tmpPath, "")
// assert kb.Zones/Objects/Agents 数量符合预期
```

### 启动窗口注意

`world_kb` 必须在 `agent_registered` 之前推送，否则 MCP 会以 `errAgentWindowClosed` 拒绝并记录 warn 日志。手工测试时可：

1. 启动 MCP（不带 Mock UE）
2. 用 wscat / 脚本推送 `world_kb`
3. 再推送 `agent_registered` 触发 worker 启动
4. 观察 MCP 日志确认 KB 已合并落盘

## 相关代码索引

| 文件 | 说明 |
|------|------|
| `agenttown-mcp/pkg/protocol/envelope.go` | 7 字段信封 + `TypeWorldKB` 常量 |
| `agenttown-mcp/pkg/protocol/messages.go:238` | `WorldKBPayload` 结构定义 |
| `agenttown-mcp/pkg/worldkb/schema.go` | `GeneratedDoc` / `AuthoredDoc` JSON schema |
| `agenttown-mcp/pkg/worldkb/merger.go` | `Merge()` + `MergeAndWriteBytes()` 合并管线 |
| `agenttown-mcp/pkg/worldkb/validator.go` | `Validate()` 校验规则 |
| `agenttown-mcp/cmd/agenttown-mcp/main.go` | `worldKBSwap()` handler + 启动窗口守卫 |
| `src/agenttown/mock_ue.py:_send_world_kb` | Mock UE 真实发送实现（可对照） |
