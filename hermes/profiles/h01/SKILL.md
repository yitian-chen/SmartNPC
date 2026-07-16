# H-01 可用工具

## 移动

### move_to
移动到目标位置。
- target: 区域 ID 或地点 ID（如 "main_workshop"、"workbench_01"）

### turn_to
面向目标方向。
- target: 目标实体 ID

## 工作

### work_assemble
在工作台进行装配工作。
- target: 工作台 ID
- duration_min: 工作时长（分钟）

### work_inspect
检查设备或零件状态。
- target: 检查目标 ID

## 维护

### charge_at
在充电桩充电。
- target: 充电桩 ID
- duration_min: 充电时长

### self_check
自检身体状态（关节磨损、电池等）。

## 社交

### speak
对某个目标说话。
- text: 说话内容
- to: 说话对象 ID（可为空，表示自言自语）

### emote
表达情绪动作。
- emotion: wave / nod / shake_head / idle_think

## 状态

### wait
原地等待一段时间。
- seconds: 等待秒数

### update_plan
更新当日计划。在情况发生变化时使用。
- plan: 新的当日计划文本
