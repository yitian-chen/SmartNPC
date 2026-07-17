# CODEBUDDY.md

This file provides guidance to CodeBuddy Code when working with code in this repository.

## 项目定位

AgentTown_v3 — AI NPC 模拟系统。一期单 Agent（H-01 老陈，车间主管机器人），通过 MCP 协议驱动完整的"感知→决策→行动"闭环。UE 桥接协议未定前用 Mock UE 模拟游戏世界。

```
Mock UE (Python, async)            agenttown-mcp (Go, WSL)              Hermes Gateway (Docker)
┌──────────────────┐   WS :9000   ┌──────────────────────────┐  HTTP  ┌────────────────┐
│ perception JSON  │─────────────▶│ wsserver                 │        │ h01-gateway    │
│ tool execution   │◀──tool req──│  → perception.Format()    │        │  :8642         │
│                  │──action resp▶│  → hermes.Client.Send()   │◀─POST──│ /v1/responses  │
│ busy state mgmt  │              │                          │        │ (prev_resp_id  │
└──────────────────┘              │ mcp.Server (10 tools)    │        │  chaining)     │
                                  │  ← /mcp (:8760) ───────Hermes calls tools──▶│        │
                                  └──────────────────────────┘        └────────────────┘
```

| 组件 | 语言 | 路径 | 端口 |
|------|------|------|------|
| Mock UE | Python 3.10+ (asyncio + websockets) | `src/agenttown/mock_ue.py` | — |
| agenttown-mcp | Go 1.25+ (MCP Go SDK) | `agenttown-mcp/` | HTTP `:8760`, WS `:9000` |
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
go test ./pkg/wsserver/ -v -count=1                         # 单个包
go test ./adapters/agenttown/perception/ -run TestFormat_Perception -v  # 单个测试

# 交叉编译 Linux 二进制（部署到 WSL）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/agenttown-mcp-linux ./cmd/agenttown-mcp
wsl cp /mnt/c/Users/yitianchen/AppData/Local/Temp/agenttown-mcp-linux ~/agenttown-mcp
```

### Hermes 集成测试

```bash
# 需要 Hermes 在线 + MCP 在线
go test -tags integration ./pkg/hermes/ -v -count=1 -timeout 120s

# 手动验证 MCP 工具发现
curl -s http://localhost:8760/status   # {"ok":true,"ws_connected":...,"hermes_session":"..."}

# 手动验证 Hermes 能调用 MCP 工具
curl -s -X POST http://localhost:8642/v1/responses \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer agenttown-test-key" \
  -d '{"model":"deepseek-v4-flash","input":"Call the mcp__agenttown__self_check tool."}'
```

### 日志检查

```bash
# Mock UE 日志
ls -t logs/*.log | head -1     # 最新日志文件

# MCP 日志（WSL）
wsl tail -f /tmp/mcp.log
wsl tail -f /tmp/mcp.log | grep "TOOL"          # 只看工具调用
wsl tail -f /tmp/mcp.log | grep "hermes turn"   # 只看 Hermes 响应

# Hermes 容器日志
wsl docker logs -f agenttown-h01
wsl docker logs agenttown-h01 2>&1 | grep -i mcp  # MCP 连接状态
```

## 架构关键点

### 启动顺序（硬约束）

**MCP 必须先于 Hermes 启动**。Hermes 启动时连接 MCP 发现工具，如果 MCP 不可用，连接失败 3 次后会 parked（不再重试），工具不会注册，LLM 只能纯叙述而不调用工具。

正确顺序：`start.sh` 已保证
1. 停掉所有旧进程
2. 启动 MCP → 等 `:8760` + `:9000` 就绪
3. 启动 Hermes → 等 `:8642` 就绪 + MCP 日志出现 `session initialized`
4. 启动 Mock UE → 预检查三项全绿后运行

### MCP 工具注册

所有工具在 `agenttown-mcp/adapters/agenttown/tools/`，一个 domain 一个文件，在 `registry.go` 的 `RegisterAll` 统一挂载。10 个工具：

| 工具 | 文件 | 说明 |
|------|------|------|
| `move_to`, `turn_to` | `movement.go` | 移动/转向 |
| `work_assemble`, `interact_with` | `work.go` | 工作/交互 |
| `charge_at`, `self_check` | `maintenance.go` | 充电/自检 |
| `speak`, `emote` | `social.go` | 说话/表情 |
| `wait` | `state.go` | 等待 |
| `update_plan` | `planning.go` | 更新计划 |

新增工具的硬约束（参考 SmartNPC 模式）：
- 命名 `<verb>` 或 `<verb>_<noun>`，全小写下划线
- Input/Output struct 必须带 `json` + `jsonschema` tag
- Output 首字段是 `OK bool`
- Handler 第一个返回值传 `nil`，让 SDK 用 Output 自动填充 content
- 在 `RegisterAll` 注册 + 配 `InMemoryTransport` 端到端测试

Hermes 注册工具名为 `mcp__agenttown__<tool_name>`（双下划线分隔）。SKILL.md 中用此名称引用工具。

### Mock UE Busy 状态

长耗时动作（`work_assemble`, `charge_at`）不跳跃时间，而是设置 `npc.busy_until_min` 状态。感知循环自然推进时间，NPC 留在原位工作直到时间到达。

- 忙碌期间拒绝破坏性动作（`move_to`, `interact_with` 等）
- 非破坏性动作（`self_check`, `speak`, `emote`）允许执行
- 完成时自动清除 busy，下一次感知通知 LLM
- 短动作（`move_to` 2min, `self_check` 1min）直接推进少量时间

### WebSocket 协议

三类帧（`pkg/wsserver/protocol.go`）：
- `request`（MCP → Mock UE，工具调用，带 `id`）
- `response`（Mock UE → MCP，工具结果，关联 `id`）
- `event`（Mock UE → MCP，推送感知，无 `id`）

事件类型：`perception_update` / `day_started` / `day_ended`

### Hermes Session 管理

MCP 的 `pkg/hermes/client.go` 拥有 `previous_response_id` 链式会话：
- 每天 `day_started` 事件触发 `ResetSession()`，开启新会话
- 后续 `perception_update` 通过 `previous_response_id` 链接，保持上下文
- 一天内的所有感知 + 工具调用共享一个 Hermes conversation

### 感知格式化

Mock UE 推送 JSON 感知快照 → MCP 的 `adapters/agenttown/perception/format.go` 转为自然语言 → POST 给 Hermes。格式包括时段（清晨/上午/中午/下午/傍晚）、位置、电池、附近物体、场景事件。

### stdio vs HTTP 模式

`agenttown-mcp/cmd/agenttown-mcp/main.go` 运行模式由 `--http` flag 切换：
- **HTTP 模式**（`--http :8760`）：Streamable HTTP 在 `/mcp`，健康检查 `/healthz`，状态 `/status`。Hermes 通过此端点发现工具。
- stdio 模式（默认）：`server.Run(ctx, &mcp.StdioTransport{})`，本地 MCP 客户端用。

**stdio 模式禁止向 stdout 写日志**，否则污染 MCP 协议流。日志统一用 `internal/log` 走 stderr。

### Docker 网络拓扑

Hermes 运行在 Docker 容器中，MCP 运行在 WSL 宿主机上。`docker-compose.yml` 中 `extra_hosts: ["host.docker.internal:host-gateway"]` 让容器内 `host.docker.internal` 解析到 WSL 宿主机 IP。MCP 监听 `0.0.0.0:8760`，Hermes 通过 `http://host.docker.internal:8760/mcp` 连接。

Mock UE 在 Windows 上运行，通过 `ws://localhost:9000/ws` 连接 MCP（WSL2 localhost 转发）。

## 代码规范

- **Go 1.25+**（`go.mod` 声明 `go 1.25.0`）
- 错误包装：`fmt.Errorf("...: %w", err)`
- 包注释和导出符号注释写英文
- 新增 Go package 必须有 `*_test.go`
- 测试命名 `Test<Func>_<Scenario>`；禁止启真实子进程，用 `InMemoryTransport` / mock
- Python 代码用 `asyncio` + `websockets`，不用同步 HTTP 调用 Hermes（MCP 接管）
- 日志走 `logging` 模块，不直接 `print`（调试除外）

## 环境配置

```bash
cp .env.example .env
# 编辑 .env，填入 HERMES_AGENT_API_KEY（DeepSeek API key）
```

关键环境变量：
- `HERMES_AGENT_API_KEY` — DeepSeek API 密钥（Hermes LLM 调用用）
- `AGENTTOWN_MCP_HTTP` — MCP HTTP 监听地址（默认 `:8760`）
- `AGENTTOWN_MCP_WS` — MCP WebSocket 监听地址（默认 `:9000`）

## Git 提交

格式：`<type>(<scope>): <subject>`（祈使句、中文或英文均可）
- type：`feat` / `fix` / `refactor` / `test` / `docs` / `chore` / `perf`
- scope：`mcp` / `mock-ue` / `hermes` / `skill-md` / `docker` / `config`

用户没明说"commit"时不要主动 commit。

## 里程碑

| Milestone | 状态 | 说明 |
|-----------|------|------|
| M-1 世界快照定义 | ✅ | `docs/我的方案/场景与人物设定.md` |
| M-2 LLM Gateway | ✅ | Hermes Gateway + DeepSeek |
| M-3 Hermes Agent Mind | ✅ | SOUL.md + SKILL.md + profile |
| M-4 Translator | ✅ | MCP 工具注册 |
| M-5 Mock UE Bridge | ✅ | Python async + WebSocket |
| MCP 层 | ✅ | Go agenttown-mcp，10 工具 |
| 端到端闭环 | ✅ | 感知→Hermes→工具→Mock UE 全链路验证 |
