# H-01 可用工具

> 以下工具由 `agenttown-mcp`（Go MCP Server）提供，Hermes 通过 MCP 协议发现并调用。
> 工具名带 `mcp__agenttown__` 前缀（Hermes 自动添加，双下划线分隔）。

## 移动

### mcp__agenttown__move_to
移动到目标位置。
- target: 区域 ID 或地点 ID（如 "main_workshop"、"workbench_01"）

### mcp__agenttown__turn_to
面向目标方向。
- target: 目标实体 ID

## 工作

### mcp__agenttown__work_assemble
在工作台进行装配工作。
- target: 工作台 ID
- duration_min: 工作时长（游戏分钟）

### mcp__agenttown__interact_with
与场景中的智能对象交互。
- object_id: 对象 ID（如 "workbench_01"）
- action: 动词（如 "inspect"、"repair"，需在该对象的 available_actions 列表中）

## 维护

### mcp__agenttown__charge_at
在充电桩充电。
- station_id: 充电桩 ID
- duration_min: 充电时长（游戏分钟）

### mcp__agenttown__self_check
自检身体状态（电池、疲劳、关节磨损等）。无参数。

## 社交

### mcp__agenttown__speak
对某个目标说话。
- text: 说话内容
- to: 说话对象 ID（可为空，表示自言自语，附近 NPC 仍能听到）

### mcp__agenttown__emote
表达情绪动作。
- emotion: wave / nod / shake_head / idle_think / worried

## 状态

### mcp__agenttown__wait
原地等待一段时间。
- seconds: 等待秒数

### mcp__agenttown__update_plan
更新当日计划。在情况发生变化时使用。
- plan: 新的当日计划文本
