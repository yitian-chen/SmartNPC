# CODEBUDDY.md

This file provides guidance to CodeBuddy Code when working with code in this repository.

## 项目定位

AgentTown_v3 — AI NPC 模拟系统。一期单 Agent（H-01 老陈，车间主管机器人），通过 MCP 协议驱动完整的"感知→决策→行动"闭环。通信协议按 `docs/AgentTown_CommProtocol_Values.md` 实现，Mock UE 模拟 UE5 游戏世界。

```
Mock UE (Python, async)            agenttown-mcp (Go, WSL)              Hermes Gateway (Docker)
┌──────────────────────┐   WS :9090  ┌───────────────────────────┐  HTTP  ┌────────────────┐
│ 7-field Envelope     │────────────▶│ wsserver (WS Server)      │        │ h01-gateway    │
│  seq atomic          │◀──resync───│                            │        │  :8642         │
│  actor_registered    │──replay───▶│ MessageHandler dispatch    │◀─POST──│ /v1/responses  │
│  perception_update   │            │  → perception.Format()     │        │ (prev_resp_id  │
│  state_report        │            │  → hermes.Client.Send()    │        │  chaining)     │
│  action_(started/    │            │                            │        │                │
│    completed)        │            │ mcp.Server (15 tools)      │        │                │
│  resync (Phase 7)    │            │  ← /mcp (:8760) ────Hermes calls tools──▶│                │
│  heartbeat 5s        │            │                            │        │                │
│  busy state mgmt     │            │ send buffer (200条/60s)     │        │                │
│  reconnect loop 3-30s│            │ lastReceivedSeq 跟踪       │        │                │
└──────────────────────┘            └───────────────────────────┘        └────────────────┘
```

| 组件 | 语言 | 路径 | 端口 |
|------|------|------|------|
| Mock UE | Python 3.10+ (asyncio + websockets) | `src/agenttown/mock_ue.py` | — |
| agenttown-mcp | Go 1.25+ (MCP Go SDK) | `agenttown-mcp/` | HTTP `:8760`, WS `:9090` |
| Hermes Gateway | Docker (hermes-agent:latest) | `docker/docker-compose.yml` | `:8642` |
| Hermes Profile | YAML + MD | `hermes/profiles/h01/` | — |

## 常用命令

### 一键启动（全部重启）

```bash
bash start.sh                    # 全部重启 + 完整一天 (06:00-22:00)
bash start.sh --quick            # 快速测试 (06:00-10:00, 600x 加速)
bash start.sh --start 6 --end 12 --speed 600 --interval 15  # 自定义参数
bash start.sh --scenario assets/scenarios_sample.yaml        # 带场景注入
```

`start.sh` 执行顺序：**先停全部 → 启动 MCP → 启动 Hermes → 启动 Mock UE**，每步健康检查通过才继续。

### Go 构建 / 测试

```bash
cd agenttown-mcp

go build ./...                                              # 编译检查
go test ./...                                               # 全部测试
go test ./pkg/wsserver/ -v -count=1                         # WS 缓冲/重放测试
go test ./pkg/protocol/ -v -count=1                         # 协议序列化测试
go test ./adapters/agenttown/perception/ -v -count=1        # 感知格式化测试

# 交叉编译 Linux 二进制（部署到 WSL）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agenttown-mcp-linux ./cmd/agenttown-mcp
wsl cp /mnt/c/Users/yitianchen/AppData/Local/Temp/agenttown-mcp-linux ~/agenttown-mcp
```

### Hermes 集成测试

```bash
# 需要 Hermes 在线 + MCP 在线
go test -tags integration ./pkg/hermes/ -v -count=1 -timeout 120s

# 手动验证 MCP 工具发现（JSON-RPC 方式）
# 注：需通过 MCP SDK 的标准工具发现协议，非直接 curl

# 手动验证 Hermes 能调用 MCP 工具
curl -s -X POST http://localhost:8642/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer agenttown-test-key" \
  -d '{"model":"deepseek-v4-flash","input":"调用 mcp__agenttown__move_to 工具，移动到 main_workshop。"}'
```

### 日志检查

```bash
# Mock UE 日志
ls -t logs/*.log | head -1     # 最新日志文件

# MCP 日志（WSL 中）
wsl tail -f /tmp/mcp.log
wsl tail -f /tmp/mcp.log | grep "TOOL"             # 只看工具调用
wsl tail -f /tmp/mcp.log | grep "hermes turn"      # 只看 Hermes 响应
wsl tail -f /tmp/mcp.log | grep "agent_registered" # 注册/重连

# Hermes 容器日志
wsl docker logs -f agenttown-h01
wsl docker logs agenttown-h01 2>&1 | grep -i mcp  # MCP 连接状态
```

## 架构关键点

### 启动顺序（硬约束）

**MCP 必须先于 Hermes 启动**。Hermes 启动时连接 MCP 发现工具，MCP 不可用则连接失败后 parked，工具不注册，LLM 只能纯叙述。

正确顺序（`start.sh` 已保证）：
1. 停掉所有旧进程
2. 启动 MCP → 等 `:8760` + `:9090` 就绪
3. 启动 Hermes → 等 `:8642` 就绪 + MCP 日志出现 `session initialized`
4. 启动 Mock UE → 预检查通过后运行

### WebSocket 通信协议（v1.0）

所有消息使用 7 字段信封（`pkg/protocol/envelope.go`）：
```go
type Envelope struct {
    Version   string          `json:"version"`    // "1.0"
    MsgID     string          `json:"msg_id"`     // UUID
    Seq       int64           `json:"seq"`        // per-sender monotonic
    Timestamp int64           `json:"timestamp"`  // Unix ms
    Type      string          `json:"type"`
    AgentID   string          `json:"agent_id"`
    Payload   json.RawMessage `json:"payload"`
}
```

**11 种消息类型**（`Type*` 常量）：
| 类型 | 方向 | 说明 |
|------|------|------|
| `perception_update` | UE→Agent | 空间+环境感知 δ |
| `action_command` | Agent→UE | 指令（MoveTo/TurnTo/Speak/…） |
| `action_started` | UE→Agent | ACK（≤2s，含 estimated_duration_sec） |
| `action_completed` | UE→Agent | 动作完成回调 |
| `stop_action` | Agent→UE | 中断当前动作 |
| `event_notification` | Agent→Agent | 导演注入事件 |
| `state_report` | UE→Agent | 四维物理状态权威通道 |
| `agent_registered` | UE→Agent | 上线注册（触发会话重置/恢复） |
| `agent_unregistered` | UE→Agent | 下线注销 |
| `heartbeat` | 双向 | 5s 保活（agent_id="system"） |
| `error` | 双向 | 错误回报 |

**9 种 action_command cmd**：`MoveTo`/`TurnTo`/`PlayAnimation`/`Speak`/`Emote`/`Wait`/`InteractSmartObject`/`ExecuteComposite`/`Stop`

**动作生命周期**：`action_command` → `action_started`（ACK，≤2s）→ `action_completed`。MCP 工具在收到 ACK 后立即返回，不等 completed；completed 后一次 perception 时折入叙事回传 Hermes。

**坐标系统**：UE5 厘米(cm)，position=[X,Y,Z]，rotation=[Pitch,Yaw,Roll] 度。所有旧小坐标 ×100 重缩放。

**物理状态四项**：energy / fatigue / joint_wear / health，通过 `state_report` 权威上报。delta 阈值：energy/fatigue/health ≥5，joint_wear ≥1。

### MCP 工具注册

所有工具在 `agenttown-mcp/adapters/agenttown/tools/`，15 个工具全部以 `agent_id` 为第一参数：

**复合行为工具**（→ `ExecuteComposite` cmd）：
| 工具 | 参数 | 说明 |
|------|------|------|
| `work_assemble` | agent_id, target, duration_min | 工作组装 |
| `patrol_route` | agent_id, route_id | 巡逻路线 |
| `charge_at` | agent_id, station_id, duration_min | 充电 |
| `repair_target` | agent_id, target_agent_id | 维修目标 |
| `social_chat_with` | agent_id, target_agent_id | 社交对话 |
| `rest_idle` | agent_id, duration_min | 休息 |
| `archive_research` | agent_id, duration_min | 档案研究 |

**原子行为工具**：
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

`duration_min` 参数内部 ×60 转 `duration_sec`。Hermes 侧工具名为 `mcp__agenttown__<tool_name>`（双下划线）。

新增工具硬约束：
- 命名 `<verb>` 或 `<verb>_<noun>`，全小写下划线
- `agent_id` 为第一参数
- Input/Output struct 带 `json` + `jsonschema` tag
- Output 首字段 `OK bool`
- Handler 第一个返回值传 `nil`，让 SDK 用 Output 填充 content
- 在 `RegisterAll` 注册

### Mock UE Busy 状态

长耗时动作（`ExecuteComposite`）不跳跃时间，设置 `npc.busy_until_min`。感知循环自然推进时间，NPC 留在原位直到时间到达。

- 忙碌期间拒绝破坏性动作：`MoveTo`/`TurnTo`/`InteractSmartObject`/`ExecuteComposite`/`Wait`
- 短动作（`MoveTo`/`InteractSmartObject`/`Wait` 等）立即执行 + 发 `action_completed`
- 完成的 busy 动作自动清除，下一次感知通知 LLM

### 断线重连与 Seq 重放补偿（Phase 7）

双方各维护发送缓冲队列（最近 200 条 / 60 秒，仅离散消息），重连后交换 `resync{last_received_seq}` 并按 seq 重放离散消息。连续状态（position/physical_state）不重放，以重连后最新快照为准。缓冲滚动丢失则发 `event_lost` 告警。

Mock UE 维护重连循环（3s→30s 指数退避），重连后重发 `agent_registered`。MCP 侧首次 `agent_registered` 触发会话重置，重连再注册保留会话（§4.2）。

### 感知格式化

Mock UE 推送 `perception_update` → MCP 的 `adapters/agenttown/perception/format.go` 转为自然语言 → POST 给 Hermes。格式包括时段（清晨/上午/中午/下午/傍晚）、位置、物理状态、附近物体、场景事件、pending action_completion 折入叙事。

### stdio vs HTTP 模式

`agenttown-mcp/cmd/agenttown-mcp/main.go` 运行模式由 `--http` flag 切换：
- **HTTP 模式**（`--http :8760`）：Streamable HTTP 在 `/mcp`，健康检查 `/healthz`，状态 `/status`。Hermes 通过此端点发现工具。
- stdio 模式（默认）：`server.Run(ctx, &mcp.StdioTransport{})`，本地 MCP 客户端用。

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

```bash
cp .env.example .env
# 编辑 .env，填入 HERMES_AGENT_API_KEY（DeepSeek API key）
```

关键环境变量：
- `HERMES_AGENT_API_KEY` — DeepSeek API 密钥
- `AGENTTOWN_MCP_HTTP` — MCP HTTP 监听（默认 `:8760`）
- `AGENTTOWN_MCP_WS` — MCP WebSocket 监听（默认 `:9090`）

## 文件地图

| 路径 | 说明 |
|------|------|
| `agenttown-mcp/pkg/protocol/envelope.go` | Envelope + 11 消息类型 + 9 cmd + error_code 常量 |
| `agenttown-mcp/pkg/protocol/messages.go` | 各消息 payload 结构体 + resync/event_lost |
| `agenttown-mcp/pkg/wsserver/server.go` | WS 服务端：收发信封、seq、send buffer、重放、Call/SendAction |
| `agenttown-mcp/pkg/wsserver/protocol.go` | 薄封装，重导出 protocol 包常量 |
| `agenttown-mcp/pkg/wsserver/server_test.go` | 缓冲淘汰/重放/rollover 测试 |
| `agenttown-mcp/pkg/hermes/client.go` | Hermes HTTP 客户端：会话链、token 阈值自动摘要重置 |
| `agenttown-mcp/adapters/agenttown/tools/registry.go` | 工具注册 + Executor 接口 |
| `agenttown-mcp/adapters/agenttown/tools/composite.go` | 7 个复合行为工具 |
| `agenttown-mcp/adapters/agenttown/tools/atomic.go` | 8 个原子行为工具 |
| `agenttown-mcp/adapters/agenttown/perception/format.go` | 感知 → 自然语言叙事 |
| `agenttown-mcp/cmd/agenttown-mcp/main.go` | 入口：端口配置、消息分发、agentContext |
| `agenttown-mcp/pkg/transport/http.go` | Streamable HTTP 传输 |
| `src/agenttown/mock_ue.py` | Mock UE：协议常量、NPCState、物理状态、感知循环、动作处理、重连+重放 |
| `src/run_day.py` | Mock UE 启动入口 |
| `hermes/profiles/h01/SKILL.md` | Hermes 工具使用指南（复合+原子行为） |
| `start.sh` | 一键启动脚本 |
| `scripts/test_phase7_reconnect.py` | Phase 7 端到端重连验证 |

## Git 提交

格式：`<type>(<scope>): <subject>`（祈使句）
- type：`feat` / `fix` / `refactor` / `test` / `docs` / `chore` / `perf`
- scope：`protocol` / `mcp` / `mock-ue` / `hermes` / `skill-md` / `docker` / `config`
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
