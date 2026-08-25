# AgentTown — AI NPC 模拟系统

AI NPC 模拟系统：5 个 NPC（H-01~H-05）通过 MCP 协议驱动"感知 → 决策 → 行动"闭环，对接真实 UE5（AgentTown 地图）。MCP 侧内置三层决策（战略/战术/反应），直连 Venus LLM。

## 项目结构

```
agenttown-mcp/                  # Go MCP 服务（核心）
  cmd/agenttown-mcp/            # 入口 + 三层决策 + capability + debug UI + 记忆/关系层
  pkg/
    prompt/                     # 战略/战术层 prompt 构建（system/user 拆分、物理分档、设施映射）
    agentstate/                 # 每-NPC 业务状态（队列、会话历史、物理状态、schedule）
    venus/                      # Venus LLM 客户端（function calling + Structured Outputs）
    ollama/                     # 反应层本地 Ollama 客户端
    protocol/                   # 7 字段信封 + 消息类型 + cmd 常量
    wsserver/                   # WS 服务端（收发、seq、重放）
    worldkb/                    # 世界 KB 加载/合并/查询
    profile/                    # NPC 人设档案（assets/profiles/*.md）
    weeklyschedule/             # 每周日程配置
    storage/                    # MySQL 持久化（内存模式默认）
  adapters/agenttown/tools/     # MCP 工具（5 复合 + 7 原子 + 2 特殊）
assets/
  world_kb.yaml                 # 世界 KB：7 zones / 57 objects / 5 agents
  profiles/H-01.md ~ H-05.md    # NPC 人设档案
  weekly_schedule.yaml          # 每周日程（工作日/休息日/运动日/冥想日）
docs/                           # 设计文档（协议/工作流/对话/世界 KB 等）
scripts/pretty_log.py           # 日志可读化工具（HTML 报告）
start-debug.sh / start-dev.sh   # 启动脚本（stable / dev 实例）
.env.example                    # 环境变量模板
CODEBUDDY.md                    # 完整开发手册（比本 README 更详细）
```

## 快速开始

**云环境（推荐）**：

```bash
cd /data/workspace/dev
cp .env.example .env            # 填入 VENUS_API_KEY
bash start-dev.sh               # dev 实例：端口 8770/9091，日志 logs-dev/

# stable 实例（另一目录）：
cd /data/workspace/stable
bash start-debug.sh             # 端口 8760/9090，日志 logs/
```

**编译 / 测试**：

```bash
cd agenttown-mcp
go build ./...                  # 编译检查
go test ./...                   # 全量测试
```

**debug 控制台**：`http://localhost:8770/debug/`（dev）或 `:8760`（stable）——单 Action 下发、Schedule 注入、当日 schedule、战术层分解情况、MCP 日志。

## 关键信息

- **三层决策**：战略层（每日 07:00 生成 6-8 时段计划）→ 战术层（每时段把 goal 分解为 1-4 动作段）→ 反应层（默认禁用，Ollama 决策 continue/observe/replan）
- **战术层 function calling**：OpenAI 原生 `tools` 字段 + `tool_choice=required` + 多轮 agentic loop；工具由 `capability_registry` 派生；段间用 `time_to_stop` 控制时长（末段不设，自然持续到时段切换）
- **LLM 后端**：战略层 `deepseek-v4-pro`、战术层 `deepseek-v4-flash`（Venus，OpenAI 兼容）；反应层本地 Ollama（默认禁用）
- **物理属性分档**：电量/疲劳/关节磨损 3 阈值切 4 档，标签注入战略层 prompt；per-NPC 可经 `profile.md` 的 `## 属性分段` 覆盖
- **capability_registry**：UE 连接后推送能力声明，MCP 据此动态增删工具；`world_kb` 推送合并落盘
- **持久化**：默认内存模式；`MYSQL_DSN` 非空启用 MySQL（调度状态 + 记忆 + 动作历史 + 关系）
- **日志**：`logs/YYYY-MM-DD/debug-mcp.log`（stable）或 `logs-dev/...`（dev），JSON Lines 全链路

## 坑点

- **Go bool flag 必须 `=` 赋值**：`--auto-plan true` 会把 `true` 当 positional arg 导致后续 flag 不解析，必须写 `--auto-plan=true`
- **战术层 LLM 输出坏 tools JSON**：deepseek-v4-flash 多轮场景偶发输出格式异常 tool_calls，存进历史后重放触发 venus 500 `trailing characters`。已做历史截断（8 轮）+ 失败兜底动作缓解，但根因在模型侧
- **time_to_stop 漏设 → 队列卡死**：LLM 常给中间动作漏设 time_to_stop，导致 NPC 一直坐长椅/一直工作。已做代码层兜底（非队尾休息 1800s / 工作 5400s）
- **zone 参数曾在下发时被丢弃**：`mapTacticalAction` 白名单式构造 params 曾漏掉 `zone`，导致"去中央广场长椅"落到 NPC 所在 zone。已修复透传
- **slot 切换时旧动作超时**：旧动作 stop 被推迟到"新 slot 分解成功之后"，LLM 慢/失败时旧动作跨 slot 执行（60s 超时 × 90 倍时间缩放 ≈ 90 游戏分钟）
- **战略层短时段裁剪（已停用）**：原 `minSlotMinutes=60` 与规则"每段 ≥30 分钟"矛盾，会把晨练/午休等 30-59 分钟短时段裁掉，计划变"工作+睡眠"简略形态
- **bash 工作目录不持久**：每条命令需显式 `cd` 到目标目录（`cd` 不跨命令保留）
- **日志目录区分**：stable 写 `logs/`，dev 写 `logs-dev/`，别查错目录
