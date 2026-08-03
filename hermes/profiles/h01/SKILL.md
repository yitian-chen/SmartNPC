# H-01 AgentTown 工具规则

AgentTown 工具由 MCP 提供，名称前缀为 `mcp__agenttown__`。

## 每次调用必填

- `agent_id="H-01"`
- `decision_epoch`：原样使用当前 `[decision_context]` 给出的值；不得猜测、复用旧值或自行递增。

每轮只选择一个主行为。优先复合工具；需要精细控制时才使用原子工具。ACK 只表示 UE 已接受动作，完成结果以后续感知为准。收到 `stale decision`、`agent offline` 或 `unknown agent` 后停止重试该旧决策。

## 复合行为（优先，共 6 个）

| 工具 | 额外参数 | 说明 |
|------|----------|------|
| `work_at_workbench` | `target_object_id`, `duration_sec`(可空) | 在指定工作台装配 |
| `work_at_workshop` | 无 | 车间例行工作（自动选工作台） |
| `chat_with` | `target_agent_id`, `topic`(可空) | 与其他 agent 社交对话 |
| `repair_target` | `target_agent_id`, `tool_id`(可空) | 维修其他 agent |
| `charge_at_station` | `target_object_id`(可空，空则自动选) | 充电 |
| `patrol_zone` | `target_zone`, `duration_sec`(可空) | 巡逻区域 |

## 原子行为（共 8 个）

| 工具 | 额外参数 | 说明 |
|------|----------|------|
| `move_to_location` | `target`, `speed`(可空，默认 walk) | 移动到语义位置（zone id 或 object id，由 MCP 解析坐标） |
| `move_to_agent` | `target_agent_id`, `speed`(可空), `stop_distance`(可空), `keep_following`(可空) | 跟随动态 agent |
| `turn_to` | `target_agent_id` 或 `direction`（二选一） | 转向目标 |
| `play_montage` | `montage_id`, `wait_finish`(可空，默认 true) | 播放蒙太奇动画 |
| `speak` | `content`, `target_agent_id`(可空), `audio_url`(可空) | 说话 |
| `emote` | `emotion`, `mode`(可空，默认 oneshot) | 表达情绪（oneshot / sustained） |
| `interact` | `target_object_id`, `interaction` | 与智能物体交互（interaction 必须来自物体的 available_interactions） |
| `wait` | `duration_sec` | 原地等待 |

## 控制工具（共 2 个，不消耗主行为）

| 工具 | 参数 | 说明 |
|------|------|------|
| `scan_area` | 无 | 请求即时感知。可在同一工具轮内与其他工具一起调用（如 scan_area 后再 move_to_location）；扫描结果会触发下一轮 decision_context，届时选择一个非扫描主行为 |
| `stop` | 无 | 停止当前在途 action；无在途动作时为 no-op |

## id 选取约束

- `move_to_location` 的 `target` 必须是当前感知中"可前往区域"或"可交互物体"明确给出的 id，不得自行命名或臆造
- `interact` 的 `target_object_id` 和 `interaction` 必须严格使用感知中物体及其 `available_interactions` 列出的值，不得拼接 zone/interaction 信息
- `work_at_workbench` 的 `target_object_id` 必须是感知中存在的工作台类物体
- `patrol_zone` 的 `target_zone` 必须是感知中存在的 zone id
- 若目标不在列表中，改用 `scan_area` 重新感知或选择列表内的替代目标
