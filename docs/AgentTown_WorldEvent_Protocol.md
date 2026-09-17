# AgentTown UE 事件上传协议

> 版本：v1.0（2026-09-17）
> 配套阅读：`AgentTown_EventDrivenAgent_Design.html`（事件驱动异步 Agent 设计，本协议的实现依据）、`AgentTown_CommProtocol_Values.md`（基础通信协议，本文复用其信封与全部约定）。

## 一、定位与设计原则

Agent 侧正在从「轮询感知」演进为「**事件驱动**」：UE 主动把世界上"发生的事"推给 Agent，Agent 侧的反应层据此决定**打断**还是**入队**。本协议定义 UE → Agent 方向的**事件上传通道**。

三条设计原则（来自设计文档第四章，协议层面必须配合）：

1. **事件 ≠ 心跳**：位置/能量等连续量仍走 `perception_update`（只刷状态栏，永不触发打断）；只有"有发生那一刻的事"才走本通道。**连续量上的"跨越"是事件**（能量跨过 20、走进新 zone）。
2. **force 不可否决**：带 `force` 标记的事件是 UE 对 Agent 的硬保证通道——不经过路由、不调 LLM、无任何抑制逻辑，微秒级触发打断。Agent 侧不得为它添加阈值、预算、频次限制。
3. **UE 只报事实，紧急度由 Agent 判**：`severity` 是客观严重度（UE 视角的量级），不是"对某个 NPC 紧不紧急"。同一条故障事件推给所有 NPC，各 NPC 自行结合关系/性格/距离裁决。

## 二、消息封装

### 2.1 信封

复用现有 7 字段信封（约定 1~4 全部适用），新增消息类型 `world_event`：

```json
{
  "version": "1.0",
  "msg_id": "550e8400-e29b-41d4-a716-446655440000",
  "seq": 1024,
  "timestamp": 1719456000000,
  "type": "world_event",
  "agent_id": "H-03",
  "payload": { ... }
}
```

| 字段 | 说明 |
|------|------|
| `type` | 新增常量 `world_event`（UE → Agent） |
| `agent_id` | **接收方** NPC 的 id。广播类事件（一条推给多个 NPC）= 多条独立消息、同一 `event_id`、不同 agent_id |
| 其余 | 与基础协议一致（msg_id 唯一、seq 单调递增、timestamp 毫秒） |

> 广播的实现方式：UE 对每个目标 NPC 各发一条 `world_event`。不引入多播字段，复用现有单连接 + agent_id 路由。

### 2.2 payload 通用结构

所有类别的事件共用一份 payload 结构：

```json
{
  "event_id": "evt_20260917_000001",
  "category": "physical_threshold",
  "event_type": "energy_below",
  "force": false,
  "severity": 4,
  "subject": "H-01",
  "game_time": "D12 10:47:03",
  "location": "main_workshop",
  "occurred_at": 1719456000000,
  "data": { ... }
}
```

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| event_id | string | ✅ | 事件唯一 ID（建议 `evt_` + 递增/时间戳）。同一广播事件推给多个 NPC 时**保持相同 event_id**，Agent 侧据此去重与关联 |
| category | string | ✅ | 事件类别（见 §三，枚举） |
| event_type | string | ✅ | 类别内具体事件类型（见 §三，枚举） |
| force | bool | ✅ | 强制打断标记，默认 false。true = 硬保证通道（见 §四） |
| severity | int | ✅ | UE 视角的客观严重度 **0~10**。10 = 最严重（死亡/被攻击/剧情强制）。仅作路由参考，Agent 侧结合自身状态裁决 |
| subject | string | ❌ | 事件的主体对象 id（谁的能量、谁在搭话、谁故障）。无明确主体时省略 |
| game_time | string | ✅ | 事件发生时刻的**游戏时间**（`"D12 10:47:03"`，D 后为天数）。所有事件以游戏时钟为准 |
| location | string | ❌ | 事件发生位置（zone id 或物体 id） |
| occurred_at | int | ✅ | 事件发生的 Unix epoch 毫秒（真实时间戳，用于排序/延迟测量） |
| data | object | ✅ | 类别专属载荷（见 §三，可为空对象） |

> **约定 22（事件时间口径）**：`game_time` 是决策依据（Agent 侧全链路用游戏时钟）；`occurred_at` 仅用于测量与调试。

### 2.3 消息类型总表增补

在基础协议 §2.2 消息类型总表中追加：

| type | 方向 | 用途 | 触发时机 |
|------|------|------|----------|
| `world_event` | UE → Agent | 世界事件上传（打断或入队的判定输入） | 事件发生那一刻（边沿触发） |

## 三、事件类别与 data 结构

六大类别。`event_type` 为推荐枚举，UE 可按实现补充，但 **category 必须落在这六类之一**（Agent 侧按 category 走对应处理分支）。

### 3.1 physical_threshold（物理跨阈值）

数值跨过预设线的那一刻推一次（**边沿触发，不是电平触发**——禁止持续低于阈值时反复推送）。

实现要点：复用现有 `LastReportedPhysicalState`（原用于算 delta）做**跨越检测**。

| event_type | 说明 | data |
|------------|------|------|
| `energy_below` | 能量跨过下阈值（默认 20） | `{"attribute": "energy", "value": 19.8, "threshold": 20, "direction": "below"}` |
| `energy_above` | 能量恢复跨过上阈值（默认 80） | 同上，`direction: "above"` |
| `fatigue_above` | 疲劳跨过上阈值（默认 70） | `{"attribute": "fatigue", ...}` |
| `joint_wear_above` | 关节磨损跨过上阈值（默认 60） | `{"attribute": "joint_wear", ...}` |
| `money_below` | 余额跨过下阈值（默认 50） | `{"attribute": "money", ...}` |

```json
"data": {
  "attribute": "energy",
  "value": 19.8,
  "threshold": 20,
  "direction": "below"
}
```

| data 字段 | 类型 | 说明 |
|-----------|------|------|
| attribute | string | `energy` / `fatigue` / `joint_wear` / `money` |
| value | number | 跨越时刻的实际值 |
| threshold | number | 被跨越的阈值 |
| direction | string | `below`（向下穿）/ `above`（向上穿） |

### 3.2 spatial（空间变化）

| event_type | 说明 | data |
|------------|------|------|
| `zone_enter` | 进入新 zone | `{"zone": "archive_station", "from": "central_plaza"}` |
| `zone_exit` | 离开 zone | `{"zone": "central_plaza", "to": "archive_station"}` |
| `agent_nearby` | 另一 NPC 进入对话距离 | `{"other_agent": "H-02", "distance_cm": 350}` |
| `agent_leave_nearby` | 另一 NPC 离开对话距离 | 同上 |

> `zone_enter`/`zone_exit` 复用现有 `OnEnterZone`/`OnExitZone` 委托；阈值默认对话距离 500cm（可调）。注意：**zone 变化本身也会触发 perception_update**（基础协议既有行为），事件通道只负责"发生那一刻"的语义，两者并存不冲突。

```json
"data": { "zone": "archive_station", "from": "central_plaza" }
```

### 3.3 social（社交）

| event_type | 说明 | data |
|------------|------|------|
| `chat_invite_incoming` | 有人向该 NPC 发起对话（转发自 SocialChat 动作） | `{"from": "H-02", "content": "老陈，借个工具？"}` |
| `broadcast_heard` | 听到广播/附近 NPC 的大声说话 | `{"source": "H-04", "content": "..."}` |
| `mentioned` | 被点名/被提及 | `{"source": "H-02", "context": "..."}` |

> `chat_invite_incoming` 与现有 `chat_invite` 消息（对话邀请）语义重叠：实现上 UE 可直接把 chat_invite 的 payload 适配为本事件（补充 category/event_type/game_time 等字段），或在 Agent 侧做一层映射。推荐前者——统一事件入口，降低 Agent 侧分支数。

### 3.4 action_anomaly（动作异常）

| event_type | 说明 | data |
|------------|------|------|
| `action_failed` | 动作执行失败（目标不可达等） | `{"action_id": "act_...", "cmd": "MoveTo", "reason": "unreachable"}` |
| `smartobject_occupied` | 目标 SmartObject 被占用 | `{"action_id": "act_...", "semantic_group": "workbench", "occupied_by": "H-02"}` |

> 作用：替代/补充现有 `error` 消息（error_code=ACTION_FAILED）的语义——error 通道保留给协议级错误，**业务级动作异常统一走 world_event**，Agent 反应层才能在同一队列里统筹它们。

```json
"data": { "action_id": "act_123", "cmd": "MoveTo", "reason": "unreachable" }
```

### 3.5 world（世界事件）

Director 注入的故障、环境事件、剧情事件。来源：Director / 调试工具。

| event_type | 说明 | data |
|------------|------|------|
| `malfunction` | 某设备/NPC 故障 | `{"target": "K-03", "description": "K-03 关节锁死"}` |
| `environment_change` | 环境变化（天气、停水停电） | `{"description": "..."}` |
| `director_directive` | 剧情指令（一般带 force） | `{"description": "..."}` |

### 3.6 force 类（强制，跨类别）

带 `force: true` 的事件属于硬保证通道，**category 不限**（可以是 physical_threshold，也可以是 world）。典型来源：

| event_type | 说明 | data |
|------------|------|------|
| `attacked` | NPC 被攻击 | `{"attacker": "...", "damage": 20}` |
| `death` | NPC 死亡（本人视角） | `{"cause": "..."}` |
| `plot_command` | 剧情强制指令 | `{"description": "..."}` |
| `debug_command` | 调试命令 | `{"description": "..."}` |

## 四、force 硬保证通道

`force: true` 的语义（Agent 侧实现承诺，UE 侧只需正确打标）：

1. **零 LLM、零路由**：从消息到达到动作中断，中间无任何模型调用与权衡，延迟微秒级。
2. **不可否决**：Agent 侧不能有任何逻辑能拒绝它——没有阈值、没有预算、没有"最近打断太频繁"的抑制。UE 侧打标即生效。
3. **打断后的动作**：复用现有 `PreemptForDialogue` 优雅中断路径（放下工具、起身），而非硬切动画。
4. **典型语义**：被攻击、死亡、剧情强制、Director 高优先级注入、调试命令。

**打标责任在 UE 侧硬编码**，不依赖配置或模型判断。

## 五、边沿触发要求（实现纪律）

| 规则 | 说明 |
|------|------|
| 跨越才推 | 能量从 20.3 掉到 19.8（跨过 20）→ 推一次；继续掉到 19.5 → **不推**（没有新跨越） |
| 恢复才再推 | 能量充回到 21（向上穿 20）→ 推 `energy_above`；再次掉到 19.8 → 推 `energy_below`（双向都各自边沿触发） |
| zone 只推变化 | 进入新 zone 推一次，停留期间不推 |
| 去抖 | 高频抖动场景（如恰好在阈值线上震荡）建议 UE 侧加 5~10 秒去抖窗口，避免事件风暴 |

## 六、Agent 侧处理承诺

UE 按本协议推送后，Agent 侧保证：

1. `force=true` 的事件：立即打断当前动作（不调 LLM），带进度快照转重规划。
2. `force=false` 的事件：交轻量路由模型判定——结果只有**打断**或**入队**；判不准时倾向入队。
3. 入队的事件在下一个安全点（当前动作完成）**一次性 drain 全部**，连同战略规划与实时状态交给战术层统筹。
4. 事件包含 Agent 决策所需的完整事实（谁/何时/何地/严重度），路由不再回查 UE。

## 七、现有协议的增改清单

| 项 | 变更 |
|----|------|
| 消息类型 | 新增 `world_event`（UE → Agent） |
| 信封 | **不变**（7 字段，无新增顶层字段；force 在 payload 内） |
| `event_notification` | 保持现状（Agent 内部路由用，Director 事件的 Agent→Agent 转发可后续并入本通道） |
| `error` 消息 | 保留（协议级错误）；业务级动作异常改走 `world_event.action_anomaly` |
| `chat_invite` | 语义重叠，推荐 UE 侧适配为 `world_event.social.chat_invite_incoming`（过渡期两者并存） |
| 感知通道 | `perception_update` / `state_report` **不变**——心跳类连续量不迁入事件通道 |

## 八、完整示例

### 8.1 K-03 故障广播（非 force，多 NPC 各收一条）

```json
{
  "version": "1.0",
  "msg_id": "uuid-a",
  "seq": 4001,
  "timestamp": 1719456402000,
  "type": "world_event",
  "agent_id": "H-03",
  "payload": {
    "event_id": "evt_20260917_000042",
    "category": "world",
    "event_type": "malfunction",
    "force": false,
    "severity": 7,
    "subject": "K-03",
    "game_time": "D12 10:47:03",
    "location": "archive_station",
    "occurred_at": 1719456402000,
    "data": {
      "target": "K-03",
      "description": "K-03 关节锁死，需要救援"
    }
  }
}
```

同 `event_id` 的事件会分别推给 H-01/H-02/H-04/H-05（各自不同的 agent_id 信封）——阿静（H-03）判紧急立即中断装配，老陈（H-01）判不紧急入队，**同一事件、N 种反应**。

### 8.2 能量跨阈值（边沿触发）

```json
{
  "version": "1.0",
  "msg_id": "uuid-b",
  "seq": 4002,
  "timestamp": 1719456405000,
  "type": "world_event",
  "agent_id": "H-01",
  "payload": {
    "event_id": "evt_20260917_000043",
    "category": "physical_threshold",
    "event_type": "energy_below",
    "force": false,
    "severity": 4,
    "subject": "H-01",
    "game_time": "D12 11:02:44",
    "location": "main_workshop",
    "occurred_at": 1719456405000,
    "data": {
      "attribute": "energy",
      "value": 19.8,
      "threshold": 20,
      "direction": "below"
    }
  }
}
```

### 8.3 剧情强制指令（force）

```json
{
  "version": "1.0",
  "msg_id": "uuid-c",
  "seq": 4003,
  "timestamp": 1719456410000,
  "type": "world_event",
  "agent_id": "H-03",
  "payload": {
    "event_id": "evt_20260917_000044",
    "category": "world",
    "event_type": "plot_command",
    "force": true,
    "severity": 10,
    "subject": "H-03",
    "game_time": "D12 12:00:00",
    "location": "central_plaza",
    "occurred_at": 1719456410000,
    "data": {
      "description": "立即前往档案馆参加紧急会议"
    }
  }
}
```

---

本文档定义 UE → Agent 的事件上传通道。Agent 侧的事件循环、路由判定、队列 drain 等消费端实现见 `AgentTown_EventDrivenAgent_Design.html` 第三~五章。
