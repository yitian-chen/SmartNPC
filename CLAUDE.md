# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## 项目定位

AgentTown_v3 — AI NPC 模拟系统。5 个 NPC（H-01~H-05，各自独立 profile 人设 + 工种），通过 MCP 协议驱动完整的"感知→决策→行动"闭环，对接真实 UE5（AgentTown 地图，2026-08-11 起弃用 Mock UE）。通信协议按 `docs/AgentTown_CommProtocol_Values.md` 实现。

**三层决策架构**：
- **战略层**（`pkg/prompt/strategic.go` + `cmd/agenttown-mcp/strategic.go`）：每日 07:00 生成当天计划（`dailyPlan`，6-8 个时段 goal）。system/user prompt 拆分、规则迁 user prompt、注入生产工作流概述与物理状态分档
- **战术层**（`pkg/prompt/tactical.go` + `cmd/agenttown-mcp/tactical.go`）：每个时段把 goal 分解为 1-4 个动作段，走 **OpenAI 原生 function calling**（`tools` 字段、`tool_choice=required`、多轮 agentic loop），段间用 `time_to_stop` 控制时长
- **反应层**（~~已退役 P4-12~~）：打断判定全部由事件系统（world_event + 路由器 + force 通道）承担；Ollama 客户端已随 2026-10-22 长期记忆重建彻底移除（关系判断迁到 compact 的 people_updates）

**模块化架构**（2026-08-27 拆分，多 module 仓库）：
- **contract/**（契约 module，无依赖）：`protocol/`（7 字段信封+消息类型）+ `Transport` 接口（决策侧与连接侧的边界）
- **wsserver/**（连接 module）：WS 收发/seq 重放/ACK，实现 `contract.Transport`
- **根 module**（`agenttown-mcp/`）：agent 决策 + 装配壳 + `pkg/` 领域库，经根 `go.mod` 的 `replace => ./contract` / `=> ./wsserver` 引用本地 module
- 依赖倒置：agent 决策侧（`guardedExecutor`/`reactiveRunner`/worker 循环）只依赖 `contract.Transport` 接口，不依赖 wsserver 实现；入站消息经 `Runtime.HandleMessage`（`cmd/agenttown-mcp/runtime.go`）单入口分发，运输层只注册 `HandleMessage`/`OnDisconnect` 两个回调

**LLM 后端**：MCP 直连 Venus（OpenAI Chat Completions 协议），战略层用 `deepseek-v4-pro`、战术层用 `deepseek-v4.1-flash`（`--venus-strategic-model`/`--venus-model`）。事件路由走 Venus 判决 API（`pkg/jev`）。旧反应层（Ollama `qwen2.5:7b`）已随 P4-12 退役并移除（2026-10-22）。

## 架构总览

```mermaid
graph LR
    subgraph UE["UE5 游戏世界"]
        UE5["真实 UE5<br/>AgentTown 地图<br/>5 个 NPC (H-01~H-05)"]
    end
    subgraph MCP["Linux 云环境"]
        MCP["agenttown-mcp (Go)<br/>MCP Server + WS Server<br/>:8760 HTTP / :9092 WS (stable)<br/>战略+战术决策 + 事件路由"]
    end
    subgraph LLM["LLM 后端"]
        VENUS["Venus<br/>战略 deepseek-v4-pro<br/>战术 deepseek-v4.1-flash<br/>(OpenAI 兼容)"]
    end
    UE5 <-->|"WebSocket :9092<br/>7-field Envelope"| MCP
    MCP -->|"HTTP POST<br/>/v1/chat/completions<br/>(战略/战术层 function calling)"| VENUS
```

### 组件职责

| 组件 | 语言 | 路径 | 端口 | 职责 |
|------|------|------|------|------|
| 真实 UE5 | C++/蓝图 | UE 侧 AgentTown 地图 | — | 游戏世界：物理状态、空间状态、动作执行、感知推送、world_kb/capability_registry 下发 |
| contract module | Go | `agenttown-mcp/contract/` | — | 契约：protocol（信封/消息类型）+ Transport 接口（两侧边界） |
| wsserver module | Go | `agenttown-mcp/wsserver/` | — | WS 连接：收发、seq 重放、ACK 等待，实现 contract.Transport |
| agenttown-mcp 根 module | Go 1.25+ | `agenttown-mcp/` | HTTP `:8760`, WS `:9092`（stable，脚本默认） | 协议适配、感知语义化、工具暴露、战略/战术决策、LLM 桥接 |
| Venus | 远程 | `--venus-url` | — | OpenAI 兼容 LLM 服务（战略/战术层后端） |

### 三层决策架构

MCP 内置三层决策，由 `runPerceptionWorker`（`main.go:279`）事件驱动循环串联：

```mermaid
graph TB
    subgraph 战略层["战略层 strategic.go"]
        S1["每日 07:00<br/>generateDailyPlan<br/>1 次 LLM 调用(Structured Outputs)"] --> S2["dailyPlan<br/>6-8 个时段 goal"]
    end
    subgraph 战术层["战术层 tactical.go"]
        T1["队列空 → selectCurrentGoal<br/>按 game_time 选 dailyPlan 时段"] --> T2["generateTacticalPlan<br/>function calling(tools + tool_choice=required)"]
        T2 --> T3["actionQueue<br/>1-4 个动作段<br/>(段间 time_to_stop)"]
        T3 --> T4["popAndSendQueueAction<br/>逐个下发 UE"]
    end
    subgraph 反应层["反应层 reactive_runner.go"]
        R1["触发: zone/action_done/<br/>physical_alert/periodic"] --> R2["Ollama 调用<br/>8s 超时(默认禁用)"]
        R2 --> R3{"决策"}
        R3 -->|continue| R4[不打断]
        R3 -->|observe| R4
        R3 -->|replan| R7[战术层重规划<br/>+ stop 当前]
    end
    S2 --> T1
    T4 --> UE5
    UE5 -.->|感知事件| R1
```

### 关键机制

- **worker 循环**：`runPerceptionWorker` 监听 `wake` 信号，队列空时调 `tacticalRefill` → `selectCurrentGoal` → `generateTacticalPlan` → 填 `actionQueue` → `popAndSendQueueAction` 下发
- **社交回应豁免 06:00-07:00 屏蔽（2026-10-16）**：`selectCurrentGoal` 对 06:00-07:00（战略规划窗口，dayStartMinute=07:00）硬返回空 goal，防止凌晨反复分解夜间睡眠段；但这也拦住了社交回应 replan——实测 06:33 注入邀请时 H-02 收到 speech_heard，回应 replan 命中屏蔽直接"无当前时段 goal"失败、H-01 收场 no_reply。`resolveReplanGoal`（`tacticalRefillForReplan` 调用）在 `selectCurrentGoal` 返回空且 hint 是【社交回应】时回退到当天首个时段（idx=0）作 goal 上下文（计划为空用中性 goal"回应对方的社交邀请"），让 NPC 无论何时收到搭话/邀请都能回应；非社交 replan（威胁/强制打断）在 06:00-07:00 仍保持屏蔽不变
- **战术层 function calling 多轮对话**：`generateTacticalPlan` 经统一 agentic loop（`agenticTurn` → `SendLoop`）携带多轮历史（user/assistant tool_calls/占位 tool）。**不做滑动窗口截断**（已取消，原 8 轮截断会丢上下文导致目标漂移/重复动作），仅跨游戏日 `ClearConversation` 清空，单日内保留完整对话
- **战术层 user prompt 首条全量 + 后续精简引用**：日内不变块（【全天日程】+ 完整分解规则）只在每天第一条战术 user 消息出现；同计划后续轮次省略两块、改为核心约束速览 + 引用行（`TacticalInput.Compact`，实测每条 2100→~900 字符，-57%）。判定与置位：`AgentState.tacticalHeaderPlan` 记录"最近成功注入全量头的计划字符串"（成功 agenticTurn 后、parse 之前置位），与当前 dailyPlan 比对——不等（跨日/日内重规划）即重新全量注入；`ClearConversation` 随历史一起重置。dailyPlan=="" 的 `/debug/schedule` 路径永不精简
- **`<agent_state>` 状态栏**（事件驱动设计 §6.1，`agent_state_bar.go`）：`agenticTurn` 在 messages 末尾追加一条瞬态 user 消息，四行：当前游戏时间（`D<DayCount+1> HH:MM:SS`）、物理状态（`PhysicalLine` 分档自然语言，非裸数值）、当前日程与剩余时间（`[序号/总数] 时段 goal（剩余约 N 分钟）`，跨午夜经 `NormalizeTodToSlot` 归一）、当前动作与剩余时间（工具名+关键参数，剩余按 `time_to_stop` 目标时刻推算）。**不入会话历史**——每轮请求从 AgentState 现查现拼（旧状态栏留在历史里只会误导），前缀保持稳定、KV cache 只失效尾巴；成功落历史的是 userContent 本身。无感知数据（UE 未推首条 perception）返回空串跳过注入
- **venus 4001 重试**：战术层 LLM 调用返回 4001（venus 校验 tools JSON 失败，LLM 输出坏 JSON）时以相同请求体重试，上限 3 次（`maxTacticalRetries`，`isVenusErrorCode` 匹配错误码）；超时/连接错误不重试，走兜底。实测重试后 4001 全部被救回
- **tool_calls 文本兜底（2026-10-15）**：venus 过载窗口内流式组装的 native tool_calls 偶发劣化（name 为空 / arguments 非法 JSON），`parseToolCalls` 静默丢弃——实测 3 次 "plan has no actions" + 1 次 4 调用只剩 1 动作，raw 文本四次全部完好（deepseek 把工具调用以文本与 native 双轨输出，文本数组始终完整）。`parseToolCallsWithFallback`：native 解析结果少于 tool_calls 数（或 native 整体为空）时从响应文本提取 JSON 数组兜底解析（容忍 `<｜end▁of▁sentence｜>` 尾巴与外围散文），取有效动作更多的一份；被丢弃条目打 warn 日志（name + arguments 现场，下次能直接看到 venus 发了什么）。`isEmptyTacticalResult` 同步感知文本调用——0-native 但文本可解析时不再误判空结果白白重试
- **会话历史消毒 + venus 400 自愈（2026-10-20）**：文本兜底救回动作队列的同时曾把毒带进历史——`agenticTurn` 落历史的 assistant 消息携带 `resp.ToolCalls` 原样，损坏条目（arguments 截断/跨条目拼接、name 为空）一并入库后 venus 对请求 messages 逐条校验 arguments JSON，该 agent 之后**每一次请求都 HTTP 400**，直到跨游戏日 `ClearConversation` 才恢复（实测 H-03 从 14:13 起 20+ 连败、每 ~3s 一轮，"同一 NPC 报错一整天"即此；落盘请求体 `docs/actual_prompts.md` 消息[4] 实锤毒条目）。双修复：① **入口消毒** `sanitizeToolCallsForHistory`——落历史前丢弃空 name/空 id/arguments 非法 JSON 的条目（与解析层丢弃同口径、warn 日志留现场），占位 tool 与消毒后列表一一对应；动作队列不受影响（文本兜底照旧），历史里只留 venus 下次会接受的条目。② **400 连击自愈**——`venus400Streak`（agentContext atomic 计数，跨层共享：战略/战术共用同一份历史）连续 `venus400StreakThreshold`=3 次 `isVenusHTTP400`（"venus status 400:" 判据；**排除 4001**——那是 HTTP 500 + body code 的 tools 校验失败、同体重试可救、与历史无关）即 `ClearConversation` 清空重建（毒消息无法定位修复，重置是唯一恢复路径；连带重置 tacticalHeaderPlan，下轮自动回全量头），成功调用清零连击；兜住入口消毒未覆盖的其余投毒路径
- **多段动作计划 + time_to_stop**：LLM 一次返回 1-4 个动作段，`duration` 在工具 schema 中必填（2026-09-03 起——实测 optional 的 duration 从不被填），规则 8 要求末段 duration = 时段剩余时长；到点 `ClearInFlightKeepQueue` 打断当前段、保留队列继续下一段，stop 延迟到下一段下发前补发（期间 NPC 继续旧动作不呆站）。**slot 切换事件化（P3-8，§4.5"时段切换也走队列"）**：slot 过期**不强切**——`advanceSlotIfNeeded` 只标记 `slotSwitchPending` + 清 currentSlot，真正清理（`ClearForSlotSwitch` + 计划内结束捕捉）等安全点（无在途动作）由 `processSlotSwitch` 执行；末段终止靠自身 time_to_stop 预算（目标时刻通常 ≈ slot 边界，±几分钟）或自然完成，反应进行中同样只挂 pending。**processSlotSwitch 反应守卫（2026-10-21）**：reactionActive 期间不处理挂起的切换——安全点定义"无在途动作"恰好覆盖"刚提交还没 pop 的反应队列"（09:42 实测：社交 replan 提交的应答 speak_to 在同一毫秒被 ClearForSlotSwitch 清掉、从未下发，邀请方等满 no_reply 窗；pending 标志可比反应开始还早，竞态窗口极宽）；反应窗口有界（截止切回/日程 refill/speech 终局都清窗口），届时切换在下一安全点自然进行。实测（2026-09-24 会话）：全天 24 个 stop_action 中 23 个是段/slot 切换的延迟补发、1 个 force 事件，slot 切换机制本身零 stop；slot 过期 → 安全点清理间隔 0-19s（反应跨边界挂起等反应结束）
- **time_to_stop 兜底**（不依赖 LLM 自觉）：`fillDefaultDurationForRest` 给所有休息动作（含队尾）补 1800s、`fillDefaultDurationForWork` 给所有工作类动作（含队尾）补 5400s——队尾不设 duration 会导致 P3-8 延迟切换后 `processSlotSwitch` 永远等不到安全点。**已知空缺**：兜底只覆盖 rest/工作类 interaction，charge/sleep/repair/surf 及复合工具漏 duration 时无兜底——复合 cmd 豁免 `armActionTimeout` 且无 time_to_stop 即无终止路径（实测 09-23/24 会话 136 个长动作仅 2 个漏 duration，均为 atomic 有超时兜底，未发生悬挂）
- **LLM 失败兜底**：战术层分解失败且队列空时补发 `fallbackRetryActions()`（speak"网络波动了"+ generic_act look_around 30s），避免呆站，动作执行完 completion 再唤醒重试
- **zone 透传**：`mapTacticalAction` 对 `InteractSmartObject` 透传 LLM 填写的 `zone` 参数（UE 支持），否则"去中央广场长椅"会落到 NPC 所在 zone 的设施
- **move_to/turn_to 目标校验（2026-09-21）**：按 target_type 校验必填参数（agent/smart_object/zone → target_id 必填；position → target_position 必填），缺目标指令在 MCP 侧拒绝（不再透传 UE——实测 UE 对无目标 MoveTo 秒回 success，逃跑从未发生且队列瞬间耗尽触发连环 refill）；拒绝原因以 user role 注入会话历史（镜像动作完成结果的注入形态，带 tool_call_id），下一轮分解 LLM 可见并自我纠正。schema 层 `capabilityParamsSchema` 对 target_id/target_position 描述按 target_type 给完整指引（无论 registry 来自 seed 还是 UE push——UE push 的描述只提 actor，LLM 曾因此输出 target_type=zone 却无 target_id）。顺带修复：LLM 坐标经 json.Unmarshal 是 []any，旧 `[]float64` 断言不成立，target_position 从未透传过
- **`replanInProgress` mutex**：防止 worker 的战术层重规划和 `/debug/schedule` 注入并发调用 `tacticalHc` 冲突
- **`debugOverride`**：仅阻止 worker 的 idle-wait refill，**不阻止**正在 LLM 调用中的 refill——所以 `/debug/schedule` handler 会同时设 `replanInProgress=true` + `debugOverride=true`
- **`currentSlot` 加 `__debug__` 前缀**：防止注入的 slot 和 dailyPlan 同名 slot 碰撞触发 `redecomposeCount >= 1` 限制
- **反应层去抖**：`lastReactiveAt` map 按 trigger 类型去抖（periodic 60s / zone_change 45s）
- **反应层 replan**：决策为 `replan` 时调 `ac.tacticalRefillForReplan`，会重置 `actionQueue` 重新调战术层 LLM
- **world_event 事件系统（事件驱动设计 §四，P1 系列）**：runtime 分发 `world_event`（`world_event_dispatch.go`）。force=true 走硬保证通道——**零 LLM、零去抖、不可否决**，stop 在 WS 接收路径同步发出（goroutine 之前）+ 清在途追踪（stash 保 action_history）+ 注入【强制打断】hint + 异步 `forceInterruptReplan` 重规划（prompt 层渲染为【紧急事件】最高优先级指令，授权暂停时段目标、豁免时长填满）；失败兜底 `abandonCurrentPlan` 清旧队列走 worker 自然 refill。**社交 speech 事件例外（2026-10-15）**：UE 的 speech_heard 恒带 force:true，渲染成【紧急事件】会把闲聊当威胁应对（实测应邀方被"处理完事件回岗位"推回分拣岗、邀请方收到应下后用独白 speak 而非 speak_to 回话）——`forceEventHint` 对社交类（`isSocialSpeechEvent`：category=social 或 event_type=speech_heard，容错 category 缺失）改注入【社交回应】hint，渲染层（`tacticalHintLine`【社交回应请求】分支）给出双向应邀决策：受邀方结合好感度/日程/性格决定应邀（接受则一条 speak_to 礼貌应下——**speak_to 一律单动作**，约定的事项由对话结束 hint 承接执行，见"对话结束逻辑"条）或婉拒；收到自己邀请的回应则按应下/婉拒跟进；路由路径的社交 hint 同步收敛为"前缀+事件"（指引统一由渲染层承担）。打断记账标签同步（"被社交对话打断："）。force=false 入 per-agent 事件队列（`pkg/agentstate/world_event_queue.go`，上限 64 丢最旧、event_id 去重防 seq 重放、drain 全取不 pop、仅 Stop 清——slot 切换/replan 不清，安全点在 completion 之后）。手动模式同反应层口径丢弃。联调注入端点 `POST /debug/event`（控制台"事件下发" tab，15 预设 + 自定义）
- **combat-detach 战斗让位（2026-09-22）**：攻击事件携带 `detach:true` = **UE 战斗 AI 接管身体，agent 让位**——与 force 反应语义相反（实测 bug：MCP 1 秒内重规划下发逃跑动作，UE 战斗 AI 从未拿到控制权）。入口 `handleCombatDetach`（handleForceEvent 首分支）：掐在途 LLM + 记住在途 action_id（`combatYieldPrevActionID`）但**宽限期内不发 stop**（`combatYieldTakeoverGrace`=2s wall：stop 的 UE 语义是"中断行为树"，迟到的 stop 会把 UE 刚启动的接管连同旧动作杀掉；UE 接管应自行停旧动作并回 interrupted completion，`recordActionCompletion` 里的挂钩收到即免 stop）+ `ClearForReplan`（清队列/slot/在途 stash，**事件队列保留**）+ 取消 timer + `clearReaction` + 清 slotSwitchPending + 置 `combatYield`；**不重规划不 hint**，重复攻击幂等（仅刷新 TTL 起点与 lastEvent）。worker 让位守卫里 `checkCombatYieldTakeoverStop`：宽限期过旧动作仍未结束（接管未触发）→ MCP 代为 stop 恰一次，**脱管物理落地**（机器人停止 agent 行为——2026-09-22 用户报告"显示脱管但继续原动作"的修复）。其余抑制面：worker 守卫（processSlotSwitch **之前**）挡 slot 切换/pop/refill（`checkCombatYieldExpiry` 查 120 游戏分钟 TTL 兜底）、detectDayRollover 跳过、`route()`/`routerInterrupt()` 顶部守卫（事件留队列）、guardedExecutor 拒绝、/debug/schedule 409。**归还**：让位中的 `combat_exit` 确定性接收（不走路由）；`reclaimFromCombatYield`：清让位 + 若旧动作仍未停再补一次精确 stop（STOP_ID_MISMATCH 无害）+ Echo 攻击回声 + `SetReplanHint(【战斗结束】…)`（tacticalHintLine 渲染战后恢复指引）+ **归还只做战略修订（2026-10-12）**：不再做专门的战术重规划——worker 自然 refill 会带着【战斗结束】hint 重新分解当前时段（`selectCurrentGoal` 按游戏时间重推），与专门战术 replan 撞车会让 NPC 连说两遍"战斗结束"（实测双 speak）；归还只回声攻击/脱战事件 + 落 hint + 唤醒 worker，再 `maybeStrategicReplan` 战略层修订剩余时段（`maybeStrategicReplan` 顶部让位守卫挡住重复攻击重新让位的竞态窗口）。**战略跳期间放行 pop（2026-09-24）**：`replanKind` 区分 slot 持有者——战略修订只改 dailyPlan/slot 不碰已提交的战术队列，worker 在其 LLM 在途期间照常 pop（slot 过期 pending 也照常，旧队列动作有 time_to_stop 有界，修订落地后安全点收敛到新计划）；修复前修订 LLM 60s 超时期间已提交队列被 replanBusy 守卫挡住不能 pop，NPC 呆站、slot 过期后队列被安全点清理丢弃。**空队列同样放行 refill（2026-10-20 傍晚）**：09-24 的放行只覆盖"队列非空可 pop"——队列空 + 无在途时禁止 refill 的"防白烧 LLM/防写回竞态"保护没有旧队列可保，只剩呆站到修订超时上限（实测：社交约定后 slot 到期跨反应窗口触发战略修订，venus 战略调用挂 67s，H-01 队列空呆满全程直到仿真停止；战术层虽已切 DeepSeek 也无用武之地——refill 被禁）。现在战略修订在途且队列空 + 无在途 + 自动规划时放行 `tacticalRefill`（按游戏时间选旧计划当前时段分解，修订落地后安全点照 P3-8 收敛）；`processSlotSwitch` 仍保持延迟（清队列语义与修订写回竞态）。战术 replan（force/路由/`/debug/schedule`）在途保持全跳过）。**脱战桥接动作（2026-09-24）**：归还即下发短 `generic_act look_around` 填补 worker 自然 refill 的 LLM 空窗（`dispatchCombatExitBridgeAction`，combat_exit 与 TTL 归还共用；goroutine 必需——wsserver handler 在读循环 inline 执行而 `SendAction` 要等同一条读循环才能读到的 action_started ack，同步调用必死等；**不 recordActionStarted 不记在途**——入账会挡 `PopActionIfIdle` 在途守卫让新队列等桥接回执、还会污染 UnfinishedTask/LastEndResult 战略修订原因，untracked completion 不合成事件），收尾复用 slot-switch 同款 `pendingStopActionID` 延迟补发（pop 下发首个真动作前精确 stop 桥接；桥接自然完成则 `RecordActionCompletion` 按 ID 匹配自动清标记不发陈旧 stop；worker refill 失败兜底后首次 pop 照样收尾）；归还 goroutine 先 `<-bridgeDone` 再战略修订（防桥接命令与 worker refill 的 action_command 交错）。**战斗动作下发（2026-10-09）**：UE 侧战斗脱管语义变更——detach 后 UE 仍立即接管并拒绝一切非战斗 action，但**不再默认启动战斗行为树**，改为**等待 agent 下发战斗动作**（未来横向扩展多风格）。agent 首次进入让位时经 jev 路由选定风格并直发：`combatStyleOptions`（choice 选项 key = cmd 名，当前仅 `ExecuteCombatBT_Default`）+ `routeCombat`（战斗风格 choice 判决，**不加** `combatYieldActive` 让位守卫——让位即战斗上下文；白名单校验 choice、失败/未知/缺客户端回退默认，战斗永不死锁；埋点进 llmmetrics layer="router"）→ `dispatchCombatAction`（goroutine：jev ~1s + `SendAction` 等 ack 不能同步于读循环；**重查 `combatYieldActive()`** 防 jev 期间 combat_exit 已归还抢身体；**不记在途**——战斗动作非战术决策产物，入账会残留 `CurrentActionID` 挡归还 pop、污染 UnfinishedTask/LastEndResult；**空 params**——UE 已知战斗上下文）。cmd 常量 `CmdExecuteCombatBTDefault` **不进** `IsCompositeCmd`/`BuiltinCmdCapabilities`/MCP 工具——战术 LLM 永不选它，战斗动作只由 detach 路由下发，与战术层隔离；UE 在 combat_exit 时自行结束战斗 BT，归还无需 stop 战斗动作。**属性基线与 delta（2026-09-23）**：让位入口快照 `combatYieldPhys`（首入口锁存，重复攻击不移动基线；无感知冷启动为 nil），归还时与当前值对比渲染"战斗期间物理属性变化：关节磨损 22→68（+46）…"——战术 hint 与战略修订原因共用（磨损暴涨正是"07:00 计划对当下无知"的典型案例）；无基线/无实变优雅降级（战略原因退"物理属性可能已显著变化"）。tacticalHintLine 的【战斗结束】分支越过警戒阈值时追加 `physicalAlertConstraints` 硬约束（与物理告警 hint 同一套"必须优先维护保养/充电"），泛化的"结合物理状态"升级为硬要求；**恢复时长建议（2026-09-24）**：磨损/疲劳越线时约束行附带 duration 建议——维修、睡眠舱休息不少于 7200 秒（约 2 游戏小时；电量充电恢复快 -80/h 不带长时建议），并明确恢复动作可超过当前时段剩余时长（P3-8 下时段切换在安全点等恢复动作结束后进行，不要为赶日程缩短恢复）；规则 7/精简规则同步"恢复类动作按恢复需要、不受档位限制"。让位状态**跨 UE 断线重连存续**（stop() 不清；TTL 有界兜底）。协议容错：UE 首版发 `event_type:"attacked"` 短名且缺 category——`pkg/prompt` 别名表 + `EffectiveCategory`/`EffectiveEventType` 统一归一，`IsCombatStartEvent`/`IsCombatExitEvent` 匹配 category 空或 player_interaction + 别名。**2026-09-22 晚联调跑通**（此前"三种配置（MCP 抢身体/静默+stop/静默+不 stop）均未触发"的根因是 UE 侧 C++ 改动未重新编译，非 MCP 侧问题）：UE 战斗行为树毫秒级抢占旧动作并即时回 interrupted completion（宽限期代为 stop 从未触发——回执先到即免 stop）、机器人真实逃跑、combat_exit 到达即归还（ttl_expiry=false）；exit 晚到/缺失时 120 游戏分钟 TTL 兜底归还（实测 exit 晚于 TTL 2s 到达的案例，落到非让位路径无害）
- **在途战术层 LLM 请求可取消（§3.3 唯一盲区，P1-4）**：`generateTacticalPlan` 咽喉点注册 cancel 句柄（coordMu + 世代号防旧调用误清新注册）；force 事件在接收路径同步掐掉在途调用（venus ctx 贯穿 HTTP，sendMu 随之中止释放）。半截思考零残留（`agenticTurn` 成功才落历史）。被取消方经 `cancelledByForce`（错误为 Canceled 且 parent ctx 存活——区别于超时 DeadlineExceeded 与父 ctx 关停）判定后**跳过一切失败兜底**：worker `tacticalRefill` 不补 fallback 动作（"网络波动"speak 会与 force 反应打架）、`tacticalRefillForReplan` 返回 `(false, cancelled=true)` 让调用方让位（不清队列不覆盖 hint）、`/debug/schedule` 返回 409。`forceReplanWaitLimit` 由 ~70s 收窄到 5s（取消后 holder 毫秒级释放 slot）
- **持续威胁情境 + 事件回声 + 反应衔接（P3-9 后续修复 A/B/C，2026-09-18）**：排查"被攻击后反应完成即声称威胁解除"的幻觉——事件是边沿触发，威胁在 combat_exit 到达前持续存在，但反应耗尽后的 refill prompt 里威胁痕迹为零（hint 已消费、事件未入队、未完成任务槽已被反应清空）。**A（情境状态）**：`activeSituations`（agentstate/active_situations.go）——`player_attacked/targeted` 在 handleWorldEvent 中央登记（同 kind 幂等保留最早起点）、`combat_exit` 解除、120 游戏分钟 TTL 兜底；注入四处：状态栏"当前处境"行、战术层【当前处境】段（每轮 refill 可见，防自行认定解除）、路由器输入、紧急事件块补"威胁在解除信号前视为持续"。**B（事件回声）**：force/router 打断的 replan 成功后 `EchoWorldEvent` 把事件再入队（跳过去重）——反应耗尽后的第一次日程 refill 在【发生的事件】里再见到它一次（消费即清）。**C（反向 hint 抑制）**：反应窗口仍 armed 的 refill 跳过"上次队列提前耗尽…安排长动作收尾"自动 hint（它会把 LLM 推回填满时段，恰与"刚被打断"的语境相反）；情境由 A 承载
- **事件驱动战略层 replan + 写回（§5.5，P3-9 升格）**：07:00 的计划对当天事件无知——反应跨时段被切断（advanceSlotIfNeeded 返回反应进行中）/ 反应超截止被切回日程（checkReactionDeadline 返回 true）/ **战斗让位归还（combat-detach 归还链，2026-09-23）**时触发 `maybeStrategicReplan`（`strategic_replan.go`，顶部让位守卫防竞态）：`generateDailyPlanCore`（triggerStrategicPlanning 抽出的无兜底核心）带修订上下文（原计划 + P4-10 未完成任务槽/上次结束原因）重规划**剩余时段** → `mergeRemainder` 合并（已过时段保留为既成事实）→ `SetDailyPlan` 写回（write-through 持久化）+ `SetCurrentPlanIndex` 指向首个新时段 + `RecordPlanRevision` 留痕（P5-15 反思输入）。节流：每游戏日上限 5 次（尝试即计数）+ 相邻间隔 ≥2 游戏小时（`TryBeginStrategicReplan` 原子判定）+ 手动模式不触发。失败/被 force 取消保留旧计划；持 replanInProgress slot 并注册 LLM 取消句柄
- **安全点 drain：事件队列 → 战术层第三输入（§4.4/§5.1，P3-7）**：`generateTacticalPlan` 咽喉点快照事件队列 → `FormatWorldEventList` 渲染 → 战术 prompt 【发生的事件】段（全量与 compact 形态都注入——事件是逐次数据非日内不变块）。**快照注入 + 成功后清空**（`ClearWorldEvents`）：LLM 失败/被 force 取消时事件保留在队列，下一次分解重新看到。drain 覆盖三个调用方（worker refill / force 与路由打断的 replan / debug schedule）——force 打断的重规划一次看到"紧急事件 hint + 队列攒下的事件"全貌
- **事件合成器（P4-12，`event_synthesizer.go`）**：本地检测到的状态变化（物理警戒带突破/动作异常完成/event_notification）合成 world_event 走统一事件管道——`dispatchSynthesizedEvent` 入队 + 路由器裁决。event_id 用 `synth_` 前缀与 UE 的 `evt_` 区分。UE 侧 world_event 推送就绪后本层整体删除。这是旧反应层退役后的替代：旧 Ollama continue/observe/replan 决策由路由器 interrupt/入队替代，物理告警升级（upgradeIfPhysicalAlert）由路由器 LLM 判断替代，去抖（lastReactiveAt）由 P2-6 反应护栏（severity 严格递增 + 截止时间）替代。**结构化数据 + 预期回包不合成（2026-11-03）**：action_failed 合成走 `dispatchActionAnomalyEvent`——Data 带 typed cmd/reason（`recordActionCompletion` 改三返回值把 `res.Cmd` plumbing 出来）+ description 渲染兜底（`ActionAnomalyData.Description` 新字段，`worldEventDescription` 在 Cmd 为空时渲染原文），修复旧实现只塞 `{"description": detail}` 导致渲染出"动作  执行失败（原因 ）"空壳、jev 无事实可判（概率 0.51≈抛硬币）；`CompletionResult.WasExpectedInterrupt`（`lastEndInterrupted` 预告位暴露）——我方主动 stop（force/路由/社交/debug 注入，debug/schedule 路径补了 recordInterrupted 预告）的 interrupted 回包不再误合成 action_failed（2026-09-20 的三条守卫 preface/untracked/self-stop 漏掉这类回包），真异常（UE 自行打断、failed/error）照常合成
- **轻量事件路由器（§4.3，P2-5）**：非 force 事件入队后异步走一次判决模型裁决（`event_router.go` + `pkg/jev`，Venus 判决 API `POST /v1/systemone`，`--jev-timeout` 默认 5s，per-agent `jevHc` 客户端做**无状态单发**——不碰会话历史，实测 ~0.5s）。请求 = 结构化 state：`conversation`（agentic loop 近期历史，`routerConversation` 取战术层会话尾部窗口 20 条——user 指令原文、assistant 文本或 tool_calls 单行渲染、tool 占位跳过）+ `user`（`prompt.BuildRouterUserAttributes`：npc/world[无区域行/无设施类别行]/role/relationships/physical_state/current_action/active_situations/event——空字段整体省略，规划上下文[生产工作流/设施名册]不入，对紧急度裁决是噪音）+ 三问：`should_interrupt`（noul 概率，阈值 0.5 严格大于）+ `motive`（choice：urgent/situation_resolved/social/no_interrupt，判据写在 instructions/criteria 里）+ `severity`（score 锚点 [日常小事, 紧急事件]）。`routerDecisionFromJev` 映射回 `RouterDecision`（severity=score×10 钳位；social 封顶 3；noul 高但 no_interrupt 自相矛盾 → 保守入队）；判不准倾向入队（超时/HTTP 错/缺 answer/矛盾全部降级 enqueue，事件已在队列不丢）。interrupt=true → `routerInterrupt`：撤下队列中该事件（`RemoveQueuedWorldEvent`，正在处理不再等安全点）+ 掐在途 LLM + stop 在途动作 + hint 带"路由裁决：紧急"+理由 → 复用 `forceInterruptReplan`。裁决输入含角色性格/人际关系（Stage 5）/当前动作与已执行时长/物理分档——同一事件不同 NPC 因关系/性格产生不同裁决（涌现验收点）。路由调用进 llmmetrics（layer="router"，事件路由）与 actual_prompts.md（`### 事件路由 · state` 段）。**收口首句例外（2026-11-03）**：END 标记的 speech_heard 到达、但接收方会话还没有任何己方轮次（`conversationHasOwnTurn`）时不提前收口——第一句就"收尾"要么是开场告别、要么是被 LLM 误标为收尾的邀请，完全收口会让开场话语无回应时机（实测误标邀请 34 墙秒无应答、邀请方以为被无视）；照常走判决/确定性打断。已有往来后的收尾句收口不变（防 ping-pong）
- **反应护栏（§4.5 两条，P2-6）**：打断（force 或路由）产生的规划窗口即"反应任务"，agentContext（coordMu）记录 `reactionActive/reactionSeverity/reactionDeadlineGameSec/reactionSocial`。① **截止时间**：60 游戏分钟（`reactionDeadlineGameSec` var），worker 循环 `checkReactionDeadline`（replanBusy 守卫后）到期硬切——stop 在途 + 清队列 + 【反应截止】hint 回到日程；"仍 armed"即自反应起无日程 refill（refill/slot 切换/下线都会清除），截止硬切因此有明确归属。② **severity 严格递增**：路由打断须严格高于当前反应的 severity 才放行，否则事件保持入队（保守方向与 §4.3 一致）；force 不受限（§4.2 不可否决）但会重置窗口 bar。severity 收敛 0-10。**armed 窗口内 refill hint 按反应类型措辞（2026-10-15）**：`reactionSocial`（社交对话开启的窗口）→"对话仍有来有回请继续用 speak_to 回应，对方无回音则回日程"；其余 →"威胁仍未解除请继续应对"——紧急措辞套在闲聊上会把应邀方推向"对方不搭理我"的结论（实测）
- **对话结束逻辑 P0（2026-10-16）**：对话此前无主动结束机制——结束 = 护栏碰巧拦断 + UE no_reply 兜底 + 5 游戏分钟静默关会话（应邀后双方无限互道"走吧"的 ping-pong 即此产物，最终靠 severity 阶梯碰巧拦断收场）。设计三层分工：LLM 判意图（软）/ 代码管护栏（硬）/ UE 报事实（边界，P1 待 UE 排期）。P0 三件套（全 MCP 侧）：① **【对话记录】注入**——`conversationContextForPrompt`（relationship.go）把进行中会话的完整 transcript（自己的话由 speak_to 下发时记录 main.go recordActionStarted、对方的话由 speech_heard 记录，own turns 渲染为"你"）+ 轮次计数注入【社交回应请求】hint（TacticalInput.Conversation，仅该分支消费、非社交场景零污染）——LLM 不再只看最后一句话，能判断"对话进行到哪一步"；② **结束条件指引 + 取消强制回话**——删"让对话有来有回"（ping-pong 推手），明确三条结束条件（目的已达成/寒暄 2-3 轮无新信息/对方明显在收尾）任一满足即主动收尾（告别 + 各自行动），且"不再回话"是合法选择；③ **轮次上限**——`maxConversationTurns=6`（一句 speak_to 计一轮，双方会话各自全量计数），超限 hint 追加硬收尾指令（【对话轮次已达上限】一句告别，禁新话题/再邀请），对话收敛不依赖采样自觉。**单动作 + 守约 hint 修订（2026-10-16 下午）**：speak_to 分解**一律单动作**（邀请/应答/告别都不追加活动段——hint 里曾经的规则 10 豁免已删，实测豁免文案扛不过规则 10 正文、queue_len=1 反复出现且约定丢失）；约定的执行改由**对话结束 hint** 承接——worker 循环 `closeConversationIfIdle` 关会话时（与好感度评估同点）`conversationEndedHint` 注入【对话结束】hint：完整对话记录 + "若约定了一起做某事立即执行，**守约优先于当前时段目标、不要因时段切换弃约**；被婉拒/寒暄则回日程"——补上 slot goal 主导下事件仅作弱参考的仲裁缺口（实测应邀后 slot 切走、约定彻底丢失）。**活跃会话回话确定性打断（route()）**：speech_heard 的说话人 == 当前会话对方且已记录 ≥2 轮时，不判紧急度直接按 social（severity 2）打断——判决模型不知道"这是我在进行的对话的回话"，其判决近似随机（实测应邀答复被判不紧急留队列）；轮数 ≥2 是"回话"与"新对话第一句"的分界（handleWorldEvent 的 transcript 记录会把任何新说话人置为会话对方，新搭话仍走 judge）。severity 阶梯照常生效——更高优先级反应窗口（威胁）进行中时回话留队列，由对话结束 hint 承接守约。**speak-only 纠正三件套（2026-10-16 晚）**：实测邀请达成后 H-01 连续 6 轮 refill 全部只返回一条 speak（"空结果"）→ 兜底 look_around 循环呆站；原请求体直放 venus 复现：无对话历史 1/5 speak-only，**带两轮单 speak_to 对话历史 6/6 speak-only**——few-shot 把模型推向"只输出一条宣告独白"（模型把"宣告执行"当成"执行"）。修复：① **纠正性重试**（`emptyResultCorrection`）——空结果第 1 次重试同体（采样方差），第 2 次起在请求尾部追加瞬态纠正指令（"宣告意图的独白不等于执行，本次必须输出至少一个带 duration 的长动作"），不落历史（成功后落历史的是 userContent 本身）；② **守约 hint 明确形态**——"只用一句独白宣告'我要去了'不算执行，本轮分解必须包含执行约定的长动作（可先一条 speak 表态，长动作不可省略）"；③ **收窄"只输出一条 speak_to"措辞**——改为"本轮对话回应只需要这一条 speak_to（此限定只针对对话回应这一轮，其他轮次仍须遵守规则 1）"，防止残留在历史里泛化到后续所有轮次。**呆站双修（2026-10-16 深夜）**：邀请达成后 NPC 呆站 ~24s 才去跑步——① **会话 idle 超时短于回话往返**：`conversationIdleTimeoutGameSec` 旧值 5 游戏分钟 @time_scale=90 只有 3.3 现实秒，比"对方回应 LLM（1-6s）+ UE speech_heard 派发"还短——会话在对方回话到达前被关（实测 H-01 会话在应答到达前 0.12s 关闭），turns 重置 → 确定性打断（需 ≥2 轮）失效 → 应答走 judge 被判不紧急留队 → 邀请方永不确认、应邀方 no_reply 等满窗。修复：idle 超时提至 **20 游戏分钟**（@90x ≈ 13 现实秒，覆盖 no_reply 窗 ~10s + 一次 LLM 往返）。② **worker refill 的守约豁免**：speak_to 在 06:00-07:00 规划窗口内完成时，`tacticalRefill` 裸 `selectCurrentGoal` 命中屏蔽静默跳过（goal=""，零日志），守约 hint 已注入却被挡 8 秒直到感知 tick 把 game_time 推过 07:00。修复：`tacticalRefill` 改走 `resolveReplanGoal`（与 replan 路径同源），【社交回应】/**【对话结束】** hint 都豁免窗口回退首个时段 goal——守约是即时行动。**ends_conversation 告别即行（2026-10-16 深夜）**：no_reply 窗（~10 现实秒）里只有前 ~2 秒有用——话语在 speak_to 下发后 1.5-2.2s 即送达（UE 派发 speech_heard），其余是纯"等一个可能永远不来的回话"。三件套：① speak_to schema 增加可选 `ends_conversation`（MCP 侧控制字段，mapTacticalAction 透传前剔除；**2026-10-20 晚改为必填 + 默认 false 语义**——实测可选时 LLM 把"对方应下后的确认句"不当告别直接省略字段 → 整窗空等 10s，必填迫使模型对每一句 speak_to 显式决策"是否收尾"，漏填时运行时按 false 容错回落等窗路径）——LLM 判断本句是告别/收尾时填 true；② **送达即释放**（`releaseFarewellSpeaker`，handleWorldEvent 的 speech_heard 派发点挂钩）：speech_heard 到达即话语送达的铁证（MCP 中转所有消息），说话人若带标记的 speak_to 在途立即 stop——记账走 recordScheduledEnd（使命完成=正常结束）+ ClearInFlightKeepQueue（stash 保 history、preface 抑制 action_failed 合成），说话人 worker 唤醒；③ **队列直通**：带标记的 speak_to 是确定性短命的（送达即停），其后可直接跟后续动作（告别句 + 约定活动同队列）——送达后立即 pop，**跳过 no_reply 窗与收尾 refill 两次 LLM 延迟**（实测 LLM 正确输出 `[speak_to(ends_conversation), InteractSmartObject(跑步机), InteractSmartObject(回岗位)]`）。speak_to 签名检查用 target_agent_id（与 recordActionStarted 的 own-turn 挂钩同口径，cmd 名是 registry 动态的）。普通对话轮不带标记照旧等窗（期待回话，回话到达时确定性打断本就会 stop 它）。**ends_conversation 透传 UE + 接收方确定性收口（2026-10-21 下午）**：UE 侧给 speak_to 加了 ends_conversation、会把它放进 speech_heard 传给收听者——`SpeechHeardData` 加 `EndsConversation` 字段、`mapTacticalAction` 不再剔除（只剔除 duration），记账 params 直接带标记（`releaseFarewellSpeaker` 的"送达即释放"据此生效，删除 049611b 的记账回填）。`route()` 新增收口：speech_heard 带 ends_conversation=true（说话人已明确收尾）时不触发确定性打断、不问 judge——事件留队列等安全点 drain，接收方继续当前动作（如赴约），从代码层终止 ping-pong（此前接收方看不到标记，把"走吧跑起来"当对方还在说话反复回话，11 轮无了期且互相打断对方的跑步动作）。ends_conversation 同步**恢复可选 + 精确语义**（撤销必填）：必填曾导致 LLM 每句都填 true、接收方据此误收口、对话起不来——现只在"对话真正的最后一句"才填 true，期待回话/继续行动一律 false；四处文案（schema 描述/规则 10/精简速览/【社交回应】结束条件③+收尾指引）统一明确"该标记会告知对方对话结束、对方不再回话"。P1 待做：UE `conversation_end` 事件（no_reply/out_of_range/action_started，MCP 确定性关会话——比静默超时更及时）。**no_reply 即关即 hint（2026-10-20）**：idle 关会话与"应答完成立即 refill"存在竞态——应邀方 speak_to 以 no_reply 完成会**当秒** signal 唤醒 worker refill，而守约 hint 要等 20 游戏分钟 idle 超时才注入；真空期里无对话上下文的 refill 被 armed hint 的"对方无回音则回日程"推回日程（实测：H-01 应下晨跑邀请后 refill #1 说"该去车间开工了"、refill #2 下发 workbench/assemble，守约 hint 晚 6s 注入、纠偏 refill 又恰逢仿真停止未落地——邀请方无此竞态，其 idle 计时从"听到应答"起算、与下次 refill 天然同步）。修复：`recordActionCompletion` 里**对话终局即时判定**——speak_to 以 `result=success` 且 **reason 含 `speech:`**（UE 回执实证有 `speech:no_reply` 与 `speech:replied` 两种——等不到回话与等到了回话对称，本句都是说话人视角的对话收尾；2026-10-20 17:58 实测 replied 不触发时，完成触发的 refill 被 armed hint + 自己的时段目标带偏，约定被稀释成"继续拉伸"后弃约）且会话 peer == 该 speak_to 的 target（`closeConversationByPeer`，签名检查用 target_agent_id 同口径）时，立即关会话（复用 close+好感评估+`conversationEndedHint` 链）+ `SetReplanHint` + **清社交反应窗口**（armed hint 的"对方无回音则回日程"措辞与守约指令互斥）；hint 在 `signal()` 之前落好，woken refill 的 prompt 当场带守约上下文，竞态关闭。replied 的 transcript 自带对方告别语，守约 hint 的"被婉拒/寒暄则回日程"分支自然接管语义。邀请方 no_reply（邀请无人应）**不再走守约 hint**——改注入未获应答变体（见下方 2026-11-03 条目）；peer 不匹配（陈旧会话）与 interrupted（被回话/stop 打断）不触发，走原 idle-close 兜底。**time_to_stop advisory hint 优先级保护（2026-10-20 傍晚）**：`checkTimeToStop` 到点注入的"本时段已执行长动作…"是低优先级 advisory，此前无条件 `SetReplanHint` 覆盖 pending hint——实测守约 hint 注入 6s 后撞上切段被覆盖，随后的 refill 只见时段目标、NPC 弃约去上网；现在 pending hint 以"【"开头（结构化高优先级：【对话结束】/【战斗结束】/【强制打断】等）时不覆盖（advisory 文案本就不带前缀，天然可区分）。**会话续接（2026-10-20 下午）**：即时终局引入新边界竞态——应邀方回话往返（收邀请 ~2s + 判决/LLM ~5s + 派发送达 ~1s ≈ 8s）与邀请方 no_reply 窗（~8-10s）**几乎等长**，应答卡窗边送达时 UE 先按超时完成 no_reply、同秒再派发 speech_heard，即时关闭清掉邀请轮次、应答重开一个只剩自己一句的"失忆"会话——社交 replan 看到孤立发言、守约 hint 只有邀请半边，约定被劈成两个碎片（实测邀请方回了车间；应邀方路径不受影响，守约 hint 带完整 2 轮 transcript 正常赴约）。修复：`closeConversationLocked` 关闭时留快照（peer+turns+关闭时刻），`recordConversationTurn` 起始新会话且同 peer 的快照在 `conversationCarryoverWindow`（60 现实秒，包级变量可测）内时旧轮次先行续接——迟到的应答落进带完整 transcript 的会话，确定性打断（≥2 轮）成立、社交 replan 看到全貌；idle 关闭后迟到回话、战斗中断后续聊同享此续接（`clearConversation` 也经快照链）。已知取舍：竞态场景下同一段轮次可能被好感度评估计两次（先关一次、续接后再关一次），best-effort ±3 钳制下可接受。**守约 hint 时间免疫声明（2026-10-20 傍晚）**：修好投毒/竞态后 LLM 仍会弃约——对话往返在高倍速下耗 33 游戏分钟，约定盖着 07:47 的戳、守约分解时已 08:14，两个 NPC 各自以"时间已过"把约定脑补成已发生（"晨跑结束了"跑去分拣 / "晨练告一段落"跑去整理内务），且【附近NPC】里对方"没在赴约"的当前动作互为弃约证据。守约 hint 追加时间免疫文案：时间戳早于当前游戏时间是正常的（对话往返在高倍速下消耗数十游戏分钟），约定刚刚达成、仍然有效，立即执行不算迟到，禁止以"时间已过"为理由把约定叙述为已发生。**守约动作 duration 豁免（2026-10-20 晚）**：19:04 实测守约链路首次全链路闭环（speech 回执终局→守约 hint→treadmill/run 下发），但分解落在时段尾声，规则 8 把跑步 duration 锚定为剩余 420 秒 = 4.7 墙秒，跑步机刚上去就被 time_to_stop 切段送走（视觉上等于没赴约）——hint 追加"执行约定动作的 duration 按活动实际需要设置（如晨跑 1800-3600 秒），可以超过当前时段的剩余时长——时段切换会在安全点等约定动作结束后再进行，不要为赶日程把约定活动压缩成几分钟的象征性动作"（镜像物理告警恢复动作的豁免文案，P3-8 安全点机制本就支持）。**ends_conversation 填写率强化（2026-10-20 晚）**：同一实测中告别句未填 ends_conversation → 整窗空等 10.1s（对话收尾延迟的 68%）——四处文案（speak_to schema 描述/规则 10 收尾句例外/精简速览/【社交回应】结束条件①+收尾指引）统一明确"对方应下你的邀请后你回的确认句（如'好，那就这么定了'）也是收尾句，必须填 ends_conversation: true（不填则整窗空等约 10 秒）"。未做的两个加强项（已提议）：hint 加"对方可能尚未到达约定设施，勿以其当前动作推断约定已取消"；邀请方听到应答后的"一句确认告别"改为带 ends_conversation 的告别句 + 约定活动同队列（队列直通现成，邀请方立即动身、应邀方也能看到对方在赴约）。**接收方等待释放（2026-11-02）**：`releaseFarewellSpeaker` 只释放说话方（告别语送达即停自己的 farewell speak_to），接收方仍会呆站——2026-11-02 生日邀约实测：H-02 的邀请 speak_to 等到 H-01 的回话（带 ends_conversation=true）后，UE 要**再等满一个 no_reply 窗**（speech:replied 完成恰在回话到达 + 900 游戏秒处）才完成动作，期间 NPC 原地呆站、队列不能 pop、refill 不能开始（对照：H-01 侧 17ms 收尾，因 farewell 释放对称钩子存在）。修复：`releaseFarewellListener`（handleWorldEvent 的 speech_heard 派发点，`d.EndsConversation` 时调用）——接收方在途动作的 `target_agent_id == 说话人`（等待的正是这位的回话；等第三方/非 speak_to 不放行）即立即 stop 释放：记账镜像 farewell 路径（recordScheduledEnd + ClearInFlightKeepQueue + 取消 timer），**并自跑对话终局闭环**（closeConversationByPeer + conversationEndedHint + 清社交反应窗口 + extractAndReplanCommitments）——被 stop 的等待型 speak_to 迟到的 completion 是 interrupted（无 `speech:` reason），recordActionCompletion 的终局判定不会替它收尾；UE 若抢先以 speech:replied 优雅完成，第二次 closeConversationByPeer 无会话可关、自然幂等。hint 在 signal() 之前落好，woken refill 当场带守约上下文。呆站从 ~15 墙秒（10s UE 慢完成 + 3s refill + 2s slot 尾幽灵动作）砍到 ~5s（refill + 空结果重试）；UE 侧治本方案（收到回复即完成 speak_to）与之二选一**告别送达即终局闭环 + 守约 hint 履约一致性（2026-11-03）**：`releaseFarewellSpeaker` 原本只做 stop+记账+signal，守约 hint 依赖 UE 的 `speech:conversation_ended` completion 经 recordActionCompletion 终局判定注入——实测竞态：signal 抢跑，woken refill 的 prompt 比守约 hint 早 35ms 构造，空窗 refill 只见旧 armed 社交 hint（"对方已无回音或对话已结束，回到当前时段目标"）+ 时段 goal，应邀方（答应了"一起去跑步机跑跑"）被推回"晨练拉伸"goal、分解出原地 Exercise 而非约定的跑步机；守约 hint 21 秒后才被 slot 切换 refill 迟到消费——届时 goal 已反转（质检）、自己刚"锻炼过"（Exercise 被切段），LLM 把约定叙述为已履行（"拉伸得差不多了，该去车间干正事了"）彻底弃约。双修复：① `releaseFarewellSpeaker` 补终局闭环（镜像 releaseFarewellListener）：stop+记账后、signal() 之前完成 closeConversationByPeer(target) + clearReaction + conversationEndedHint 落 hint + 异步 extractAndReplanCommitments——farewell 送达即对话终局（UE 的 conversation_ended 回执本就是这个语义，只是把判定提前到送达时刻），woken refill 的 prompt 当场带守约上下文；UE 优雅 completion 迟到到达时终局判定发现会话已关（closeConversationByPeer 无会话可关）自然幂等。② 守约 hint 追加履约一致性条款："执行约定的动作必须与对话中约定的具体活动一致——约定里点名了设施或地点（如'去跑步机跑步'）就用 InteractSmartObject 使用该设施；原地拉伸、散步、做操等只是相近的活动，不算履约，更不得以'已经活动/锻炼过了'为由跳过约定——你在对话之后做过的其他动作不等于约定已经执行"（对齐 UE 新增 Exercise cmd 的语义竞争：其描述"晨练、拉伸"会在晨练 goal 语境下吸走约定语义）。**未获应答≠答应 + hold_sec 下限钳制（2026-11-03）**：邀约链路实测"被拒绝了还以为对方答应了"（H-02 对 H-05 的跑步邀请，拒绝 0.3s 后到达，H-02 已说"好，老张也答应了"并上跑步机）。故障链：① 邀请 speak_to 带 LLM 自填 `hold_sec:4`——UE registry 声明的 SpeakTo 参数（描述仅"说完后面向对方等待的时长"，无下限指引），等待窗 4s < 回话往返（路由 ~0.5s + 对方 replan LLM 2-6s + 派发送达 ~1s ≈ 4-8s）→ `speech:no_reply` 提前终局，turns=1（transcript 只有邀请、无拒绝）；② 守约 hint 对无应答会话无差别注入"若约定了立即去执行"（退出分支只枚举"被婉拒/寒暄/告别"，"未获应答"缺席）→ refill LLM 把无人应答的邀请读成已达成的约定；③ 迟到的拒绝带 ends_conversation=true → route() 收口跳过打断（"接收方继续当前动作"），错误动作跑满 time_to_stop。双修复：① `conversationEndedHint` 检测 transcript **无任何 peer 轮次**（`hasPeerTurn`，全部 speakerID==自己）时改注入"没有回应不等于答应：不要把邀请当作已达成的约定去执行，回当前时段目标"——保持【对话结束】前缀（slot 回退/advisory 优先级机制不变），覆盖 no_reply 即时终局与 idle 关闭两条路径；对话待办抽取 prompt 同步加守则"对方未明确应允的邀请不是约定，不抽"（本次实测抽取层侥幸没出错，守则把运气变设计）。② `mapTacticalAction` 透传点钳制：等待型 speak_to（ends_conversation != true）`hold_sec < 10` 一律提到 10（`minSpeakToHoldSec`，对齐 UE 默认 no_reply 窗）；farewell 型不受影响（送达即释放，hold 无意义）。**邀请误标收尾三连修（2026-11-03 晚）**：再实测发现邀请被 LLM 标成 `ends_conversation:true`（+hold_sec:2）——速览里「约定的事项在对话结束后由系统提示驱动执行」与「收尾句填 true 系统送达即执行不等回话」连读，LLM 推出"邀请=我最后一句话，说完对话就结束（系统接管）"→ farewell 释放让邀请方立即走人 + route() 收口让接收方完全不应答（被在途动作拖延 34 墙秒）+ 迟到的同意撞上邀请方在飞的 refill LLM（prompt 已发出、无同意）→ H-01「老张没应声去上工」vs H-05「好嘞走起」独自跑步。三修：① 速览邀请条款/完整规则 10/ends_conversation schema 三处 prompt 手术——邀请**必须填 false 且不要填 hold_sec**（邀请是等待对方答复的句子，对话要等对方回话之后才结束）、schema 判据收窄为"对方不需要回答任何问题"、写明误标后果（对方不被提醒回应、你收不到答复就走掉）；② route() 收口**首句例外**（`conversationHasOwnTurn`）——END 标记的 speech_heard 到达但接收方会话还没有任何己方轮次时不提前收口，照常走判决/确定性打断，保证开场话语必获回应时机；已有往来后的收尾句收口不变（防 ping-pong）；③ `correctUnansweredHint`（handleWorldEvent 的 speech_heard 派发点挂钩）——END 标记回话到达且当前 replanHint 是未获应答变体（含"没有回应不等于答应"标记）时，用完整 transcript（`conversationTurnsSnapshot`）重写为守约 hint + signal worker——"未获应答"的判断已过期，下一 refill 即按真实答复行事，不打断在途动作/LLM（纠正延迟有界于一个 refill）

### LLM 后端

MCP 直连 Venus（OpenAI Chat Completions 协议），战略/战术层调用 Venus。事件路由走 Venus 判决 API（`pkg/jev`，`POST /v1/systemone`，模型 `jev-1.13.0`——state+questions 三型问题：noul/choice/score，与 chat completions 同网关同凭据，`/v1/models` 列表不含它）。

**function calling**：战术层经 `tools` 请求字段下发工具（由 `capability_registry` 派生），`tool_choice=required`，多轮 messages（system + 历史 + 最新 user）。战略层用 Structured Outputs（`response_format` json_schema strict）。**DeepSeek 官方 thinking 模型降级（2026-10-21）**：`deepseek-flash`/`deepseek-reasoner` 是 thinking 模型，**拒绝 `tool_choice=required`**（实测返回 `Thinking mode does not support this tool_choice` 400）——`llmconfig.Backend.ToolChoice`（yaml `tool_choice`）设为 `"auto"` 时，`venus.Client.doSend` 把战术层的强制 required 降级为 auto（function calling 仍可用，只是不强制；战术层已有的 isEmptyTacticalResult 空结果重试 + parseToolCallsWithFallback 文本兜底承接"LLM 不调工具"的情况）。默认空串保持 forced-required（Venus / 非 thinking 后端行为不变）。

**启动示例**：
```bash
./agenttown-mcp --http :8760 --ws :9092 \
  --venus-url http://v2.open.venus.oa.com/llmproxy \
  --venus-api-key $VENUS_API_KEY \
  --venus-model deepseek-v4.1-flash \
  --venus-strategic-model deepseek-v4-pro
```

Venus 客户端无状态——每次调用全量 prompt，不复用会话链（战术层多轮历史由 MCP 侧 `agentstate.Conversation` 维护，非 Venus session）。战略/战术层 prompt 完全由 MCP 构造，所有上下文（角色、世界知识、物理状态）显式注入。

## 通信流向

核心消息流（三层决策细节见上方"三层决策架构"）：

```mermaid
sequenceDiagram
    participant UE as UE5
    participant WS as wsserver (MCP)
    participant LLM as venus.Client
    participant Tools as MCP Tools

    Note over UE: UE 连接后首发 world_kb → agent_registered → capability_registry
    UE->>WS: perception_update {location, physical_state, nearby_objects...}
    Note over WS: worker 循环：slot 判断 / time_to_stop 检测 / 队列 pop
    WS->>LLM: 战术层 function calling（tools + tool_choice=required）
    LLM-->>WS: tool_calls（1-4 个动作段）
    WS->>Tools: 解析 tool_calls → actionQueue
    WS->>UE: action_command {cmd, params}
    UE-->>WS: action_started (ACK ≤2s)
    Note over UE: 执行动作...
    UE->>WS: action_completed {result, duration_ms, progress}
    Note over WS: completion → 队列下一段 / 重分解 / 记忆写入 / action_history 落盘
```

## 常用命令

### 一键启动（云环境）

```bash
cd /data/workspace/dev
bash start-dev.sh              # dev 实例：端口 8770/9093，日志 logs-dev/
cd /data/workspace/stable
bash start-debug.sh            # stable 实例：端口 8760/9092，日志 logs/
```

`start-debug.sh`/`start-dev.sh` 执行顺序：**读取 .env → 拉起 MySQL → 编译+启动 MCP → 等健康检查通过**。UE5 端由外部启动连接 MCP 的 WS 端点（`:9092` stable / `:9093` dev）。

### Go 构建 / 测试

```bash
cd agenttown-mcp
go build ./...                                              # 编译检查（根 module，replace 自动解析本地 module）
go test ./...                                               # 全部测试
go test ./cmd/agenttown-mcp/ -v -count=1                    # 战术/战略层 + 决策
go test ./pkg/prompt/ -v -count=1                           # prompt 构建 + 物理分档
cd wsserver  && go test ./... -v -count=1                   # WS 缓冲/重放测试（独立 module）
cd contract  && go test ./... -v -count=1                   # 协议序列化测试（独立 module）
```

### 日志检查

**统一日志文件**：`logs/YYYY-MM-DD/debug-mcp.log`（stable 实例）或 `logs-dev/YYYY-MM-DD/debug-mcp.log`（dev 实例）。MCP 进程独占写入，JSON Lines 格式，含 UE + MCP + LLM 三层全链路；`YYYY-MM-DD` 为仿真启动日期

**推荐：用 `scripts/pretty_log.py` 可读化查看**（每条 JSON 渲染为多行，方向标记着色，长字段按行展开）：

```bash
# HTML 报告（推荐，自动打开浏览器，可折叠/搜索/过滤）
python scripts/pretty_log.py --html                       # 今天的日志
python scripts/pretty_log.py --html 2026-07-20            # 指定日期
python scripts/pretty_log.py --html -f PERCEPTION -n 50   # 最近 50 条 PERCEPTION
python scripts/pretty_log.py --html -o report.html        # 指定输出路径
python scripts/pretty_log.py --html --no-open             # 生成但不自动打开

# 终端渲染
python scripts/pretty_log.py                              # 查看今天的 debug-mcp.log
python scripts/pretty_log.py -f PERCEPTION -n 5           # 最近 5 条 MCP→LLM 感知原文
python scripts/pretty_log.py -f RESPONSE -n 5             # 最近 5 条 LLM 响应
python scripts/pretty_log.py --raw                        # 原始 JSON（grep/awk 友好）
```

`--html` 模式生成独立 HTML 文件（默认 `logs/YYYY-MM-DD/sim_report.html`），自动打开浏览器，支持：
- 点击条目展开/折叠详情
- 顶部按钮按方向过滤（UE→MCP / MCP→UE / PERCEPTION / RESPONSE / TOOL / HEARTBEAT）
- 搜索框（支持正则）
- 长字段（perception text / payload）自然换行，不受终端宽度限制
- 暗色主题，方向标记彩色高亮

**历史 Hermes 日志整合（DEPRECATED）**：`--hermes` 系列参数仅供解析 2026-08 之前的历史日志使用（Hermes Gateway 已移除）。新日志仅含 UE/MCP/LLM 三层，无 Hermes 容器日志。

方向过滤器（`-f`）简写：`UE→MCP` / `MCP→UE` / `PERCEPTION` / `RESPONSE` / `TOOL` / `HEARTBEAT`。heartbeat 默认隐藏。

**原始 grep（不渲染，单行 JSON）**：

```bash
grep '\[UE→MCP\]' logs/YYYY-MM-DD/debug-mcp.log           # UE5 → MCP（感知/状态/动作完成）
grep '\[MCP→UE\]' logs/YYYY-MM-DD/debug-mcp.log           # MCP → UE5（动作命令）
grep '\[MCP→LLM/PERCEPTION\]' logs/YYYY-MM-DD/debug-mcp.log    # MCP → LLM（感知文本）
grep '\[LLM→MCP/RESPONSE\]' logs/YYYY-MM-DD/debug-mcp.log      # LLM → MCP（LLM 响应 + narrative）
grep '\[MCP→LLM/STRATEGIC-PROMPT\]' logs/YYYY-MM-DD/debug-mcp.log   # 战略层 prompt（每日规划输入）
grep '\[LLM→MCP/STRATEGIC-RESPONSE\]' logs/YYYY-MM-DD/debug-mcp.log # 战略层 LLM 响应（每日计划 JSON）
grep '\[MCP→LLM/TACTICAL-PROMPT\]' logs/YYYY-MM-DD/debug-mcp.log    # 战术层 prompt（任务分解输入）
grep '\[LLM→MCP/TACTICAL-RESPONSE\]' logs/YYYY-MM-DD/debug-mcp.log  # 战术层 LLM 响应（actions JSON）
grep '队列已填充' logs/YYYY-MM-DD/debug-mcp.log           # 战术层任务队列形成（含完整 actions）
grep 'perception decision triggered' logs/YYYY-MM-DD/debug-mcp.log  # LLM 决策触发点
grep 'state_report' logs/YYYY-MM-DD/debug-mcp.log         # 状态报告摘要

# 按决策轮次关联：PERCEPTION / TOOL / RESPONSE 共享 agent_id + decision_epoch
# 例如查看 decision_epoch=1 的完整链路：
grep '"decision_epoch":1' logs/YYYY-MM-DD/debug-mcp.log   # 同一轮次的 PERCEPTION/TOOL/RESPONSE

# 战术规划链路：TACTICAL-PROMPT → TACTICAL-RESPONSE → 队列已填充 → 下发 action
# 例如查看某次战术分解的完整链路：
grep -E 'TACTICAL-PROMPT|TACTICAL-RESPONSE|队列已填充|\[战术层\] 下发 action' logs/YYYY-MM-DD/debug-mcp.log
```

**轮次关联**：`[MCP→LLM/PERCEPTION]`、`[LLM→MCP/TOOL]`、`[LLM→MCP/RESPONSE]` 三种日志都带结构化字段 `agent_id` 和 `decision_epoch`，匹配这两个字段即可关联同一次决策回合的输入 prompt、工具调用、LLM 响应。同一 `decision_epoch` 的 TOOL 可能出现在 RESPONSE 之前（工具调用在 LLM 流式输出时实时回调，而 RESPONSE 日志在 HTTP 响应完成后才写）。

**战术/战略层日志**：战略层和战术层使用独立的 LLM 调用（无状态，不复用决策链），因此不带 `decision_epoch`。链路按 `agent_id` + 时间顺序关联：`[MCP→LLM/STRATEGIC-PROMPT]` → `[LLM→MCP/STRATEGIC-RESPONSE]` → `[战略层] 每日计划生成成功`；`[MCP→LLM/TACTICAL-PROMPT]` → `[LLM→MCP/TACTICAL-RESPONSE]` → `[战术层] 队列已填充`（含完整 actions JSON）→ `[战术层] 下发 action`（逐个 pop）。

UE5 不写独立日志文件，MCP 的 `logs/`（stable）或 `logs-dev/`（dev）日志为唯一权威记录。

## 联调 Debug 工具

MCP 启动后暴露 HTTP debug 端点（dev 端口 `:8770`，stable `:8760`，以 `--http` flag 为准）：

| 端点 | 方法 | 用途 |
|------|------|------|
| `GET /debug/` | GET | 浏览器控制台 UI（单页 HTML，`//go:embed` 嵌入） |
| `POST /debug/action` | POST | 直接下发单个 action_command 到 UE（单步调试） |
| `POST /debug/schedule` | POST | 注入一条 schedule 到战术层，立即分解为 action 序列入队 |
| `POST /debug/event` | POST | 合成 world_event 注入事件系统（走与 UE 上报相同的分发入口；force 打断/入队全链路联调） |
| `GET /debug/kb` | GET | 返回 world_kb JSON（zones/objects） |
| `GET /debug/cap` | GET | 返回 capability_registry 当前状态（global + per-agent cmd） |
| `GET /debug/agents` | GET | 返回已注册 agent ID 列表（供前端 agent 下拉） |
| `GET /debug/logs` | GET | 返回最近 MCP 日志（环形缓冲 500 条，按 level 筛选） |
| `GET /debug/plan` | GET | 返回指定 agent 当日 dailyPlan 快照（items/current_slot/game_time） |
| `GET /debug/tactical` | GET | 返回所有 agent 战术层分解情况（当前时段 goal + 在途 action 及参数 + 待执行队列） |
| `GET /debug/ue-errors` | GET | 返回最近 UE 上报 error 消息（环形缓冲 50 条） |
| `GET /debug/llm-metrics` | GET | 返回 LLM 调用表现聚合指标（各层 E2E/TTFT/TPOT/ITL 分位数 + 错误分布 + 重试率 + JSON 正确率） |

### `/debug/schedule`（2026-07 新增）

给战术层注入一条单行 schedule，立即触发 LLM 分解 → 填充 actionQueue → signal worker 下发。**会强制中断当前在途 action**（`force` 默认 true）。

请求体：
```json
{
  "agent_id": "H-01",
  "schedule": "车间装配作业",
  "force": true
}
```

`schedule` 支持两种形态：
- 纯 goal：`"车间装配作业"`（时间段可选，内部用 `__debug__` 前缀避免和 dailyPlan 碰撞）
- 带时段：`"07:00-11:00: 车间装配作业"`（时段仅作 prompt 时长提示）

详见 `docs/DebugAction_Tool.md`。

### `/debug/event`（2026-09 新增，事件驱动 P4-13）

合成 `world_event` 注入事件系统，**走与 UE 上报完全相同的分发入口**（`Runtime.handleWorldEvent`）——force=true 走硬保证通道（同步 stop 在途动作 + 异步重规划），force=false 入队等安全点 drain。只注入入站消息，不直接向 UE 发任何东西，与 UE 自身推事件不冲突（注入事件的 event_id 用 `evt_debug_` 前缀与 UE 的 id 空间隔离）。

请求体（`event` 为完整 `WorldEventPayload`，缺省字段服务端自动补齐：`event_id`（evt_debug_*）、`occurred_at`（now）、`game_time`（该 NPC 当前权威游戏时间）、`data`（空对象））：
```json
{
  "agent_id": "H-01",
  "event": {
    "category": "player_interaction",
    "event_type": "player_attacked",
    "force": true,
    "severity": 10,
    "data": {"attacker": "player_1", "damage": 20, "damage_type": "physical"}
  }
}
```

`category`/`event_type` 六类别枚举见 `docs/AgentTown_WorldEvent_Protocol.md`。浏览器控制台"事件下发" tab 提供 14 个快速预设（被玩家攻击/脱离战斗/K-03 故障广播/能量跌破等）+ 自定义表单。

### 浏览器 UI

`/debug/` 单页控制台，多面板：
- **单 Action**：填 cmd + params，直接下发 UE
- **Schedule 注入**：填 schedule 文本，触发战术层分解
- **事件下发**：快速预设（被玩家攻击/战斗接管 detach/脱离战斗等 15 项，含 detach 复选框）+ 自定义 world_event，走事件系统分发入口
- **当日 schedule**：右侧面板展示 dailyPlan（时段 + goal + 当前高亮）
- **战术层分解情况**：全宽面板展示每个 NPC 当前时段 goal + 在途 action（含全部参数）+ 待执行队列（每 5s 刷新）
- **MCP 日志**：全宽面板，按 level 筛选的环形日志

UI 特性：curl 预览、历史记录（支持 replay）、响应字段高亮、强制中断复选框。

## 通信协议（v1.0）

### 7 字段信封

所有消息共用外层结构（`contract/protocol/envelope.go`），业务字段一律放入 `payload`：

```go
type Envelope struct {
    Version   string          `json:"version"`    // "1.0"
    MsgID     string          `json:"msg_id"`     // UUID
    Seq       int64           `json:"seq"`        // per-sender 单调递增
    Timestamp int64           `json:"timestamp"`  // Unix 毫秒
    Type      string          `json:"type"`
    AgentID   string          `json:"agent_id"`   // "system" 保留给系统消息
    Payload   json.RawMessage `json:"payload"`
}
```

### 关键约定

| 约定 | 内容 |
|------|------|
| 信封纯净 | `action_id` 等业务字段一律放入 payload，不得出现在信封顶层 |
| 时间单位 | 所有时间戳为**毫秒**；时长字段以 `_ms`/`_sec` 后缀标注 |
| 坐标单位 | UE5 厘米(cm)，position=[X,Y,Z]，rotation=[Pitch,Yaw,Roll] 度 |
| 保留 ID | `agent_id = "system"` 仅用于 heartbeat/error 等系统级消息 |
| 感知 vs 状态分工 | perception_update 负责空间+环境+物理状态全量三项；state_report 兜底物理状态 + 任务进度 |
| 物理 delta 阈值 | perception_update 携带全量 energy/fatigue/joint_wear 三项；state_report 兜底 |

### 消息类型总表

| type | 方向 | 用途 | 触发时机 |
|------|------|------|----------|
| `perception_update` | UE→Agent | 空间+环境感知（物理仅带变化项） | 每 N 游戏分钟（normal=60, behavior=15, quick-smoke=30）/ zone 变化 |
| `action_command` | Agent→UE | 下发动作指令 | 工具调用 / LLM 决策 |
| `action_started` | UE→Agent | 动作已接收的 ACK（≤2s） | UE 收到 action_command 后 |
| `action_completed` | UE→Agent | 动作完成回调 | 动作执行完毕 |
| `action_queued` | UE→Agent | 排队状态通知（queued/advanced/timeout） | auto_queue=true 的 action 目标 Smart Object 被占用且支持排队时 |
| `stop_action` | Agent→UE | 停止当前动作 | 反应层打断 |
| `event_notification` | Agent→Agent | 事件通知（内部路由） | Director 投放事件 |
| `state_report` | UE→Agent | 物理状态兜底上报 + 任务进度 | 兜底（perception_update 主数据源） |
| `agent_registered` | UE→Agent | 机器人上线 | RobotActor BeginPlay |
| `agent_unregistered` | UE→Agent | 机器人下线 | RobotActor EndPlay |
| `heartbeat` | 双向 | 心跳保活 | 每 5 秒 |
| `error` | 双向 | 错误上报 | 异常情况 |
| `capability_registry` | UE→Agent | NPC 能力声明（哪些 cmd 可执行） | UE 连接后 / 能力变更时 |
| `world_kb` | UE→Agent | 世界知识库下发（generated + authored） | UE 连接后（首个 `agent_registered` 之前） |
| `world_event` | UE→Agent | 世界事件上传（事件驱动反应层的判定输入；force=硬保证打断，非 force=入队/路由） | 事件发生那一刻（边沿触发），见 `docs/AgentTown_WorldEvent_Protocol.md` |

### 动作生命周期

```mermaid
sequenceDiagram
    participant Agent as MCP (Agent)
    participant UE as UE5
    Note over Agent: 工具调用触发
    Agent->>UE: action_command {action_id, cmd, params}
    UE-->>Agent: action_started {action_id, accepted, estimated_duration_sec} (≤2s)
    Note over Agent: 工具收到 ACK 后立即返回<br/>不等 completed
    Note over UE: 执行动作...
    UE->>Agent: action_completed {action_id, result, duration_ms, progress}
    Note over Agent: completed 存入 pendingCompletion 队列<br/>下次 perception 时折入叙事
```

**12 种 cmd**（7 原子 + 5 复合，2026-08-11 对齐真实 UE5 `capability_registry`）：
- 原子：`GenericAct`/`MoveTo`/`Wait`/`TurnTo`/`Speak`/`InteractSmartObject`/`Emote`
- 复合：`WorkShift`/`ChargeAtStation`/`SelfMaintenance`/`RestAtResidence`/`SurfInternet`

`Stop` 不再是 cmd，改为 `stop_action` 消息类型（Agent→UE）。`ExecuteComposite`/`PlayAnimation` 已移除，复合动作直接用各自 cmd 下发。

**error_code 取值**：`ACTION_FAILED` / `STOP_ID_MISMATCH` / `INVALID_MESSAGE` / `UNKNOWN_AGENT` / `INTERNAL_ERROR`

### 超时机制

| 操作 | 超时 | 超时后行为 |
|------|------|------------|
| action_started 等待 (ACK) | 2 秒 | 认为指令丢失，重发或重新决策 |
| action_completed 等待 | `estimated_duration_sec × 1.5`（默认 60s） | 发 stop_action + 重新决策 |
| LLM 调用 | 120 秒 | 返回错误，跳过该轮 |
| 心跳响应 | 15 秒 | 认为断线 |
| 重连尝试 | 3 秒间隔，指数退避到 30 秒 | 持续重试 |

### 排队机制（约定21，2026-08-11 新增）

当 Agent 发起 `auto_queue=true` 的 action 但目标 Smart Object 已被占用且支持排队时，UE 将 Agent 加入排队队列，通过 `action_queued` 消息通知状态变化。MCP 侧处理：

- **auto_queue 发送端**：`shouldAutoQueue(cmd)` 对 6 个智能体对象 cmd（`WorkShift`/`ChargeAtStation`/`SelfMaintenance`/`RestAtResidence`/`SurfInternet`/`InteractSmartObject`）返回 true，其他 cmd 默认 false。`guardedExecutor.SendAction` 和 `popAndSendQueueAction` 自动按 cmd 决定 `ActionCommandPayload.AutoQueue` 字段。`/debug/action` 路径传 false（手动调试不排队）
- **action_queued 接收端**：WS handler 解析 `ActionQueuedPayload` → `agentstate.RecordQueueStatus` 按 status 处理：`queued` 写入排队字段（actionID/group/position/estimatedWait/queuedAt）、`advanced`/`timeout` 清空。不触发反应层 Ollama 决策（queued 是信息性状态，下次 periodic/action_done 等触发时 prompt 的【排队状态】段会带上）
- **排队状态清理点**：`RecordActionCompletion`（action 完成）、`ClearForSlotSwitch`/`ClearForReplan`（slot 切换/replan）、`Stop`（agent 下线）都调 `clearQueueStatusLocked`，确保排队状态不残留
- **timeout 路径**：UE 在排队超时后会补一条 `action_completed {result: failed, reason: queue_timeout}`，走现有 `TriggerActionDone` 路径触发反应层重新决策
- **取消排队**：Agent 发 `stop_action` 即可。UE 收到后中断行为树，`QueueForSmartObject` 的 abort 处理会把自己移出队列，并回 `action_completed {result: interrupted}`
- **反应层 prompt 注入**：`ReactiveInput.QueuedFor` 由 `reactive_runner.buildInput` 从 snapshot 构造为 `正在排队等待 <group>（位置 N，预计等待 M 秒）`，注入 prompt 新增的【排队状态】段。不在排队时整段省略

## 数值系统

### 数值归属原则

**谁产生这个数值，谁就是主人，谁负责存储和变更。**

| 数值类别 | 主人 | 变更触发 | UE 需要 | 同步方式 |
|----------|------|----------|---------|----------|
| Agent 内部状态（mood/social_need/emotion） | Agent | LLM 反思 / 交互判断 | ❌ | 不同步 |
| 物理状态（energy/fatigue/joint_wear） | UE | 行为消耗 / 充电恢复 | ✅ 主人 | perception_update 全量上报（state_report 兜底） |
| 关系数值（affinity 好感度 0-100） | Agent | compact 时更新 | ❌ | 不同步 |
| 空间状态（position/rotation/zone） | UE | 每帧 / Overlap 触发 | ✅ 主人 | perception_update 上报 |
| 任务状态（plan/queue/stack） | Agent | 分层思考产出 | ❌ | 不同步 |

### 物理状态三项

energy / fatigue / joint_wear，通过 `perception_update` 全量上传（`physical_state_delta` map 携带三项）。`state_report` 保留 `PhysicalState` 作为兜底（perception 未带物理状态时补写）。delta 阈值不再适用（perception_update 始终全量）。

## MCP 工具

所有工具在 `agenttown-mcp/adapters/agenttown/tools/`。14 个工具均以 `agent_id` 为第一参数、`decision_epoch` 为第二个必填参数。

**工具列表由 `capability_registry` 动态驱动**：UE 连接 MCP 后发送 `capability_registry` 声明可执行 cmd，MCP 据此调 `tools.ReconcileTools` 增删工具（`AddTool`/`RemoveTools`）。启动时 seed 内置 12 cmd 默认值（`BuiltinCmdCapabilities`），保证 UE 不发 `capability_registry` 也能跑。per-agent 差异化在 `guardedExecutor.SendAction` 这一咽喉点拦截——查 `CapabilityRegistry.HasCmd(agentID, cmd)`，不通过则拒绝下发。战术层 prompt 中的可用工具列表也按 registry 对 agentID 的有效能力集动态生成（`tacticalToolMeta` 是工具元数据单一来源）。

### 复合行为工具（5 个，各自独立 cmd）

| 工具 | 参数 | cmd | 说明 |
|------|------|-----|------|
| `work_shift` | agent_id, semantic_group, interaction | `WorkShift` | 工作班次（装配/分拣/作业） |
| `charge_at_station` | agent_id, semantic_group, interaction | `ChargeAtStation` | 在充电站充电 |
| `self_maintenance` | agent_id, semantic_group, interaction | `SelfMaintenance` | 自我维护保养 |
| `rest_at_residence` | agent_id, semantic_group, interaction | `RestAtResidence` | 在住所休息 |
| `surf_internet` | agent_id, semantic_group, interaction | `SurfInternet` | 上网浏览 |

5 个复合工具共享 `semantic_group` + `interaction` 参数 schema（按真实 UE5 `capability_registry` 声明的参数名），`semantic_group` 引用 `world_kb` 中对应 category 的物体 id（语义组名，如 `workbench`/`charger`/`sleep_pod`/`repair_table`/`computer`），UE5 从该组自动选一个空闲实例。`auto_queue` 作为 `params` 内字段传 `"true"`（约定21）。

### 原子行为工具（7 个 + 2 特殊）

| 工具 | 参数 | cmd |
|------|------|-----|
| `generic_act` | agent_id, thought, behavior | `GenericAct` |
| `move_to` | agent_id, target_type, target_id/target_position | `MoveTo` |
| `turn_to` | agent_id, target_type, target_id/target_position | `TurnTo` |
| `speak` | agent_id, content | `Speak` |
| `emote` | agent_id, emotion | `Emote` |
| `interact` | agent_id, semantic_group, interaction | `InteractSmartObject` |
| `wait` | agent_id, duration_sec | `Wait` |
| `scan_area` | agent_id | （请求即时 perception，无 cmd） |
| `stop` | agent_id | （发 `stop_action` 消息，非 cmd） |

`move_to`/`turn_to` 的 `target_type` 取值 `agent`/`smart_object`/`zone`/`position`；`target_id` 对应 actor id，`target_position` 对应 `[x,y,z]` 坐标。语义目标（如 `move_to(target_type="smart_object", target_id="workbench")`）由 UE 自行解析坐标，MCP 不做 KB 坐标解析。`generic_act` 是兜底通用动作，`behavior` 取值 `idle`/`look_around`/`wave_hand`/`groom`/`think`，替代旧 `PlayMontage`。

### 新增工具硬约束

- 命名 `<verb>` 或 `<verb>_<noun>`，全小写下划线
- `agent_id` 为第一参数，`decision_epoch` 为第二个必填参数
- Input/Output struct 带 `json` + `jsonschema` tag
- Output 首字段 `OK bool`
- Handler 第一个返回值传 `nil`，让 SDK 用 Output 填充 content
- 在 `RegisterAll` 注册

## 关键机制

### 启动顺序（硬约束）

MCP 是唯一的 LLM 调用入口，启动后即可接收感知事件、调用 Venus、下发工具调用。

正确顺序（`start-debug.sh`/`start-dev.sh` 已保证）：
1. 停掉所有旧进程
2. 编译 MCP 二进制
3. 启动 MCP → 等 `:8760` + `:9092` 就绪
4. 启动 UE5（AgentTown 地图）→ 连接 MCP WS 端点
5. 仿真日志统一写入 `logs/YYYY-MM-DD/debug-mcp.log`（stable）或 `logs-dev/YYYY-MM-DD/debug-mcp.log`（dev），MCP 独占，无需合并

**UE 连接消息序列**（硬约束）：UE 连接 MCP 后按以下顺序首发系统消息：
1. `world_kb`（`agent_id="system"`）— 推送完整世界 KB（generated + authored JSON），MCP 合并+落盘+swap 内存 KB。**必须在首个 `agent_registered` 之前**，确保 worker 启动时捕获新 KB
2. `agent_registered` — 触发 worker 启动 + 战略层生成当日计划
3. `capability_registry` — 声明 NPC 能力，MCP 动态增删工具
4. `resync` → `state_report` → `perception_update` …

`world_kb` 仅在启动窗口内（首个 `agent_registered` 之前）接受；之后到达的 `world_kb` 被拒绝并告警（worker goroutine 已持 kb 指针，热替换会竞态）。合并失败保留旧 KB + 不写盘。

### 手动模式（`--auto-plan=false`）

默认 `--auto-plan=true` 保持自动决策行为。设为 `false` 时 MCP 进入手动模式：

- **战略层**：worker 启动时跳过 `generateDailyPlan`，`dailyPlan` 保持空
- **战术层**：worker 循环跳过 `tacticalRefill` 和 `sendIdleWait`，不主动填队列、不主动发 wait
- **反应层**：WS handler 4 处 `reactiveRunnerRef.trigger` 调用全部跳过，Ollama 不被调用
- **保留**：`popAndSendQueueAction`（`/debug/schedule` 注入的 action 进队列后由 `ac.signal()` 唤醒 worker 走此路径下发）、`/debug/action`（直接 `ws.SendAction` 下发，不经 worker）

手动模式适合联调时隔离 UE 端、单独验证 MCP 工具链/协议层/特定 schedule 分解效果。关闭后断连不再触发战略层重新规划（因为根本不调），间接缓解断连风暴导致的计划漂移。

### 持久化存储（`--mysql-dsn`，Stage 3）

默认 `--mysql-dsn=""` 为内存模式（`NoopStore`，无持久化，测试/quick-smoke 默认）。设为有效 MySQL DSN 时启用持久化层：

- **写入策略**：write-through 同步写。4 个调度字段（`dailyPlan`/`currentDay`/`currentPlanIndex`/`currentSlot`）任一变更即 upsert 到 `agent_schedule_state` 表（单行 per agent）。写入频率低（计划生成 1 次/天 + slot 切换 ~7 次/天），同步写无性能压力
- **加载时机**：`agent_registered` → `SetIdentity(agentID, store)` → `LoadPersistent` 从 DB 恢复 4 字段。冷启动（无行）保持默认值，worker 生成新计划；热重启（有行）跳过 `generateDailyPlan`，计划跨进程存活
- **降级**：DB 写失败仅 log warn，不回滚内存状态（内存已正确，DB 下次写追上）；`LoadPersistent` 非 `ErrNotFound` 错误降级为 cold start
- **迁移**：`//go:embed migrations/*.sql` 原生 SQL，启动时自动跑（`schema_migrations` 版本表跟踪）。无需外部迁移工具
- **持久化表全集**：`agent_schedule_state`（Stage 3 调度状态）+ `action_history`（动作历史）+ `episodic_memories` / `episodic_commitments` / `semantic_people`（Stage 4/5 长期记忆三表，2026-10-22 重建）
- **优雅关停**：write-through 已同步落盘，SIGTERM 仅 `defer store.Close()`，无需 flush

DSN 必须含 `parseTime=true` 以正确扫描 `DATETIME` 列。示例：`user:pass@tcp(127.0.0.1:3306)/agenttown?parseTime=true&charset=utf8mb4`。

### 长期经历记忆（Stage 4）

Stage 4 在 Stage 3 的存储层之上接入 NPC 长期记忆，2026-10-22 按
`docs/AgentTown_LongTermMemory_SQLite_Design.md` 重建为三张新表（替代旧
`agent_memories` / `agent_relationships`）：`episodic_memories`（过去经历，
game_time 锚定）+ `episodic_commitments`（未来待办）+ `semantic_people`
（对人认知）。仅 `--mysql-dsn` 非空时启用，内存模式（`NoopStore`）全程 no-op。

- **action_history 记录**：`recordActionCompletion` 钩子在 `WasInFlight=true` 时单条 INSERT（`SaveActionRecord`）。`CompletionResult` 在 in-flight 清空前捕获 `Cmd`/`Params`/`Start` 三字段，完整还原动作生命周期。`/debug/action` 路径不经 `recordActionStarted`，`WasInFlight=false`，自然不写历史。best-effort：5s 超时 + `slog.Warn`，不阻塞决策管线
- **昨日总结（narrative）**：`detectDayRollover` 命中后先调 `generateDailyMemories`（`memory.go`），从昨日 `action_history(500)` 倒序转正序后格式化为编号列表，1 次 LLM 调用产出 narrative（昨日总结）注入战略层 prompt，并落盘为一条 `daily_summary` episode。失败/冷启动返回空串，`generateDailyPlan` 内部回退到 `yesterdaySummaryForFirstDay` 常量
- **记忆写入仅在 compact（2026-10-22 重建）**：`maybeCompactConversation` 压缩上下文后调 `summarizeConversation`（结构化输出 `SendWithSchema`，一次产出 `summary` + `episodes` + `new_commitments` + `commitment_changes` + `people_updates`），再 `persistExtractedMemory` 落三表。**不 compact 时经历仍在上下文中，无需落盘**；摘要滚动合并，读路径取最近 N 条收敛到最新。**日切强制 compact**：跨日 `detectDayRollover` 清空历史前，`compactAtDayRollover` 把当天整段历史也抽一次落盘，兜住低活动日（中途历史不足阈值、未触发 compact）的经历丢失窗口
- **规则直写（0 LLM，事件时点）**：被攻击（combat-detach 入口）→ combat episode + 攻击者好感度 -50。攻击是边沿触发的关键事实、等不到 compact（combat-detach 直接清上下文不 compact），故在事件时点直写。动作失败不再单独立 memory episode（`action_history` 已完整落盘，且失败多为 UE 侧 bug，记忆价值有限）
- **战术层记忆注入**：读记忆走内部工具 `memoryReader`（`memory_reader.go`）——`recentEpisodes`（【过往经验】top-3 经历）、`commitmentsNearby`（【当前需兑现的约定】临近 1h/眼前人）、`people`（【人际关系】）。**不注册进 MCP 工具列表、不进 function calling，仅供程序调用、读路径零 LLM**
- **待办闭环**：`episodic_commitments` 读时 SQL 动态过滤过期（`due_end < now` 的 pending 视为 expired、不回写）；compact 的 `commitment_changes` 核销 done / 改期 / 取消；对话结束抽取（`extractAndReplanCommitments`）落盘新待办，`due_start` 落在 **1~12 游戏小时窗口**（`commitmentNeedsReplan`）的触发战略 replan 预留时段——≤1h 的不触发：守约 hint + 战术层【当前需兑现的约定】（`due_start ≤ now+1h` 注入）已驱动立即履行，再排计划时段是双驱动，槽位与在途履约错位时同一约定执行两遍（2026-11-03 邀约跑步双跑修复）。配套：`normalizeDailyPlan` 丢弃退化槽位（start==end）——LLM 收尾跨午夜末段残留的零长槽位（如 "07:09-07:09"）会被按起始时间排序排到全天最前，把深夜睡眠拉成晨间首段、后续时段整体后移出约定窗口（同一次实测的引爆点）
- **JSON 解析容错**：`parseMemoryGenerationResult` 容忍 markdown 围栏 ```json ... ``` 和尾随散文，定位首个 `{` 到末个 `}` 后 `json.Unmarshal`

### 对人认知（Stage 5）

Stage 5 是 NPC 间对人认知，2026-10-22 重建为 `semantic_people` 单表：**单一数值维度 affinity（0-100，50 中性）+ 一句话 impression**（替代旧 `agent_relationships`，familiarity / interaction_count 已删）。

- **更新仅在 compact**：affinity/impression 由 compact LLM 的 `people_updates` 一次性产出（affinity_delta + impression），`UpsertPerson` 落库（新行 = 50 + delta、重复键累加，夹紧 [0,100]）。**删除旧实时关系写** `maybeUpdateRelationship`（Ollama 判断）+ `evaluateConversationAffection`（对话结束 LLM delta）——Ollama 客户端随之彻底移除
- **规则直写**：被攻击 → 攻击者 `UpsertPerson(delta=-50)`
- **KB 种子导入**：`agent_registered` 时调 `seedRelationshipsFromKB`，把 KB 关系用 `SeedPerson`（INSERT IGNORE，KB affection 0-10 ×10 → affinity 0-100）导入，不覆盖既有累积。重连提前 return，不重复导入
- **名单+好感度合并渲染（三层共享）**：`formatRelationshipsForPrompt(people, kb, profiles, agentID)` 以 KB peers 为骨架——每个其他 NPC 一行 `- 老王（H-02，物流分拣兼加工员）：好感 65/100，印象：…`，存储行叠加其上，**无行默认好感 50**（冷启动/内存模式/未互动不再整段消失）；显示名/职业取 profile > KB（profile 职业的动作映射括注截掉）。同一份内容三层注入：战略层【其他NPC】段（`BuildStrategicUserContext` otherNPCs 参数）、战术层【人际关系】段（`TacticalInput.Relationships`，非空时跳过 OtherAgentsLine 花名册 fallback 防重复）、路由层 relationships 属性（`buildInput`）。单 agent 场景（KB ≤1）整段省略；`/debug/schedule` 路径保持空串
- **战术层注入条件**：仅 `len(kb.Agents) > 1` 注入（名单+好感度合并段）；单 agent 场景不污染 prompt

### world_kb 自动适配

UE 推送新 `world_kb` 后，MCP 重启即自动适配全链路，无需改任何代码：

- **两层共享 system prompt**：战略/战术层的 system prompt 严格统一为 `BuildSharedSystemPrompt`（【世界背景】`WorldOverview` + 【人物背景】`AgentRole` + 【世界详细信息】`worldDetailCore` + 【生产工作流】），单次仿真内静态；所有层专属内容（规则、花名册、JSON 格式）都在各层 user prompt。角色段 `AgentRole(kb, profiles, agentID)` 两层 per-field 回退；世界详情 `worldDetailCore` 从 KB 派生各区域描述 + 设施映射表 + 属性影响/使用门槛，LLM 不会编造 KB 外概念
- **工具列表动态派生**：`capability_registry` 驱动 `ReconcileTools` 增删工具；战术层 tools 经 `tacticalToolsFromRegistry` 从 registry 对 agent 的有效能力集生成 function calling 的 `tools` 数组（`capabilityParamsSchema` 转 JSON Schema + 追加 time_to_stop）。新 cmd 由 `registerGenericActionTool` 自动注册
- **反应层 prompt 注入角色**：`ReactiveInput.AgentRole` 由 `reactiveRunner.buildInput` 从 kb 取，注入反应层 prompt 开头。反应决策（continue/observe/replan）受 NPC 性格影响
- **反应层决策简化**：反应层仅支持 `continue`/`observe`/`replan` 三种决策（已移除 `interrupt`/`act`）。物理告警时代码层 `upgradeIfPhysicalAlert` 强制升级 continue/observe → replan
- **工具 jsonschema 描述去硬编码 id**：参数描述引用 `world_kb` 语义组名，LLM 从 prompt 注入的【世界详细信息】段获取合法 id
- **兜底每日计划从 KB 派生**：`DefaultDailyPlan(kb)` 用首个 zone 显示名 + 首个 object 显示名组装工作时段；`kb == nil` 时降级为中性表述

**仅启动时适配**：不支持运行时热替换 KB。worker 按值捕获 kb，swap 仅在 worker 启动前发生，当前架构安全。换 KB 流程：UE 推送新 `world_kb` → MCP 重启 → worker 启动时拿新 kb。

### NPC profile.md 人设档案

NPC 性格、背景、说话风格等 persona 字段从 `assets/profiles/<agentID>.md` 加载，作为三层决策 prompt 的 override 层：

- **三层 per-field 回退**：`prompt.AgentRole(kb, profiles, agentID)` 逐字段按 **profile 非空 > KB 非空 > hardcoded fallback 非空** 取值。任一层字段空则降级到下一层，避免空值覆盖有效值。
- **文件格式**：纯 markdown 固定标题分段，5 个字段：`## 名字` / `## 职业` / `## 背景` / `## 性格特质` / `## 说话风格`。性格特质按 `、` 或换行分隔为 trait 列表。
- **加载机制**：`pkg/profile.LoadDir(dir)` 启动时扫描 `*.md`，文件名（去 `.md`）= agentID → `*Profile` map。空目录或 `--profiles-dir=""` → `profiles=nil`，三层决策仅走 KB → fallback，行为与改动前完全一致。
- **进程级只读**：启动加载后不热重载（与 KB 启动时适配一致）。UE 推 `world_kb` 不触发 profile 重载。作为函数参数传递（与 kb 同级），不入 `agentContext`。
- **三层决策注入**：战略层（`BuildStrategic`）、战术层（`BuildTactical` via `TacticalInput.Profiles`）、反应层（`reactiveRunner.profiles` → `AgentRole`）、昨日总结层（`generateDailyMemories`）均接收 profiles 参数并透传到 `AgentRole`。`/debug/schedule` 路径传 nil（调试上下文不需 persona 注入）。
- **字段优先级示例**：H-01 的 `description` 和 `speech_style` 在 KB 中为空，profile.md 填充后三层决策可读到完整角色段；H-02/H-03 的 `display_name`/`profession`/`traits` 在 KB 已有值，profile.md 以更自然的中文表述覆盖（如 `supervisor、worker、maintainer` → `车间主管、装配工人、维护技师`）。

### 每周日程配置

NPC 行为随 7 天周期变化：前 5 天工作日、后 2 天休息日；第 2 天运动日（今天可以早点结束工作去运动）、第 3 天冥想日（21:00-22:00 集体冥想）。仅注入战略层 prompt，影响当日计划安排。

- **配置文件**：`assets/weekly_schedule.yaml`，7 条 `day_of_week` 1-7 各一条，每条声明 `type`（work/rest）、可选 `after_work` 语义标签（exercise/meditation，不注入 prompt）、可选 `note`（注入 prompt 的提示语）
- **加载机制**：`pkg/weeklyschedule.Load(path)` 启动时加载，`--weekly-schedule=""` → 禁用（nil，不注入【今日日程】段，行为不变），非空路径格式错 fail-fast。进程级只读，与 kb/profiles 同级参数化传递，不入 `agentContext`
- **星期映射**：UE `Environment.DayCount` 从 0 开始，`dayOfWeek = (dayCount % 7) + 1`（Day 0 → 周一，Day 6 → 周日）。`WeeklyLine(dayCount, sched)` 返回预格式化字符串（如"今天是周二（工作日）。下班后适合上网休闲放松。"），dayCount<0 或 nil 返回空
- **战略层注入**：`BuildStrategic` 接收 `dayContext string` 参数，【今日日程】段插在【你的角色】后、【物理状态】前。`pkg/prompt` 不依赖 `pkg/weeklyschedule`（预格式化字符串解耦，镜像 `PhysicalLine` 模式）
- **调用点**：worker 启动计划用 `ac.as.LatestDayCount()`（首条 perception 未到为 -1，无星期上下文）；跨日计划用 `detectDayRollover` 返回的 `newDay`
- **仅战略层**：战术层不动 — 它分解战略层为晚间时段写的 goal（如"傍晚去上网休闲"→`surf_internet` 复合动作）

### UE Busy 状态

长耗时复合动作（`WorkShift`/`ChargeAtStation`/`SelfMaintenance`/`RestAtResidence`/`SurfInternet`）由 UE5 行为树执行，MCP 侧通过 time_to_stop 预算到点打断（duration schema 必填）；slot 过期不硬切——标记 pending 等动作结束后安全点切换（P3-8）。感知循环自然推进时间，NPC 留在原位直到时间到达。

- 忙碌期间拒绝破坏性动作：`MoveTo`/`TurnTo`/`InteractSmartObject`/5 个复合 cmd/`Wait`
- 短动作立即执行 + 发 `action_completed`
- 完成的 busy 动作自动清除，下一次感知通知 LLM

### 断线重连与 Seq 重放补偿

```mermaid
sequenceDiagram
    participant UE as UE5
    participant MCP as MCP (WS Server)
    Note over UE,MCP: 连接断开
    Note over UE: 心跳超时 15s → 标记断线
    loop 3s→30s 指数退避
        UE->>MCP: 尝试重连
        alt 成功
            UE->>MCP: agent_registered (重注册)
            UE->>MCP: resync {last_received_seq}
            MCP->>UE: resync {last_received_seq}
            Note over UE,MCP: 双方重放 seq 之后的离散消息<br/>连续状态以最新快照为准
            MCP->>UE: event_lost {from_seq, to_seq} (如缓冲滚动丢失)
        else 失败
            Note over UE: 等待退避时间后重试
        end
    end
```

- 双方各维护发送缓冲队列（最近 200 条 / 60 秒，仅离散消息）
- 重连后交换 `resync{last_received_seq}`，重放 seq 之后的离散消息（action_completed/event_notification）
- 连续状态（position/physical_state）不重放，以重连后最新快照为准
- 缓冲滚动丢失则发 `event_lost` 告警
- MCP 侧：首次 `agent_registered` 触发 worker 启动 + 战略层生成当日计划，重连再注册保留状态

### 事件驱动决策与 epoch

- 所有 perception 都更新最新世界缓存，但只有首次感知、动作完成、任务生命周期、关键环境变化、场景事件或物理警戒带变化才调用 LLM
- 纯时间变化、相同 scan_area、busy progress 普通变化不触发决策
- LLM 调用在途时合并触发原因，并只保留最新世界快照
- 每次实际决策生成单调递增 `decision_epoch`；全部 14 个工具必须携带当前 `[decision_context]` 中的 epoch
- guarded executor 在发送 UE 前校验 Agent 已注册、在线、decision_epoch 当前有效且 WebSocket 已连接
- `agent_unregistered` 立即失效当前决策；迟到工具调用被拒绝

### LLM 调用

MCP 直连 Venus（OpenAI Chat Completions 协议）。战略层用 Structured Outputs（json_schema strict），战术层用 function calling（tools + tool_choice=required + 多轮历史）。三层各自构造完整 prompt：
- **战略层**：每日 07:00 一次调用，`SendWithSchema` + `dailyPlanSchema`，输入 = `BuildSharedSystemPrompt`（共享 system prompt）+ user（物理状态/其他NPC/昨日总结/规则），输出 = 当日 plan JSON
- **战术层**：每个时段开始时调用，统一 agentic loop（`SendLoop`，tool_choice=required），输入 = `BuildSharedSystemPrompt`（共享 system prompt）+ user。user prompt 首条全量（全天日程/当前时段目标/实时状态/完整规则），同计划后续轮次精简（省略日程与完整规则，速览+引用行，见"TacticalInput.Compact"），工具经 `tools` 字段下发，输出 = tool_calls（1-4 动作段）
- **反应层**：触发时调本地 Ollama（5-8s 超时），输入 = `buildReactivePrompt(in)`（含角色/状态/在途动作/触发原因），输出 = `{"reaction": "...", "reason": "..."}`

### 感知格式化

UE5 推送 `perception_update` → MCP 的 `pkg/agentstate` 语义化（zone 判断、物理状态、附近物体、可见 NPC）→ 作为战术层 prompt 的【NPC与环境实时状态】段注入。`adapters/agenttown/perception/format.go` 已移除（旧的"第一人称叙事"流不再使用）。

### stdio vs HTTP 模式

`agenttown-mcp/cmd/agenttown-mcp/main.go` 运行模式由 `--http` flag 切换：
- **HTTP 模式**（`--http :8760`）：Streamable HTTP 在 `/mcp`，健康检查 `/healthz`，状态 `/status`。
- stdio 模式（默认）：本地 MCP 客户端用。

**stdio 模式禁止向 stdout 写日志**，否则污染 MCP 协议流。日志走 `internal/log` 打 stderr。

### 网络拓扑

MCP 监听 `0.0.0.0:8760`（HTTP）+ `0.0.0.0:9092`（WS，stable 脚本默认）。UE5 通过 `ws://<host>:9092/ws` 连接（dev 实例为 `:9093`）。Venus 远程服务通过 HTTPS 调用。

## 代码规范

- **Go 1.25+**（`go.mod` 声明 `go 1.25.0`）
- 错误包装：`fmt.Errorf("...: %w", err)`
- 包注释和导出符号注释写英文
- 新增 Go package 必须有 `*_test.go`
- 测试命名 `Test<Func>_<Scenario>`；禁止启真实子进程，用 `InMemoryTransport` / mock
- Python 代码用 `asyncio` + `websockets`，不用同步 HTTP 调用 LLM（MCP 接管所有 LLM 调用）
- 日志走 `logging` 模块，不直接 `print`（调试除外）
- WebSocket 库：Go 用 `github.com/coder/websocket`，Python 用 `websockets`

## 环境配置

### 环境变量（.env）

```bash
cp .env.example .env
# 编辑 .env，填入 VENUS_API_KEY
```

关键环境变量（`.env`，详见 `.env.example`）：
- `VENUS_API_KEY` — Venus 后端 API key（**必填**，MCP 直连 Venus 凭据）
- `AGENTTOWN_MCP_AUTO_PLAN` — 自动规划总开关（默认 `true`）
- `HTTP_PORT` / `WS_PORT` — MCP 监听端口（start-debug.sh 读取，默认 `8760`/`9092`）
- `MYSQL_DSN` / `MYSQL_DB` / `SKIP_MYSQL` — 持久化存储（默认内存模式）

### MCP 启动 flag 速查

| flag | 默认值 | 说明 |
|------|--------|------|
| `--http` | `:8760` | MCP HTTP 监听地址（空=stdio 模式） |
| `--ws` | `:9090` | WebSocket 监听（UE5 连接；start-debug.sh 默认传 `:9092` stable / `:9093` dev） |
| `--venus-url` | `http://v2.open.venus.oa.com/llmproxy` | Venus 后端 URL |
| `--venus-api-key` | `""` | Venus API key（**必填**，否则 401）。env 回退 `VENUS_API_KEY` |
| `--venus-model` | `deepseek-v4.1-flash` | Venus 模型 ID（战术层） |
| `--venus-strategic-model` | `deepseek-v4-pro` | 战略层模型 ID（空值回退到 `--venus-model`） |
| `--venus-timeout` | `60s` | Venus 调用超时 |
| `--jev-model` | `jev-1.13.0` | 事件路由判决模型（Venus `/v1/systemone` 判决 API，与 chat completions 同网关同凭据；空串=禁用路由裁决，非 force 事件只入队等安全点 drain） |
| `--jev-timeout` | `5s` | 事件路由判决调用超时（实测单次 ~0.5s） |
| `--tactical-timeout` | `60s` | 战术层 LLM 调用超时（time_scale=90 下 ≈90 游戏分钟，slot 切换拖尾主因之一） |
| `--tactical-stream` | `false` | 战术层流式输出（`true` 时战术层走 `SendLoopStreaming`，可采集 TTFT/TPOT/ITL；非流式只能测 E2E） |
| `--llm-metrics-doc` | `docs/llm_metrics.md` | LLM 指标 markdown 报告落盘路径（每次 LLM 调用后 best-effort 覆盖写入；空串关闭） |
| `--auto-plan` | `true` | 自动规划总开关（false=手动模式，跳过战略/战术/反应层自动决策，仅响应 /debug/schedule 注入和 /debug/action 手动下发） |
| `--mysql-dsn` | `""` | MySQL DSN（空=内存模式无持久化；非空启用 Stage 3 存储层，DSN 需含 `parseTime=true`）。env 回退 `MYSQL_DSN` |
| `--world-kb` | `assets/world_kb.yaml` | 世界 KB 路径（fail-fast 启动加载；UE 推送 world_kb 时也写入此路径） |
| `--world-kb-manifest` | `assets/world_kb.manifest.json` | manifest.json 输出路径（UE 推送 world_kb 时写入；空串=跳过 manifest） |
| `--profiles-dir` | `assets/profiles` | NPC profile.md 目录（文件名 = `<agentID>.md`；空串=禁用 profile override，仅走 KB → fallback） |
| `--weekly-schedule` | `assets/weekly_schedule.yaml` | 每周日程配置 YAML（7 天周期：工作日/休息日/运动日/冥想日；空串=禁用，不注入【今日日程】段） |
| `--log-level` | `info` | `debug`/`info`/`warn`/`error` |

### 云开发环境（AnyDev / 远程 Linux）

云环境用 `start-debug.sh`（stable）/ `start-dev.sh`（dev）一键启动，或分组件启动：

```bash
# 1. 编译 MCP
cd agenttown-mcp && go build -o ../mcp ./cmd/agenttown-mcp && cd ..

# 2. 拷贝 .env（至少需要 VENUS_API_KEY）
cp .env.example .env  # 填入 VENUS_API_KEY

# 3. 启动 MCP（直连 Venus）
bash start-debug.sh    # stable 实例（或 bash start-dev.sh 起 dev 实例）

# 4. UE5 端启动 AgentTown 地图，连接 MCP 的 WS 端点（:9092 stable / :9093 dev）
```

**云环境限制**：
- 内网工蜂 `git.woa.com` 一般可达，可用 HTTPS clone
- Venus `v2.open.venus.oa.com` 需确认云端网络可达

### stable / dev 目录分离（云开发环境）

云开发环境（AnyDev / 远程 Linux）下，项目 clone 到 `/data/workspace/` 下两个独立目录，用端口隔离同时运行：

| 目录 | 分支 | 用途 | MCP HTTP | MCP WS | debug 控制台 | 日志目录 |
|------|------|------|----------|--------|--------------|----------|
| `/data/workspace/stable` | `master` | 稳定运行、验证 | `:8760` | `:9092` | `http://localhost:8760/debug/` | `logs/` |
| `/data/workspace/dev` | `dev-working` | 日常开发、调试 | `:8770` | `:9093` | `http://localhost:8770/debug/` | `logs-dev/` |

**初始化**（每个目录独立 clone + 编译）：
```bash
cd /data/workspace
git clone https://git.woa.com/yitianchen/smartnpc.git stable
cd stable && git checkout master && cd ..
git clone https://git.woa.com/yitianchen/smartnpc.git dev
cd dev && git checkout dev-working && cd ..

# 各自编译 MCP（需要 Go 1.25+）
cd /data/workspace/stable/agenttown-mcp && go build -o ../mcp ./cmd/agenttown-mcp && cd ~
cd /data/workspace/dev/agenttown-mcp && go build -o ../mcp ./cmd/agenttown-mcp && cd ~

# 各自配 .env（至少 VENUS_API_KEY）
cp .env.example /data/workspace/stable/.env  # 填入 VENUS_API_KEY
cp .env.example /data/workspace/dev/.env
```

**启动 stable**（终端 1 — MCP）：
```bash
cd /data/workspace/stable
bash start-debug.sh     # 或直接 ./mcp --http :8760 --ws :9092 --venus-api-key "$VENUS_API_KEY"
```

**启动 dev**（终端 2 — MCP）：
```bash
cd /data/workspace/dev
bash start-dev.sh       # 偏移端口 8770/9093 + logs-dev/ 日志目录
```

**端口隔离原则**：stable 用 `8760/9092`，dev 用 `8770/9093`，互不干扰，可同时运行各自独立的仿真。日志分别写入 `/data/workspace/stable/logs/` 和 `/data/workspace/dev/logs-dev/`（各按 `YYYY-MM-DD/debug-mcp.log` 组织，由 `start-debug.sh` / `start-dev.sh` 分别写入）。

**本地 Windows 对比**：本地用 `D:\SmartNPC_v3`（dev worktree）和 `D:\SmartNPC_v3-stable`（stable worktree，`master` 分支）两个 worktree 实现同样的分离，端口约定一致。

## 文件地图

| 路径 | 说明 |
|------|------|
| `docs/AgentTown_CommProtocol_Values.md` | 通信协议与数值系统设计文档（唯一权威） |
| `docs/AgentTown_Reactive_Layer.md` | 反应层设计文档 |
| `docs/DebugAction_Tool.md` | 联调 Debug 工具 `/debug/action` + `/debug/schedule` 使用文档 |
| `agenttown-mcp/cmd/agenttown-mcp/main.go` | 入口：flag、端口、agentContext、worker 循环、guardedExecutor、debug handler、装配（Runtime 创建 + 回调注册） |
| `agenttown-mcp/cmd/agenttown-mcp/runtime.go` | Runtime：入站消息单入口分发（原 main() 的 SetMessageHandler 巨型闭包）+ OnDisconnect，实现契约回调 |
| `agenttown-mcp/cmd/agenttown-mcp/fake_transport_test.go` | fakeTransport：contract.Transport 测试替身（记录出站调用、可切换连接状态） |
| `agenttown-mcp/cmd/agenttown-mcp/strategic.go` | 战略层：每日计划生成 |
| `agenttown-mcp/cmd/agenttown-mcp/tactical.go` | 战术层：goal → action 分解 |
| `agenttown-mcp/cmd/agenttown-mcp/event_router.go` | 事件路由器运行时：jev 判决调用（routerQuestions 三问 + routerDecisionFromJev 映射）+ routerInterrupt |
| `agenttown-mcp/cmd/agenttown-mcp/memory.go` | Stage 4 记忆层：昨日总结 narrative（`generateDailyMemories`）+ compact 结构化落盘（`persistExtractedMemory`）+ 规则直写（combat） |
| `agenttown-mcp/cmd/agenttown-mcp/memory_reader.go` | 读记忆内部工具 `memoryReader`（recentEpisodes/commitmentsNearby/commitmentsUpcoming/people）——不开放给 LLM、程序调用、零 LLM |
| `agenttown-mcp/cmd/agenttown-mcp/relationship.go` | Stage 5 对人认知：好感度合并渲染 + KB 种子导入（semantic_people） |
| `agenttown-mcp/cmd/agenttown-mcp/capability.go` | NPC 能力注册表：per-agent cmd 能力声明（system 全局默认 + 具体 agent 覆盖） |
| `agenttown-mcp/cmd/agenttown-mcp/debug_ui.go` | `/debug/` 浏览器控制台 + `/debug/{kb,cap,agents,logs,plan,tactical,ue-errors,llm-metrics}` JSON 端点 |
| `agenttown-mcp/cmd/agenttown-mcp/metrics.go` | LLM 指标收集器单例 + `dumpLLMMetrics` 落盘 docs/llm_metrics.md（best-effort，仿 prompt_doc） |
| `agenttown-mcp/cmd/agenttown-mcp/web/debug.html` | debug 控制台单页 HTML（单 Action + Schedule 注入 + 当日 schedule + 战术层分解情况 + MCP 日志多面板） |
| `agenttown-mcp/contract/transport.go` | **契约 module**：Transport 接口 + MessageHandler/DisconnectHandler（决策侧与连接侧边界，无依赖） |
| `agenttown-mcp/contract/protocol/envelope.go` | Envelope + 12 消息类型 + 12 cmd + error_code 常量 |
| `agenttown-mcp/contract/protocol/messages.go` | 各消息 payload 结构体 + resync/event_lost/capability_registry |
| `agenttown-mcp/wsserver/server.go` | **连接 module**：WS 服务端，实现 contract.Transport（收发信封、seq、send buffer、重放、SendAction ACK 等待） |
| `agenttown-mcp/pkg/llmtypes/types.go` | LLM 共享响应类型（Response/Block/Content/Usage），venus/战略/战术层复用 |
| `agenttown-mcp/pkg/llmmetrics/` | LLM 指标聚合（E2E/TTFT/TPOT/ITL 分位数 + 错误分布 + 重试率 + JSON 正确率），手写最近秩分位数，无第三方依赖 |
| `agenttown-mcp/pkg/venus/client.go` | Venus 客户端：OpenAI Chat Completions 协议直连（唯一战略/战术层后端） |
| `agenttown-mcp/pkg/jev/client.go` | jev 判决模型客户端：事件路由专用（POST /v1/systemone，state+三型 questions：noul/choice/score） |
| `agenttown-mcp/pkg/storage/store.go` | 持久化 Store 接口 + NoopStore（内存模式）+ ScheduleState |
| `agenttown-mcp/pkg/storage/mysql.go` | MySQLStore：write-through 持久化 + upsert |
| `agenttown-mcp/pkg/storage/migrations.go` | `//go:embed` 原生 SQL 迁移 runner |
| `agenttown-mcp/pkg/storage/migrations/0001_init.sql` | 初始 schema：调度状态表 + 预埋记忆/关系/动作历史表 |
| `agenttown-mcp/pkg/worldkb/loader.go` | world_kb.yaml 加载 + 内存索引 |
| `agenttown-mcp/pkg/worldkb/types.go` | KB/Zone/Object/Agent 权威类型（新 schema） |
| `agenttown-mcp/pkg/worldkb/query.go` | KB 查询：GetPosition/WhichZone/WhichObject/ResolveTarget |
| `agenttown-mcp/pkg/worldkb/merger.go` | `MergeMaps(gen, auth)` deep merge + 受保护字段白名单（protectedZone/Object/AgentFields）+ `MergeAndWriteBytes`（UE 推送 world_kb 时合并+落盘） |
| `agenttown-mcp/pkg/worldkb/validator.go` | `Validate(kb)` — ID 格式、cross-reference 合法性 |
| `agenttown-mcp/pkg/worldkb/serializer.go` | `WriteYAML`（按 ID 排序，原子替换）+ `WriteManifest`（SHA256 + RFC3339） |
| `agenttown-mcp/pkg/profile/profile.go` | NPC profile.md 加载：`LoadDir` 扫描 `*.md` → agentID → Profile map |
| `agenttown-mcp/pkg/weeklyschedule/loader.go` | 每周日程配置加载：`Load` 解析 7 天 YAML + `Day` 查询 |
| `agenttown-mcp/pkg/weeklyschedule/format.go` | `WeeklyLine(dayCount, sched)` 把 UE DayCount 映射到星期 + 注入【今日日程】段 |
| `agenttown-mcp/pkg/prompt/agent_role.go` | `AgentRole(kb, profiles, agentID)` 三层 per-field 回退构造【你的角色】段 |
| `agenttown-mcp/pkg/prompt/strategic.go` | 三层共享 system prompt `BuildSharedSystemPrompt` + 战略层 user prompt（`StrategicRules`/`BuildStrategicUserContext` 含【其他NPC】） |
| `agenttown-mcp/pkg/prompt/tactical.go` | 战术层 user prompt：`BuildTactical` + `TacticalRules`（system prompt 用共享 `BuildSharedSystemPrompt`，工具走 function calling tools 字段） |
| `agenttown-mcp/pkg/prompt/physical_bands.go` | 物理属性分档（电量/疲劳/关节磨损 3 阈值切 4 档）+ per-NPC `## 属性分段` 覆盖 |
| `agenttown-mcp/pkg/prompt/cmd_effects.go` | 设施每小时属性影响 + 使用门槛（由 world KB 声明速率派生） |
| `agenttown-mcp/pkg/prompt/kb_context.go` | 【世界详细信息】各区域可交互设施映射表（按实例真实分布） |
| `agenttown-mcp/adapters/agenttown/tools/registry.go` | 工具注册 + Executor 接口 |
| `agenttown-mcp/adapters/agenttown/tools/composite.go` | 5 个复合行为工具 |
| `agenttown-mcp/adapters/agenttown/tools/atomic.go` | 7 个原子行为工具 + 2 个特殊工具（scan_area/stop） |
| `agenttown-mcp/internal/log/logger.go` | slog JSON 日志（环形缓冲 + 写日志文件/stderr） |
| `assets/world_kb.yaml` | 世界 KB：7 zones / 57 objects / 5 agents（新 schema，locations 已合并进 objects） |
| `assets/world_kb.manifest.json` | merge 产物：源 SHA256 + 时间戳（UE 推送 world_kb 时写入） |
| `assets/profiles/H-01.md` ~ `H-05.md` | NPC 人设档案（5 个）：纯 markdown 固定标题分段（名字/职业/背景/性格特质/说话风格/属性分段），三层决策 persona override |
| `assets/weekly_schedule.yaml` | 每周日程配置：7 天周期（工作日/休息日/运动日/冥想日），战略层注入【今日日程】段 |
| `start-debug.sh` | 云环境启动脚本：拉起 MySQL + MCP + 读取 .env（stable 实例，端口 8760/9092，日志 logs/） |
| `start-dev.sh` | dev 实例启动 wrapper（偏移端口 8770/9093，日志 logs-dev/） |
| `scripts/pretty_log.py` | 日志可读化工具（HTML 报告 + 终端渲染；--hermes 系列参数 DEPRECATED 仅供历史日志） |
| `scripts/smoke_combat_detach.py` | combat-detach 冒烟脚本：模拟最小 UE 客户端（心跳 + perception 推进游戏时钟），经 `/debug/event` 注入 detach 攻击 → 断言宽限 stop/静默期零下发/脱管徽标 → combat_exit 归还重规划，无 UE 单侧验证全闭环 |
| `.env` | 环境变量（VENUS_API_KEY 等，不入库） |
| `.env.example` | 环境变量模板（分组注释 + 默认值 + 读取方说明） |

## Git 提交

格式：`<type>(<scope>): <subject>`（祈使句）
- type：`feat` / `fix` / `refactor` / `test` / `docs` / `chore` / `perf`
- scope：`protocol` / `mcp` / `mock-ue` / `venus` / `config` / `start-script` / `logging` / `llmtypes`
- **提交信息（subject 和 body）使用中文**

完成一项工作之后就主动commit。

## 里程碑

| Milestone | 状态 | 说明 |
|-----------|------|------|
| M-1 世界快照定义 | ✅ | 世界 KB + 场景人物设定（docs 已整理归档） |
| M-2 LLM Gateway | ✅（已归档） | Hermes Gateway + DeepSeek，2026-08 移除 |
| M-3 Hermes Agent Mind | ✅（已归档） | SOUL.md + SKILL.md + profile，2026-08 移除 |
| M-4 Translator | ✅ | MCP 工具注册 |
| M-5 Mock UE Bridge | ✅（已弃用） | Python async + WebSocket，2026-08-11 起转真实 UE5 |
| MCP 层 | ✅ | Go agenttown-mcp，12 cmd（5 复合+7 原子）+ 2 特殊工具 |
| 协议重构 Phase 1-7 | ✅ | 7 字段信封、消息类型、seq+ACK、物理状态、动作异步生命周期、断线重连+seq 重放 |
| 端到端闭环 | ✅ | 感知→LLM→工具→真实 UE5 全链路验证 |
| 三层决策架构 | ✅ | 战略层（每日计划）+ 战术层（任务分解）+ 反应层（Ollama 打断） |
| Venus 直连 | ✅ | MCP 直连 Venus，无状态调用，2026-08 取缔 Hermes |
| 三层决策注入 NPC 性格 | ✅ | `AgentRole` 共享 helper，战略/战术/反应层注入【你的角色】段 |
| 反应层 P0-P1 | ✅ | 本地 Ollama + zone/physical/periodic 触发 + replan 决策 |
| Debug 工具升级 | ✅ | `/debug/action` + `/debug/schedule`（注入 schedule 调试战术层） |
| 战术层流式输出 | ✅ | `--tactical-stream` flag（默认关，DeepSeek 高峰排队时回退） |
| 状态访问与业务逻辑分离 Stage 3 | ✅ | MySQL 持久化层：4 调度字段 write-through + `LoadPersistent` 热重启 + `//go:embed` SQL 迁移 |
| 状态访问与业务逻辑分离 Stage 4 | ✅ | 长期经历记忆：三表重建（episodic_memories/commitments + semantic_people）+ compact 结构化抽取 + 战略层注入昨日总结 + 战术层注入 top-3 经历（memoryReader 内部工具）+ action_history 落盘 |
| 状态访问与业务逻辑分离 Stage 5 | ✅ | 对人认知：semantic_people（单一 affinity 0-100 + impression）+ compact people_updates + 战术层注入【人际关系】段 + KB 种子导入 |
| 12 cmd 体系迁移 | ✅ | 旧 14 cmd（8 原子+6 复合）→ 新 12 cmd（7 原子+5 复合）对齐真实 UE5；统一 MoveTo（target_type+target_id/target_position）；复合动作共享 semantic_group+interaction schema（按真实 UE5 capability_registry 参数名）；GenericAct 兜底；MCP 不再做 KB 坐标解析 |
| NPC profile.md 人设档案 | ✅ | `pkg/profile` 加载 `assets/profiles/<agentID>.md`；三层 per-field 回退（profile > KB > hardcoded）；战略/战术/反应/记忆层透传 profiles 参数到 `AgentRole` |
| 每周日程配置 | ✅ | `pkg/weeklyschedule` 加载 `assets/weekly_schedule.yaml`；7 天周期（工作日/休息日/运动日/冥想日）；战略层注入【今日日程】段；`WeeklyLine` 预格式化解耦 pkg/prompt |
| 战术层 function calling 迁移 | ✅ | OpenAI 原生 function calling（`tools` 字段 + `tool_choice=required` + 多轮 agentic loop）；工具由 capability_registry 派生；多段动作计划（1-4 段 + time_to_stop）；会话历史仅跨游戏日清空（滑动窗口截断已取消） |
| 战略/战术层 prompt 重构 | ✅ | system/user 拆分；规则迁 user prompt；注入生产工作流概述；物理属性分档（电量/疲劳/关节磨损）；"各区域可交互设施"映射表按实例真实分布 |
| 战术层健壮性兜底 | ✅ | time_to_stop 兜底（非队尾休息 1800s/工作 5400s）；LLM 失败兜底动作（speak+look_around 防呆站）；zone 参数透传修复；**venus 4001 相同请求体重试（上限 3 次，实测全部救回）** |
| debug 控制台升级 | ✅ | 新增 `/debug/tactical`（战术层分解情况面板）、`/debug/cap`、`/debug/agents`、`/debug/logs`、`/debug/plan`、`/debug/ue-errors` |
| **wsserver/agent 模块化解耦** | ✅ | 三 module（根=agent 决策+壳 / contract=契约 / wsserver=连接）；`contract.Transport` 接口 + `Runtime` 入站单入口分发；决策侧 14 处具体类型改接口；replace 本地引用（2026-08-27，MR !57） |
| **事件驱动反应体系** | ✅ | `world_event` 事件通道（force 硬保证：零 LLM 零去抖微秒级打断 / 非 force 入队）+ 安全点 drain（事件 → 战术层第三输入）+ 反应护栏（60 游戏分钟截止 + severity 严格递增）+ 持续威胁情境与事件回声（防"幻觉解除威胁"）+ 事件合成器过渡层；旧 Ollama 反应层退役（2026-09，P1-P4 系列） |
| 事件路由器接入 jev 判决模型 | ✅ | 非 force 事件走 Venus 判决 API（`/v1/systemone`）三问裁决（should_interrupt/motive/severity），state 结构化携带 agentic loop 近期历史，实测单次 ~0.5s；同一事件不同 NPC 因性格/关系/当前动作产生不同裁决（涌现验收点）（2026-09，MR !67） |
| **combat-detach 战斗接管** | ✅ | `detach:true` = UE 战斗 AI 接管、agent 确定性让位（掐在途 LLM + 清队列 + 全抑制面 + 2s 宽限期 stop 保险丝）；combat_exit 确定性归还 + 120 游戏分钟 TTL 兜底；跨断线重连存续；协议容错（短名/缺 category 别名归一）；22 项单测 + 冒烟脚本；2026-09-22 联调跑通（毫秒级抢占、宽限 stop 零触发） |

## 当前已知问题（2026-08-27 仿真分析）

按严重度排序：

1. **UE 端 zone 查找 bug（no_smartobject_in_zone）**：带旋转（yaw≠0）的 zone（logistics_hub 41°、residential_quarters 45°）内物体查找失败——world_kb 导出与 UE 运行时的旋转方向约定不一致，`InteractSmartObject zone=logistics_hub` 全部秒拒（bench/process_machine 均中招）；WorkShift 等复合动作按 semantic_group 全地图查找不受影响。**需 UE 端修复**；MCP 侧可做规避（失败剥离 zone 重试 / 引导优先用 WorkShift）
2. **充电/维修动作在 slot 边界被打断**：战略层给的充电 slot 仅 30 分钟，战术层动作（speak 前置 + tts 1500s）还没充完，末段 time_to_stop 预算（规则 8 要求 ≈ 时段剩余时长）就在 slot 边界到点切段，充电未生效——低电量 NPC 尝试充电 8 次全部 interrupted，电量持续下降（注：slot 切换机制本身已事件化不强切，P3-8；打断来自末段预算设计）。修复方向：战略层给充电/维修安排 ≥1 小时 slot，或战术层在 slot 末尾不安排会被打断的长动作
3. **夜间时段切换请求突发超时**：22:00 前后所有 NPC 日程同时切到睡眠时段，5 个战术层请求 18 秒内并发打向 venus，后端排队导致 4 个 60s 超时（重试错峰后恢复）。修复方向：slot 切换触发的分解加随机错峰延迟
4. **战术层队列提前耗尽**：LLM 给的 action 总时长不够 slot 时长，触发重分解。已用 time_to_stop 兜底（rest/work 补默认值）部分缓解
5. **战略层日程质量参差**：同一 prompt 下 LLM 采样方差大（4-9 条不等），首段可能违反"禁止安排工作"硬约束。短时段裁剪逻辑已停用
6. ~~LLM 输出坏 tools JSON（venus 4001）~~ **已解决**（2026-08-27）：4001 相同请求体重试（上限 3 次）后全部被救回，长仿真中 0 次最终失败；超时类失败走兜底动作 + 下一感知周期重试，恢复时间 5-11 秒
7. ~~反应层冷启动超时~~ **已随反应层退役**（2026-09）：Ollama 反应层整体退役（P4-12），打断判定由事件系统承担；2026-10-22 长期记忆重建后 Ollama 关系判断也迁到 compact，Ollama 客户端无消费者（死代码待清理）
8. **UE 双发攻击事件**（2026-09-22 战斗联调）：同一攻击 UE 同毫秒发两条事件（`player_attacked` 规范名 + `attacked` 短名）——MCP 侧幂等容错（重复 detach 仅刷新 TTL 起点），无害但建议 UE 收敛为单条规范命名
9. **combat_exit 偶发晚于 TTL 兜底**（2026-09-22 战斗联调）：部分战斗的 exit 晚于 120 游戏分钟 TTL 到达（实测 TTL 归还后 2s 才收到 exit，落到非让位路径无害）——若期望 agent 始终等 UE 归还信号而非超时接管，UE 侧需检查 exit 发出时机
10. **UE 睡眠舱以 too_tired 拒绝 sleep**（2026-09-24 实测）：战斗后疲劳 100 的 NPC 规划 sleep_pod/sleep 被 UE 秒拒 `too_tired`——KB 声明睡眠舱 `max_fatigue_to_use=100` 无疲劳门槛，"太累不能睡"不通，UE 侧疑似门禁配反或失败原因误标。连锁后果：恢复动作全部秒拒（sleep/assemble too_tired + bench rest `no_smartobject_in_zone` 即已知问题 #1 的旋转 zone bug），队列秒耗尽 → 2.5s 一次 refill 循环、speak 连发。**需 UE 侧核对**

## 历史重构记录

**取缔 Hermes、MCP 回归无状态**（2026-08 完成）：
- 移除 `pkg/hermes` 客户端、Docker 配置、启动脚本逻辑、`--llm-backend` flag
- 抽取 `pkg/llmtypes` 共享类型，Venus 成为唯一战略/战术层后端
- 三层决策注入 NPC 性格（`AgentRole` 共享 helper）
- `hermes/` 目录及 `docs/archive` 下 Hermes 文档已彻底移除
- `scripts/pretty_log.py` `--hermes` 系列参数 DEPRECATED，仅供历史日志解析

**战术层迁移 function calling**（2026-08 完成）：
- 战术层从 prompt 文本工具清单迁至 OpenAI 原生 function calling（`tools` 字段 + `tool_choice=required`）
- 多段动作计划（1-4 段 + time_to_stop）；多轮 agentic loop 保留会话历史
- time_to_stop 兜底 + LLM 失败兜底动作；历史截断（8 轮）后于 2026-08-27 取消（改为仅跨日清空）
- 战略/战术层 prompt 重构（system/user 拆分、规则迁 user prompt、生产工作流概述、物理分档"能量→电量"）

**wsserver/agent 模块化解耦**（2026-08-27 完成，MR !57）：
- 新建 `contract` 契约 module（protocol + Transport 接口）与 `wsserver` 连接 module，多 module 仓库（replace 本地引用；go.work 对嵌套 module 解析失败弃用）
- Stage 1 接口抽象：决策侧 14 处 `*wsserver.Server` → `contract.Transport`，debug UI 的 `Call` 并入 `SendAction`
- Stage 2 入站事件化：main() 的 SetMessageHandler 巨型闭包（~250 行）提取为 `Runtime.HandleMessage`/`OnDisconnect`
- Stage 3 module 物理拆分：`pkg/protocol`→`contract/protocol`、`pkg/wsserver`→`wsserver/`，import 路径全局改写
- Stage 4 测试加固：`fakeTransport` 测试替身 + executor 单测改写（SendAction 断连/capability gate/成功记账）
- agent 决策与 main 壳进一步分离（agent/app 两 module）待后续单独处理（main() 装配与决策深度交织，需重新设计 agent 工厂 API）
