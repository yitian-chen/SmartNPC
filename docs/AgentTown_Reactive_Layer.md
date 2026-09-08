# AgentTown 反应层设计 — 本地小模型实时决策

> 本文档描述反应层的功能划分、设计决策与分阶段实现计划。
> 反应层用本地 Ollama 模型（qwen2.5:7b-instruct-q4_K_M）低延迟处理突发事件，
> 与战略层（云 LLM 每日规划）+ 战术层（云 LLM 时段分解）共同构成三层决策架构。
>
> 对应协议设计：`docs/AgentTown_Core_DeepDive.md` 第 633-703 行（反应层原始设计）、
> `docs/AgentTown_CommProtocol_Values.md` 第 505-537 行（event_notification）。

---

## 一、定位与三层关系

| 层 | 模型 | 触发频率 | 决策范围 | 延迟预算 |
|---|---|---|---|---|
| 战略 | 云 LLM | 每日 1 次 | 全天时段分配 | 30-60s 可接受 |
| 战术 | 云 LLM | 每时段 | 时段任务分解为 action 队列 | 10-30s 可接受 |
| **反应** | **本地 7B** | **感知/事件即时** | **是否打断 + 即时反应动作** | **≤3s 必须** |

反应层核心价值：用本地模型低延迟应对突发事件，避免每个感知都走云 LLM（曾被移除的旧反应层每个感知走 Hermes，单轮 token 从 1-2KB 飙到 187k，commit c8c4d22 移除）。本地模型无会话链累积，每轮独立 prompt，token 开销固定。

---

## 二、用户确认的设计决策

### 决策 1：打断权限边界

**选 B：任何动作都可打断，复合动作被打断后剩余时长作废。**

- 原子动作（move_to / wait / speak / interact）：直接发 `stop_action`，UE 立即停止
- 复合动作（charge_at / work_assemble / archive_research / rest_idle）：发 `stop_action`，UE 清除 `busy_until_min`，剩余进度作废
- 不保存复合动作中断点，反应完成后战术层在下一时段自然 refill 重新规划

理由：第一期简化实现，复合动作的 StateTree 优雅退出（`Core_DeepDive.md:900`）留到 P2。

### 决策 2：反应 action 与战术队列的关系

**选 A：反应 action 直接插队，执行完继续战术队列。**

- 反应 action 不清空战术队列，仅在 `currentActionID` 锁上插队执行
- 反应 action 完成后，`runPerceptionWorker` 自然回到队列驱动的 pop/refill 循环
- 战术队列中原有的 action 不会被丢弃

与"打断"的关系：打断只发 `stop_action` 停止当前在途 action；插队的反应 action 是一个新的 action_command。两步可连续做（先 stop，再发反应 action），也可只 stop 不发新 action（让队列下一个 action 自然接上）。

### 决策 3：Ollama 调用频率

**选 B：只在感知有"显著变化"时调用，不是每个 perception_update 都调。**

显著变化定义：
- **zone 变化**：NPC 进入新区域
- **新物体出现**：perception 中 nearby_objects 出现之前没见过的 object_id
- **事件到达**：收到 `event_notification` 消息
- **物理状态突破警戒带**：energy < 40 / fatigue > 80 / joint_wear > 70（从正常区间跨入警戒带的那一刻，而非持续在警戒带内重复触发）

非显著变化（不触发反应层）：
- 纯时间推进、物理状态正常波动、busy progress 普通变化、相同 scan_area 重复结果

去抖：相同触发原因 60 秒内不重复调用 Ollama。

### 决策 4：第一期范围

**P0 only**：完成最小闭环（Ollama 客户端 + 触发器 + prompt + 打断），验证本地模型决策质量后再扩展 P1/P2。

---

## 三、P0 — 最小闭环（第一期目标）

### P0.1 Ollama 客户端

**新增文件**：`agenttown-mcp/pkg/ollama/client.go`

接口设计：
```go
type Client struct {
    baseURL    string        // http://localhost:11434
    model      string        // qwen2.5:7b-instruct-q4_K_M
    httpClient *http.Client
    logger     *slog.Logger
}

// Chat 发送一次性对话请求，无会话链。
// 超时由 ctx 控制，建议 3-5 秒。
// 返回模型输出的文本（期望是 JSON）。
func (c *Client) Chat(ctx context.Context, prompt string) (string, error)
```

关键约束：
- 无 `previous_response_id` 链式会话（区别于 Hermes client）
- 无 token 累积问题——每轮独立 prompt，固定大小
- HTTP 超时 5 秒，context 超时 3 秒（取较短者生效）
- 失败不重试（反应层失败应快速放弃，让战术层接管）

配置：通过 `--ollama-url` 和 `--ollama-model` 命令行 flag，默认 `http://localhost:11434` 和 `qwen2.5:7b-instruct-q4_K_M`。

### P0.2 反应决策触发器

**修改文件**：`agenttown-mcp/cmd/agenttown-mcp/main.go`

新增 `runReactiveCheck` 函数，从三个入口被调用：

1. **`observePerception` 回调**（`main.go:87` 附近）
   - 检测 zone 变化、新物体出现
   - 满足"显著变化"则调 `runReactiveCheck`

2. **`observePerception` 回调**（`main.go` 附近）
   - 检测物理状态突破警戒带（energy/fatigue/joint_wear 跨阈值）
   - 满足则调 `runReactiveCheck`

3. **`recordEventNotification` 回调**（`main.go:140`，当前是 no-op）
   - 改为：收到 event_notification 立即调 `runReactiveCheck`
   - 这是反应层最核心的触发源

`runReactiveCheck` 逻辑：
```
1. 去抖检查：相同触发原因 60s 内已调用过 → 跳过
2. 构造反应 prompt（见 P0.3）
3. 调 ollamaClient.Chat(ctx, prompt)，3s 超时
4. 解析 JSON（见 P0.3）
5. 按 reaction 字段分发：
   - continue  → 不做事
   - observe   → 记录到 latestPerception.audible_events 供战术层参考
   - interrupt → 调 ws.SendStopAction(currentActionID)
   - act       → 调 ws.SendAction 下发 reaction.action
6. 任何步骤失败 → 静默放弃，不影响战术层
```

并发控制：`runReactiveCheck` 用独立 mutex 串行化，避免多个触发源并发调用 Ollama。但**不阻塞**战术层——反应层调用 Ollama 期间，战术队列正常 pop 下发 action。

### P0.3 反应 prompt 与决策解析

**新增文件**：`agenttown-mcp/cmd/agenttown-mcp/reactive.go`

Prompt 模板（中文，qwen2.5 中文表现好）：
```
你是 NPC 老陈的反应决策模块。当前情况需要你判断是否打断当前行动。

【当前状态】
时段：{time_of_day}
位置：{zone}
物理：体力={energy}/100, 疲劳={fatigue}/100, 关节磨损={joint_wear}/100
在途动作：{current_action}（{elapsed_sec}秒前开始）

【触发原因】
{trigger_reason}

【可选反应】
- continue：不打断，让当前行动继续
- observe：不打断，记录这个事件供后续参考
- interrupt：打断当前行动（会发送 stop_action）
- act：打断当前行动并立即执行一个新动作

请输出 JSON，格式严格如下：
{"reaction": "continue|observe|interrupt|act", "reason": "简短理由", "action": {"cmd": "...", "params": {...}}}

action 字段仅在 reaction=act 时填写，cmd 可选：move_to / speak / emote / wait / interact。
不要输出 JSON 以外的任何内容。
```

决策解析：
```go
type ReactiveDecision struct {
    Reaction string          `json:"reaction"`  // continue|observe|interrupt|act
    Reason   string          `json:"reason"`
    Action   *ReactionAction `json:"action,omitempty"`
}

type ReactionAction struct {
    Cmd    string                 `json:"cmd"`
    Params map[string]interface{} `json:"params"`
}
```

容错：
- JSON 解析失败 → 视为 `continue`（最保守）
- `reaction` 字段不在枚举内 → 视为 `continue`
- `act` 但 `action` 为空或 cmd 非法 → 降级为 `interrupt`
- `action.params` 不完整 → 用默认值填充（如 `move_to` 缺 target 则用当前 zone 中心）

### P0.4 stop_action 打断

**复用现有实现**：
- `pkg/wsserver/server.go:578` `SendStopAction(actionID)` 已实现
- `main.go:57` `currentActionID` 已维护
- `mock_ue.py:733` `_handle_stop_action` 已实现约定 9 ID 匹配

打断流程：
1. 反应层决策 `interrupt` 或 `act`
2. 若 `currentActionID != ""`：调 `ws.SendStopAction(currentActionID)`
3. UE 收到后回 `action_completed` 带 `interrupted` 状态
4. `recordActionCompletion` 清空 `currentActionID`
5. 若 `act`：构造新 action_command 下发，设置新 `currentActionID`
6. 若 `interrupt`：不发新 action，`runPerceptionWorker` 下一轮 pop 队列下一个 action

注意：**不清空战术队列**（决策 2 选 A）。打断只影响在途的当前 action，队列里的后续 action 保留。

### P0.5 测试

**新增文件**：
- `agenttown-mcp/pkg/ollama/client_test.go` — 客户端单元测试（mock HTTP server）
- `agenttown-mcp/cmd/agenttown-mcp/reactive_test.go` — prompt 构造、决策解析、容错测试

测试不依赖真实 Ollama，用 `httptest.Server` 模拟 `/api/chat` 响应。

### P0 验收标准

1. 启动 stable 实例 + Mock UE，NPC 正常跑一天，战术层行为不受反应层影响
2. 手动构造场景事件（mock_ue 注入），反应层在 3 秒内输出决策
3. 反应层决策 `interrupt` 时，UE 收到 `stop_action` 并回 `interrupted`
4. Ollama 离线时，反应层静默失败，不影响战术层
5. `go test ./...` 全部通过

---

## 四、P1 — 联调增强（第二期，待 P0 验证后启动）

### P1.1 scan_area 工具恢复

**修改**：`agenttown-mcp/adapters/agenttown/tools/atomic.go:226-232`

当前 handler 直接返回 error `"disabled: reactive layer removed"`。P1 解除 disabled，handler 重新实现：
- 请求 MCP 立即拉一次 UE 感知（不等下一个 perception_update 周期）
- 返回当前 zone 的详细物体列表
- 反应层在信息不足时可调用 scan_area 获取更多上下文

### P1.2 stop 工具恢复

**修改**：`agenttown-mcp/adapters/agenttown/tools/atomic.go:235-241`

当前 handler 返回 error。P1 解除 disabled，handler 实现：
- 调 `ws.SendStopAction(currentActionID)`
- 反应层决策 `interrupt` 时内部调用此工具（而非直接调 `ws.SendStopAction`）

### P1.3 event_notification 独立通道

**修改**：`src/agenttown/mock_ue.py`

当前 mock_ue 把场景事件折入 `perception_update.audible_events`（`mock_ue.py:524`）。P1 新增独立 `event_notification` 消息发送：
- 场景事件到达时，先发 `event_notification` envelope（即时）
- 同时仍折入下一个 perception（供战术层参考）
- MCP 侧 `recordEventNotification` 收到后立即触发 `runReactiveCheck`

协议文档 `AgentTown_CommProtocol_Values.md:1035` 已注明第一期牺牲了实时性，P1 是补上的时候。

### P1.4 反应层日志

**修改**：`main.go` 日志输出

新增日志字段：
- `[反应层/触发]` reason=zone_change agent_id=H-01 zone=main_workshop
- `[反应层/PROMPT]` 显示完整 prompt
- `[反应层/RESPONSE]` 显示 Ollama 原始输出
- `[反应层/决策]` reaction=interrupt reason="体力过低" action_id=act_xxx
- `[反应层/失败]` err="ollama timeout after 3s"

日志格式与战略层/战术层保持一致，便于 `scripts/pretty_log.py` 渲染。

### P1 验收标准

1. scan_area / stop 工具可被反应层调用并返回正确结果
2. mock_ue 场景事件能通过 event_notification 即时到达反应层
3. 反应层决策链路在 `pretty_log.py` 中可完整追踪

---

## 五、P2 — 高级特性（第三期，可选）

### P2.1 复合动作 StateTree 优雅退出

**背景**：`Core_DeepDive.md:900` 提到复合动作打断应走 StateTree 优雅退出，而非硬 stop。

当前实现（P0 决策 1 选 B）：复合动作被打断后剩余时长作废。P2 引入 StateTree：
- 复合动作维护子状态机（如 `work_assemble` = `move_to_workbench` → `play_animation` → `wait` → `play_animation_end`）
- 打断时根据当前子状态决定如何退出（如装配中 → 播放"放下零件"动画 → 退出）
- mock_ue 侧需配合实现 StateTree 状态推进

### P2.2 反应层熔断

**新增**：`reactive.go` 熔断逻辑

- 连续 5 次 Ollama 调用失败（超时/连接拒绝/解析失败）→ 进入熔断状态
- 熔断期间反应层退化为"只 observe，不 interrupt/act"（仍记录事件供战术层参考）
- 每 60 秒尝试一次恢复探测，成功则退出熔断

避免本地模型故障导致 NPC 行为异常。

### P2.3 反应层指标统计

**新增**：`reactive.go` 指标收集

- 触发次数 / 决策分布（continue/observe/interrupt/act 占比）
- 平均延迟 / P99 延迟
- 失败率 / 熔断次数
- 通过 `/status` 或独立 `/metrics` 端点暴露

### P2.4 多 Agent 反应层

当前一期单 Agent（H-01）。P2 支持多 Agent 时：
- 每个 agent 独立的 `runReactiveCheck` goroutine
- Ollama 客户端共享（串行化调用避免过载）
- 触发去抖按 agent_id + reason 维度

### P2.5 模型热切换

**新增**：`--ollama-model` 支持 RPC 热切换

联调时可动态切换反应层模型（如从 qwen2.5:7b 换到 qwen2.5:14b 对比决策质量），无需重启 MCP。

---

## 六、实现顺序（P0）

建议按以下顺序实现，每步可独立编译验证：

1. **Ollama 客户端**（`pkg/ollama/client.go` + 测试）— 独立包，无依赖
2. **reactive.go**（prompt 构造 + 决策解析 + 容错）— 纯函数，易测试
3. **main.go 接线**（`runReactiveCheck` + 三个触发入口改造）— 集成
4. **命令行 flag**（`--ollama-url` / `--ollama-model`）
5. **端到端验证**（启动 stable + mock_ue，构造场景事件观察反应）

每步完成后 `go build ./... && go test ./...` 验证，最后做端到端。

---

## 七、关键文件清单

### P0 新增
- `agenttown-mcp/pkg/ollama/client.go` — Ollama HTTP 客户端
- `agenttown-mcp/pkg/ollama/client_test.go` — 客户端测试
- `agenttown-mcp/cmd/agenttown-mcp/reactive.go` — 反应层逻辑（prompt + 决策 + 触发）
- `agenttown-mcp/cmd/agenttown-mcp/reactive_test.go` — 反应层测试

### P0 修改
- `agenttown-mcp/cmd/agenttown-mcp/main.go` — 接线 `runReactiveCheck`，改造三个回调
- `agenttown-mcp/cmd/agenttown-mcp/main_test.go` — 配套测试调整

### P0 复用（不修改）
- `agenttown-mcp/pkg/wsserver/server.go:578` `SendStopAction`
- `agenttown-mcp/pkg/wsserver/server.go` `SendAction`
- `src/agenttown/mock_ue.py:733` `_handle_stop_action`

---

## 八、风险与缓解

| 风险 | 缓解 |
|---|---|
| Ollama 7B 模型决策质量不足，频繁误打断 | P0 验收时观察决策分布，若 interrupt 占比过高则调整 prompt 约束 |
| 本地模型延迟超 3s（CPU 推理慢） | 加 context 超时，超时视为 `continue`；后续可考虑 GPU 加速或换更小模型 |
| 反应层与战术层并发冲突（同时操作 currentActionID） | `runReactiveCheck` 串行化 + `currentActionID` 加 mutex 保护 |
| Ollama 进程崩溃导致反应层持续失败 | P2 熔断机制；P0 阶段静默失败即可 |
| mock_ue 场景事件触发频率与反应层去抖冲突 | P0 阶段先观察，必要时调整去抖窗口 |

---

## 九、与协议文档的对应关系

| 本文档章节 | 协议文档对应 |
|---|---|
| 反应层定位 | `Core_DeepDive.md:516-522`（trigger=perception_update OR event_notification） |
| 决策输出格式 | `Core_DeepDive.md:633-703`（{interrupt, reason, reaction}） |
| stop_action 打断 | `CommProtocol_Values.md:239,461-479`（约定 9 ID 匹配） |
| event_notification | `CommProtocol_Values.md:505-537`（Director 投放） |
| 第一期实时性牺牲 | `CommProtocol_Values.md:1035`（折入 perception） |
| 复合动作优雅退出 | `Core_DeepDive.md:900-1002`（StateTree，P2 实现） |
