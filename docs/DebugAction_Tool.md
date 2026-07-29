# 联调 Debug 工具：/debug/action 手动触发端点

> 本文档说明联调用 debug HTTP 端点 `/debug/action` 的用法。
> 该端点让终端通过 curl 直接触发 MCP 向 UE 发送 action_command，
> 无需改 UE 代码、无需重启 MCP，便于联调时快速验证 UE 侧动作执行。
>
> 对应代码：`agenttown-mcp/cmd/agenttown-mcp/main.go` 的 `handleDebugAction`。

## 浏览器控制台

除了下方的 curl 用法，还可以直接打开浏览器访问 MCP HTTP 端口的 `/debug/` 路径：

```
http://localhost:8760/debug/      # stable 实例
http://localhost:8770/debug/      # dev 实例
```

网页提供：

- cmd 下拉选择（8 种动作）
- 按 cmd 动态切换的 params 表单：
  - `move_to` 有"输入模式"下拉，可在 **target id**（从 world_kb 自动加载下拉）和 **直接坐标**（手动填 x/y/z UE5 厘米）之间切换
  - `interact` 的 object_id 从 world_kb 加载，action 选项随 object_id 变化
- **force 复选框**（默认勾选）：先 stop 当前动作再发手动 action，解决战术层 idle wait 占用导致的 busy 拒绝。取消勾选可测试 busy 拒绝路径。
- 一键发送 + 响应展示（HTTP 状态码、耗时、action_id、estimated_duration）
- 等价 curl 命令预览（可折叠）
- 历史记录（最近 20 条，点击回填表单）
- 右上角 UE 连接状态徽章（每 5s 自动刷新）

对应代码：`agenttown-mcp/cmd/agenttown-mcp/debug_ui.go` + `web/debug.html`（`//go:embed` 打包进二进制，无外部依赖）。

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

### PowerShell 用户须知（重要）

**Windows PowerShell 下 `curl` 是 `Invoke-WebRequest` 的别名**，不是真 curl。两者参数风格完全不同，直接复制 bash 风格的 curl 命令会报错：

- `-H "Content-Type: ..."` → 报 "无法将 System.String 转换为 IDictionary"（`-H` 被绑定到 `-Headers`，需要哈希表）
- `-d '...'` → 不存在此参数

**解决方法 A — 用真 curl（`curl.exe` + `\"` 转义，推荐）**：

加 `.exe` 后缀绕过别名。PowerShell 单引号字符串里的 `"` 传给 native exe 时会被吞掉，导致 JSON 变成 `{agent_id:...}` 解析失败。**必须用 `\"` 转义**（PowerShell 单引号不解析反斜杠，整串原样传给 curl，curl 自己把 `\"` 解析成 `"`）：

```powershell
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"cmd\":\"move_to\",\"params\":{\"target\":\"workbench_01\"}}'
```

**解决方法 B — 用 PowerShell 原生 `Invoke-RestMethod`**：

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:8760/debug/action -ContentType "application/json" -Body '{"agent_id":"H-01","cmd":"move_to","params":{"target":"workbench_01"}}'
```

> 下方所有 curl 示例按 **bash 语法**给出（`-d '{"key":"value"}'`，无需转义）。PowerShell 用户请按上方规则转换：方法 A 在每个 `"` 前加 `\`，或直接用方法 B。

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

支持两种输入模式：

**模式 A：传 target id（走 kb 解析）**

`params.target` 传 world_kb 中的 zone 或 location id，MCP 会自动解析坐标和 kind。

```bash
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"cmd\":\"move_to\",\"params\":{\"target\":\"workbench_01\"}}'
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

**模式 B：直接传坐标（跳过 kb 解析）**

`params.dest` 传 `[x, y, z]` 数组（UE5 厘米），MCP 直接构造 `{dest, kind:"coord", speed:"walk"}` 下发，不查 world_kb。适用于临时调试未在 kb 中注册的位置。

```bash
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"cmd\":\"move_to\",\"params\":{\"dest\":[15000,11000,0]}}'
```

可选附带 `target` 字段作为日志标签（不影响解析）：`{"dest":[15000,11000,0],"target":"custom_spot"}`。

**两种模式都没有 → 报错**：`move_to requires params.dest ([x,y,z]) or params.target (kb id)`

**dest 校验规则**：
- 必须是长度为 3 的数组，元素为数字（`float64` / `int` / 数字字符串均可）
- 长度不对 → `dest must have exactly 3 elements [x, y, z], got N`
- 元素非数字 → `dest[i] (xxx): not a number`

**浏览器 UI**：网页控制台的 move_to 表单顶部有"输入模式"下拉，切换 `target id` / `直接坐标`，对应字段自动显示/隐藏。

### 3.2 `force` 字段 — 解决 busy 拒绝（默认 true）

**问题背景**：战术层在队列空且无 goal 时会发 60 秒 `wait` 避免忙循环（`sendIdleWait`），这会让 UE 一直处于 busy 状态。手动 debug 调用 `move_to` 等破坏性命令时会被 UE 的 busy guard 拒绝：

```
HTTP 502 {"ok":false,"error":"ws.Call failed: action rejected: busy with Wait (1 game-min remaining)"}
```

**force 模式**（默认开启）：MCP 在发手动 action 前自动做两件事：

1. 设置 `agentContext.debugOverride=true`，暂停战术层 worker dispatch（防止 stop 后 worker 立刻补一个新 idle wait 重新占用）
2. 对当前 `currentActionID` 发 `stop_action`，清掉 UE 的 busy 状态
3. 等 100ms 让 UE 处理 stop（fire-and-forget），然后发手动 action
4. defer 清除 `debugOverride` 并 signal worker 恢复正常 dispatch

```bash
# 默认 force=true，无需显式传
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"cmd\":\"move_to\",\"params\":{\"dest\":[15000,11000,0]}}'

# 显式关闭 force（测试 busy 拒绝路径）
curl.exe -X POST http://localhost:8760/debug/action -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"cmd\":\"move_to\",\"params\":{\"dest\":[15000,11000,0]},\"force\":false}'
```

**日志**：force 模式下会记录 `stopped` 字段（被 stop 的 action_id）：

```
[debug/action] manual trigger agent_id=H-01 cmd=move_to proto_cmd=MoveTo params=... force=true stopped=act_xxxxx
```

**注意**：force 模式要求 `lookupAgent` 能找到对应 agent。如果 agent 未注册（UE 从未连接），force 退化为直发模式（无 stop，可能被 busy 拒）。

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

1. **完全旁路 + force 默认开启**：debug 端点不经过战术层/战略层队列，不影响 NPC 正常决策流程。默认 `force=true` 会先 stop 战术层正在执行的 idle wait 再发手动 action，避免 busy 拒绝。设 `force:false` 可测试 busy 拒绝路径——此时若战术层正在执行 action，手动触发的 action 会被 UE 拒绝（`accepted:false`）。

2. **无认证**：端点对局域网开放，仅适用于联调环境。生产环境应在 `runHTTP` 里加 Bearer token 校验或直接禁用此路由。

3. **move_to 坐标来源**：target id 经 `kb.GetPosition` 解析，坐标和 kind 来自 `assets/world_kb.yaml`。如果 world_kb 没有该 id，返回 400 错误。

4. **复合动作的 name 字段**：`charge_at` / `work_assemble` / `archive_research` / `rest_idle` 这四个 cmd 在协议层统一走 `ExecuteComposite`，MCP 会自动在 params 里注入 `name` 字段告诉 UE 具体执行哪个动作，调用方不需要传 `name`。

5. **ACK 超时**：`ws.Call` 内置 2 秒超时，UE 必须在 2 秒内回 `action_started`，否则 curl 会收到 502 错误。

6. **WSL 端口幽灵排查（curl 返回 `404 page not found` 但 `/status` 正常时）**：

   如果曾经在 WSL 里跑过 MCP，WSL2 的 `wslrelay.exe` 会把 WSL 内监听的端口镜像到 Windows 的 `[::1]:<port>`（IPv6 localhost）。WSL 里 MCP 停了但 `wslrelay` 不会自动退出，继续占着 `[::1]:8760`。Windows 上 `localhost` 解析优先返回 `[::1]`，curl 会命中 `wslrelay` 而非真正的 MCP，`wslrelay` 对任何 HTTP 请求返回 `404 page not found`。

   `start-debug.sh` 的 `--stop` 已修复（`kill_port_listeners` 会杀掉端口上所有监听者，含 `[::1]` 上的 wslrelay），但若手动启动 MCP 后仍遇 404，检查并清理：

   ```powershell
   # 查看 8760 上所有监听者
   Get-NetTCPConnection -LocalPort 8760 -State Listen | Select-Object LocalAddress, OwningProcess
   # 如果 [::1] 上有非 MCP 进程（wslrelay），杀掉
   Stop-Process -Id <PID> -Force
   ```

   或者直接 `bash start-debug.sh --stop` 后重启即可。

---

# 联调 Debug 工具：/debug/schedule 注入 schedule 端点

> 本文档说明联调用 debug HTTP 端点 `/debug/schedule` 的用法。
> 该端点让终端通过 curl 给战术层注入一条单行 schedule（如
> `07:00-11:00: 车间装配作业`），战术层立即分解成 3-5 个 action 入队下发，
> 用于调试战术层的分解 + 下发全链路。
>
> 对应代码：`agenttown-mcp/cmd/agenttown-mcp/main.go` 的 `handleDebugSchedule`。

## 与 /debug/action 的区别

| 端点 | 用途 | 是否走战术层 |
|---|---|---|
| `POST /debug/action` | 直接发单个 action_command 到 UE | 否，绕过战术层 |
| `POST /debug/schedule` | 注入 schedule，战术层 LLM 分解成 action 序列入队 | 是，走完整战术层分解流程 |

`/debug/schedule` 适合调试战术层 prompt、LLM 分解质量、action 映射、队列下发全链路；`/debug/action` 适合单独验证某个 action 在 UE 侧的执行。

## 浏览器控制台

打开 `/debug/` 页面后，左上角表单顶部有两个 tab：

- **单 Action**：原有的 `/debug/action` 表单
- **Schedule 注入**：新增的 schedule 表单

Schedule tab 提供：

- `agent_id` 输入框（默认 H-01）
- `schedule` 文本框（placeholder `07:00-11:00: 车间装配作业`）
- **force 复选框**（默认勾选）：先 stop 当前动作再分解
- "分解并下发"按钮 + 等价 curl 预览

提交后响应展示在右侧（与单 Action tab 共用响应区 + 历史区）。历史记录区分类型显示：schedule 类型显示 `schedule <文本>`，action 类型显示 `<cmd> <params>`。点击历史项自动切到对应 tab 并回填表单。

## 一、端点信息

| 项 | 值 |
|---|---|
| 路径 | `POST /debug/schedule` |
| Content-Type | `application/json` |
| 端口 | 与 `/debug/action` 相同（stable 8760 / dev 8770） |
| 认证 | 无（仅联调用） |
| 超时 | 战术层 LLM 调用硬超时 60s（`tacticalCallTimeout`） |

## 二、请求格式

```json
{
  "agent_id": "H-01",
  "schedule": "07:00-11:00: 车间装配作业",
  "force": true
}
```

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `agent_id` | string | 是 | UE 注册的 agent id（当前固定 H-01） |
| `schedule` | string | 是 | 单行 `HH:MM-HH:MM: 目标描述`，多行会被拒 |
| `force` | bool | 否 | 默认 true：先 stop 当前 action 再分解；false 时不中断 |

**schedule 格式要求**：
- 必须是单行（多行返回 400 `schedule must be a single line`）
- 时段格式 `HH:MM-HH:MM`（如 `07:00-11:00`），起止时间用 `-` 分隔
- 时段后跟 `: `（冒号空格），再跟目标描述
- 完整示例：`07:00-11:00: 车间装配作业`

## 三、curl 示例

```bash
# 注入一条 schedule，战术层立即分解并下发
curl -X POST http://localhost:8760/debug/schedule \
  -H "Content-Type: application/json" \
  -d '{"agent_id":"H-01","schedule":"07:00-11:00: 车间装配作业","force":true}'
```

PowerShell 用户用 `curl.exe` 并转义 `"`（规则同 `/debug/action`）：

```powershell
curl.exe -X POST http://localhost:8760/debug/schedule -H "Content-Type: application/json" -d '{\"agent_id\":\"H-01\",\"schedule\":\"07:00-11:00: 车间装配作业\",\"force\":true}'
```

## 四、响应格式

### 成功（HTTP 200）

```json
{
  "ok": true,
  "slot": "07:00-11:00",
  "goal": "车间装配作业",
  "actions": [
    {"action":"move_to","params":{"target":"main_workshop"}},
    {"action":"work_assemble","params":{"target":"workbench_01","duration_min":240}}
  ],
  "queue_len": 2,
  "inner_thought": "先去车间再开始装配",
  "dispatched": false,
  "warning": ""
}
```

| 字段 | 说明 |
|---|---|
| `ok` | 是否分解成功 |
| `slot` | 解析出的时段（如 `07:00-11:00`） |
| `goal` | 解析出的目标描述 |
| `actions` | 战术层分解出的 action 列表（3-5 个） |
| `queue_len` | 入队 action 数量 |
| `inner_thought` | 战术层 LLM 的内心独白（第一行 NDJSON） |
| `dispatched` | 固定 `false`：actions 已入 `actionQueue`，实际下发由 worker 异步完成 |
| `warning` | agent 未上报感知时非空，提示 zone/timeOfDay/physical 为空 |

**关于 `dispatched: false`**：handler 只负责"分解 + 入队 + signal 唤醒 worker"，实际下发由 worker 的 `popAndSendQueueAction` 异步完成（含 `currentActionID` 守卫 + UE busy 重试）。这样避免 handler 在 UE stop 未处理完时下发被拒。下发进度看日志 `[战术层] 下发 action`。

### 错误响应

| HTTP 状态码 | 场景 | 响应体 |
|---|---|---|
| 400 | 请求体非法 / 缺字段 / schedule 多行 / slot 格式非法 | `{"ok":false,"error":"..."}` |
| 404 | agent_id 未注册 | `{"ok":false,"error":"unknown agent_id: ..."}` |
| 405 | 非 POST 方法 | `{"ok":false,"error":"method not allowed, use POST"}` |
| 409 | 另一个 replan/debug 正在进行 | `{"ok":false,"error":"another replan/debug in progress, retry later"}` |
| 502 | 战术层 LLM 调用失败或超时 | `{"ok":false,"error":"decompose failed: ..."}` |
| 503 | UE 未连接 / 战术层未就绪 | `{"ok":false,"error":"no mock ue connected"}` |

## 五、联调流程

### 5.1 确认 UE 已连接

```bash
curl http://localhost:8760/status
# 预期：{"ok":true,"ws_connected":true}
```

### 5.2 注入 schedule

用上方 curl 命令注入。MCP 日志会记录：

```
[MCP→Hermes/TACTICAL-PROMPT] agent_id=H-01 goal=车间装配作业 ...
[Hermes→MCP/TACTICAL-RESPONSE] agent_id=H-01 tokens=... raw=...
[战术层] 分解成功 agent_id=H-01 steps=3 thought=... actions=[...]
[debug/schedule] decompose ok agent_id=H-01 slot=07:00-11:00 goal=车间装配作业 queue_len=3
[战术层] 队列已填充 agent_id=H-01 slot=__debug__07:00-11:00 queue_len=3 ...
[战术层] 下发 action agent_id=H-01 action=move_to ...
[MCP→UE/CMD] cmd=MoveTo action_id=act_xxx agent_id=H-01 ...
```

### 5.3 观察下发

actions 入队后由 worker 异步逐个下发。每个 action 走 `action_command` → UE `action_started` ACK → 执行 → `action_completed` 流程。completion 唤醒 worker pop 下一个，直到队列空。

## 六、注意事项

1. **不覆盖 dailyPlan**：注入的 schedule 仅用于本次分解，不修改 `agentContext.dailyPlan`。注入队列执行完后，worker 下次 refill 会回到战略层生成的原 dailyPlan 正轨。`currentSlot` 加 `__debug__` 前缀避免与 dailyPlan 同时段撞 `redecomposeCount` 限制。

2. **force 默认开启**：默认 `force=true` 会先 stop 当前 action + 清空 `actionQueue` 再分解，避免新分解的 action 被 UE busy 拒。设 `force:false` 可保留当前 action（但新队列会在当前 action 完成后才下发）。

3. **互斥**：handler 复用 `replanInProgress` 互斥，防止与 worker 的 `tacticalRefill` 或反应层 `tacticalRefillForReplan` 并发调用 `tacticalHc`（撞 session）。若冲突返回 409，稍后重试即可。

4. **忽略当前游戏时间**：注入的 schedule 立即分解，不要求当前游戏时间在 slot 时段内。slot 仅用于 prompt 提示时段时长（引导 LLM 给出总时长接近的步骤）。

5. **agent 无感知时仍可分解**：若 agent 尚未上报感知（`latestPerception` 为空），zone/timeOfDay/physical 为空，prompt 仍能构造（buildTacticalPrompt 对 nil 用 0.0），响应 `warning` 字段提示分解质量可能下降。

6. **同步等待分解**：handler 同步等战术层 LLM 分解完成（最长 60s）再返回 actions 列表。LLM 超时返回 502。下发是异步的（`dispatched: false`）。
