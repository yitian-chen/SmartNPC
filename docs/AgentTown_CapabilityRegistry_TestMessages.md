# AgentTown `capability_registry` 测试消息集

## 文档目的

本文提供三份不同 NPC 的 `capability_registry` 测试消息，格式与 UE 通过 WebSocket 推送给 MCP 的真实消息完全一致（7 字段信封 + `CapabilityRegistryPayload`）。可用于：

- 端到端联调：手工通过 `wscat` / Python `websockets` 推送到 MCP 验证工具增删与战术层 prompt 生成
- 单元测试 fixture：直接复制粘贴为 Go / Python 测试的 payload 输入
- 协议演示：理解 `capability_registry` 消息的完整结构与 per-agent override 语义

## 消息格式约定

每条消息是符合 `pkg/protocol/envelope.go` 的 7 字段信封，`type="capability_registry"`，`payload` 为 `CapabilityRegistryPayload`（见 `pkg/protocol/messages.go:205`）：

```jsonc
{
  "version": "1.0",                    // 协议版本，固定 "1.0"
  "msg_id": "<UUID v4>",               // 每条消息唯一 ID
  "seq": <int>,                        // per-sender 单调递增
  "timestamp": <unix_ms>,              // Unix 毫秒
  "type": "capability_registry",
  "agent_id": "<agent_id>",            // "system" = 全局默认；具体 ID = per-agent override
  "payload": {
    "actions": [
      {
        "cmd": "<Cmd*>",              // 14 种 cmd 之一，见 envelope.go Cmd* 常量
        "kind": "atomic|composite",
        "description": "<人类/LLM 可读说明>",
        "usage_hint": "<可选用法提示>",           // omitempty
        "estimated_duration_sec": <int>,          // omitempty
        "params": [                               // omitempty
          {
            "name": "<参数名>",
            "type": "string|number|bool|vector|enum",  // 见 §2.4 CapabilityParam
            "description": "<可选参数说明>",       // omitempty
            "required": <bool>,
            "default_value": "<可选默认值字符串>",  // omitempty
            "enum_values": ["<可选枚举值>"]        // omitempty, type=enum 时使用
          }
        ]
      }
    ]
  }
}
```

**`agent_id` 语义**（见 `capability.go:37` `Register`）：

| `agent_id` | 含义 | 覆盖行为 |
|------------|------|----------|
| `"system"`（或空） | 全局默认 | 整体替换 `global` 表，所有未发送 per-agent override 的 agent 都用这份 |
| 具体 ID（如 `"H-01"`） | per-agent override | 整体替换该 agent 的 override 表（**不是**对 global 的增量补充，而是**全量替换**） |

**Effective actions 算法**（`capability.go:61` `EffectiveActions`）：

```
if 存在 per-agent override(agentID):
    return per-agent override（全量替换 global）
else:
    return global default
```

> 即：per-agent 一旦发送，就**完全替代** global，而不是叠加。若要让某 agent 保留 global 的所有 cmd 再额外加一个，必须在 per-agent 消息里把所有 cmd 全部列出。

**14 种 cmd 常量**（`envelope.go` Cmd* 常量）：

| 常量 | 值 | kind | 说明 |
|------|----|------|------|
| `CmdMoveToLocation` | `MoveToLocation` | atomic | 移动到静态坐标（dest + speed） |
| `CmdMoveToAgent` | `MoveToAgent` | atomic | 跟随动态目标 Agent（target_agent_id + speed + stop_distance + keep_following） |
| `CmdTurnTo` | `TurnTo` | atomic | 转向目标 Agent 或指定方向（target_agent_id 或 direction） |
| `CmdPlayMontage` | `PlayMontage` | atomic | 播放蒙太奇动画（montage_id + wait_finish） |
| `CmdSpeak` | `Speak` | atomic | 对目标说话（content + target_agent_id + audio_url） |
| `CmdEmote` | `Emote` | atomic | 表现情绪表情（emotion + mode） |
| `CmdWait` | `Wait` | atomic | 原地等待（duration_sec） |
| `CmdInteractSmartObject` | `InteractSmartObject` | atomic | 与智能对象交互（target_object_id + interaction） |
| `CmdWorkAtWorkbench` | `WorkAtWorkbench` | composite | 在指定工作台工作（target_object_id + duration_sec?） |
| `CmdWorkAtWorkshop` | `WorkAtWorkshop` | composite | 车间例行工作（无必填参数） |
| `CmdChatWith` | `ChatWith` | composite | 与其他 Agent 聊天（target_agent_id + topic?） |
| `CmdRepairTarget` | `RepairTarget` | composite | 修理目标 Agent（target_agent_id + tool_id?） |
| `CmdChargeAtStation` | `ChargeAtStation` | composite | 充电（target_object_id?） |
| `CmdPatrolZone` | `PatrolZone` | composite | 巡逻区域（target_zone + duration_sec?） |

> **注意**：旧版 9 cmd 中的 `MoveTo` / `PlayAnimation` / `ExecuteComposite` / `Stop` 已于 2026-08 协议重构中删除。`Stop` 不再作为 cmd 存在——MCP 的 `stop` 工具通过控制消息 `TypeStopAction` 下发，**不依赖**任何 `Cmd*` 常量（`registry.go` 中 `RequiredCmd=""`），因此 `capability_registry` 无需声明 `Stop`。

**CapabilityParam schema**（`messages.go:222`，按文档 §2.4）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 参数名 |
| `type` | string | `"string"` / `"number"` / `"bool"` / `"vector"` / `"enum"` |
| `description` | string | 可选，参数说明 |
| `required` | bool | 是否必填 |
| `default_value` | string | 可选，默认值（字符串形式） |
| `enum_values` | []string | 可选，`type="enum"` 时的合法取值列表 |

**动态字段说明**：`msg_id` / `seq` / `timestamp` 在真实发送时由发送方实时填充，本文用占位值标注。

---

## 测试消息一：全局默认 capability（agent_id="system"，14 cmd 全集）

**用途**：设置全局默认能力集，作为所有未声明 per-agent override 的 NPC 的 fallback。内容与 `BuiltinCmdCapabilities`（`capability.go:145`）和 Mock UE 的 `DEFAULT_CAPABILITY_ACTIONS`（`mock_ue.py:82`）一致。

**特点**：
- `agent_id="system"` 写入 global 表
- 完整 14 cmd（8 atomic + 6 composite）
- 与 `BuiltinCmdCapabilities` seed 对齐，UE 连接时发送此消息可覆盖 seed 成为权威声明

```json
{
  "version": "1.0",
  "msg_id": "11111111-0000-4000-8000-000000000001",
  "seq": 1,
  "timestamp": 1722470400000,
  "type": "capability_registry",
  "agent_id": "system",
  "payload": {
    "actions": [
      {
        "cmd": "MoveToLocation",
        "kind": "atomic",
        "description": "移动到静态坐标",
        "usage_hint": "需要到达某个位置时使用；dest 由 MCP 解析为 [x,y,z] 坐标",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "dest", "type": "vector", "description": "目标世界坐标 [x,y,z]，单位为厘米", "required": true},
          {"name": "speed", "type": "enum", "description": "移动速度档位", "required": false, "default_value": "walk", "enum_values": ["walk", "run"]}
        ]
      },
      {
        "cmd": "MoveToAgent",
        "kind": "atomic",
        "description": "移动到动态 agent 身边",
        "usage_hint": "需要靠近或跟随其他 agent 时使用",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "目标 agent ID", "required": true},
          {"name": "speed", "type": "enum", "description": "移动速度档位", "required": false, "default_value": "walk", "enum_values": ["walk", "run"]},
          {"name": "stop_distance", "type": "number", "description": "停止距离（厘米）", "required": false, "default_value": "150"},
          {"name": "keep_following", "type": "bool", "description": "true=持续跟随；false=到达后停止", "required": false, "default_value": "false"}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向目标",
        "usage_hint": "需要转向某个 agent 或方向时使用",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "目标 agent ID（与 direction 二选一）", "required": false},
          {"name": "direction", "type": "vector", "description": "方向向量 [dx,dy,dz]（与 target_agent_id 二选一）", "required": false}
        ]
      },
      {
        "cmd": "PlayMontage",
        "kind": "atomic",
        "description": "播放蒙太奇动画",
        "usage_hint": "需要播放特定动画时使用；空闲情绪表达优先用 Emote",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "montage_id", "type": "string", "description": "蒙太奇动画 ID", "required": true},
          {"name": "wait_finish", "type": "bool", "description": "是否等待动画播放完成", "required": false, "default_value": "true"}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对目标说话",
        "usage_hint": "target_agent_id 可空表示自言自语；content 控制话语长度",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容", "required": true},
          {"name": "target_agent_id", "type": "string", "description": "对话目标 agent ID（可空）", "required": false},
          {"name": "audio_url", "type": "string", "description": "可选音频 URL", "required": false}
        ]
      },
      {
        "cmd": "Emote",
        "kind": "atomic",
        "description": "表现情绪表情",
        "usage_hint": "mode=oneshot 一次性表情；mode=sustained 持续到下次覆盖",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "emotion", "type": "string", "description": "情绪类型", "required": true},
          {"name": "mode", "type": "enum", "description": "oneshot 或 sustained", "required": false, "default_value": "oneshot", "enum_values": ["oneshot", "sustained"]}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "原地等待",
        "usage_hint": "duration_sec 上限 600；更长等待应使用复合行为",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "number", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "InteractSmartObject",
        "kind": "atomic",
        "description": "与智能对象交互",
        "usage_hint": "target_object_id 必须存在于 world_kb.objects；interaction 取值见该对象的 available_interactions",
        "estimated_duration_sec": 15,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "智能对象 ID", "required": true},
          {"name": "interaction", "type": "string", "description": "交互动作", "required": true}
        ]
      },
      {
        "cmd": "WorkAtWorkbench",
        "kind": "composite",
        "description": "在工作台装配",
        "usage_hint": "target_object_id 为工作台 ID；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "工作台 ID", "required": true},
          {"name": "duration_sec", "type": "number", "description": "持续秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "WorkAtWorkshop",
        "kind": "composite",
        "description": "车间例行工作",
        "usage_hint": "无需特定目标；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "duration_sec", "type": "number", "description": "持续秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "ChatWith",
        "kind": "composite",
        "description": "与其他 agent 聊天",
        "usage_hint": "target_agent_id 必填；topic 可选",
        "estimated_duration_sec": 300,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "聊天目标 agent ID", "required": true},
          {"name": "topic", "type": "string", "description": "聊天话题（可选）", "required": false}
        ]
      },
      {
        "cmd": "RepairTarget",
        "kind": "composite",
        "description": "修理目标 agent",
        "usage_hint": "target_agent_id 必填；tool_id 可选",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "要修理的 agent ID", "required": true},
          {"name": "tool_id", "type": "string", "description": "工具 ID（可选）", "required": false}
        ]
      },
      {
        "cmd": "ChargeAtStation",
        "kind": "composite",
        "description": "充电",
        "usage_hint": "target_object_id 可空（自动选最近充电站）；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "充电站 ID（可空）", "required": false},
          {"name": "duration_sec", "type": "number", "description": "充电秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "PatrolZone",
        "kind": "composite",
        "description": "巡逻区域",
        "usage_hint": "target_zone 必填；duration_sec 可选",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "target_zone", "type": "string", "description": "巡逻区域 ID", "required": true},
          {"name": "duration_sec", "type": "number", "description": "巡逻秒数（可选）", "required": false}
        ]
      }
    ]
  }
}
```

---

## 测试消息二：H-01 车间主管机器人（per-agent override，12 cmd 工业场景）

**用途**：声明 `H-01` 老陈的能力集，**全量替换** global 默认（不是叠加）。车间工业机器人不需要 `PlayMontage`（不演动画）和 `Emote`（不表达情绪），但需要全部 6 个复合行为执行装配/巡检/充电/维修等长耗时任务。

**特点**：
- `agent_id="H-01"` 写入 `perAgent["H-01"]` override 表
- 12 cmd：8 atomic 中去掉 `PlayMontage` / `Emote`，保留全部 6 个 composite
- `MoveToLocation` 的 `description` 强调车间场景化定位
- `Speak` 的 `description` 强调工作指令/简短交流，与老陈"惜字如金"人设对齐
- `InteractSmartObject` 的 `description` 强调操作装配台/充电桩等车间设备
- `WorkAtWorkbench` 的 `description` 强调主装配作业

**生效效果**：
- `HasCmd("H-01", "PlayMontage")` → `false`（MCP 拒绝下发该 cmd，`play_montage` 工具从战术层 prompt 中移除）
- `HasCmd("H-01", "Emote")` → `false`（`emote` 工具从战术层 prompt 中移除）
- `HasCmd("H-01", "WorkAtWorkbench")` → `true`
- 战术层 prompt 中给 `H-01` 列出的可用工具会去掉 `play_montage` / `emote` 对应的 MCP 工具，保留全部 6 个复合行为工具
- 其他 agent（如 `T-01`）未发 override 时仍用 global 默认

```json
{
  "version": "1.0",
  "msg_id": "22222222-0000-4000-8000-000000000002",
  "seq": 2,
  "timestamp": 1722470400000,
  "type": "capability_registry",
  "agent_id": "H-01",
  "payload": {
    "actions": [
      {
        "cmd": "MoveToLocation",
        "kind": "atomic",
        "description": "移动到车间内的目标位置或语义目标",
        "usage_hint": "dest 可填 zones/objects 中的 ID 或语义名称，由 MCP 解析为 [x,y,z] 坐标",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "dest", "type": "vector", "description": "目标世界坐标 [x,y,z]，单位为厘米", "required": true},
          {"name": "speed", "type": "enum", "description": "移动速度档位", "required": false, "default_value": "walk", "enum_values": ["walk", "run"]}
        ]
      },
      {
        "cmd": "MoveToAgent",
        "kind": "atomic",
        "description": "移动到车间内其他人员身边，用于维修靠近或协同作业",
        "usage_hint": "目标可能移动时使用",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "目标 agent ID", "required": true},
          {"name": "speed", "type": "enum", "description": "移动速度档位", "required": false, "default_value": "walk", "enum_values": ["walk", "run"]},
          {"name": "stop_distance", "type": "number", "description": "停止距离（厘米）", "required": false, "default_value": "150"},
          {"name": "keep_following", "type": "bool", "description": "true=持续跟随；false=到达后停止", "required": false, "default_value": "false"}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向车间内的目标",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "目标 agent ID（与 direction 二选一）", "required": false},
          {"name": "direction", "type": "vector", "description": "方向向量 [dx,dy,dz]（与 target_agent_id 二选一）", "required": false}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对车间内其他人员说话，用于工作指令或简短交流",
        "usage_hint": "简短指令为主；target_agent_id 可空表示广播",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容（简短指令为主）", "required": true},
          {"name": "target_agent_id", "type": "string", "description": "对话目标 ID（可空，空表示自言自语）", "required": false},
          {"name": "audio_url", "type": "string", "description": "可选音频 URL", "required": false}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "在车间原地等待，常用于等待物料或下一道工序",
        "usage_hint": "duration_sec 上限 600；更长等待应使用复合行为",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "number", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "InteractSmartObject",
        "kind": "atomic",
        "description": "与车间智能对象交互，如操作装配台、使用充电桩、开启设备",
        "usage_hint": "target_object_id 取值见 world_kb.objects；interaction 取值见该对象的 available_interactions",
        "estimated_duration_sec": 15,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "智能对象 ID（如 workbench_01 / charging_station_01）", "required": true},
          {"name": "interaction", "type": "string", "description": "交互动作（如 assemble / charge / inspect）", "required": true}
        ]
      },
      {
        "cmd": "WorkAtWorkbench",
        "kind": "composite",
        "description": "执行车间主装配作业：前往指定工作台并完成装配流程",
        "usage_hint": "target_object_id 为工作台 ID；duration_sec 可选",
        "estimated_duration_sec": 7200,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "工作台 ID", "required": true},
          {"name": "duration_sec", "type": "number", "description": "持续秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "WorkAtWorkshop",
        "kind": "composite",
        "description": "车间例行工作：选择可用工作台执行一般作业",
        "usage_hint": "无具体目标工作台时使用",
        "estimated_duration_sec": 7200,
        "params": [
          {"name": "duration_sec", "type": "number", "description": "持续秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "ChatWith",
        "kind": "composite",
        "description": "与车间内其他人员交流，用于交接班、请教技术问题",
        "usage_hint": "target_agent_id 必填；topic 可选",
        "estimated_duration_sec": 300,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "聊天目标 agent ID", "required": true},
          {"name": "topic", "type": "string", "description": "聊天话题（可选）", "required": false}
        ]
      },
      {
        "cmd": "RepairTarget",
        "kind": "composite",
        "description": "维修车间内故障机器人，包含接近、检查、更换零件流程",
        "usage_hint": "target_agent_id 必填；tool_id 可选",
        "estimated_duration_sec": 1800,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "待修理 agent ID", "required": true},
          {"name": "tool_id", "type": "string", "description": "工具 ID（可选）", "required": false}
        ]
      },
      {
        "cmd": "ChargeAtStation",
        "kind": "composite",
        "description": "前往充电桩充电，持续到电量满足",
        "usage_hint": "target_object_id 可空（自动选最近充电站）",
        "estimated_duration_sec": 3600,
        "params": [
          {"name": "target_object_id", "type": "string", "description": "充电站 ID（可空）", "required": false},
          {"name": "duration_sec", "type": "number", "description": "充电秒数（可选）", "required": false}
        ]
      },
      {
        "cmd": "PatrolZone",
        "kind": "composite",
        "description": "巡检车间指定区域，按区域策略巡逻",
        "usage_hint": "target_zone 必填；duration_sec 可选",
        "estimated_duration_sec": 1800,
        "params": [
          {"name": "target_zone", "type": "string", "description": "巡逻区域 ID", "required": true},
          {"name": "duration_sec", "type": "number", "description": "巡逻秒数（可选）", "required": false}
        ]
      }
    ]
  }
}
```

---

## 测试消息三：T-01 服务接待机器人（per-agent override，7 cmd 服务场景）

**用途**：声明 `T-01` 服务机器人的能力集，**全量替换** global 默认。服务接待机器人不需要 6 个工业复合行为（不执行装配/巡检/充电/维修等工业任务）和 `InteractSmartObject`（不操作工业设备），但需要 `PlayMontage`（欢迎手势）和 `Emote`（服务态度表达）。同时不需要 `MoveToAgent`（不跟随客户）。

**特点**：
- `agent_id="T-01"` 写入 `perAgent["T-01"]` override 表
- 7 cmd：`MoveToLocation` / `TurnTo` / `PlayMontage` / `Speak` / `Emote` / `Wait` / `ChatWith`
- **删除** 5 个工业相关 cmd：`MoveToAgent`（不跟随） / `InteractSmartObject`（不操作设备） / `WorkAtWorkbench` / `WorkAtWorkshop` / `RepairTarget` / `ChargeAtStation` / `PatrolZone`
- `PlayMontage` 的 `description` 强调欢迎/引导手势
- `Emote` 的 `description` 强调服务态度（亲切/耐心/抱歉）
- `Speak` 的 `description` 强调接待问答
- `MoveToLocation` 的 `usage_hint` 指向接待区/休息区等服务场景
- 保留 `ChatWith` 用于与访客进行多轮对话

**生效效果**：
- `HasCmd("T-01", "WorkAtWorkbench")` → `false`（`work_at_workbench` 工具从战术层 prompt 中移除）
- `HasCmd("T-01", "InteractSmartObject")` → `false`（`interact` 工具从战术层 prompt 中移除）
- `HasCmd("T-01", "PatrolZone")` → `false`（`patrol_zone` 工具从战术层 prompt 中移除）
- `HasCmd("T-01", "PlayMontage")` → `true`（`play_montage` 工具保留）
- `HasCmd("T-01", "Emote")` → `true`（`emote` 工具保留）
- 战术层 prompt 中给 `T-01` 列出的可用工具会去掉 `work_at_workbench` / `work_at_workshop` / `repair_target` / `charge_at_station` / `patrol_zone` / `interact` / `move_to_agent` 等 7 个工具，保留 `play_montage` / `emote` / `chat_with` 等

```json
{
  "version": "1.0",
  "msg_id": "33333333-0000-4000-8000-000000000003",
  "seq": 3,
  "timestamp": 1722470400000,
  "type": "capability_registry",
  "agent_id": "T-01",
  "payload": {
    "actions": [
      {
        "cmd": "MoveToLocation",
        "kind": "atomic",
        "description": "移动到接待区、休息区等服务场景的目标位置",
        "usage_hint": "dest 可填 zone ID（如 lobby / lounge）或语义名称，由 MCP 解析为坐标",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "dest", "type": "vector", "description": "目标世界坐标 [x,y,z]，单位为厘米", "required": true},
          {"name": "speed", "type": "enum", "description": "移动速度档位", "required": false, "default_value": "walk", "enum_values": ["walk", "run"]}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向访客或引导方向",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "目标 agent ID（与 direction 二选一）", "required": false},
          {"name": "direction", "type": "vector", "description": "方向向量 [dx,dy,dz]（与 target_agent_id 二选一）", "required": false}
        ]
      },
      {
        "cmd": "PlayMontage",
        "kind": "atomic",
        "description": "播放欢迎、引导、指示等接待手势动画",
        "usage_hint": "montage_id 取值如 wave / bow / point_to_direction",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "montage_id", "type": "string", "description": "蒙太奇动画 ID（如 wave / bow / point_to_direction）", "required": true},
          {"name": "wait_finish", "type": "bool", "description": "是否等待动画播放完成", "required": false, "default_value": "true"}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对访客说话，用于接待问答、引导说明、信息广播",
        "usage_hint": "target_agent_id 可空表示广播",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容（接待话术为主）", "required": true},
          {"name": "target_agent_id", "type": "string", "description": "对话目标 ID（访客 ID，可空表示广播）", "required": false},
          {"name": "audio_url", "type": "string", "description": "可选音频 URL", "required": false}
        ]
      },
      {
        "cmd": "Emote",
        "kind": "atomic",
        "description": "表现服务态度情绪（亲切、耐心、抱歉、欢迎等）",
        "usage_hint": "mode=oneshot 一次性表情；mode=sustained 持续到下次覆盖",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "emotion", "type": "string", "description": "情绪类型（如 friendly / patient / apologetic / welcoming）", "required": true},
          {"name": "mode", "type": "enum", "description": "oneshot 或 sustained", "required": false, "default_value": "oneshot", "enum_values": ["oneshot", "sustained"]}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "在接待位原地等待访客，常用于空闲待命",
        "usage_hint": "duration_sec 上限 600",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "number", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "ChatWith",
        "kind": "composite",
        "description": "与访客进行多轮对话，包含接近、面对面交流、结束交流",
        "usage_hint": "target_agent_id 必填；topic 可选",
        "estimated_duration_sec": 300,
        "params": [
          {"name": "target_agent_id", "type": "string", "description": "对话目标 agent ID", "required": true},
          {"name": "topic", "type": "string", "description": "聊天话题（可选）", "required": false}
        ]
      }
    ]
  }
}
```

---

## 三份消息对比速查

| 维度 | 消息一（全局默认） | 消息二（H-01 车间主管） | 消息三（T-01 服务接待） |
|------|---------------------|------------------------|------------------------|
| `agent_id` | `"system"` | `"H-01"` | `"T-01"` |
| 写入位置 | `global` 表 | `perAgent["H-01"]` override | `perAgent["T-01"]` override |
| 覆盖语义 | 替换全局默认 | 全量替换 H-01 的 effective set | 全量替换 T-01 的 effective set |
| cmd 数 | 14 | 12 | 7 |
| 原子 cmd（8） | 全部 | 去掉 `PlayMontage` / `Emote` | 保留 `PlayMontage` / `Emote`，去掉 `MoveToAgent` / `InteractSmartObject` |
| 复合 cmd（6） | 全部 | 全部 | 仅 `ChatWith` |
| `MoveToLocation` | ✅ | ✅（车间场景化描述） | ✅（接待场景化描述） |
| `MoveToAgent` | ✅ | ✅（维修靠近） | ❌（不跟随客户） |
| `TurnTo` | ✅ | ✅ | ✅ |
| `PlayMontage` | ✅ | ❌（车间机器人不演动画） | ✅（欢迎手势） |
| `Speak` | ✅ | ✅（工作指令） | ✅（接待问答） |
| `Emote` | ✅ | ❌（不表达情绪） | ✅（服务态度） |
| `Wait` | ✅ | ✅（等物料） | ✅（待命） |
| `InteractSmartObject` | ✅ | ✅（操作装配台/充电桩） | ❌（不操作工业设备） |
| `WorkAtWorkbench` | ✅ | ✅（主装配作业） | ❌ |
| `WorkAtWorkshop` | ✅ | ✅ | ❌ |
| `ChatWith` | ✅ | ✅（交接班/技术交流） | ✅（多轮对话） |
| `RepairTarget` | ✅ | ✅（维修机器人） | ❌ |
| `ChargeAtStation` | ✅ | ✅ | ❌ |
| `PatrolZone` | ✅ | ✅（巡检车间） | ❌ |
| `params.enum_values` 用法 | `speed` / `mode` | `speed` | `speed` / `mode` |
| `params.default_value` 用法 | `speed` / `stop_distance` / `keep_following` / `wait_finish` / `mode` | 同消息一 | 同消息一 |
| `usage_hint` 场景化 | 通用 | 车间装配/维修/巡检 | 接待/引导/服务态度 |

> **第一期单 Agent 约束**：三份消息示例均为单 agent 场景设计，`relationships` 一律留空。多 Agent 与 per-agent override 的实际组合留待后续里程碑启用。

## 关键语义说明

### per-agent override 是全量替换，不是叠加

```go
// capability.go:48 — per-agent override 分支
m := make(map[string]protocol.CapabilityAction, len(actions))
for _, a := range actions {
    m[a.Cmd] = a
}
r.perAgent[agentID] = m  // 直接替换，不合并 global
```

**反例**：若想让 `H-01` 在全局默认基础上仅"去掉 `PlayMontage`"，**不能**只发一条只含 13 cmd 的 per-agent 消息——必须列出所有想保留的 cmd（即 13 个）。本文消息二列出 12 cmd 是因为同时去掉了 `PlayMontage` 和 `Emote`。

### `Clear` 在 `agent_unregistered` 时触发

```go
// capability.go:97
func (r *CapabilityRegistry) Clear(agentID string) {
    if agentID == "" || agentID == protocol.SystemAgentID {
        r.global = make(map[string]protocol.CapabilityAction)
        return
    }
    delete(r.perAgent, agentID)
}
```

NPC 下线时 MCP 自动清除其 per-agent override，该 agent ID 重新走 global 默认。

### `effective actions` 驱动战术层 prompt

战术层（`tactical.go`）生成 LLM prompt 时，可用工具列表由 `CapabilityRegistry.EffectiveActions(agentID)` 动态生成。删除一个 cmd 后，战术层 prompt 不会列出依赖该 cmd 的工具，LLM 也就不会调用它。`guardedExecutor.SendAction` 在发送 UE 前还会再校验一次 `HasCmd`，双重保险。

### `stop` 工具不依赖任何 cmd

MCP 的 `stop` 工具（用于打断当前在途动作）通过控制消息 `TypeStopAction` 下发，**不经过** `action_command` 通道，因此 `RequiredCmd=""`（`registry.go:92`）。`capability_registry` 中**无需声明** `Stop` cmd——它始终可用，不受 capability 限制。同样地，`scan_area` 工具也通过控制消息工作，不依赖任何 cmd。

## 使用方式

### 手工推送到运行中的 MCP

`capability_registry` 与 `world_kb` 不同，**没有启动窗口约束**——可以在任何时刻发送，MCP 会即时替换对应 agent 的能力集并触发工具增删。典型顺序：

1. `world_kb`（启动窗口内，首个 `agent_registered` 之前）
2. `capability_registry`（`agent_id="system"` 设置全局默认，或紧随 `agent_registered` 发送 per-agent override）
3. `agent_registered`（每个 NPC 上线时）
4. `capability_registry`（能力变更时随时发送）

```bash
# 用 wscat 推送消息二（H-01 车间主管）：
wscat -c ws://localhost:9091/ws
# 连接后粘贴消息二 JSON 并回车

# 或用 Python 推送：
python3 -c "
import asyncio, json, websockets
async def push():
    msg = json.load(open('docs/AgentTown_CapabilityRegistry_TestMessages.jsonl'))  # 选一条
    async with websockets.connect('ws://localhost:9091/ws') as ws:
        await ws.send(json.dumps(msg, ensure_ascii=False))
asyncio.run(push())
"
```

### 作为 Go 测试 fixture

`payload` 字段可直接 unmarshal 进 `protocol.CapabilityRegistryPayload`，再调 `CapabilityRegistry.Register` 验证 effective set：

```go
var env protocol.Envelope
json.Unmarshal(rawMsg, &env)
var cap protocol.CapabilityRegistryPayload
json.Unmarshal(env.Payload, &cap)

reg := NewCapabilityRegistry()
reg.Register(env.AgentID, cap.Actions)

// 验证 H-01 删除了 PlayMontage
assert.False(t, reg.HasCmd("H-01", protocol.CmdPlayMontage))
assert.True(t,  reg.HasCmd("H-01", protocol.CmdWorkAtWorkbench))

// 验证其他 agent 仍走 global（若已发送消息一）
assert.True(t, reg.HasCmd("T-01", protocol.CmdPlayMontage))
```

### 作为 Python 测试 fixture

Mock UE 的 `_send_capability_registry`（`mock_ue.py:930`）是单条消息的发送实现，可参考它构造自定义 capability 推送逻辑。

## 相关代码索引

| 文件 | 说明 |
|------|------|
| `agenttown-mcp/pkg/protocol/envelope.go` | 7 字段信封 + `TypeCapabilityRegistry` 常量 + 14 个 `Cmd*` 常量 |
| `agenttown-mcp/pkg/protocol/messages.go:205-229` | `CapabilityRegistryPayload` / `CapabilityAction` / `CapabilityParam` 结构定义（`default_value` + `enum_values`） |
| `agenttown-mcp/cmd/agenttown-mcp/capability.go` | `CapabilityRegistry` 实现：`Register` / `EffectiveActions` / `HasCmd` / `Clear` / `Snapshot` + `BuiltinCmdCapabilities` seed（14 cmd） |
| `agenttown-mcp/cmd/agenttown-mcp/main.go:1119` | `case protocol.TypeCapabilityRegistry` handler，调 `tools.ReconcileTools` 动态增删工具 |
| `agenttown-mcp/adapters/agenttown/tools/registry.go:81-103` | `BuiltinToolSpecs()` 16 个工具定义 + `RequiredCmd` 映射 |
| `agenttown-mcp/adapters/agenttown/tools/registry.go:107-130` | `ReconcileTools`：根据 effective actions 增删 MCP 工具 |
| `agenttown-mcp/cmd/agenttown-mcp/tactical.go` | 战术层 prompt 中可用工具列表由 `EffectiveActions` 动态生成 |
| `src/agenttown/mock_ue.py:82-273` | Mock UE `DEFAULT_CAPABILITY_ACTIONS`（14 cmd 全集，真实发送实现可对照） |
