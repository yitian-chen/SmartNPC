# Venus 平台模型评测报告

> 评测日期：2026-08-11
> 评测对象：Venus 平台 5 个候选模型，用于 AgentTown 三层决策架构的战略层与战术层后端选型
> 评测脚本：`scripts/eval_venus_models.py`
> 完整数据：`logs/2026-08-11/eval_report.json`

---

## 一、概论

AgentTown 三层决策架构中，战略层（每日 06:00 生成当日 7 时段计划）与战术层（每时段把 goal 分解为 action 队列）均调用云端 LLM。当前默认后端为 Venus 平台的 `qwen3.6-35b-a3b`。战术层调用频繁（每时段 1 次，replan 时更多），其延迟直接决定 worker 循环响应速度——战术层 TTFB 过高会加剧"队列提前耗尽触发频繁重分解"问题。

本次评测在 Venus 平台挑选 5 个候选模型，用真实仿真日志提取的 prompt 测量延迟与输出质量，为后端选型提供数据依据。

### 候选模型

| 模型 | 厂商 | 说明 |
|------|------|------|
| `qwen3.6-35b-a3b` | 阿里 | 当前默认后端 |
| `deepseek-v4-flash` | DeepSeek | 轻量快速版 |
| `deepseek-v4-pro` | DeepSeek | 高质量版 |
| `kimi-k2-light` | Moonshot | 轻量版 |
| `hy3-external` | 字节 | 外部接入版 |

---

## 二、测评原理

### 2.1 方法

1. **prompt 来源**：运行一次 quick-smoke 仿真，从 `logs/YYYY-MM-DD/debug-mcp.log`（slog JSON Lines 格式）提取第一条 `[MCP→LLM/STRATEGIC-PROMPT]` 与第一条 `[MCP→LLM/TACTICAL-PROMPT]` 的 `text` 字段，作为真实输入。
2. **调用方式**：对每个模型分别用战略层 / 战术层 prompt 调用 Venus `POST /v1/chat/completions` 端点，`stream=true`，`max_tokens=2048`，每组合重复 2 轮取平均。
3. **测量指标**：
   - **总耗时（elapsed）**：从发请求到流结束的 wall-clock 时间
   - **TTFB**：首个 token 到达时间（streaming 模式可测）
   - **输出字符数**：响应文本长度
   - **completion_tokens**：Venus 返回的 token usage（部分模型流式不返回，记为 0）
   - **HTTP 状态码**：识别限流 / 模型名无效等错误

### 2.2 实际传入 prompt

#### 战略层 prompt（2129 字符）

```
[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了，你刚从休眠舱醒来，当前位于休眠舱区域。

【你的角色】
名字：老陈
职业：车间主管、装配工人、维护技师
背景：车间主管机器人，在工厂工作多年，对每台设备和每道工序都了如指掌。手下带着小林和小赵两个年轻机器人，习惯了亲力亲为。
性格特质：沉稳、念旧、重视工艺、务实
说话风格：简短有力，多用行业术语，偶尔提起过去的旧事。
【世界知识】
可前往区域（move_to 的 target_id 用 id）: 档案馆·图书馆与网络中心（id=archive_station）、中央广场（id=central_plaza）、物流转运站（id=logistics_hub）、主生产车间（id=main_workshop）、废料回收与再制造场（id=recycling_yard）、机械维修厂（id=repair_bay）、休眠舱居住区（id=residential_quarters）。
可交互物体（interact 和复合动作的 smart_object 用 id，interact 的 interaction 用下列可用动词）:
  - 充电桩（id=charge），位于 zone=central_plaza，可用 interaction: charge
  - 电脑（id=computer），位于 zone=archive_station，可用 interaction: surf_internet
  - 修理台（id=repairtable），位于 zone=repair_bay，可用 interaction: repair
  - 睡眠舱（id=sleeppod），位于 zone=residential_quarters，可用 interaction: sleep
  - 分拣传送带（id=sortingconveyor），位于 zone=logistics_hub，可用 interaction: sort_cargo
  - 工作台（id=workbench），位于 zone=main_workshop，可用 interaction: assemble
【可用能力】
长时段活动用以下复合动作（自动移动到对应位置，覆盖整段工作时间）：
- 在充电站充电（charge_at_station）
- 与其他 agent 聊天（chat_with）
- 兜底通用动作（带内心独白，无匹配复合动作时用）（generic_act）
- 巡逻区域（patrol_zone）
- 修理目标 agent（repair_target）
- 在工作台装配（work_at_workbench）
- 车间例行工作（work_at_workshop）
此外始终可用基础动作：移动、说话、表达情绪、与物体交互、等待（用于短耗时或衔接）。


昨日总结：昨天按计划完成了车间装配。

请基于你的角色身份和性格，规划今天一天的活动安排。一天从 07:00 到次日 07:00，你从 07:00 开始活动，夜间活动可持续到次日清晨。

要求：
1. 输出一个 JSON 数组，6-8 条
2. 每条包含 "time"（时段，如 "07:00-12:00"）和 "goal"（这个时段你要做什么，一句话）
3. 安排要符合你的角色身份和性格特点
4. 每个时段时长不少于 120 分钟（起止时间差 ≥ 120 分钟），每个时段原则上仅安排一项主要任务
5. 只输出 JSON 数组，不要任何其他文字
6. 必须以字符 [ 开头，以字符 ] 结尾，不要输出设计思路、不要解释、不要 markdown 围栏
7. goal 中提到的地点、人物、设备必须是【你的角色】和【世界知识】中存在的，不得编造未提及的人物或设施
8. 末段若跨午夜（如 "22:00-07:00"），结束时间表示次日该时刻，调度器会自动识别跨午夜时段
9. goal 的主要活动应能用【可用能力】中列出的复合动作实现（如装配→work_shift、充电→charge_at_station），不得规划【可用能力】未列出且无法用基础动作（移动/说话/表达情绪/交互物体/等待）组合实现的活动——如"整理仪容""冥想"等无对应能力的活动会被战术层拒绝。
10. 首个时段（从 07:00 起）必须是日间活动（如晨间补电、装配、维护），不得安排休眠——你刚从休眠舱醒来，应立即离开开始当日活动；休眠只能安排在午间和夜间。

示例：[{"time":"07:00-09:00","goal":"去中央广场晨间补电"},{"time":"09:00-12:00","goal":"上午车间装配作业"},{"time":"12:00-14:00","goal":"午间停工，前往充电区域短暂补电休息"},{"time":"14:00-18:00","goal":"下午继续在车间装配"},{"time":"18:00-22:00","goal":"傍晚去维修台维护修理"},{"time":"22:00-07:00","goal":"夜间在休眠舱休息"}]
```

#### 战术层 prompt（3166 字符）

```
[战术层/任务分解] 当前时段目标：前往中央广场充电桩进行晨间补电，确保全天作业能量充足
你目前在：residential_quarters，游戏时间 07:00。
物理状态：能量 99、疲劳 1、关节磨损 0、健康 100。
【你的角色】
名字：老陈
职业：车间主管、装配工人、维护技师
背景：车间主管机器人，在工厂工作多年，对每台设备和每道工序都了如指掌。手下带着小林和小赵两个年轻机器人，习惯了亲力亲为。
性格特质：沉稳、念旧、重视工艺、务实
说话风格：简短有力，多用行业术语，偶尔提起过去的旧事。

请把这个目标分解为一个或多个 action，按顺序执行。

当前时段 07:00-08:30，约 90 分钟；请让步骤总时长接近此时长，避免过短导致队列提前耗尽触发重分解。

可前往区域（move_to 的 target_id 用 id）: 档案馆·图书馆与网络中心（id=archive_station）、中央广场（id=central_plaza）、物流转运站（id=logistics_hub）、主生产车间（id=main_workshop）、废料回收与再制造场（id=recycling_yard）、机械维修厂（id=repair_bay）、休眠舱居住区（id=residential_quarters）。
可交互物体（interact 和复合动作的 smart_object 用 id，interact 的 interaction 用下列可用动词）:
  - 充电桩（id=charge），位于 zone=central_plaza，可用 interaction: charge
  - 电脑（id=computer），位于 zone=archive_station，可用 interaction: surf_internet
  - 修理台（id=repairtable），位于 zone=repair_bay，可用 interaction: repair
  - 睡眠舱（id=sleeppod），位于 zone=residential_quarters，可用 interaction: sleep
  - 分拣传送带（id=sortingconveyor），位于 zone=logistics_hub，可用 interaction: sort_cargo
  - 工作台（id=workbench），位于 zone=main_workshop，可用 interaction: assemble

可用工具（仅限以下 14 个）。工具分两类：
- 复合动作（标记 [复合]）：长耗时、单步即可完成一段工作（如装配、充电、巡逻、聊天），会自动移动到对应位置，无需自己调用 move_to。若目标语义与某复合动作匹配，应优先使用复合动作。
- 原子动作（标记 [原子]）：短耗时、作为基本 building block（如移动、说话、等待、交互）。仅当复合动作无法覆盖 schedule 要求时，才用 2-5 个原子动作组合实现。
- charge_at_station [复合]: 在充电站充电。params: {"smart_object":"充电站id","interaction":"动词"}
- chat_with [复合]: 与其他 agent 聊天。params: {target_agent_id:...,topic:...}
- emote [原子]: 表达情绪。params: {"emotion":"happy|sad|..."}
- generic_act [复合]: 兜底通用动作（带内心独白，无匹配复合动作时用）。params: {"thought":"...","behavior":"idle|wave_hand|look_around"}
- interact [原子]: 与智能物体交互。params: {"smart_object":"...","interaction":"动词"}
- move_to_agent [原子]: 移动到动态 agent 身边。params: {target_agent_id:...,speed:walk|run,stop_distance:秒数,keep_following:true|false}
- move_to_location [原子]: 移动到静态坐标。params: {dest:[x,y,z],speed:walk|run}
- patrol_zone [复合]: 巡逻区域。params: {target_zone:...,duration_sec:秒数}
- play_montage [原子]: 播放蒙太奇动画。params: {montage_id:...,wait_finish:true|false}
- repair_target [复合]: 修理目标 agent。params: {target_agent_id:...,tool_id:...}
- speak [原子]: 说话。params: {"content":"..."}
- turn_to [原子]: 转向目标。params: {"target_type":"agent|smart_object|zone|position","target_id":"...","target_position":[x,y,z]}
- work_at_workbench [复合]: 在工作台装配。params: {target_object_id:...,duration_sec:秒数}
- work_at_workshop [复合]: 车间例行工作。params: {duration_sec:秒数}

要求：
1. 第一行输出 {"inner_thought":"一句话内心独白"}
2. 后续每行输出一个 {"action":"工具名","params":{...}}，按执行顺序排列
3. 队列必须以长复合动作（标记 [复合]）结尾——长复合动作会持续执行直到时段切换，让 NPC 一直工作到下一 schedule 节点被 worker 主动打断
4. 禁止输出 wait 动作；若无需移动/转身等前置步骤，可直接输出单个长复合动作，长复合动作包含移动到对应位置的逻辑
5. 仅当目标确实没有匹配的长复合动作时（极少见），才用原子动作组合、结合调用兜底的 generic_act 通用动作实现目标
6. move_to/turn_to 的 target_id、interact 和复合动作的 smart_object 必须严格使用上面"可前往区域"和"可交互物体"中给出的 id，禁止编造、禁止拼接 zone/interaction 信息
7. 每行一个 JSON 对象，不要输出 JSON 数组，不要输出 markdown 围栏，不要输出任何其他文字
8. 必须以字符 {"inner_thought 开头，不要输出步骤说明、不要解释、不要编号列表、不要 markdown 加粗

示例（id 来自上方可用列表，不可照抄示例中的 id）：
{"inner_thought":"先去目标区域补充能量"}
{"action":"move_to","params":{"target_type":"zone","target_id":"central_plaza"}}
{"action":"charge_at_station","params":{"smart_object":"charge","interaction":"charge"}}
```

---

## 三、测评结果

### 3.1 汇总数据

每模型每层 2 轮，取平均。`completion_tokens=0` 表示该模型流式响应未返回 usage（已知行为，非异常）。

| 模型 | 层 | 状态 | 平均耗时(s) | TTFB(s) | 输出字符 | completion_tokens |
|------|-----|------|------------|---------|---------|-------------------|
| qwen3.6-35b-a3b | 战略 | OK | 8.09 | 3.38 | 478 | 0 |
| qwen3.6-35b-a3b | 战术 | OK | 10.18 | 9.50 | 188 | 0 |
| deepseek-v4-flash | 战略 | OK | 1.07 | 0.55 | 330 | 162 |
| deepseek-v4-flash | 战术 | OK | 0.61 | 0.60 | 140 | 48 |
| deepseek-v4-pro | 战略 | OK | 12.03 | 6.78 | 384 | 193 |
| deepseek-v4-pro | 战术 | OK | 13.17 | 12.29 | 138 | 48 |
| kimi-k2-light | 战略 | **ERR 400** | — | — | — | — |
| kimi-k2-light | 战术 | **ERR 400** | — | — | — | — |
| hy3-external | 战略 | OK | 3.64 | 1.45 | 350 | 165 |
| hy3-external | 战术 | **ERR 429** | — | — | — | — |

### 3.2 错误详情

- **kimi-k2-light**：两层均返回 HTTP 400，响应体 `{"error":{"message":"状态错误","type":"venus_error","code":"4001}}`。推测模型名无效或服务异常，不可用。
- **hy3-external 战术层**：HTTP 429 限流。战略层正常，战术层连发时触发限流，稳定性不足。

### 3.3 延迟对比（仅 OK 模型）

```
战术层（高频调用，延迟敏感）:
  deepseek-v4-flash   0.61s  ██████              ← 最优
  qwen3.6-35b-a3b    10.18s  ███████████████████ ← 当前默认，偏慢
  deepseek-v4-pro    13.17s  ██████████████████████

战略层（每日 1 次，延迟不敏感）:
  deepseek-v4-flash   1.07s  ███
  hy3-external        3.64s  ██████████
  qwen3.6-35b-a3b     8.09s  ████████████████████
  deepseek-v4-pro    12.03s  █████████████████████████
```

### 3.4 输出质量观察

**战略层**（4 个 OK 模型均产出合法 JSON 数组，6-7 条时段计划，符合 prompt schema）：

| 模型 | 时段数 | 内容特征 |
|------|--------|---------|
| qwen3.6-35b-a3b | 7 | 时段划分细致（07-08:30 / 08:30-12 ...），goal 含角色语气 |
| deepseek-v4-flash | 6 | 标准 2-3 小时时段，goal 简洁 |
| deepseek-v4-pro | 7 | goal 带角色细节（"带小林小赵赶这批精密件"），最贴合人设 |
| hy3-external | 7 | 标准 2-3 小时时段，goal 平实 |

**战术层**（3 个 OK 模型均遵循 `{"inner_thought":"..."}` + `{"action":...}` NDJSON 格式）：

| 模型 | 步骤数 | 内容特征 |
|------|--------|---------|
| qwen3.6-35b-a3b | 3 | inner_thought + move_to_location + charge_at_station |
| deepseek-v4-flash | 2 | inner_thought + charge_at_station（复合动作自带移动） |
| deepseek-v4-pro | 2 | inner_thought + charge_at_station |

所有 OK 模型都正确引用了 `smart_object=charge`、`interaction=charge`，未编造 KB 外 id。

---

## 四、结论与建议

1. **kimi-k2-light 不可用**（400 错误），**hy3-external 战术层不稳定**（429 限流），两者排除。
2. **战术层建议切到 `deepseek-v4-flash`**：延迟 0.61s，比当前 `qwen3.6-35b-a3b`（10.18s）快 **16×**；输出格式同样合规，能显著缓解"战术层队列提前耗尽触发频繁重分解"问题。
3. **战略层可保持 `qwen3.6-35b-a3b` 或切 `deepseek-v4-flash`**：战略层每日仅 1 次调用，延迟不敏感；`deepseek-v4-pro` 输出最贴合人设但延迟最高（12s），性价比低。
4. **`deepseek-v4-pro` 不推荐**：延迟最高（战术层 13s），输出质量较 flash 提升有限，不值得 20× 延迟代价。

**推荐配置**：

```bash
# 战术层（高频，延迟敏感）
--venus-model deepseek-v4-flash

# 战略层（低频，可选用更高质量）
--venus-strategic-model qwen3.6-35b-a3b   # 或 deepseek-v4-flash 统一
```
