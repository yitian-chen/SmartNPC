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
        "cmd": "<Cmd*>",              // 9 种 cmd 之一，见 envelope.go:76
        "kind": "atomic|composite",
        "description": "<人类/LLM 可读说明>",
        "usage_hint": "<可选用法提示>",           // omitempty
        "estimated_duration_sec": <int>,          // omitempty
        "params": [                               // omitempty
          {
            "name": "<参数名>",
            "type": "string|number|integer|boolean|object|array",
            "description": "<可选参数说明>",       // omitempty
            "required": <bool>,
            "enum": ["<可选枚举值>"]               // omitempty
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

**9 种 cmd 常量**（`envelope.go:76`）：

| 常量 | 值 | kind |
|------|----|------|
| `CmdMoveTo` | `MoveTo` | atomic |
| `CmdTurnTo` | `TurnTo` | atomic |
| `CmdPlayAnimation` | `PlayAnimation` | atomic |
| `CmdSpeak` | `Speak` | atomic |
| `CmdEmote` | `Emote` | atomic |
| `CmdWait` | `Wait` | atomic |
| `CmdInteractSmartObject` | `InteractSmartObject` | atomic |
| `CmdExecuteComposite` | `ExecuteComposite` | composite |
| `CmdStop` | `Stop` | atomic |

**动态字段说明**：`msg_id` / `seq` / `timestamp` 在真实发送时由发送方实时填充，本文用占位值标注。

---

## 测试消息一：全局默认 capability（agent_id="system"，9 cmd 全集）

**用途**：设置全局默认能力集，作为所有未声明 per-agent override 的 NPC 的 fallback。内容与 `BuiltinCmdCapabilities`（`capability.go:115`）和 Mock UE 的 `_send_capability_registry`（`mock_ue.py:503`）一致。

**特点**：
- `agent_id="system"` 写入 global 表
- 完整 9 cmd（7 atomic + 1 composite + 1 Stop）
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
        "cmd": "MoveTo",
        "kind": "atomic",
        "description": "移动到指定目标位置或语义目标",
        "usage_hint": "target 可填 zones/objects 中的 ID 或语义名称",
        "estimated_duration_sec": 30,
        "params": [
          {"name": "target", "type": "string", "description": "目标位置或语义目标 ID", "required": true}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向指定目标",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target", "type": "string", "description": "目标朝向 ID", "required": true}
        ]
      },
      {
        "cmd": "PlayAnimation",
        "kind": "atomic",
        "description": "播放一段动画",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "animation", "type": "string", "description": "动画名称", "required": true}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对目标说话",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容", "required": true},
          {"name": "target", "type": "string", "description": "对话目标 ID", "required": false}
        ]
      },
      {
        "cmd": "Emote",
        "kind": "atomic",
        "description": "表现情绪表情",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "emotion", "type": "string", "description": "情绪类型", "required": true},
          {"name": "mode", "type": "string", "description": "oneshot 或 sustained", "required": false}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "原地等待一段时间",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "integer", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "InteractSmartObject",
        "kind": "atomic",
        "description": "与智能对象交互",
        "estimated_duration_sec": 15,
        "params": [
          {"name": "object_id", "type": "string", "description": "智能对象 ID", "required": true},
          {"name": "action", "type": "string", "description": "交互动作", "required": true}
        ]
      },
      {
        "cmd": "ExecuteComposite",
        "kind": "composite",
        "description": "执行复合行为（封装一段时长内的多步骤活动）",
        "usage_hint": "duration_min 内部 ×60 转 duration_sec",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "action", "type": "string", "description": "复合行为类型", "required": true},
          {"name": "target", "type": "string", "description": "目标 ID", "required": false},
          {"name": "duration_min", "type": "integer", "description": "持续分钟数", "required": false}
        ]
      },
      {
        "cmd": "Stop",
        "kind": "atomic",
        "description": "停止当前在途动作",
        "estimated_duration_sec": 1,
        "params": []
      }
    ]
  }
}
```

---

## 测试消息二：H-01 车间主管机器人（per-agent override，7 cmd 工业场景）

**用途**：声明 `H-01` 老陈的能力集，**全量替换** global 默认（不是叠加）。车间工业机器人不需要 `PlayAnimation` / `Emote`（不演情感戏），但需要 `ExecuteComposite` 执行装配/巡检/充电等长耗时复合行为。

**特点**：
- `agent_id="H-01"` 写入 `perAgent["H-01"]` override 表
- 7 cmd：`MoveTo` / `TurnTo` / `Speak` / `Wait` / `InteractSmartObject` / `ExecuteComposite` / `Stop`
- **删除** `PlayAnimation`（车间机器人不演动画）和 `Emote`（不表达情绪）
- `ExecuteComposite` 的 `description` 改为车间场景化说明（装配/巡检/充电/维修）
- `Speak` 的 `description` 强调工作指令/简短交流，与老陈"惜字如金"人设对齐
- `InteractSmartObject` 的 `description` 强调操作装配台/充电桩等车间设备

**生效效果**：
- `HasCmd("H-01", "PlayAnimation")` → `false`（MCP 拒绝下发该 cmd）
- `HasCmd("H-01", "ExecuteComposite")` → `true`
- 战术层 prompt 中给 `H-01` 列出的可用工具会去掉 `play_animation` / `emote` 对应的 MCP 工具
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
        "cmd": "MoveTo",
        "kind": "atomic",
        "description": "移动到车间内的目标位置或语义目标",
        "usage_hint": "target 可填 zone ID（如 main_workshop）、object ID（如 assemble_station_01）或语义名称",
        "estimated_duration_sec": 30,
        "params": [
          {"name": "target", "type": "string", "description": "目标位置或语义目标 ID", "required": true}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向车间内的目标",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target", "type": "string", "description": "目标朝向 ID", "required": true}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对车间内其他人员说话，用于工作指令或简短交流",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容（简短指令为主）", "required": true},
          {"name": "target", "type": "string", "description": "对话目标 ID（可空，空表示自言自语）", "required": false}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "在车间原地等待，常用于等待物料或下一道工序",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "integer", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "InteractSmartObject",
        "kind": "atomic",
        "description": "与车间智能对象交互，如操作装配台、使用充电桩、开启设备",
        "estimated_duration_sec": 15,
        "params": [
          {"name": "object_id", "type": "string", "description": "智能对象 ID（如 assemble_station_01）", "required": true},
          {"name": "action", "type": "string", "description": "交互动作（如 assemble / charge / inspect）", "required": true}
        ]
      },
      {
        "cmd": "ExecuteComposite",
        "kind": "composite",
        "description": "执行车间复合行为：装配作业、巡检路线、充电、维修、休息等",
        "usage_hint": "action 取值：work_assemble / patrol_route / charge_at / repair_target / rest_idle；duration_min 内部 ×60 转 duration_sec",
        "estimated_duration_sec": 600,
        "params": [
          {"name": "action", "type": "string", "description": "复合行为类型", "required": true, "enum": ["work_assemble", "patrol_route", "charge_at", "repair_target", "rest_idle"]},
          {"name": "target", "type": "string", "description": "目标 ID（如 workbench_01 / route_A / charge_station_01）", "required": false},
          {"name": "duration_min", "type": "integer", "description": "持续分钟数", "required": false}
        ]
      },
      {
        "cmd": "Stop",
        "kind": "atomic",
        "description": "停止当前在途动作（反应层打断或重规划时触发）",
        "estimated_duration_sec": 1,
        "params": []
      }
    ]
  }
}
```

---

## 测试消息三：T-01 服务接待机器人（per-agent override，6 cmd 服务场景）

**用途**：声明 `T-01` 服务机器人的能力集，**全量替换** global 默认。服务接待机器人不需要 `ExecuteComposite`（不执行长耗时工业任务）和 `InteractSmartObject`（不操作工业设备），但需要 `PlayAnimation`（欢迎手势）和 `Emote`（服务态度表达）。

**特点**：
- `agent_id="T-01"` 写入 `perAgent["T-01"]` override 表
- 6 cmd：`MoveTo` / `TurnTo` / `PlayAnimation` / `Speak` / `Emote` / `Wait`（无 `Stop` 仅为示例差异，实际生产建议保留 `Stop`）
  - **注意**：此处刻意保留 `Stop` 以符合反应层打断需求，最终为 **7 cmd**：`MoveTo` / `TurnTo` / `PlayAnimation` / `Speak` / `Emote` / `Wait` / `Stop`
- **删除** `InteractSmartObject`（不操作车间设备）和 `ExecuteComposite`（不执行长耗时复合行为）
- `PlayAnimation` 的 `description` 强调欢迎/引导手势
- `Emote` 的 `description` 强调服务态度（亲切/耐心/抱歉）
- `Speak` 的 `description` 强调接待问答
- `MoveTo` 的 `usage_hint` 指向接待区/休息区等服务场景

**生效效果**：
- `HasCmd("T-01", "ExecuteComposite")` → `false`
- `HasCmd("T-01", "InteractSmartObject")` → `false`
- `HasCmd("T-01", "PlayAnimation")` → `true`
- `HasCmd("T-01", "Emote")` → `true`
- 战术层 prompt 中给 `T-01` 列出的可用工具会去掉 `work_assemble` / `patrol_route` / `charge_at` / `repair_target` / `interact` 等工具，保留 `play_animation` / `emote` 等

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
        "cmd": "MoveTo",
        "kind": "atomic",
        "description": "移动到接待区、休息区等服务场景的目标位置",
        "usage_hint": "target 可填 zone ID（如 lobby / lounge）或语义名称",
        "estimated_duration_sec": 30,
        "params": [
          {"name": "target", "type": "string", "description": "目标位置或语义目标 ID", "required": true}
        ]
      },
      {
        "cmd": "TurnTo",
        "kind": "atomic",
        "description": "转身面向访客或引导方向",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "target", "type": "string", "description": "目标朝向 ID（如访客 ID 或方向标识）", "required": true}
        ]
      },
      {
        "cmd": "PlayAnimation",
        "kind": "atomic",
        "description": "播放欢迎、引导、指示等接待手势动画",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "animation", "type": "string", "description": "动画名称（如 wave / bow / point_to_direction）", "required": true}
        ]
      },
      {
        "cmd": "Speak",
        "kind": "atomic",
        "description": "对访客说话，用于接待问答、引导说明、信息广播",
        "estimated_duration_sec": 10,
        "params": [
          {"name": "content", "type": "string", "description": "说话内容（接待话术为主）", "required": true},
          {"name": "target", "type": "string", "description": "对话目标 ID（访客 ID，可空表示广播）", "required": false}
        ]
      },
      {
        "cmd": "Emote",
        "kind": "atomic",
        "description": "表现服务态度情绪（亲切、耐心、抱歉、欢迎等）",
        "estimated_duration_sec": 5,
        "params": [
          {"name": "emotion", "type": "string", "description": "情绪类型（如 friendly / patient / apologetic / welcoming）", "required": true},
          {"name": "mode", "type": "string", "description": "oneshot 或 sustained", "required": false}
        ]
      },
      {
        "cmd": "Wait",
        "kind": "atomic",
        "description": "在接待位原地等待访客，常用于空闲待命",
        "estimated_duration_sec": 60,
        "params": [
          {"name": "duration_sec", "type": "integer", "description": "等待秒数", "required": true}
        ]
      },
      {
        "cmd": "Stop",
        "kind": "atomic",
        "description": "停止当前在途动作（反应层打断时触发）",
        "estimated_duration_sec": 1,
        "params": []
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
| cmd 数 | 9 | 7 | 7 |
| `MoveTo` | ✅ | ✅（车间场景化描述） | ✅（接待场景化描述） |
| `TurnTo` | ✅ | ✅ | ✅ |
| `PlayAnimation` | ✅ | ❌（车间机器人不演动画） | ✅（欢迎手势） |
| `Speak` | ✅ | ✅（工作指令） | ✅（接待问答） |
| `Emote` | ✅ | ❌（不表达情绪） | ✅（服务态度） |
| `Wait` | ✅ | ✅（等物料） | ✅（待命） |
| `InteractSmartObject` | ✅ | ✅（操作装配台/充电桩） | ❌（不操作工业设备） |
| `ExecuteComposite` | ✅ | ✅（装配/巡检/充电/维修） | ❌（不执行长耗时复合任务） |
| `Stop` | ✅ | ✅ | ✅ |
| `params.enum` 用法 | 未使用 | `ExecuteComposite.action` 用 enum 约束 5 种复合行为 | 未使用 |
| `usage_hint` 用法 | `MoveTo` / `ExecuteComposite` | `MoveTo` / `ExecuteComposite`（含 action 取值列表） | `MoveTo` |

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

**反例**：若想让 `H-01` 在全局默认基础上仅"去掉 `PlayAnimation`"，**不能**只发一条只含 8 cmd 的 per-agent 消息——必须列出所有想保留的 cmd（即 8 个）。本文消息二列出 7 cmd 是因为同时去掉了 `PlayAnimation` 和 `Emote`。

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

## 使用方式

### 手工推送到运行中的 MCP

`capability_registry` 与 `world_kb` 不同，**没有启动窗口约束**——可以在任何时刻发送，MCP 会即时替换对应 agent 的能力集并触发工具增删。典型顺序：

1. `world_kb`（启动窗口内，首个 `agent_registered` 之前）
2. `agent_registered`（每个 NPC 上线时）
3. `capability_registry`（每个 NPC 紧随 `agent_registered` 发送，或能力变更时随时发送）

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

// 验证 H-01 删除了 PlayAnimation
assert.False(t, reg.HasCmd("H-01", protocol.CmdPlayAnimation))
assert.True(t,  reg.HasCmd("H-01", protocol.CmdExecuteComposite))

// 验证其他 agent 仍走 global（若已发送消息一）
assert.True(t, reg.HasCmd("T-01", protocol.CmdPlayAnimation))
```

### 作为 Python 测试 fixture

Mock UE 的 `_send_capability_registry`（`mock_ue.py:503`）是单条消息的发送实现，可参考它构造自定义 capability 推送逻辑。

## 相关代码索引

| 文件 | 说明 |
|------|------|
| `agenttown-mcp/pkg/protocol/envelope.go:60-65` | `TypeCapabilityRegistry` 常量 |
| `agenttown-mcp/pkg/protocol/envelope.go:76-86` | 9 个 `Cmd*` 常量 |
| `agenttown-mcp/pkg/protocol/messages.go:195-226` | `CapabilityRegistryPayload` / `CapabilityAction` / `CapabilityParam` 结构定义 |
| `agenttown-mcp/cmd/agenttown-mcp/capability.go` | `CapabilityRegistry` 实现：`Register` / `EffectiveActions` / `HasCmd` / `Clear` + `BuiltinCmdCapabilities` seed |
| `agenttown-mcp/cmd/agenttown-mcp/main.go` | `case protocol.TypeCapabilityRegistry` handler，调 `tools.ReconcileTools` 动态增删工具 |
| `agenttown-mcp/adapters/agenttown/tools/registry.go` | `ReconcileTools`：根据 effective actions 增删 MCP 工具 |
| `agenttown-mcp/cmd/agenttown-mcp/tactical.go` | 战术层 prompt 中可用工具列表由 `EffectiveActions` 动态生成 |
| `src/agenttown/mock_ue.py:503` | Mock UE 真实发送实现（可对照） |
