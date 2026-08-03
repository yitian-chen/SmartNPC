# CODEBUDDY.md

This file provides guidance to CodeBuddy Code when working with code in this repository.

## 项目定位

AgentTown_v3 — AI NPC 模拟系统。一期单 Agent（H-01 老陈，车间主管机器人），通过 MCP 协议驱动完整的"感知→决策→行动"闭环。通信协议按 `docs/AgentTown_CommProtocol_Values.md` 实现，Mock UE 模拟 UE5 游戏世界。

**三层决策架构**（2026-07 落地）：
- **战略层**（`strategic.go`）：每日 06:00 生成当天计划（`dailyPlan`），一条 LLM 调用产出 7 个时段的 goal
- **战术层**（`tactical.go`）：每个时段把 goal 分解为 3-5 个 action 进 `actionQueue`，worker 逐个 pop 下发 UE
- **反应层**（`reactive.go` + `reactive_runner.go`）：监听 zone 变化/动作完成/物理警戒/周期触发，调本地 Ollama 决策 continue/observe/act/interrupt/replan

**LLM 后端可切换**（`--llm-backend` flag）：
- `hermes`（默认）：MCP → Hermes Gateway → Venus/DeepSeek
- `venus`：MCP 直连 Venus（OpenAI Chat Completions 协议），跳过 Hermes
- 反应层始终直连本地 Ollama，不走 Hermes/Venus

## 架构总览

```mermaid
graph LR
    subgraph Win["Windows 宿主"]
        UE["Mock UE (Python)<br/>asyncio + websockets<br/>src/agenttown/mock_ue.py"]
    end
    subgraph WSL["WSL2 / Linux"]
        MCP["agenttown-mcp (Go)<br/>MCP Server + WS Server<br/>:8760 HTTP / :9090 WS<br/>三层决策：战略+战术+反应"]
        DOCKER["Docker"]
        HERMES["Hermes Gateway<br/>hermes-agent:latest<br/>:8642<br/>(可选, --llm-backend=hermes)"]
        DOCKER --- HERMES
    end
    subgraph LLM["LLM 后端"]
        VENUS["Venus<br/>qwen3.6-35b-a3b<br/>(OpenAI 兼容)"]
        OLLAMA["Ollama 本地<br/>qwen2.5:7b<br/>(反应层专用)"]
    end
    UE <-->|"WebSocket :9090<br/>7-field Envelope"| MCP
    MCP -->|"HTTP POST<br/>/v1/responses"| HERMES
    MCP -.->|"HTTP POST<br/>/v1/chat/completions<br/>(--llm-backend=venus)"| VENUS
    MCP -->|"HTTP POST<br/>/api/chat<br/>(反应层)"| OLLAMA
    HERMES -->|"MCP Tool Calls<br/>/mcp :8760"| MCP
```

### 组件职责

| 组件 | 语言 | 路径 | 端口 | 职责 |
|------|------|------|------|------|
| Mock UE | Python 3.10+ | `src/agenttown/mock_ue.py` | — | 模拟 UE5：物理状态、空间状态、动作执行、感知推送 |
| agenttown-mcp | Go 1.25+ | `agenttown-mcp/` | HTTP `:8760`, WS `:9090` | 协议适配、感知语义化、工具暴露、三层决策、LLM 桥接 |
| Hermes Gateway | Docker | `docker/docker-compose.yml` | `:8642` | LLM Agent Mind（可选，`--llm-backend=hermes` 时启用） |
| Venus | 远程 | `--venus-url` | — | OpenAI 兼容 LLM 服务（战略/战术层后端） |
| Ollama | 本地 | `--ollama-url` | `:11434` | 反应层本地 LLM（qwen2.5:7b） |

### 三层决策架构

MCP 内置三层决策，由 `runPerceptionWorker`（`main.go:279`）事件驱动循环串联：

```mermaid
graph TB
    subgraph 战略层["战略层 strategic.go"]
        S1["每日 06:00<br/>generateDailyPlan<br/>1 次 LLM 调用"] --> S2["dailyPlan<br/>7 个时段 goal"]
    end
    subgraph 战术层["战术层 tactical.go"]
        T1["队列空 → selectCurrentGoal<br/>按 game_time 选 dailyPlan 时段"] --> T2["generateTacticalPlan<br/>1 次 LLM 调用"]
        T2 --> T3["actionQueue<br/>3-5 个 plannedAction"]
        T3 --> T4["popAndSendQueueAction<br/>逐个下发 UE"]
    end
    subgraph 反应层["反应层 reactive_runner.go"]
        R1["触发: zone/action_done/<br/>physical_alert/periodic"] --> R2["Ollama 调用<br/>8s 超时"]
        R2 --> R3{"决策"}
        R3 -->|continue| R4[不打断]
        R3 -->|observe| R4
        R3 -->|act| R5[stop + 新 action]
        R3 -->|interrupt| R6[stop 当前]
        R3 -->|replan| R7[战术层重规划]
    end
    S2 --> T1
    T4 --> UE
    UE -.->|感知事件| R1
```

### 关键机制

- **worker 循环**：`runPerceptionWorker` 监听 `wake` 信号，队列空时调 `tacticalRefill` → `selectCurrentGoal` → `generateTacticalPlan` → 填 `actionQueue` → `popAndSendQueueAction` 下发
- **`replanInProgress` mutex**：防止 worker 的战术层重规划和 `/debug/schedule` 注入并发调用 `tacticalHc` 冲突。worker 在 main.go:311 检查此标志
- **`debugOverride`**：仅阻止 worker 的 idle-wait refill（main.go:327），**不阻止**正在 LLM 调用中的 refill——所以 `/debug/schedule` handler 会同时设 `replanInProgress=true` + `debugOverride=true`
- **`currentSlot` 加 `__debug__` 前缀**：防止注入的 slot 和 dailyPlan 同名 slot 碰撞触发 `redecomposeCount >= 1` 限制
- **反应层去抖**：`lastReactiveAt` map 按 trigger 类型去抖（periodic 60s / zone_change 45s）
- **反应层 replan**：决策为 `replan` 时调 `ac.tacticalRefillForReplan`，会重置 `actionQueue` 重新调战术层 LLM

### LLM 后端切换

`--llm-backend` flag 选择战略/战术层的 LLM 通道：

| backend | 协议 | 路径 | 用途 |
|---------|------|------|------|
| `hermes`（默认） | OpenAI Responses | `pkg/hermes/client.go` | MCP → Hermes → Venus/DeepSeek，有会话链+摘要 |
| `venus` | OpenAI Chat Completions | `pkg/venus/client.go` | MCP 直连 Venus，无会话链，每次全量 prompt |

反应层**始终**走 `pkg/ollama/client.go`（本地 Ollama，5-8s 超时），不受 `--llm-backend` 影响。

**切换示例**：
```bash
# 默认走 Hermes
./agenttown-mcp --llm-backend=hermes --http :8760

# 直连 Venus（取缔 Hermes 路径）
./agenttown-mcp --llm-backend=venus \
  --venus-url http://v2.open.venus.oa.com/llmproxy \
  --venus-api-key $VENUS_API_KEY \
  --venus-model qwen3.6-35b-a3b
```

## 通信流向

```mermaid
sequenceDiagram
    participant UE as Mock UE
    participant WS as wsserver (MCP)
    participant Fmt as perception.Format
    participant HC as hermes.Client
    participant H as Hermes Gateway
    participant Tools as MCP Tools

    Note over UE: 感知循环 (每 N 游戏分钟，按模式配置)
    UE->>WS: perception_update {location, physical_delta, nearby_objects...}
    WS->>Fmt: 原始 payload → 第一人称叙事
    Fmt->>HC: 格式化文本
    HC->>H: POST /v1/responses {input, previous_response_id}
    H-->>HC: 响应 (narrative 或 tool_call)
    
    alt 响应含工具调用
        H->>Tools: MCP Tool Call (agent_id, params)
        Tools->>WS: SendAction → action_command
        WS->>UE: action_command {cmd, params}
        UE-->>WS: action_started (ACK ≤2s)
        WS-->>Tools: ACK → 工具返回
        Note over UE: 执行动作...
        UE->>WS: action_completed {result, progress}
        WS->>Fmt: 下次感知时折入叙事
    end
    
    HC->>WS: narrative 文本
    WS->>UE: narrative {text} (显示用)
```

## 常用命令

### 一键启动

```bash
bash start.sh normal             # 完整日：06:00-22:00, 150x
bash start.sh behavior           # 行为联调：06:00-18:00, 60x, 场景事件
bash start.sh quick-smoke        # 协议烟测：06:00-10:00, 600x
bash start.sh --quick            # quick-smoke 兼容别名
bash start.sh behavior --speed 100 --end 12  # 模式参数覆盖
SKIP_MCP_BUILD=1 bash start.sh normal        # 跳过 Go 编译
```

`start.sh` 执行顺序：**先停全部 → 编译+部署 MCP → 启动 MCP → 启动 Hermes → 启动 Mock UE → 仿真结束后合并日志**。每步健康检查通过才继续。

### Go 构建 / 测试

```bash
cd agenttown-mcp
go build ./...                                              # 编译检查
go test ./...                                               # 全部测试
go test ./pkg/wsserver/ -v -count=1                         # WS 缓冲/重放测试
go test ./pkg/protocol/ -v -count=1                         # 协议序列化测试
go test ./adapters/agenttown/perception/ -v -count=1        # 感知格式化测试
```

### 日志检查

**统一日志文件**：`logs/YYYY-MM-DD/sim.log`（MCP 进程独占写入，JSON Lines 格式，含 UE + MCP + Hermes 三层全链路；`YYYY-MM-DD` 为仿真启动日期）

**推荐：用 `scripts/pretty_log.py` 可读化查看**（每条 JSON 渲染为多行，方向标记着色，长字段按行展开）：

```bash
# HTML 报告（推荐，自动打开浏览器，可折叠/搜索/过滤）
python scripts/pretty_log.py --html                       # 今天的日志
python scripts/pretty_log.py --html 2026-07-20            # 指定日期
python scripts/pretty_log.py --html -f PERCEPTION -n 50   # 最近 50 条 PERCEPTION
python scripts/pretty_log.py --html -o report.html        # 指定输出路径
python scripts/pretty_log.py --html --no-open             # 生成但不自动打开
python scripts/pretty_log.py --html --hermes              # 整合 Hermes 容器日志（推荐）
python scripts/pretty_log.py --html --hermes --hermes-all # 整合 Hermes 全部条目（含噪声）
python scripts/pretty_log.py --html --hermes-log PATH     # 指定 Hermes 日志路径

# 终端渲染
python scripts/pretty_log.py                              # 查看今天的 sim.log
python scripts/pretty_log.py -f PERCEPTION -n 5           # 最近 5 条 MCP→Hermes 感知原文
python scripts/pretty_log.py -f RESPONSE -n 5             # 最近 5 条 Hermes 响应
python scripts/pretty_log.py --raw                        # 原始 JSON（grep/awk 友好）
```

`--html` 模式生成独立 HTML 文件（默认 `logs/YYYY-MM-DD/sim_report.html`），自动打开浏览器，支持：
- 点击条目展开/折叠详情
- 顶部按钮按方向过滤（UE→MCP / MCP→UE / PERCEPTION / RESPONSE / TOOL / Hermes）
- 搜索框（支持正则）
- 长字段（perception text / payload）自然换行，不受终端宽度限制
- 暗色主题，方向标记彩色高亮

`--hermes` 模式整合 Hermes 容器日志（`hermes/profiles/h01/logs/agent.log`）：
- Hermes 日志按时间戳与 sim.log 合并排序，统一展示
- 容器内 UTC 时间自动转 +08:00，与 sim.log 对齐
- 默认按 sim.log 时间范围过滤（前后扩展 5 分钟），仅保留同期条目
- 默认只保留 LLM 决策相关条目（`agent.conversation_loop` / `agent.tool_executor` / `run_agent` / `POST /v1/responses`）以及所有 WARNING/ERROR，其余噪声默认隐藏
- `--hermes-all` 显示全部条目（包括插件注册、健康检查、cron、housekeeping 等）
- 新增 `Hermes/internal` 方向标签（红色边框）和 `Hermes` 过滤按钮

方向过滤器（`-f`）简写：`UE→MCP` / `MCP→UE` / `PERCEPTION` / `RESPONSE` / `TOOL` / `HERMES` / `HEARTBEAT`。heartbeat 默认隐藏。

**原始 grep（不渲染，单行 JSON）**：

```bash
grep '\[UE→MCP\]' logs/YYYY-MM-DD/sim.log           # Mock UE → MCP（感知/状态/动作完成）
grep '\[MCP→UE\]' logs/YYYY-MM-DD/sim.log           # MCP → Mock UE（动作命令/叙事）
grep '\[MCP→Hermes/PERCEPTION\]' logs/YYYY-MM-DD/sim.log  # MCP → Hermes（感知文本）
grep '\[Hermes→MCP/RESPONSE\]' logs/YYYY-MM-DD/sim.log   # Hermes → MCP（LLM 响应 + narrative）
grep '\[Hermes→MCP/TOOL\]' logs/YYYY-MM-DD/sim.log       # Hermes 调用的工具
grep '\[MCP→Hermes/STRATEGIC-PROMPT\]' logs/YYYY-MM-DD/sim.log   # 战略层 prompt（每日规划输入）
grep '\[Hermes→MCP/STRATEGIC-RESPONSE\]' logs/YYYY-MM-DD/sim.log # 战略层 LLM 响应（每日计划 JSON）
grep '\[MCP→Hermes/TACTICAL-PROMPT\]' logs/YYYY-MM-DD/sim.log    # 战术层 prompt（任务分解输入）
grep '\[Hermes→MCP/TACTICAL-RESPONSE\]' logs/YYYY-MM-DD/sim.log  # 战术层 LLM 响应（actions JSON）
grep '队列已填充' logs/YYYY-MM-DD/sim.log           # 战术层任务队列形成（含完整 actions）
grep 'perception decision triggered' logs/YYYY-MM-DD/sim.log  # LLM 决策触发点
grep 'state_report' logs/YYYY-MM-DD/sim.log         # 状态报告摘要

# 按决策轮次关联：PERCEPTION / TOOL / RESPONSE 共享 agent_id + decision_epoch
# 例如查看 decision_epoch=1 的完整链路：
grep '"decision_epoch":1' logs/YYYY-MM-DD/sim.log   # 同一轮次的 PERCEPTION/TOOL/RESPONSE

# 战术规划链路：TACTICAL-PROMPT → TACTICAL-RESPONSE → 队列已填充 → 下发 action
# 例如查看某次战术分解的完整链路：
grep -E 'TACTICAL-PROMPT|TACTICAL-RESPONSE|队列已填充|\[战术层\] 下发 action' logs/YYYY-MM-DD/sim.log

# Hermes 容器日志（独立，不进 sim.log）
wsl docker logs -f agenttown-h01
```

**轮次关联**：`[MCP→Hermes/PERCEPTION]`、`[Hermes→MCP/TOOL]`、`[Hermes→MCP/RESPONSE]` 三种日志都带结构化字段 `agent_id` 和 `decision_epoch`，匹配这两个字段即可关联同一次决策回合的输入 prompt、工具调用、LLM 响应。同一 `decision_epoch` 的 TOOL 可能出现在 RESPONSE 之前（Hermes 在 LLM 流式输出时实时回调工具，而 RESPONSE 日志在 HTTP 响应完成后才写）。

**战术/战略层日志**：战略层和战术层使用独立的 Hermes session（不复用决策链），因此不带 `decision_epoch`。链路按 `agent_id` + 时间顺序关联：`[MCP→Hermes/STRATEGIC-PROMPT]` → `[Hermes→MCP/STRATEGIC-RESPONSE]` → `[战略层] 每日计划生成成功`；`[MCP→Hermes/TACTICAL-PROMPT]` → `[Hermes→MCP/TACTICAL-RESPONSE]` → `[战术层] 队列已填充`（含完整 actions JSON）→ `[战术层] 下发 action`（逐个 pop）。

Mock UE 不再写独立日志文件，但控制台仍输出 `[PERCEPTION]`/`[STATE]`/`[SPEAK]` 等人类可读摘要供实时观察。

## 联调 Debug 工具

MCP 启动后暴露 HTTP debug 端点（dev 端口 `:8770`，stable `:8760`，以 `--http` flag 为准）：

| 端点 | 方法 | 用途 |
|------|------|------|
| `GET /debug/` | GET | 浏览器控制台 UI（单页 HTML，`//go:embed` 嵌入） |
| `POST /debug/action` | POST | 直接下发单个 action_command 到 UE（单步调试） |
| `POST /debug/schedule` | POST | 注入一条 schedule 到战术层，立即分解为 action 序列入队 |
| `GET /debug/kb` | GET | 返回 world_kb JSON（zones/objects） |

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

### 浏览器 UI

`/debug/` 提供 tab 切换：
- **单 Action**：填 cmd + params，直接下发 UE
- **Schedule 注入**：填 schedule 文本，触发战术层分解

UI 特性：curl 预览、历史记录（支持 replay）、响应字段高亮、强制中断复选框。

## 通信协议（v1.0）

### 7 字段信封

所有消息共用外层结构（`pkg/protocol/envelope.go`），业务字段一律放入 `payload`：

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
| 感知 vs 状态分工 | perception_update 负责空间+环境；state_report 是物理状态权威通道 |
| 物理 delta 阈值 | energy/fatigue/health 变化 ≥5，joint_wear 变化 ≥1 才携带 |

### 消息类型总表

| type | 方向 | 用途 | 触发时机 |
|------|------|------|----------|
| `perception_update` | UE→Agent | 空间+环境感知（物理仅带变化项） | 每 N 游戏分钟（normal=60, behavior=15, quick-smoke=30）/ zone 变化 |
| `action_command` | Agent→UE | 下发动作指令 | 工具调用 / LLM 决策 |
| `action_started` | UE→Agent | 动作已接收的 ACK（≤2s） | UE 收到 action_command 后 |
| `action_completed` | UE→Agent | 动作完成回调 | 动作执行完毕 |
| `stop_action` | Agent→UE | 停止当前动作 | 反应层打断 |
| `event_notification` | Agent→Agent | 事件通知（内部路由） | Director 投放事件 |
| `state_report` | UE→Agent | 物理状态权威上报 | 变化超阈值 / 每 15 秒兜底 |
| `agent_registered` | UE→Agent | 机器人上线 | RobotActor BeginPlay |
| `agent_unregistered` | UE→Agent | 机器人下线 | RobotActor EndPlay |
| `heartbeat` | 双向 | 心跳保活 | 每 5 秒 |
| `error` | 双向 | 错误上报 | 异常情况 |
| `capability_registry` | UE→Agent | NPC 能力声明（哪些 cmd 可执行） | UE 连接后 / 能力变更时 |
| `world_kb` | UE→Agent | 世界知识库下发（generated + authored） | UE 连接后（首个 `agent_registered` 之前） |

### 动作生命周期

```mermaid
sequenceDiagram
    participant Agent as MCP (Agent)
    participant UE as Mock UE
    Note over Agent: 工具调用触发
    Agent->>UE: action_command {action_id, cmd, params}
    UE-->>Agent: action_started {action_id, accepted, estimated_duration_sec} (≤2s)
    Note over Agent: 工具收到 ACK 后立即返回<br/>不等 completed
    Note over UE: 执行动作...
    UE->>Agent: action_completed {action_id, result, duration_ms, progress}
    Note over Agent: completed 存入 pendingCompletion 队列<br/>下次 perception 时折入叙事
```

**9 种 cmd**：`MoveTo`/`TurnTo`/`PlayAnimation`/`Speak`/`Emote`/`Wait`/`InteractSmartObject`/`ExecuteComposite`/`Stop`

**error_code 取值**：`ACTION_FAILED` / `STOP_ID_MISMATCH` / `INVALID_MESSAGE` / `UNKNOWN_AGENT` / `INTERNAL_ERROR`

### 超时机制

| 操作 | 超时 | 超时后行为 |
|------|------|------------|
| action_started 等待 (ACK) | 2 秒 | 认为指令丢失，重发或重新决策 |
| action_completed 等待 | `estimated_duration_sec × 1.5`（默认 60s） | 发 stop_action + 重新决策 |
| LLM 调用 | 120 秒 | 返回错误，跳过该轮 |
| 心跳响应 | 15 秒 | 认为断线 |
| 重连尝试 | 3 秒间隔，指数退避到 30 秒 | 持续重试 |

## 数值系统

### 数值归属原则

**谁产生这个数值，谁就是主人，谁负责存储和变更。**

| 数值类别 | 主人 | 变更触发 | UE 需要 | 同步方式 |
|----------|------|----------|---------|----------|
| Agent 内部状态（mood/social_need/emotion） | Agent | LLM 反思 / 交互判断 | ❌ | 不同步 |
| 物理状态（energy/fatigue/joint_wear/health） | UE | 行为消耗 / 充电恢复 | ✅ 主人 | state_report 上报 |
| 关系数值（familiarity/affection） | Agent | 交互后 LLM 更新 | ❌ | 不同步 |
| 空间状态（position/rotation/zone） | UE | 每帧 / Overlap 触发 | ✅ 主人 | perception_update 上报 |
| 任务状态（plan/queue/stack） | Agent | 分层思考产出 | ❌ | 不同步 |

### 物理状态四项

energy / fatigue / joint_wear / health，通过 `state_report` 权威通道上报。delta 阈值：energy/fatigue/health ≥5，joint_wear ≥1。每 15 秒兜底全量上报。

## MCP 工具

所有工具在 `agenttown-mcp/adapters/agenttown/tools/`。15 个工具均以 `agent_id` 为第一参数、`decision_epoch` 为第二个必填参数。Hermes 侧工具名为 `mcp__agenttown__<tool_name>`（双下划线）。

**工具列表由 `capability_registry` 动态驱动**：UE 连接 MCP 后发送 `capability_registry` 声明可执行 cmd，MCP 据此调 `tools.ReconcileTools` 增删工具（`AddTool`/`RemoveTools`）。启动时 seed 内置 9 cmd 默认值（`BuiltinCmdCapabilities`），保证 UE 不发 `capability_registry` 也能跑。per-agent 差异化在 `guardedExecutor.SendAction` 这一咽喉点拦截——查 `CapabilityRegistry.HasCmd(agentID, cmd)`，不通过则拒绝下发。战术层 prompt 中的可用工具列表也按 registry 对 agentID 的有效能力集动态生成（`tacticalToolMeta` 是工具元数据单一来源）。

### 复合行为工具（→ `ExecuteComposite` cmd）

| 工具 | 参数 | 说明 |
|------|------|------|
| `work_assemble` | agent_id, target, duration_min | 工作组装 |
| `patrol_route` | agent_id, route_id | 巡逻路线 |
| `charge_at` | agent_id, station_id, duration_min | 充电 |
| `repair_target` | agent_id, target_agent_id | 维修目标 |
| `social_chat_with` | agent_id, target_agent_id | 社交对话 |
| `rest_idle` | agent_id, duration_min | 休息 |
| `archive_research` | agent_id, duration_min | 档案研究 |

### 原子行为工具

| 工具 | 参数 | cmd |
|------|------|-----|
| `move_to` | agent_id, target | MoveTo |
| `turn_to` | agent_id, target | TurnTo |
| `speak` | agent_id, content, target | Speak |
| `emote` | agent_id, emotion, mode | Emote |
| `interact` | agent_id, object_id, action | InteractSmartObject |
| `wait` | agent_id, duration_sec | Wait |
| `scan_area` | agent_id | 请求即时 perception |
| `stop` | agent_id | Stop |

`duration_min` 内部 ×60 转 `duration_sec`。语义目标（如 `move_to(target="workbench_01")`）由 Mock UE 解析坐标，Agent 不接触坐标。

### 新增工具硬约束

- 命名 `<verb>` 或 `<verb>_<noun>`，全小写下划线
- `agent_id` 为第一参数，`decision_epoch` 为第二个必填参数
- Input/Output struct 带 `json` + `jsonschema` tag
- Output 首字段 `OK bool`
- Handler 第一个返回值传 `nil`，让 SDK 用 Output 填充 content
- 在 `RegisterAll` 注册

## 关键机制

### 启动顺序（硬约束）

**MCP 必须先于 Hermes 启动**。Hermes 启动时连接 MCP 发现工具，MCP 不可用则连接失败后 parked，工具不注册，LLM 只能纯叙述。

正确顺序（`start.sh` 已保证）：
1. 停掉所有旧进程
2. 编译+部署 MCP 二进制到 WSL `~/agenttown-mcp`
3. 启动 MCP → 等 `:8760` + `:9090` 就绪
4. 启动 Hermes → 等 `:8642` 就绪 + MCP 日志出现 `session initialized`
5. 启动 Mock UE → 预检查通过后运行
6. 仿真日志统一写入 `logs/YYYY-MM-DD/sim.log`（MCP 独占，无需合并）

**UE 连接消息序列**（硬约束）：UE 连接 MCP 后按以下顺序首发系统消息：
1. `world_kb`（`agent_id="system"`）— 推送完整世界 KB（generated + authored JSON），MCP 合并+落盘+swap 内存 KB。**必须在首个 `agent_registered` 之前**，确保 worker 启动时捕获新 KB
2. `agent_registered` — 触发 Hermes 会话重置 + worker 启动
3. `capability_registry` — 声明 NPC 能力，MCP 动态增删工具
4. `resync` → `state_report` → `perception_update` …

`world_kb` 仅在启动窗口内（首个 `agent_registered` 之前）接受；之后到达的 `world_kb` 被拒绝并告警（worker goroutine 已持 kb 指针，热替换会竞态）。合并失败保留旧 KB + 不写盘。

### world_kb 自动适配

UE 推送新 `world_kb` 后，MCP 重启即自动适配全链路，无需改任何代码：

- **战略层 prompt 注入 KB**：`generateDailyPlan` 接收 `kb`，`buildStrategicContext(kb, agentID)` 构造【你的角色】+【世界知识】两段——角色段从 `kb.GetAgent(agentID)` 取 `DisplayName`/`Profession`/`Description`/`Personality`；世界知识段复用 `buildKBContext(kb)`（与战术层同源）列出全部 zone/object id。LLM 据此规划当日计划，不会编造 KB 外概念。
- **战术层工具列表动态派生**：`capability_registry` 驱动 `ReconcileTools` 增删工具；`buildTacticalToolEntries` 按 registry 对 agent 的有效能力集生成 prompt 工具列表；`buildTacticalExample(kb)` 从 KB 取首个 zone/object 作示例。新 cmd 由 `registerGenericActionTool` 自动注册通用工具。
- **反应层 cmd 列表动态派生**：`isValidReactionCmd` / `buildReactiveCmdList` 从 registry 派生原子 cmd 集合（排除 `TurnTo`/`PlayMontage`）。
- **工具 jsonschema 描述去硬编码 id**：`MoveToLocationInput.Target` / `InteractInput.TargetObjectID` / `WorkAtWorkbenchInput.AgentID` 等不再写死 `e.g. main_workshop`/`workbench_01`/`"H-01"`，改为引用 `world_kb`，LLM 从 prompt 注入的【世界知识】段获取合法 id。
- **兜底每日计划从 KB 派生**：`buildDefaultDailyPlan(kb)` 用首个 zone 显示名 + 首个 object 显示名组装工作时段；`kb == nil` 时降级为中性表述（不引用"车间"/"装配"/"充电"等当前 KB 专属词）。

**仅启动时适配**：不支持运行时热替换 KB。worker 按值捕获 kb，swap 仅在 worker 启动前发生，当前架构安全。换 KB 流程：UE 推送新 `world_kb` → MCP 重启 → worker 启动时拿新 kb。

### Mock UE Busy 状态

长耗时动作（`ExecuteComposite`）不跳跃时间，设置 `npc.busy_until_min`。感知循环自然推进时间，NPC 留在原位直到时间到达。

- 忙碌期间拒绝破坏性动作：`MoveTo`/`TurnTo`/`InteractSmartObject`/`ExecuteComposite`/`Wait`
- 短动作立即执行 + 发 `action_completed`
- 完成的 busy 动作自动清除，下一次感知通知 LLM

### 断线重连与 Seq 重放补偿

```mermaid
sequenceDiagram
    participant UE as Mock UE
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
- MCP 侧：首次 `agent_registered` 触发 Hermes 会话重置，重连再注册保留会话

### 事件驱动决策与 epoch

- 所有 perception 都更新最新世界缓存，但只有首次感知、动作完成、任务生命周期、关键环境变化、场景事件或物理警戒带变化才调用 Hermes
- 纯时间变化、相同 scan_area、busy progress 普通变化不触发决策
- Hermes 在途时合并触发原因，并只保留最新世界快照
- 每次实际决策生成单调递增 `decision_epoch`；全部 15 个工具必须携带当前 `[decision_context]` 中的 epoch
- guarded executor 在发送 UE 前校验 Agent 已注册、在线、decision_epoch 当前有效且 WebSocket 已连接
- `agent_unregistered` 立即失效当前决策；迟到工具调用被拒绝

### Hermes 会话管理

- `hermes.Client` 通过 `previous_response_id` 链式维护会话
- 每天首次 `agent_registered` 触发 `ResetSession()`，开启新会话
- token 超 80K 时立即断链并保存本地结构化摘要；不再额外调用 LLM 摘要
- 本地摘要只含时间位置、物理状态、当前任务、最近动作和环境事件，不保存 Hermes 叙事
- **上游错误检测**：Hermes 将上游 LLM API 错误（如 HTTP 400 empty tool_calls）包装为 200 响应返回。MCP 检测 `tokens=0` + narrative 以 `HTTP 4`/`HTTP 5` 开头时，清空 `prevResponseID` 断链，返回 `ErrUpstreamError`，下一轮以全新会话开始

### 感知格式化

Mock UE 推送 `perception_update` → MCP 的 `adapters/agenttown/perception/format.go` 转为第一人称叙事 → POST 给 Hermes。格式包括时段（清晨/上午/中午/下午/傍晚）、位置、物理状态、附近物体、pending action_completion 折入叙事。

### stdio vs HTTP 模式

`agenttown-mcp/cmd/agenttown-mcp/main.go` 运行模式由 `--http` flag 切换：
- **HTTP 模式**（`--http :8760`）：Streamable HTTP 在 `/mcp`，健康检查 `/healthz`，状态 `/status`。Hermes 通过此端点发现工具。
- stdio 模式（默认）：本地 MCP 客户端用。

**stdio 模式禁止向 stdout 写日志**，否则污染 MCP 协议流。日志走 `internal/log` 打 stderr。

### Docker 网络拓扑

Hermes 在 Docker 容器中运行，MCP 在 WSL 宿主机。`docker-compose.yml` 中 `extra_hosts: ["host.docker.internal:host-gateway"]` 让容器内解析宿主机 IP。MCP 监听 `0.0.0.0:8760`，Hermes 通过 `http://host.docker.internal:8760/mcp` 连接。

Mock UE 在 Windows 上通过 `ws://localhost:9090/ws` 连接 MCP（WSL2 localhost 转发）。

## 代码规范

- **Go 1.25+**（`go.mod` 声明 `go 1.25.0`）
- 错误包装：`fmt.Errorf("...: %w", err)`
- 包注释和导出符号注释写英文
- 新增 Go package 必须有 `*_test.go`
- 测试命名 `Test<Func>_<Scenario>`；禁止启真实子进程，用 `InMemoryTransport` / mock
- Python 代码用 `asyncio` + `websockets`，不用同步 HTTP 调用 Hermes（MCP 接管）
- 日志走 `logging` 模块，不直接 `print`（调试除外）
- WebSocket 库：Go 用 `github.com/coder/websocket`，Python 用 `websockets`

## 环境配置

### 本地 Windows + WSL 开发（默认）

```bash
cp .env.example .env
# 编辑 .env，填入 HERMES_AGENT_API_KEY（占位值即可）和 VENUS_API_KEY
```

关键环境变量（`.env`）：
- `HERMES_AGENT_API_KEY` — 占位值即可（适配层复用 CLI OAuth，不校验）
- `VENUS_API_KEY` — Venus 后端 API key（`--llm-backend=venus` 时必需）
- `AGENTTOWN_MCP_HTTP` — MCP HTTP 监听（默认 `:8760`）
- `AGENTTOWN_MCP_WS` — MCP WebSocket 监听（默认 `:9090`）

### MCP 启动 flag 速查

| flag | 默认值 | 说明 |
|------|--------|------|
| `--http` | `:8760` | MCP HTTP 监听地址（空=stdio 模式） |
| `--ws` | `:9090` | WebSocket 监听（Mock UE 连接） |
| `--llm-backend` | `hermes` | `hermes` / `venus`，选战略/战术层后端 |
| `--hermes-url` | `http://localhost:8642` | Hermes Gateway URL |
| `--venus-url` | `http://v2.open.venus.oa.com/llmproxy` | Venus 后端 URL |
| `--venus-api-key` | `""` | Venus API key（**必填**，否则 401） |
| `--venus-model` | `qwen3.6-35b-a3b` | Venus 模型 ID |
| `--venus-timeout` | `60s` | Venus 调用超时 |
| `--tactical-timeout` | `60s` | 战术层 LLM 调用超时 |
| `--tactical-stream` | `false` | 战术层流式输出（实验性，默认关） |
| `--ollama-url` | `http://localhost:11434` | Ollama URL（空串=禁用反应层） |
| `--ollama-model` | `qwen2.5:7b-instruct-q4_K_M` | 反应层模型 |
| `--ollama-num-thread` | `16` | Ollama CPU 推理线程数（0=默认 16，-1=让 Ollama 自决）。高核数 CPU 上默认用满所有核反而劣化，实测 96 vCPU EPYC 限制到 16 线程可获得 3x 加速 |
| `--world-kb` | `assets/world_kb.yaml` | 世界 KB 路径（fail-fast 启动加载；UE 推送 world_kb 时也写入此路径） |
| `--world-kb-manifest` | `assets/world_kb.manifest.json` | manifest.json 输出路径（UE 推送 world_kb 时写入；空串=跳过 manifest） |
| `--log-level` | `info` | `debug`/`info`/`warn`/`error` |

### 云开发环境（AnyDev / 远程 Linux）

`start.sh` 是为 Windows+WSL+Docker 设计的，**纯 Linux 环境不能直接跑**。分组件启动：

```bash
# 1. 编译 MCP
cd agenttown-mcp && go build -o ../mcp ./cmd/agenttown-mcp && cd ..

# 2. 拷贝 .env（至少需要 VENUS_API_KEY）
cp .env.example .env  # 填入 VENUS_API_KEY

# 3. 启动 MCP（直连 Venus，跳过 Hermes）
./mcp --llm-backend=venus \
  --http :8760 --ws :9090 \
  --venus-api-key "$VENUS_API_KEY" \
  --log-level debug 2>&1 | tee logs/$(date +%Y-%m-%d)/sim.log

# 4. 另开终端启动 Mock UE
pip install websockets pyyaml
python src/run_day.py

# 5.（可选）反应层需要本地 Ollama
ollama serve &  # 或用 systemd
ollama pull qwen2.5:7b-instruct-q4_K_M
```

**云环境限制**：
- 通常无 Docker-in-Docker → 跑不了 Hermes，必须用 `--llm-backend=venus`
- 无 CodeBuddy CLI OAuth → CodeBuddy 适配层无法用
- 内网工蜂 `git.woa.com` 一般可达，可用 HTTPS clone
- Venus `v2.open.venus.oa.com` 需确认云端网络可达

### stable / dev 目录分离（云开发环境）

云开发环境（AnyDev / 远程 Linux）下，项目 clone 到 `/data/workspace/` 下两个独立目录，用端口隔离同时运行：

| 目录 | 分支 | 用途 | MCP HTTP | MCP WS | Mock UE 连接 | debug 控制台 |
|------|------|------|----------|--------|--------------|--------------|
| `/data/workspace/stable` | `master` | 稳定运行、验证 | `:8760` | `:9090` | `--mcp-ws ws://localhost:9090/ws` | `http://localhost:8760/debug/` |
| `/data/workspace/dev` | `dev-working` | 日常开发、调试 | `:8770` | `:9091` | 默认（`ws://localhost:9091/ws`） | `http://localhost:8770/debug/` |

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

**启动 stable**（终端 1 — MCP，终端 2 — Mock UE）：
```bash
# 终端 1
cd /data/workspace/stable
./mcp --llm-backend=venus --http :8760 --ws :9090 \
  --venus-api-key "$VENUS_API_KEY" --log-level debug

# 终端 2
cd /data/workspace/stable
python3 src/run_day.py --mcp-ws ws://localhost:9090/ws
```

**启动 dev**（终端 3 — MCP，终端 4 — Mock UE）：
```bash
# 终端 3
cd /data/workspace/dev
./mcp --llm-backend=venus --http :8770 --ws :9091 \
  --venus-api-key "$VENUS_API_KEY" --log-level debug

# 终端 4
cd /data/workspace/dev
python3 src/run_day.py   # 默认连 :9091
```

**端口隔离原则**：stable 用 `8760/9090`，dev 用 `8770/9091`，互不干扰，可同时运行各自独立的仿真。日志分别写入 `/data/workspace/{stable,dev}/logs/YYYY-MM-DD/sim.log`。

**本地 Windows 对比**：本地用 `D:\SmartNPC_v3`（dev worktree，`dev-working` 分支）和 `D:\SmartNPC_v3-stable`（stable worktree，`master` 分支）两个 worktree 实现同样的分离，端口约定一致。

### LLM 模型配置（Hermes 后端专用）

Hermes 通过 CodeBuddy CLI 适配层（`src/agenttown/codebuddy_adapter.py`）调用公司模型，
适配层会启动一个独立的 CLI 子进程（`codebuddy --serve --model <name>`），复用 CLI 的
OAuth 认证。模型配置在 `src/agenttown/adapter_config.yaml`：

```yaml
cli_port: 52001                          # CLI 子进程监听端口
model: deepseek-v4-flash-ioa             # 模型 ID（见 `codebuddy --help` 的 --model 参数）
```

**换模型步骤**（仅 `--llm-backend=hermes` 时相关）：
1. 改 `src/agenttown/adapter_config.yaml` 的 `model` 字段
2. 改 `hermes/profiles/h01/config.yaml` 的 `default` / `default_model` 字段（保持一致）
3. 跑 `bash start.sh` 重启

可用模型：`deepseek-v4-flash-ioa` / `deepseek-v4-pro-ioa` / `glm-5.2-internal-ioa` /
`claude-sonnet-5` / `gpt-5.6-sol` / `gemini-3.1-pro` 等（完整列表见 `codebuddy --help`，
实际能否使用取决于账号权限）。

前置条件：CodeBuddy CLI 已登录（在终端跑 `codebuddy` 登录腾讯账号）。

## 文件地图

| 路径 | 说明 |
|------|------|
| `docs/AgentTown_CommProtocol_Values.md` | 通信协议与数值系统设计文档（唯一权威） |
| `docs/AgentTown_Reactive_Layer.md` | 反应层设计文档 |
| `docs/DebugAction_Tool.md` | 联调 Debug 工具 `/debug/action` + `/debug/schedule` 使用文档 |
| `agenttown-mcp/cmd/agenttown-mcp/main.go` | 入口：flag、端口、消息分发、agentContext、worker 循环、debug handler |
| `agenttown-mcp/cmd/agenttown-mcp/strategic.go` | 战略层：每日计划生成 |
| `agenttown-mcp/cmd/agenttown-mcp/tactical.go` | 战术层：goal → action 分解 |
| `agenttown-mcp/cmd/agenttown-mcp/reactive.go` | 反应层纯函数：prompt 构建 + 决策解析 |
| `agenttown-mcp/cmd/agenttown-mcp/reactive_runner.go` | 反应层运行时：Ollama 调用 + WS 副作用 |
| `agenttown-mcp/cmd/agenttown-mcp/capability.go` | NPC 能力注册表：per-agent cmd 能力声明（system 全局默认 + 具体 agent 覆盖） |
| `agenttown-mcp/cmd/agenttown-mcp/debug_ui.go` | `/debug/` 浏览器控制台 + `/debug/kb` JSON 端点 |
| `agenttown-mcp/cmd/agenttown-mcp/web/debug.html` | debug 控制台单页 HTML（单 Action + Schedule 注入双 tab） |
| `agenttown-mcp/pkg/protocol/envelope.go` | Envelope + 12 消息类型 + 9 cmd + error_code 常量 |
| `agenttown-mcp/pkg/protocol/messages.go` | 各消息 payload 结构体 + resync/event_lost/capability_registry |
| `agenttown-mcp/pkg/wsserver/server.go` | WS 服务端：收发信封、seq、send buffer、重放、Call/SendAction |
| `agenttown-mcp/pkg/hermes/client.go` | Hermes HTTP 客户端：会话链、token 阈值自动摘要重置、上游错误检测 |
| `agenttown-mcp/pkg/venus/client.go` | Venus 客户端：OpenAI Chat Completions 协议直连 |
| `agenttown-mcp/pkg/ollama/client.go` | Ollama 客户端：反应层专用，非流式 |
| `agenttown-mcp/pkg/worldkb/loader.go` | world_kb.yaml 加载 + 内存索引 |
| `agenttown-mcp/pkg/worldkb/types.go` | KB/Zone/Object/Agent 权威类型（新 schema） |
| `agenttown-mcp/pkg/worldkb/query.go` | KB 查询：GetPosition/WhichZone/WhichObject/ResolveTarget |
| `agenttown-mcp/pkg/worldkb/schema.go` | merge 输入 JSON schema（GeneratedDoc/AuthoredDoc）+ 受保护字段白名单 |
| `agenttown-mcp/pkg/worldkb/merger.go` | `Merge(gen, auth)` deep merge + `MergeAndWriteBytes`（UE 推送 world_kb 时合并+落盘） |
| `agenttown-mcp/pkg/worldkb/validator.go` | `Validate(kb)` — ID 格式、cross-reference 合法性 |
| `agenttown-mcp/pkg/worldkb/serializer.go` | `WriteYAML`（按 ID 排序，原子替换）+ `WriteManifest`（SHA256 + RFC3339） |
| `agenttown-mcp/adapters/agenttown/tools/registry.go` | 工具注册 + Executor 接口 |
| `agenttown-mcp/adapters/agenttown/tools/composite.go` | 7 个复合行为工具 |
| `agenttown-mcp/adapters/agenttown/tools/atomic.go` | 8 个原子行为工具 |
| `agenttown-mcp/adapters/agenttown/perception/format.go` | 感知 → 自然语言叙事 |
| `agenttown-mcp/internal/log/logger.go` | slog JSON 日志（写 stderr） |
| `assets/world_kb.yaml` | 世界 KB：7 zones / 3 objects / 1 agent（新 schema，locations 已合并进 objects） |
| `assets/world_kb.manifest.json` | merge 产物：源 SHA256 + 时间戳（UE 推送 world_kb 时写入） |
| `src/agenttown/mock_ue.py` | Mock UE：协议常量、NPCState、物理状态、感知循环、动作处理、重连+重放 |
| `src/agenttown/codebuddy_adapter.py` | CodeBuddy CLI OpenAI 适配层（仅 Hermes 后端用） |
| `src/agenttown/adapter_config.yaml` | 适配层配置：CLI 子进程端口 + 模型 ID |
| `src/run_day.py` | Mock UE 启动入口 |
| `hermes/profiles/h01/SKILL.md` | Hermes 工具使用指南 |
| `start.sh` | 一键启动脚本（Windows+WSL+Docker 专用） |
| `scripts/pretty_log.py` | 日志可读化工具（HTML 报告 + 终端渲染） |
| `.env` | 环境变量（VENUS_API_KEY 等，不入库） |

## Git 提交

格式：`<type>(<scope>): <subject>`（祈使句）
- type：`feat` / `fix` / `refactor` / `test` / `docs` / `chore` / `perf`
- scope：`protocol` / `mcp` / `mock-ue` / `hermes` / `skill-md` / `docker` / `config` / `start-script` / `logging` / `codebuddy`
- **提交信息（subject 和 body）使用中文**

用户没明说"commit"时不要主动 commit。

## 里程碑

| Milestone | 状态 | 说明 |
|-----------|------|------|
| M-1 世界快照定义 | ✅ | `docs/我的方案/场景与人物设定.md` |
| M-2 LLM Gateway | ✅ | Hermes Gateway + DeepSeek |
| M-3 Hermes Agent Mind | ✅ | SOUL.md + SKILL.md + profile |
| M-4 Translator | ✅ | MCP 工具注册 |
| M-5 Mock UE Bridge | ✅ | Python async + WebSocket |
| MCP 层 | ✅ | Go agenttown-mcp，15 工具（7 复合+8 原子） |
| 协议重构 Phase 1-7 | ✅ | 7 字段信封、11 消息类型、seq+ACK、物理四态、动作异步生命周期、断线重连+seq 重放 |
| 端到端闭环 | ✅ | 感知→Hermes→工具→Mock UE 全链路验证 |
| 上游错误检测 | ✅ | Hermes 400 错误自动断链重置会话 |
| 三层决策架构 | ✅ | 战略层（每日计划）+ 战术层（任务分解）+ 反应层（Ollama 打断） |
| Venus 后端集成 | ✅ | `--llm-backend=venus` 直连，跳过 Hermes |
| 反应层 P0-P1 | ✅ | 本地 Ollama + zone/physical/periodic 触发 + replan 决策 |
| Debug 工具升级 | ✅ | `/debug/action` + `/debug/schedule`（注入 schedule 调试战术层） |
| 战术层流式输出 | ✅ | `--tactical-stream` flag（默认关，DeepSeek 高峰排队时回退） |

## 当前已知问题（2026-07-29 仿真分析）

按严重度排序：

1. **Hermes token 黑箱**：MCP 发出的战术层 prompt 仅 ~870 token，但 Hermes 实投 ~13k token，差值 ~12k 是 Hermes 服务端隐式拼接的 SOUL/SKILL/示例，MCP 不可见不可控。这是"取缔 Hermes、MCP 直连 Venus"重构的核心动机
2. **战术层输出 schema 漂移**：模型偶尔把 `target` 放顶层而非 `params` 内，或发明不存在的动作（如 `patrol_route`）和路线。"巡检"类目标强诱导漂移。**部分缓解**：战略层 prompt 现注入【你的角色】+【世界知识】段（`buildStrategicContext`），LLM 可见 KB 内合法 zone/object/agent 名，减少编造 KB 外概念；工具 jsonschema 描述已去硬编码 id 示例
3. **战术层队列提前耗尽**：模型给的 action 总时长不够 slot 时长，触发频繁重分解（50 秒内重调 LLM），浪费 token
4. **反应层冷启动超时**：Ollama 模型卸载后首 call >8s 超时，预热后稳定 1.3s
5. **反应层 0% 打断率**：当前 prompt 强偏向 continue/observe，从未触发 act/interrupt/replan（成本中心问题）

## 规划中重构

**取缔 Hermes、MCP 回归无状态**（评估中）：
- MCP 只负责转发感知事件、暴露 tools、与 UE ws 沟通
- 所有带存储的功能移到新 memory 层（dailyPlan/actionQueue/对话历史等）
- MCP 直连 Venus/Ollama
- 详见 memory: `project_smartnpc_v3.md`
