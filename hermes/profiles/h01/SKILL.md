# H-01 可用工具

> 以下工具由 `agenttown-mcp`（Go MCP Server）提供，Hermes 通过 MCP 协议发现并调用。
> 工具名带 `mcp__agenttown__` 前缀（Hermes 自动添加，双下划线分隔）。
> **所有工具的第一个参数固定为 `agent_id`**（你的 ID，如 "H-01"）。
> 执行任何动作必须调用工具，不要只用文字叙述。

## 复合行为（优先使用）

### mcp__agenttown__work_assemble
在工作台进行装配（一整套装配流程）。
- agent_id: 你的 ID
- target: 工作台 ID（如 workbench_01）
- duration_min: 时长（分钟）

### mcp__agenttown__patrol_route
巡逻预设路线。
- agent_id, route_id

### mcp__agenttown__charge_at
在充电桩充电。
- agent_id, station_id, duration_min

### mcp__agenttown__repair_target
修理另一个 agent。
- agent_id, target_agent_id

### mcp__agenttown__social_chat_with
与另一个 agent 社交对话。
- agent_id, target_agent_id

### mcp__agenttown__rest_idle
休息待机一段时间。
- agent_id, duration_min

### mcp__agenttown__archive_research
在档案馆做研究。
- agent_id, duration_min

## 原子行为（需精细控制时使用）

### mcp__agenttown__move_to
移动到语义目标（区域或地点 ID，世界自动解析坐标）。
- agent_id, target（如 main_workshop、workbench_01）

### mcp__agenttown__turn_to
面向目标。
- agent_id, target

### mcp__agenttown__speak
说话，附近 agent 能听到。
- agent_id, content, target（可空=对附近说）

### mcp__agenttown__emote
表达情绪。mode=oneshot 播一次，mode=sustained 持续保持。
- agent_id, emotion（happy/sad/worried/...）, mode（可空，默认 oneshot）

### mcp__agenttown__interact
与智能物件交互，用其 available_actions 中的动词。
- agent_id, object_id, action

### mcp__agenttown__wait
原地等待。
- agent_id, duration_sec（秒）

### mcp__agenttown__scan_area
主动请求一次即时感知（环顾四周）。
- agent_id

### mcp__agenttown__stop
停止当前动作。
- agent_id
