# AgentTown 新人上手指南（交接文档）

> 写给接手 AgentTown_v3 项目的同事。覆盖：环境访问、双实例启动、UE 连接、debug 控制台、日志与请求体排查、数据库、日常开发流程。
> 项目全貌与架构请配合 `README.md` 与 `CLAUDE.md` 阅读（CLAUDE.md 记录了全部机制的演进细节，是排障时的第一手资料）。

---

## 1. 项目是什么

AgentTown 是 LLM 驱动的 AI NPC 模拟系统：UE5 机器人小镇里 5 个 NPC（H-01~H-05）自主生活——上班、充电、社交、赴约、战斗逃跑。行为由 MCP 侧（Go）的**战略层**（每日计划）+ **战术层**（时段分解为动作）+ **事件路由**（打断判决）三层决策驱动，经 WebSocket 下发给 UE5 执行。

**仓库**：工蜂 `https://git.woa.com/yitianchen/smartnpc.git`（project_id 1875516）

**关键分工**：本仓库是 MCP 决策侧（Go）。UE5 侧（AgentTown 地图，C++/蓝图）在另一个仓库/工程里，由 UE 同事维护；两侧通过 WebSocket 协议（`docs/AgentTown_CommProtocol_Values.md`）交互。

**分支模型**：
- `master` — 稳定分支，stable 实例跑它
- `dev-working` — 日常开发分支，dev 实例跑它
- `feature/*`、`fix/*` — 按任务开分支，完成后提 MR 合入 master

---

## 2. 云开发容器（AnyDev）

项目运行在**公司云开发容器**（AnyDev，远程 Linux 环境）上，不在本地跑：

| 路径 | 说明 |
|------|------|
| `/data/workspace/dev` | dev 实例（`dev-working` 分支），端口 8770/9093 |
| `/data/workspace/stable` | stable 实例（`master` 分支），端口 8760/9092 |
| `/data/mysql-data` | MySQL 数据目录（挂持久化盘，跨容器重建不丢） |
| `/data/mysql-run/mysql.sock` | MySQL socket |

两个目录是**同一仓库的两个独立 clone**，端口与数据库全隔离（dev 用 `agenttown_dev`，stable 用 `agenttown_stable`），可以同时运行互不干扰。

> **FIXME（交接时补充）**：AnyDev 工作区的申请/打开入口 URL、账号流程，请在此补上具体链接与截图。

**本地访问云端端口**（debug 页面、日志报告都用得到）：
- AnyDev 自带的端口转发/网页预览功能（优先用这个）
- 或 SSH 隧道：`ssh -L 8770:localhost:8770 -L 9093:localhost:9093 <容器>`，然后本地浏览器开 `http://localhost:8770/debug/`

**网络可达性**：容器内可达工蜂（`git.woa.com`）与 Venus（`v2.open.venus.oa.com`）。

---

## 3. 启动 / 停止

两个实例各有一条启动命令（在各自目录下执行）：

```bash
cd /data/workspace/dev     && bash start-dev.sh       # dev 实例：8770/9093，日志 logs-dev/
cd /data/workspace/stable  && bash start-debug.sh     # stable 实例：8760/9092，日志 logs/
```

`start-dev.sh` 是 wrapper：设好偏移端口/库名/二进制名后 exec `start-debug.sh`，所以两者行为一致。

**日常联调最常用的是带 `--drop-tables` 启动**（重置数据库，从零开始一轮干净的仿真——记忆三表、调度状态、动作历史全部清空，migrations 自动重建）：

```bash
cd /data/workspace/dev && bash start-dev.sh --drop-tables
```

**停止**：

```bash
bash start-dev.sh --stop        # 或 bash start-debug.sh --stop（stable）
```

**启动脚本做了什么**（顺序）：
1. 杀掉端口上的旧进程（MCP 不残留）
2. 拉起 MySQL（仅 Linux 云环境；已在跑则复用，幂等）
3. `--drop-tables` 时：`DROP DATABASE + CREATE DATABASE`（默认不带此 flag，保留累积状态）
4. 编译 MCP（`go build`，产物 `agenttown-mcp/agenttown-mcp-dev` 或 `agenttown-mcp.exe`，两实例二进制名不同避免锁冲突）
5. 启动 MCP（`nohup`，读 `.env` 的 `VENUS_API_KEY`，`--tactical-stream` 默认开）并等健康检查通过
6. 打印 UE 连接地址等信息

**前置条件**：
- `.env` 存在且配置了 `VENUS_API_KEY`（模板见 `.env.example`；缺 key 时 MCP 启动直接 fail）
- Go 1.25+ 在 PATH（或设 `GO_BIN`）
- mysqld 已安装（容器上已就绪）

**验证启动成功**：

```bash
curl -s http://localhost:8770/healthz    # dev；stable 用 8760
```

> MySQL 是基础设施，`--stop` 不会停它；`SKIP_MYSQL=1` 环境变量可跳过（MCP 降级为内存模式 NoopStore，无持久化）。

---

## 4. UE 连接

UE 同事在本地 Windows 打开 AgentTown 地图工程，把 WS 地址指向云端容器：

| 实例 | UE 连接地址 |
|------|------------|
| dev | `ws://<容器IP>:9093/ws` |
| stable | `ws://<容器IP>:9092/ws` |

容器 IP 用 `hostname -I` 查（启动脚本 summary 里也会打印）。UE 侧的地址配置项在 UE 工程里（问 UE 同事）。

**连接后 UE 会按序首发**：`world_kb` → `agent_registered`（×5 个 NPC）→ `capability_registry` → `resync`/`state_report`/`perception_update`。看到这串消息就是连上了（`grep 'agent_registered' logs-dev/*/debug-mcp.log`）。

之后无需人工干预：每个 NPC 注册时自动生成当日计划并开始决策循环。

---

## 5. Debug 控制台

浏览器打开（云容器需端口转发，见 §2）：

- **dev**：`http://localhost:8770/debug/`
- **stable**：`http://localhost:8760/debug/`

单页控制台，主要面板：

| 面板 | 用途 |
|------|------|
| 单 Action | 直接下发一条 action_command 到 UE（单步调试） |
| Schedule 注入 | 注入一条 goal 文本，立即触发战术层分解（测试"某个 goal 会分解出什么动作"） |
| 事件下发 | 15 个预设 world_event（被攻击/战斗接管/脱离战斗…）+ 自定义，走与 UE 上报完全相同的分发入口 |
| 当日 schedule | 各 NPC 的 dailyPlan（时段 + 当前高亮） |
| 战术层分解情况 | 每 NPC 当前时段 goal、在途 action 参数、待执行队列（5s 自动刷新）——**看 NPC 现在在干嘛、接下来要干嘛，首选这里** |
| MCP 日志 | 环形日志，按 level 筛选 |

**常用 JSON 端点**（curl 直接打）：

```bash
curl -s localhost:8770/debug/agents     # 已注册 NPC 列表
curl -s localhost:8770/debug/plan?agent_id=H-01      # 当日计划快照
curl -s localhost:8770/debug/tactical   # 全部 NPC 战术分解情况
curl -s localhost:8770/debug/memory?agent_id=H-01    # 长期记忆三表快照（经历/待办/对人认知）
curl -s localhost:8770/debug/ue-errors  # UE 上报的错误
curl -s localhost:8770/debug/kb         # world_kb
curl -s localhost:8770/debug/cap        # capability_registry
curl -s localhost:8770/debug/llm-metrics # LLM 指标聚合
```

**注入测试事件示例**（模拟"被玩家攻击"，force 走硬保证打断通道）：

```bash
curl -s -X POST localhost:8770/debug/event -d '{
  "agent_id": "H-01",
  "event": {"category": "player_interaction", "event_type": "player_attacked",
            "force": true, "severity": 10,
            "data": {"attacker": "player_1", "damage": 20}}
}'
```

---

## 6. 查看日志

**日志路径**（按启动日期分目录，JSON Lines 格式，MCP 独占写入）：

- dev：`/data/workspace/dev/logs-dev/YYYY-MM-DD/debug-mcp.log`
- stable：`/data/workspace/stable/logs/YYYY-MM-DD/debug-mcp.log`

日志里包含 UE↔MCP↔LLM 全链路（每条消息一行 JSON，`msg` 字段带方向标记如 `[UE→MCP]`、`[MCP→LLM/TACTICAL-PROMPT]`）。

**推荐：pretty_log.py 可读化**：

```bash
cd /data/workspace/dev
python scripts/pretty_log.py --html                # HTML 报告（可折叠/搜索/过滤，自动开浏览器）
python scripts/pretty_log.py --html 2026-11-04     # 指定日期
python scripts/pretty_log.py -f PERCEPTION -n 5    # 终端看最近 5 条感知原文
python scripts/pretty_log.py --raw | less          # 原始 JSON（grep 友好）
```

**裸 grep 关键词**（排查问题的入口）：

```bash
LOG=logs-dev/2026-11-04/debug-mcp.log

grep '队列已填充' $LOG             # 战术层每次分解出的动作序列（含参数）
grep '下发 action' $LOG            # 实际下发给 UE 的动作
grep 'TACTICAL-PROMPT' $LOG        # 战术层发给 LLM 的完整 prompt
grep 'TACTICAL-RESPONSE' $LOG      # LLM 返回
grep 'STRATEGIC' $LOG              # 战略层（每日计划）
grep 'speech' $LOG                 # 对话链路（speak_to / speech_heard / 回执）
grep '事件路由' $LOG               # 事件判决（打断概率/动机/紧急度）
grep 'time_to_stop\|时段切换' $LOG # 动作切段与 slot 切换
grep 'ue-errors\|error' $LOG       # 错误
```

**排障思路**（实战总结的链路还原法）：NPC 行为异常时，用毫秒级时间戳把相关行串成时间线——`下发 action → action_completed → 队列已填充 → 下一个 action`，配合 `[对话层]`/`[事件路由]` 的日志行定位是决策问题还是执行问题。`CLAUDE.md` 的「关键机制」条目按日期记录了历次实测问题的完整故障链，**遇到似曾相识的现象先查它**。

---

## 7. 查看 actual_prompts（LLM 请求体留存）

MCP 运行时会把 H-01 最新一次的**完整 LLM 请求体**（model/messages/tools 全字段）覆盖落盘：

- **`docs/actual_prompts.md`**（相对实例目录，dev 就是 `/data/workspace/dev/docs/actual_prompts.md`）——战略层/战术层/对话层/事件路由的请求体 + 可读 prompt 预览。**想看"LLM 到底收到了什么"（世界背景/角色/规则/对话记录的最终形态），看这个文件**，不用去日志里翻
- **`docs/llm_metrics.md`** — LLM 调用表现聚合（各层 E2E/TTFT 分位延迟、重试率、JSON 正确率、错误分布），每次调用后覆盖写。**判断"是 LLM 慢/出错还是逻辑问题"先看这里**

两者由 flag 控制路径（`--prompt-doc` / `--llm-metrics-doc`，默认上述路径，启动脚本未改）。注意：这两个文件是运行时产物但被 git 跟踪，本地跑完仿真后工作区会脏，**提交前注意不要把仿真残留的 actual_prompts.md/llm_metrics.md 带进无关 commit**。

---

## 8. MySQL（记忆与调度状态）

容器上 MySQL 免密 root，socket 连接：

```bash
mysql --socket=/data/mysql-run/mysql.sock -uroot agenttown_dev
```

核心表（`--drop-tables` 启动时全部重建）：

| 表 | 内容 | 排查场景 |
|----|------|---------|
| `agent_schedule_state` | 每个 NPC 的 dailyPlan / 当前时段 | NPC 日程不对、二次赴约 |
| `episodic_memories` | 过去经历（compact 抽取 + 规则直写） | 记忆有没有沉淀 |
| `episodic_commitments` | 对话约定/待办（due_start/due_end/status） | **守约问题首选**——约定抽出来了吗、due 窗对吗、status 退出了吗 |
| `semantic_people` | 对人认知（affinity 0-100 + impression） | 好感度变化 |
| `action_history` | 全部动作生命周期 | 动作有没有真的下发/完成 |
| `schema_migrations` | 迁移版本 | 建表是否到位 |

```bash
# 例：看 H-05 的待办约定
mysql --socket=/data/mysql-run/mysql.sock -uroot agenttown_dev \
  -e "SELECT owner_id,kind,counterpart_id,content,due_start,due_end,status FROM episodic_commitments"
```

游戏时间换算：游戏秒字段（如 `due_start`）÷3600 = 游戏小时（如 27180 → 07:33）。仿真默认 `time_scale=90`（1 现实秒 ≈ 90 游戏秒 ≈ 1.5 游戏分钟）。

---

## 9. 日常开发流程

```bash
cd /data/workspace/dev
git checkout dev-working && git pull

# 改代码（agenttown-mcp/ 下，Go 三 module 结构：根 + contract/ + wsserver/）
cd agenttown-mcp
go build ./...                          # 编译检查
go test ./...                           # 根 module 全量
(cd contract && go test ./...)          # 契约 module
(cd wsserver && go test ./...)          # 连接 module

# 验证：重启 dev 实例跑仿真
cd /data/workspace/dev && bash start-dev.sh --drop-tables
```

**提交规范**：`<type>(<scope>): <subject>` 中文提交信息（type：feat/fix/refactor/test/docs/chore；scope：mcp/protocol/venus/config 等）。完成一项工作主动 commit。

**提 MR**（工蜂 API，注意是 `/api/v3/` + Bearer 头）：

```bash
source /etc/profile.d/codev_system_env.sh   # GF_TOKEN
curl -s -H "Authorization: Bearer $GF_TOKEN" "https://git.woa.com/api/v3/projects/1875516/merge_requests?state=opened"
```

**安全红线**：
- `.env`（VENUS_API_KEY）与 `assets/llm_backend.yaml` 已 gitignore，**永远不要提交**
- `git push` 只推 `origin`（工蜂）；`github` remote 是公共镜像，不经专门确认不要推
- 工作区可能有 `assets/world_kb.yaml` 的运行时改动（UE 启动时推送覆盖）与 `docs/actual_prompts.md`/`llm_metrics.md` 的仿真残留，commit 前看清 `git status`，别带进无关提交

---

## 10. 常见问题速查

| 症状 | 先查 |
|------|------|
| MCP 启动即退 | `.env` 里 `VENUS_API_KEY`；端口被占（`ss -ltnp \| grep 8770`） |
| UE 连不上 | 容器 IP 对不对、防火墙放行 9092/9093；`grep 'ue connected'` 日志 |
| NPC 全部呆站不动 | `grep 'venus\|status 4\|timeout'` 日志——LLM 后端过载/超时；`curl localhost:8770/debug/llm-metrics` 看错误分布 |
| NPC 行为怪异（弃约/重复/呆站） | §6 的链路还原法 + `CLAUDE.md`「关键机制」按现象搜——绝大部分已知问题都在里面有完整故障链记录 |
| 想复现某个场景 | `/debug/schedule` 注入 goal、`/debug/event` 注入事件，不用等自然发生 |
| 数据脏了想重来 | `bash start-dev.sh --drop-tables` |

---

## 11. 文档索引

| 文档 | 内容 |
|------|------|
| `README.md` | 项目全貌、架构图、机制总述（先读这个） |
| `CLAUDE.md` | 全部机制的实现细节与演进史（排障字典） |
| `docs/AgentTown_CommProtocol_Values.md` | 通信协议唯一权威（信封/消息/cmd/数值系统） |
| `docs/AgentTown_WorldEvent_Protocol.md` | 事件系统协议 |
| `docs/DebugAction_Tool.md` | `/debug/action` + `/debug/schedule` 详细用法 |
| `docs/AgentTown_Core_DeepDive.md` | 核心设计深潜 |
| `docs/AgentTown_LongTermMemory_SQLite_Design.md` | 长期记忆三表设计 |
| `docs/AgentTown_Workflow.md` | 工作流 |
| `docs/actual_prompts.md` / `docs/llm_metrics.md` | 运行时产物（§7） |
