# 第一期工作清单 —— Agent 侧

> 基于 `AgentTown_Design.md` 与 `AgentTown_Core_DeepDive.md`。
> **第一期目标**：一个 NPC 完整跑完一天的"感知 → 决策 → 行动"闭环，打通 Agent 端与 UE 端流程。
> **本清单范围**：与 UE 无关的 Agent 侧工作。UE 桥接协议暂未制定，故用 Mock UE Bridge 模拟 UE 行为，保证 Agent 侧可独立跑通闭环。
> **Agent 实现**：先使用 Hermes Agent。

---

## 一、第一期目标

> 一个 NPC 完整跑完一天的"感知 → 决策 → 行动"闭环。不需要 UE 联调，Agent 侧能独立运行并产出可观测的行为日志。

---

## 二、边界说明

| 我做（Agent 侧） | 我不用做（UE 侧） |
|------|-----------|
| World KB 解析与查询（加载 UE 同事提供的 world_kb.yaml） | World KB 的格式设计、UE 端导出工具（Editor Exporter） |
| Agent Mind（Hermes）全部决策逻辑 | RobotActor / SmartObject / Zone Trigger 的 UE 实现 |
| Action Translator 翻译层 | MoveTo / StateTree 的 UE 侧执行 |
| LLM Gateway | ActionExecutor / PerceptionPackager |
| Mock UE Bridge（模拟 UE 行为） | 真正的 WebSocket 客户端 |
| 生成一份开发用 World KB 样例文档 | — |

> **原则**：Agent 侧负责"想"，UE 侧负责"做"。协议未定前，Agent 与 Mock UE 之间用内部约定的消息格式；真协议确定后只替换序列化层。

---

## 三、模块一览

```
┌──────────────────────────────────────────────────┐
│  M-6  测试与可观测性                                │
│  日志系统 · 场景测试 · 行为回放                       │
└──────────────────────┬───────────────────────────┘
                       │
┌──────────────────────┴───────────────────────────┐
│  M-1  World Knowledge Base                        │
│  解析器 · 语义查询（文档由UE侧提供）                  │
└──────────────────────┬───────────────────────────┘
                       │
┌──────────────────────┴───────────────────────────┐
│  M-3  Hermes Agent Mind                            │
│  SOUL.md · 战略层 · 战术层 · 反应层 · 执行层         │
└──────┬───────────────────────┬────────────────────┘
       │                       │
       ▼                       ▼
┌──────────────┐    ┌──────────────────────────────┐
│  M-2  LLM    │    │  M-4  Action Translator        │
│  Gateway     │    │  语义→指令翻译 · 目标解析        │
└──────────────┘    └──────────────┬────────────────┘
                                   │
                        ┌──────────┴────────────────┐
                        │  M-5  Mock UE Bridge       │
                        │  伪造感知 · 伪造完成回调      │
                        └───────────────────────────┘
```

---

## 四、模块详解

### M-1：World Knowledge Base（世界知识库）

**定位**：World KB 的格式设计与 UE 端导出由 UE 同事负责。Agent 侧只需**加载并使用**这份文档。

**目的**：Agent 不认识 3D 坐标，通过 World KB 将语义 ID 翻译为坐标和可用的交互行为。

| # | 任务 | 说明 |
|---|------|------|
| 1.1 | **生成开发用样例 world_kb.yaml** | UE 同事真文档交付前，生成一份 Phase 1 最小化样例（1 zone + 1 NPC + 2 locations），供本地开发使用 |
| 1.2 | 实现 YAML 解析器 | 加载 world_kb.yaml → Python 数据结构。**真文档交付后无需改动**（格式由 UE 侧约定） |
| 1.3 | 实现 `resolve_target(id_or_desc)` | 判断输入是 zone / location / agent / object，返回对应实体结构 |
| 1.4 | 实现 `get_position(id)` | 返回 interaction_point（location）或 entry_point（zone） |
| 1.5 | 实现 `which_zone(pos)` | 坐标 → zone ID 查询 |
| 1.6 | 实现 `get_available_actions(location_id)` | 返回该位置允许的动词列表 |

> **与 UE 同事的接口约定**：Agent 侧只依赖 world_kb.yaml 的最终格式，不关心 UE 侧如何生成。开发阶段用自生成的样例文档替代；真文档交付后直接替换文件即可。

#### 开发用样例行

```yaml
version: "1.0"
site:
  id: industrial_park
  name: "工业机器人园区"

zones:
  - id: main_workshop
    name: "主生产车间"
    entry_point: [200, 100, 0]
    locations: [workbench_01, charging_station_01]

locations:
  - id: workbench_01
    name: "一号工作台"
    zone: main_workshop
    position: [200, 100, 0]
    interaction_point: [195, 105, 0]

  - id: charging_station_01
    name: "一号充电桩"
    zone: main_workshop
    position: [220, 80, 0]
    interaction_point: [215, 85, 0]

objects:
  - id: workbench_01
    available_actions: [assemble, inspect, repair]

agents:
  - id: H-01
    name: "老陈"
    type: humanoid
    default_zone: main_workshop
    default_position: [200, 100, 0]
```

---

### M-2：LLM Gateway（大模型网关）

**目的**：统一管理所有 LLM 调用，封装 prompt 组装、响应解析、错误重试。

| # | 任务 | 说明 |
|---|------|------|
| 2.1 | 封装 LLM Client 基类 | 支持 Claude / GPT，统一 `chat(prompt, model)` 接口 |
| 2.2 | 实现 JSON 响应解析器 | 从 LLM 返回中提取结构化 JSON，含容错与重试 |
| 2.3 | 实现 Prompt Template 管理器 | 战略层、战术层、反应层各一套模板，支持变量填充 |
| 2.4 | 实现调用日志 | 记录每次调用的 model、token 数、延迟、成本 |

#### Prompt 模板示例（战术层）

```python
TACTICAL_PROMPT = """
你是{name}（{id}），{role}。
性格：{personality}
说话风格：{speech_style}

当前时间：{game_time}
当前位置：{current_location}
当前目标：{goal}

可用行为：
{available_actions}

请规划接下来 3-5 步具体行动。

输出 JSON：
{{
  "inner_thought": "...",
  "actions": [
    {{"action": "...", "params": {{"target": "...", ...}}}}
  ]
}}
"""
```

---

### M-3：Hermes Agent Mind（个体心智）★ 核心模块

**目的**：NPC 的"大脑"。用 Hermes Agent 承载战略 / 战术 / 反应 / 执行四层。

#### Hermes 文件结构

```
hermes/profiles/H-01/
├── SOUL.md          # 角色卡（persona）
├── MEMORY.md        # 每日记忆（跨天不丢失的持久层）
├── SKILL.md         # 可用技能/工具列表
└── state/
    └── daily_state.json  # 当日运行时状态
```

#### 工作项

| # | 任务 | 说明 |
|---|------|------|
| 3.1 | 编写 H-01 的 SOUL.md | 角色卡：老陈，18 年车间主管，沉稳严厉护短。含性格、目标、说话风格 |
| 3.2 | 编写 H-01 的 SKILL.md | 声明可用行为：move_to、work_assemble、charge_at、wait、emote、speak |
| 3.3 | **战略层**：每日计划生成 | Hermes cron 在每天 06:00 触发，调 LLM 生成 6-8 条今日大纲，存入 MEMORY.md + 内存 daily_plan |
| 3.4 | **战术层**：任务分解 | 每个 time slot 开始时，调 LLM 把 "去车间装配 5 小时" 分解为 3-5 步具体 action |
| 3.5 | **执行层**：动作队列 | 维护 action_queue，逐个取出 → Translator 翻译 → 发 Mock UE → 等回调 → pop 下一个 |
| 3.6 | **反应层**：感知处理 | 收到 perception_update 时判断是否需打断。Phase 1 简化：无 Director，只处理"状态异常"（如 energy < 20 必须充电） |
| 3.7 | 状态管理 | 维护运行时状态：energy / fatigue / current_zone / current_task / daily_plan / action_queue / task_stack |
| 3.8 | MEMORY.md 读写 | 当天重要事件写入 MEMORY.md（格式：`[时间][重要性] 事件描述`），次日战略层加载昨日记忆作为上下文 |

#### 执行层状态机

```
        ┌──────────┐
        │  IDLE    │
        └─────┬────┘
              │ new time slot 到了
              ▼
        ┌──────────┐
        │ PLANNING │──→ 战术层调 LLM 分解任务
        └─────┬────┘
              │ 返回 actions
              ▼
        ┌──────────┐
        │ EXECUTING│──→ 从 queue pop action
        └─────┬────┘  → Translator 翻译
              │        → 发给 Mock UE
              ▼
        ┌──────────┐
        │ WAITING  │──→ 等 action_completed
        └─────┬────┘
              │
        ┌─────┴─────┐
        │queue empty?│
        └──┬──────┬──┘
       yes │      │ no
           ▼      ▼
       PLANNING  EXECUTING
```

---

### M-4：Action Translator（行动翻译器）

**目的**：连接 LLM 输出的语义意图和 UE 可执行指令。协议未定前，输出自定的内部指令格式。

| # | 任务 | 说明 |
|---|------|------|
| 4.1 | 定义内部指令格式 | `{cmd: str, params: dict, action_id: uuid}`，如 `{cmd: "MoveTo", params: {dest: [x,y,z]}}` |
| 4.2 | 实现 target 解析 | 查 World KB，把 "main_workshop" 翻译为 zone.entry_point 坐标 |
| 4.3 | 实现 action 类型表 | 维护 `action_name → cmd_name` 映射：`move_to→MoveTo`、`work_assemble→ExecuteComposite` 等 |
| 4.4 | 实现复合行为参数翻译 | `work_assemble(target, duration_min)` → `{cmd: "ExecuteComposite", params: {name: "work_assemble", target: "workbench_01", duration_sec: 18000}}` |
| 4.5 | 预留协议适配接口 | UE 真协议确定后，仅需替换 `Command → wire_format` 序列化层，Translator 核心逻辑不动 |

---

### M-5：Mock UE Bridge（模拟 UE 桥接）

**目的**：UE 未接通阶段，伪造 UE 端行为，让 Agent 侧可独立跑通完整一天。

| # | 任务 | 说明 |
|---|------|------|
| 5.1 | 实现 Mock 消息接收器 | 接收 Agent 发来的 action_command，打印日志 + 返回 action_id |
| 5.2 | 实现伪执行计时 | 按 action 类型模拟耗时：move_to 走若干秒返回 completed，work_assemble 按 duration 等待后返回 |
| 5.3 | 实现伪感知推送 | 定时（每 3 秒）返回伪 perception_update：当前位置、所在 zone、附近可见 agent/object |
| 5.4 | 实现 Zone 模拟 | 坐标进入 zone 的 bounding box → 自动更新 current_zone |
| 5.5 | 实现时间加速模式 | 可配置时间倍率，例如 1 秒 = 游戏 1 分钟，让 24 小时的一天可在 24 分钟内跑完（调试用） |
| 5.6 | 实现场景注入 | 从 YAML 读取一天内预设的"突发事件"，在指定时间自动注入 perception_update（如 14:23 注入 "K-03 故障"） |

#### Mock UE 与 Agent 的交互协议（内部）

```
Agent → Mock UE:
  {"action_id": "act_001", "cmd": "MoveTo", "dest": [200, 100, 0]}

Mock UE → Agent:
  // 每 3 秒
  {"type": "perception_update", "agent_id": "H-01", "payload": {...}}

  // 动作完成时
  {"type": "action_completed", "action_id": "act_001", "result": "success"}

  // Zone 变化时
  {"type": "zone_changed", "agent_id": "H-01", "from_zone": "central_plaza", "to_zone": "main_workshop"}
```

---

### M-6：测试与可观测性

**目的**：让开发过程可见、可调试。第一天跑完后能看到 NPC 干了什么、想了什么。

| # | 任务 | 说明 |
|---|------|------|
| 6.1 | 决策日志 | 每次 LLM 调用的输入 prompt、返回的 raw + parsed JSON、耗时，写入结构化日志 |
| 6.2 | 行为日志 | 每条 action 的发起时间、Translator 翻译结果、Mock UE 返回的 result、耗时 |
| 6.3 | 时序日志 | 按游戏时间排列：`[06:00] 战略层生成每日计划` → `[07:00] 战术层分解 → [MoveTo main_workshop]` → ... |
| 6.4 | 一天跑完报告 | 汇总：执行了多少条 action、几次 LLM 调用、总 token 量、总耗时、哪些 slot 没完成 |
| 6.5 | 一个集成测试场景 | 固定时间、固定世界状态，跑完一天的 end-to-end 测试，验证确定性行为 |

---

## 五、工作优先级与依赖关系

```
阶段一：地基
  M-1 (World KB) ──→ M-2 (LLM Gateway)
                           │
                           ▼
                      M-4 (Action Translator)

阶段二：核心
  M-3 (Hermes Agent Mind)
    ├── 战略层 + 战术层
    ├── 执行层状态机
    └── MEMORY.md 读写

阶段三：闭环
  M-5 (Mock UE Bridge) ──→ 打通完整一天
       │
       ▼
  M-6 (测试与可观测性) ──→ 验证 + 调试
```

---

## 六、直接可行动的第一步

最优先开始的三个文件：

1. **`hermes/profiles/H-01/SOUL.md`** —— 老陈的角色卡
2. **`src/world_kb.py`** —— World KB 解析器 + `resolve_target()` + `get_position()`（用自生成的样例文档驱动）
3. **`src/llm_gateway.py`** —— LLM Client 封装 + 战略层 prompt 模板
4. **`assets/world_kb_sample.yaml`** —— 开发用样例 World KB（1 zone + 1 NPC + 2 locations）

---

## 七、验收标准

第一期视为完成，需满足：

- [ ] World KB 解析器能正确加载样例文档并完成语义 ID → 坐标查询
- [ ] H-01 老陈能在每天 06:00 生成一份合理的今日大纲
- [ ] 战术层能把大纲任务分解为可执行的 action 序列
- [ ] 执行层能逐个发送 action 给 Mock UE 并正确处理完成回调
- [ ] Mock UE 能伪造感知回流与动作完成，Agent 侧据此推进
- [ ] 一整天（加速模式下约 24 分钟）能无阻塞跑完，产出完整时序日志
- [ ] 跑完后能生成一份行为报告，人可读地看出老陈这一天做了什么、想了什么

---

*本清单基于 AgentTown 设计文档整理，仅覆盖第一期 Agent 侧工作。UE 桥接协议确定后，M-4 序列化层与 M-5 Mock 需替换为真实通信实现。*
