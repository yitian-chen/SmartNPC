# 联调 Debug 工具：/debug/action 手动触发端点

> 本文档说明联调用 debug HTTP 端点 `/debug/action` 的用法。
> 该端点让终端通过 curl 直接触发 MCP 向 UE 发送 action_command，
> 无需改 UE 代码、无需重启 MCP，便于联调时快速验证 UE 侧动作执行。
>
> 对应代码：`agenttown-mcp/cmd/agenttown-mcp/main.go` 的 `handleDebugAction`。

---

## 一、端点信息

| 项 | 值 |
|---|---|
| 路径 | `POST /debug/action` |
| Content-Type | `application/json` |
| 端口 | MCP HTTP 端口（见下方端口对照表） |
| 认证 | 无（仅联调用，生产环境应禁用或加 Bearer 校验） |
| 超时 | 复用 `ws.Call` 内置的 2 秒 ACK 等待 |

### 端口对照表

本项目支持双实例并行运行（stable + dev），端口互不冲突。curl 时请按你启动的实例选对应端口。

| 用途 | stable 实例 | dev 实例 | 说明 |
|---|---|---|---|
| MCP HTTP（本端点） | `8760` | `8770` | `/debug/action` 走这个端口 |
| MCP WebSocket | `9090` | `9091` | UE 连这个收 action_command |
| Hermes | `8642` | `8643` | LLM 推理服务 |
| CodeBuddy Adapter | `8761` | `8771` | 模型适配层 |
| CLI | `52001` | `52002` | Hermes CLI 端口 |
| 二进制名 | `agenttown-mcp.exe` | `agenttown-mcp-dev.exe` | 避免编译时文件锁冲突 |

启动方式：

```bash
# 稳定实例（默认端口 8760，worktree d:/SmartNPC_v3-stable）— 推荐
cd d:/SmartNPC_v3-stable
bash start-debug.sh

# 开发实例（默认端口 8770，本仓库 d:/SmartNPC_v3）
cd d:/SmartNPC_v3
bash start-dev.sh
```

> 下方所有 curl 示例使用稳定实例端口 `8760`。若使用开发实例，请将端口改为 `8770`。

---

## 二、请求格式

```json
{
  "agent_id": "H-01",
  "cmd": "move_to",
  "params": {"target": "workbench_01"}
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `agent_id` | string | 是 | UE 注册的 agent id（当前固定为 `H-01`） |
| `cmd` | string | 是 | 动作名，见下方支持的 cmd 列表 |
| `params` | object | 是 | 动作参数，结构随 cmd 不同 |

---

## 三、支持的 cmd

### 3.1 `move_to` — 移动到目标位置

`params.target` 传 world_kb 中的 zone 或 location id，MCP 会自动解析坐标和 kind。

```bash
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{"agent_id":"H-01","cmd":"move_to","params":{"target":"workbench_01"}}'
```

MCP 内部处理：调 `kb.GetPosition(target)` 解析坐标 → 构造 `{dest, target, kind, speed:"walk"}` → 发 `MoveTo` 命令。

**可用 target id**（来自 `assets/world_kb.yaml`）：

| 类型 | id | 名称 |
|---|---|---|
| zone | `main_workshop` | 主生产车间 |
| zone | `central_plaza` | 中央广场 |
| zone | `charging_station` | 充电站 |
| zone | `rest_area` | 休息厅 |
| location | `workbench_01` | 工作台一号 |
| location | `charging_station_01` | 充电桩一号 |
| location | `rest_bench_01` | 休息长椅 |

### 3.2 `speak` — 说话

```bash
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"speak","params":{"content":"测试一句话","target":""}}'
```

| 参数 | 类型 | 说明 |
|---|---|---|
| `content` | string | 说话内容 |
| `target` | string | 目标 agent_id，空串表示自言自语 |

### 3.3 `interact` — 与智能物体交互

```bash
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"interact","params":{"object_id":"workbench_01","action":"inspect"}}'
```

| 参数 | 类型 | 说明 |
|---|---|---|
| `object_id` | string | 物体 id（见上方 location 表） |
| `action` | string | 动作词，如 `inspect` / `assemble` / `charge` / `rest` |

### 3.4 `wait` — 原地等待

```bash
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"wait","params":{"duration_sec":30}}'
```

| 参数 | 类型 | 说明 |
|---|---|---|
| `duration_sec` | number | 等待秒数 |

### 3.5 复合动作（`charge_at` / `work_assemble` / `archive_research` / `rest_idle`）

这几个 cmd 走 `ExecuteComposite` 协议命令，MCP 会自动在 params 里加 `name` 字段。

```bash
# 充电 30 分钟
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"charge_at","params":{"station_id":"charging_station_01","duration_min":30}}'

# 工作台装配 60 分钟
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"work_assemble","params":{"target":"workbench_01","duration_min":60}}'

# 档案研究 15 分钟
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"archive_research","params":{"duration_min":15}}'

# 休息 10 分钟
curl -X POST http://localhost:8760/debug/action \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","cmd":"rest_idle","params":{"duration_min":10}}'
```

复合动作参数表：

| cmd | 必填参数 | 说明 |
|---|---|---|
| `charge_at` | `station_id`, `duration_min` | 充电站 id + 充电分钟数 |
| `work_assemble` | `target`, `duration_min` | 工作台 id + 装配分钟数 |
| `archive_research` | `duration_min` | 档案研究分钟数 |
| `rest_idle` | `duration_min` | 休息分钟数 |

---

## 四、响应格式

### 成功（HTTP 200）

```json
{
  "ok": true,
  "action_id": "act_3b40e356-e1c",
  "accepted": true,
  "estimated_duration_sec": 120
}
```

| 字段 | 说明 |
|---|---|
| `ok` | 是否成功 |
| `action_id` | MCP 生成的 action id（用于后续 stop_action） |
| `accepted` | UE 是否接受 |
| `estimated_duration_sec` | UE 估计的执行秒数 |

### 错误响应

| HTTP 状态码 | 场景 | 响应体 |
|---|---|---|
| 400 | 请求体非法 / 缺字段 / 未知 cmd / target 解析失败 | `{"ok":false,"error":"..."}` |
| 405 | 非 POST 方法 | `{"ok":false,"error":"method not allowed, use POST"}` |
| 502 | `ws.Call` 失败（ACK 超时或 UE 拒绝） | `{"ok":false,"error":"ws.Call failed: ..."}` |
| 503 | UE 未连接 | `{"ok":false,"error":"no mock ue connected"}` |

---

## 五、联调流程

> 启动服务见上方"端口对照表"。

### 5.1 确认 UE 已连接

```bash
curl http://localhost:8760/status
# 预期：{"ok":true,"ws_connected":true}
```

### 5.2 触发动作

用上方任意 curl 命令触发。MCP 日志会记录：

```
[debug/action] manual trigger agent_id=H-01 cmd=move_to proto_cmd=MoveTo params=map[dest:[21500 8500 0] ...]
[MCP→UE/CMD] cmd=MoveTo action_id=act_xxx agent_id=H-01 params=...
```

### 5.3 观察 UE 侧

UE 应收到 `action_command` envelope 并回 `action_started` ACK，执行完毕后回 `action_completed`。

---

## 六、注意事项

1. **完全旁路**：debug 端点不经过战术层/战略层队列，不影响 NPC 正常决策流程。手动触发的 action 与战术层下发的 action 共享同一个 `currentActionID` 锁——如果战术层正在执行 action，手动触发的 action 会被 UE 拒绝（`accepted:false`），反之亦然。

2. **无认证**：端点对局域网开放，仅适用于联调环境。生产环境应在 `runHTTP` 里加 Bearer token 校验或直接禁用此路由。

3. **move_to 坐标来源**：target id 经 `kb.GetPosition` 解析，坐标和 kind 来自 `assets/world_kb.yaml`。如果 world_kb 没有该 id，返回 400 错误。

4. **复合动作的 name 字段**：`charge_at` / `work_assemble` / `archive_research` / `rest_idle` 这四个 cmd 在协议层统一走 `ExecuteComposite`，MCP 会自动在 params 里注入 `name` 字段告诉 UE 具体执行哪个动作，调用方不需要传 `name`。

5. **ACK 超时**：`ws.Call` 内置 2 秒超时，UE 必须在 2 秒内回 `action_started`，否则 curl 会收到 502 错误。
