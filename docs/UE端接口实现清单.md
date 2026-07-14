# UE 端接口实现清单

> **文档目的**：给 UE 端开发同事的实现规范。UE 只负责"世界模拟"——为每个 NPC 提供**文本化感知**、执行**行动指令**，并与 MCP 中间层通信。**不涉及任何 LLM、记忆、决策逻辑。**
>
> **通信约定**：UE ⇄ MCP 中间层，本地 loopback，采用 **HTTP + WebSocket 双通道**。
> - HTTP：MCP → UE 下发行动指令、查询状态（请求-响应）
> - WebSocket：UE → MCP 主动推送感知事件（异步、低延迟）
>
> **数据格式**：全部 JSON，UTF-8。坐标一律用**语义 ID**，不暴露原始世界坐标。

---

## 〇、前置准备：语义标签体系

Agent 只理解文本，不认识 UE 的网格和坐标。**所有可被感知/交互的实体必须先打上稳定的唯一 ID 和可读名称。**

| 实体类型 | ID 规范 | 示例 | 说明 |
|---|---|---|---|
| 区域 | `zone.<name>` | `zone.warehouse` `zone.repair_bay` `zone.common_room` `zone.charging` | 四个场景 |
| NPC | `npc.<code>` | `npc.AH-01`(辛博) `npc.AH-03`(柚子) | 与设定文档一致 |
| 可交互物体 | `obj.<type>_<num>` | `obj.charging_pod_02` `obj.shelf_A` `obj.repair_bench_01` | 每个物体唯一 |
| 物品（可拿取） | `item.<type>_<num>` | `item.gear_007` `item.record_disc` | |

> **交付物 1**：一份 `EntityRegistry`（可用 DataTable 或 DataAsset），列出全部 ID → 名称 → 类型 → 所属区域 → 可用交互动词。MCP 层需要这份清单做合法性校验。

---

## 一、感知接口（UE → MCP，WebSocket 推送）

### 1.1 `perception_snapshot` —— 局部感知快照
**触发时机**（事件驱动，非逐帧）：
- NPC 进入新区域
- 视野内出现/消失了对象
- 附近有 NPC 说话
- 当前动作完成
- 兜底：最长间隔 N 秒强制推送一次（可配置，建议 5s）

**推送 Payload**：
```json
{
  "type": "perception_snapshot",
  "npc_id": "npc.AH-03",
  "game_time": "day3 09:14",
  "location": "zone.warehouse",
  "self_status": {
    "battery": 0.72,
    "wear_level": 0.15,
    "holding": "item.gear_007",
    "current_action": "idle"
  },
  "vision": [
    { "id": "npc.AH-01", "name": "辛博", "distance": 4.2, "activity": "在3号货架清点物资" },
    { "id": "obj.shelf_A", "name": "A货架", "state": "可存取", "verbs": ["take", "store"] },
    { "id": "obj.record_player", "name": "旧留声机", "state": "闲置", "verbs": ["play"] }
  ],
  "hearing": [
    { "from": "npc.AH-01", "text": "柚子，B区的零件对过账了吗？", "distance": 4.2 }
  ],
  "exits": [
    { "to_zone": "zone.repair_bay", "via": "东门", "state": "open" }
  ]
}
```

> **关键**：`vision` 只包含**该 NPC 视野范围内**的对象（用 `AIPerceptionComponent` 或 `USphereComponent` + 视锥/遮挡判定）。这是斯坦福小镇"局部感知"的核心，不要给全局信息。

### 1.2 `action_result` —— 行动执行回执
每条行动指令执行完毕（或失败/被打断）后推送：
```json
{
  "type": "action_result",
  "npc_id": "npc.AH-03",
  "action_id": "act_20260714_0012",
  "status": "success",          // success | failed | interrupted
  "message": "已到达修理厂",
  "game_time": "day3 09:16"
}
```

### 1.3 `world_event` —— 突发世界事件
非某个 NPC 主动引起、但需要广播给相关 NPC 的事件（火灾、设备故障、外来者闯入等）：
```json
{
  "type": "world_event",
  "event_id": "evt_fire_01",
  "scope_zone": "zone.repair_bay",
  "affected_npcs": ["npc.AH-02", "npc.AH-04"],
  "description": "修理厂2号工位冒出火花，温度异常升高",
  "game_time": "day3 14:30"
}
```

---

## 二、行动接口（MCP → UE，HTTP POST）

统一入口：`POST /ue/action`
统一请求头：`X-NPC-ID: npc.AH-03`
统一响应：立即返回 `{ "accepted": true, "action_id": "..." }`（**异步**，执行结果后续通过 `action_result` 推送）。

> **重要设计**：所有行动都是**异步**的。UE 收到即入队并立即应答，NPC 通过行为树/状态机执行；不要让 HTTP 请求阻塞等待动作完成。

### 2.1 移动
```json
{ "action": "move_to", "target_id": "zone.repair_bay" }
```
- UE 用 NavMesh 寻路。`target_id` 可为区域、物体或 NPC。
- 到达/失败/被打断 → 推送 `action_result`。

### 2.2 交互
```json
{ "action": "interact", "object_id": "obj.charging_pod_02", "verb": "charge" }
```
- `verb` 必须在该物体的 `verbs` 列表内（如 `charge` / `repair` / `take` / `store` / `play`）。
- 播放对应交互动画 + 改变物体/自身状态（如充电时 `battery` 缓升）。

### 2.3 说话
```json
{ "action": "speak", "text": "辛博，B区对过了，差两个齿轮。", "to_npc_id": "npc.AH-01" }
```
- 头顶显示气泡/字幕。
- **触发听觉传播**：把该文本注入 `to_npc_id`（若在场）以及**听觉半径内所有其他 NPC** 的下一次感知快照的 `hearing` 字段。
- `to_npc_id` 可为空 = 自言自语（仍会被附近 NPC 听到）。

### 2.4 拿取 / 放下
```json
{ "action": "pick_up", "item_id": "item.gear_007" }
{ "action": "put_down", "target_id": "obj.shelf_A" }
```
- 物品附加/分离到手部插槽，更新 `self_status.holding`。

### 2.5 情绪动作
```json
{ "action": "emote", "emotion": "wave" }
```
- 播放情绪动画（`wave` / `nod` / `shake_head` / `look_around` / `idle_think` 等）。纯表现，不改状态。

### 2.6 等待
```json
{ "action": "wait", "seconds": 3 }
```
- NPC 待机指定时长（用于日常节奏、等待他人响应）。

### 2.7 打断当前动作
```json
{ "action": "cancel" }
```
- 中止 NPC 当前正在执行的动作（如突发事件需要立即改变行为），回到 idle。

---

## 三、查询接口（MCP → UE，HTTP GET，同步返回）

供 MCP 层在需要时主动拉取，作为 WebSocket 推送的补充。

| 接口 | 说明 | 返回 |
|---|---|---|
| `GET /ue/npc/{npc_id}/status` | 查询单个 NPC 完整状态 | 同 `self_status` + `location` |
| `GET /ue/npc/{npc_id}/perceive` | 主动请求一次实时感知快照 | 同 `perception_snapshot` |
| `GET /ue/entity/{entity_id}` | 查询某个物体/区域的细节 | 状态、可用动词、所在区域 |
| `GET /ue/world/time` | 查询当前游戏时间 | `{ "game_time": "day3 09:14" }` |
| `GET /ue/world/npcs` | 列出所有 NPC 及其所在区域 | NPC 列表 |
| `GET /ue/health` | 健康检查（连接探活） | `{ "ok": true }` |

---

## 四、世界时钟（UE 内实现）

- UE 维护统一的**游戏内时间**，格式建议 `"day{N} HH:MM"`，可加速运行（如现实 1 秒 = 游戏 1 分钟）。
- 时钟节点（每天开始、整点等）通过 `world_event` 推送给 MCP，供 Agent 触发日程规划（对应斯坦福小镇的"每天早晨生成日程"）。
- 提供 `GET /ue/world/time` 供随时查询。
- **交付物 2**：可配置的时间流速参数。

---

## 五、连接与生命周期

| 场景 | UE 侧行为 |
|---|---|
| MCP 层启动连接 | UE 作为 WS Server 接受连接，或作为 WS Client 主动连（二选一，建议 UE 起 WS Server 更简单） |
| 断线重连 | 保持 NPC 当前状态，重连后补发最新一次全量感知快照 |
| NPC 未被任何 Agent 接管 | 走内置的默认待机/巡逻行为，不推送感知（省算力） |
| MCP 层请求全量同步 | 提供 `GET /ue/world/snapshot` 返回所有 NPC + 关键物体状态 |

---

## 六、交付清单

- [ ] **实体语义注册表**（EntityRegistry DataTable/DataAsset）：全部 ID/名称/类型/区域/可用动词
- [ ] **感知系统**：每个 NPC 的局部视野/听觉检测，生成文本化 `perception_snapshot`
- [ ] **Agent Bridge 子系统**（`UGameInstanceSubsystem`，C++）：
  - [ ] WebSocket 服务（推送感知 / action_result / world_event）
  - [ ] HTTP 服务（接收 action、响应 query）
  - [ ] NPC ID 路由（把指令分发到正确的 Character）
- [ ] **行动执行**：move_to / interact / speak / pick_up / put_down / emote / wait / cancel 的行为树或状态机实现
- [ ] **听觉传播机制**：speak 文本注入半径内 NPC 的感知
- [ ] **世界时钟**：可配置流速 + 时间节点事件推送
- [ ] **默认行为**：无 Agent 接管时的兜底待机/巡逻
- [ ] **健康检查 / 断线重连**