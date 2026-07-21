# AgentTown_v3

AI NPC 模拟系统 — 一期单 Agent（H-01 老陈，车间主管机器人），通过 MCP 协议驱动 "感知 → 决策 → 行动" 闭环。Mock UE 模拟 UE5 游戏世界，Hermes Gateway 作为 LLM Agent，agenttown-mcp 作为中间层适配协议并暴露工具。

## 架构

```
Mock UE (Python)  ←→  agenttown-mcp (Go)  ←→  Hermes Gateway (Docker)
    :9090/ws              :8760/mcp               :8642
```

- **Mock UE** — 模拟游戏世界：物理状态、空间状态、感知推送、动作执行
- **agenttown-mcp** — 协议适配、感知语义化、MCP 工具暴露、Hermes 桥接
- **Hermes Gateway** — LLM Agent Mind：决策、工具调用、叙事生成

## 快速开始

```bash
cp .env.example .env          # 填入 HERMES_AGENT_API_KEY
bash start.sh quick-smoke     # 协议烟测 (06:00-10:00, 600x)
bash start.sh behavior        # 行为联调 (06:00-18:00, 60x, 带场景事件)
bash start.sh normal          # 完整一天 (06:00-22:00, 300x)
```

## 统一日志（按测试日期归档至 `logs/YYYY-MM-DD/`）

```bash
cat logs/$(date +%Y-%m-%d)/sim.log               # 含 UE + MCP + Hermes 全链路
grep '\[UE→MCP\]' logs/YYYY-MM-DD/sim.log         # UE → MCP 消息
grep '\[MCP→Hermes/PERCEPTION\]' logs/YYYY-MM-DD/sim.log  # 感知推送
grep '\[Hermes→MCP/RESPONSE\]' logs/YYYY-MM-DD/sim.log   # LLM 响应
grep '\[Hermes→MCP/TOOL\]' logs/YYYY-MM-DD/sim.log       # 工具调用
grep '\[MCP→UE\]' logs/YYYY-MM-DD/sim.log         # MCP → UE 命令
```

## 测试

```bash
cd agenttown-mcp && go test ./...
python -m unittest discover -s tests -p "test_mock_ue.py"
```

## 协议

按 `docs/AgentTown_CommProtocol_Values.md` 实现，使用 7 字段信封 + 11 种消息类型，坐标单位 UE5 厘米，动作生命周期 command → ACK → completed。
