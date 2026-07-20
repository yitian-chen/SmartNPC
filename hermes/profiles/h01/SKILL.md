# H-01 AgentTown 工具规则

AgentTown 工具由 MCP 提供，名称前缀为 `mcp__agenttown__`。

## 每次调用必填

- `agent_id="H-01"`
- `decision_epoch`：原样使用当前 `[decision_context]` 给出的值；不得猜测、复用旧值或自行递增。

每轮只选择一个主行为。优先复合工具；需要精细控制时才使用原子工具。ACK 只表示 UE 已接受动作，完成结果以后续感知为准。收到 `stale decision`、`agent offline` 或 `unknown agent` 后停止重试该旧决策。

## 复合行为（优先）

| 工具 | 额外参数 |
|------|----------|
| `work_assemble` | target, duration_min |
| `patrol_route` | route_id |
| `charge_at` | station_id, duration_min |
| `repair_target` | target_agent_id |
| `social_chat_with` | target_agent_id |
| `rest_idle` | duration_min |
| `archive_research` | duration_min |

## 原子行为

| 工具 | 额外参数 |
|------|----------|
| `move_to` | target |
| `turn_to` | target |
| `speak` | content, target(可空) |
| `emote` | emotion, mode(可空) |
| `interact` | object_id, action |
| `wait` | duration_sec |
| `scan_area` | 无；扫描用于补充感知。可在同一工具轮内与其他工具一起调用（如 scan_area 后再 move_to）；扫描结果会触发下一轮 decision_context，届时选择一个非扫描主行为 |
| `stop` | 无 |
