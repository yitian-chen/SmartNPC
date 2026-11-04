# 记忆评测系统（Memory Bench）

> 交接现状、已知缺陷与下一步计划见 [MemoryBench_Handover.md](MemoryBench_Handover.md)。

## 一、原理

### 评什么

评测 NPC 的**待办记忆**（`episodic_commitments`：约定、传话、邀请、待办）在完整生命周期里是否正确：

```
对话 ──抽取──▶ 写入(write) ──改期/取消/履约──▶ 更新(update) ──查询──▶ 检索(read) ──决策──▶ 行动(use)
```

### 怎么评

- **直接跑生产代码**：抽取用生产的 `extractConversationCommitments`，检索用生产的 `memoryReader`，决策用生产的 `generateTacticalPlan`，跨日压缩用生产的 compact，存储用生产的 `MySQLStore`（真实 MySQL，表结构由生产 migrations 建出）。评测的就是仿真里实际运行的链路，不是替身
- **不启动仿真**：不连 UE、不起 WebSocket 和 worker 循环。用固定对话 + 虚拟游戏时钟驱动，步骤串行执行，不等真实时间
- **双组对照**：

  | 组 | 存储 | 写入 |
  |---|---|---|
  | memory | 生产 `MySQLStore`，每次运行一个独立临时库 | 正常抽取写入 |
  | no-memory | `NoopStore`（即生产的无持久化模式） | 跳过 |

  两组的人设、当前任务、物理状态、可用工具完全相同，只差有没有记忆，因此两组行为差异 = 记忆带来的效果
- **运行隔离**：memory 组的每一次运行（场景×重复）都在本机 MySQL 上新建一个临时库 `memory_bench_<时间>_<进程号>_<序号>`，经生产 `storage.NewMySQLStore` 打开（自动执行生产 migrations 建表），运行结束即删库。不同场景、重复、并发 worker、多次运行之间互不共享数据，也不会碰到 `agenttown_dev`/`agenttown_stable`
- **防泄漏**：每次决策前重建 Agent（清空会话历史、滚动摘要、守约 hint、旧计划，只保留本组 Store）——模型只能通过记忆检索拿到信息。原始对话和评分标签都不进决策请求

### 四阶段评分

| 阶段 | 检查对象 | 判定规则 |
|---|---|---|
| write | 抽取后的存储记录 | 记录数量、类型、对象、地点、起止游戏时间、内容关键词 |
| update | 改期/取消/履约后的存储记录 | 状态与时间正确，且其他未变更事项不受影响 |
| read | 生产待办查询的格式化文本 | 必须出现的文本 / 不得出现的文本 |
| use | 战术层输出的工具调用 | 至少命中一个允许的工具+参数组合，且不命中禁止组合（不比较整段文本） |

记录按字段匹配，不依赖随机生成的数据库 ID。

**组间对比只看 use**：write/update/read 只对 memory 组适用，场景总通过数不能直接当作组间能力差。

### 两种运行模式

| 模式 | 模型 | 用途 |
|---|---|---|
| 离线（默认） | 回放场景里预写的模型响应（`replay` 字段），零网络请求 | 验证接口、存储、评分链路是否正常——**不是模型能力分数** |
| 真实（`--live`） | 调真实模型，配置与仿真同源（`.env` / `LLM_CONFIG`） | 测量真实记忆效果 |

离线模式下决策回放固定返回中性的休息动作，use 断言失败是正常的。

## 二、结构

### 代码

均位于 `agenttown-mcp/cmd/agenttown-mcp/`（与生产代码同包，可直接调用内部函数）：

| 文件 | 职责 |
|---|---|
| `memory_bench_runner_test.go` | 主流程：`TestMemoryBench_Run` 入口，逐场景×组×重复执行（`benchRunOne`），生成报告 |
| `memory_bench_types_test.go` | 场景 YAML 结构定义与加载；四阶段评分器（记录匹配、检索文本、动作匹配） |
| `memory_bench_mysql_test.go` | MySQL 隔离：每次运行建临时库 → 生产 `NewMySQLStore` 打开 → 结束删库 |
| `memory_bench_store_test.go` | 内存版 Store（`--store memory` 时使用，以及框架单测用；实现生产 `storage.Store` 接口） |
| `memory_bench_transport_test.go` | HTTP 层：调用预算硬上限、请求响应记录、凭据脱敏 |
| `memory_bench_suite_test.go` / `memory_bench_unit_test.go` | 框架自身的测试（场景集校验、评分器、预算、防泄漏） |

HTTP 预算通过生产 Venus 客户端的可选 `Transport` 接缝注入（`pkg/venus/client.go`，生产默认 nil，零影响），agentic loop 内的重试也计入预算。

### 场景集

`agenttown-mcp/cmd/agenttown-mcp/testdata/memory_bench/suite.yaml`，30 个场景，六类各 5 个：

| 类别 | 覆盖内容 |
|---|---|
| `creation` | 跑步约定、传话、公开邀请、自己主持活动、多个事项 |
| `time` | 明天、三天后、跨午夜、绝对游戏日、深夜说的"明早" |
| `reschedule` | 推迟、提前、换地点、顺延一天、只改其中一项 |
| `closure` | 取消、履约、取消其中一项、拒绝邀约、过期过滤 |
| `retention` | 重建 Agent、噪声 compact、跨日、重复 compact、无关新对话 |
| `isolation` | 未获知消息者、假设性对话、他人私事、否定句、两个拥有者 |

其中 5 个标记 `smoke: true` 作为冒烟集：`creation-running`、`time-tomorrow`、`closure-cancel`、`retention-rebuild`、`isolation-owner`。

### 场景格式

每个场景由三部分组成：`steps`（输入，只含可观察内容）、`checks`（存储断言）、`probes`（检索与行动断言）。`checks`/`probes` 只交给评分器，不进模型请求。示例（`closure-cancel` 精简）：

```yaml
- id: closure-cancel
  category: closure
  smoke: true
  agents: [H-01, H-02, H-03]
  steps:                              # 输入：按顺序执行
    - type: dialogue                  # 一段对话结束 → 触发生产抽取
      at: {day: 1, time: "10:00"}
      agent: H-01                     # 记忆拥有者
      peer: H-02
      turns:
        - {speaker: H-02, content: 今天16:00到17:00，在central_plaza一起跑步，好吗？}
        - {speaker: H-01, content: 好，我答应在今天16:00到central_plaza跑步，直到17:00。}
      replay: '{"new_commitments":[...],"commitment_changes":[]}'   # 仅离线模式用
    - type: dialogue
      at: {day: 1, time: "11:00"}
      ...                             # H-02 取消约定
  checks:                             # 存储断言：after = 第几个 step 之后检查
    - after: 1
      stage: write
      agent: H-01
      count: 1
      records:
        - {kind: appointment, status: pending, counterpart: H-02, location: central_plaza,
           start: {day: 1, time: "16:00"}, end: {day: 1, time: "17:00"}, content_any: [跑步, 慢跑]}
    - after: 2
      stage: update
      ...                             # 同一条记录 status 应为 cancelled
  probes:                             # 在指定时刻检索 + 决策
    - agent: H-01
      at: {day: 1, time: "15:30"}
      visible: []                     # 决策时眼前的 NPC
      read:
        must: []
        forbidden: ["16:00", central_plaza]   # 已取消的约定不应再被检索到
      actions:
        any: []                       # 允许组合，例：{tool: move_to, params: {target_type: zone, target_id: central_plaza}}
        none: []                      # 禁止组合
```

步骤类型：

| type | 作用 |
|---|---|
| `dialogue` | 结束一段对话并调用生产抽取写入待办 |
| `advance` | 推进游戏时钟 |
| `restart` | 重建该 NPC 的 Agent 状态，保留存储 |
| `compact` | 把给定干扰历史交给生产跨日 compact，之后清空会话 |

`after` 从 1 开始计数；时间统一写成 `{day: 1, time: "16:00"}`。

## 三、使用方法

### 运行

在仓库根目录执行（Go 1.25+）。默认使用本机 MySQL（`127.0.0.1:3306`，root 无密码，即 start 脚本拉起的那个实例）——先确认 MySQL 已运行（启动过 `start-dev.sh` 即可，或 `mysql --socket=/data/mysql-run/mysql.sock -uroot -e 'SELECT 1'` 能通）：

```bash
# 离线全量：30 场景 × 2 组 × 2 次 = 120 次回放，零模型调用
scripts/run-memory-bench.sh

# 真实冒烟：默认 5 个冒烟场景、2 组、1 次、最多 30 次 HTTP
scripts/run-memory-bench.sh --live

# 真实跑指定若干场景（逗号分隔）
scripts/run-memory-bench.sh --live --cases creation-relay,time-midnight

# 只重跑一个场景，只跑 memory 组，并保留临时库供事后查看
scripts/run-memory-bench.sh --live --cases closure-cancel --groups memory --max-calls 5 --keep-db

# 没有 MySQL 的环境：改用内存版 Store
scripts/run-memory-bench.sh --store memory
```

参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--live` | 关 | 调真实模型 |
| `--cases` | 离线 `all` / 真实 `smoke` | `all`、`smoke` 或逗号分隔的场景 ID |
| `--groups` | `memory,no-memory` | 运行哪些组 |
| `--repeats` | 离线 2 / 真实 1 | 每个场景重复次数 |
| `--concurrency` | 1 | 并发数 |
| `--max-calls` | 30 | HTTP 调用上限，超出即中止。**真实模式硬上限 30**，只能调低不能调高 |
| `--llm-config` | `.env` 的 `LLM_CONFIG` | 分层模型配置（格式同仿真的 `llm_backend.yaml`） |
| `--suite` | 上述 suite.yaml | 场景集文件 |
| `--output` | `tmp/memory-bench/<时间-进程号>/` | 报告目录 |
| `--store` | `mysql` | `mysql`（生产 MySQLStore，每次运行一个临时库）或 `memory`（内存版，无需数据库） |
| `--mysql-dsn` | `.env` 的 `MYSQL_DSN`，否则 `root@tcp(127.0.0.1:3306)/?parseTime=true&charset=utf8mb4` | MySQL 服务器地址；DSN 里的库名会被忽略，替换为临时库 |
| `--keep-db` | 关 | 运行结束后保留临时库（默认删除），用于直接查表排障 |

**真实全量 30 场景**：单次运行最多 30 次 HTTP，而一个场景×两组通常要 3-4 次调用，全量需按类别分批跑（如 `--cases` 每次列一类的 5 个场景 ID）；要一次跑完需修改 `TestMemoryBench_Run` 里的 30 上限。

真实模式自动加载项目 `.env`，凭据只从环境读取，不经命令行、不写入报告。

### 看报告

输出目录（`tmp/`，不入库）下三个文件：

| 文件 | 内容 |
|---|---|
| `summary.md` | 分组/分类结果、各阶段断言通过数、token、耗时、场景状态——**先看这个** |
| `results.json` | 每个场景的存储快照、检索文本、生成的动作、完整模型请求与响应（已脱敏）——排查失败看这个 |
| `cases.json` | 本次运行的输入与标签，配合场景集 SHA256 可复现 |

场景状态：

| 状态 | 含义 | 脚本退出码 |
|---|---|---|
| `passed` | 全部断言通过 | 0 |
| `ability_failed` | 有断言失败（能力问题） | 0（保留报告便于收集基线） |
| `offline_validated` | 离线回放链路跑通 | 0 |
| `service_error` / `budget_stopped` / `invalid_fixture` | 服务错误 / 预算耗尽 / 场景配置非法 | 非 0 |

排查失败：在 `results.json` 中找到对应场景，依次看存储快照（写入对不对）→ 检索文本（查到了没有）→ 模型请求响应（为什么这么决策）。关键词评分可能误判同义表达，需人工复核。

直接查表：加 `--keep-db` 运行，`results.json` 里每条 memory 组结果的 `database` 字段就是它的临时库名：

```bash
mysql --socket=/data/mysql-run/mysql.sock -uroot <database> -e "SELECT * FROM episodic_commitments\G"
```

用完（或运行被中途杀掉留下残库时）清理所有临时库：

```bash
for d in $(mysql --socket=/data/mysql-run/mysql.sock -uroot -N -e \
  "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'memory\_bench\_%'"); do
  mysql --socket=/data/mysql-run/mysql.sock -uroot -e "DROP DATABASE \`$d\`"
done
```

### 可视化（Promptfoo）

```bash
cd eval/memory-promptfoo && npm ci && cd ../..                           # 首次装依赖
scripts/run-memory-promptfoo.sh --live                                   # 跑评测并导入
scripts/run-memory-promptfoo.sh --report tmp/memory-bench/<报告目录>     # 或导入已有报告（零模型调用）
scripts/run-memory-promptfoo.sh view                                     # 启动查看器（127.0.0.1:15500）
```

本机建 SSH 隧道 `ssh -N -L 15500:127.0.0.1:15500 root@<容器地址> -p 36000` 后浏览器打开 `http://localhost:15500`。详见 `eval/memory-promptfoo/README.md`。

### 新增场景

1. 在 `suite.yaml` 的 `cases` 下按上面的格式加一个场景，`id` 唯一、`category` 用六类之一
2. `dialogue` 步骤写好 `replay`（离线回放用的预期抽取 JSON，格式不合法会报 `invalid_fixture`）
3. 先跑框架校验，再跑真实单场景：

   ```bash
   cd agenttown-mcp && go test ./cmd/agenttown-mcp -run '^TestBench' -count=1 && cd ..
   scripts/run-memory-bench.sh --live --cases <新场景id> --max-calls 10
   ```

`TestBenchSuite_ThirtyCasesAndFiveSmoke` 断言了 30/5 的数量，增删场景时同步修改。

### 开发验证

```bash
cd agenttown-mcp
go test ./cmd/agenttown-mcp -run '^TestBench' -race -count=1   # 框架自身测试
```

普通 `go test ./...` 不会调用真实模型，也不需要 MySQL（`TestMemoryBench_Run` 需脚本显式触发；框架单测用内存版 Store）。

## 四、局限

- use 只判断工具调用意图，**不验证 UE 中实际履约**
- 世界、人设、能力声明使用固定小镇样本，不加载运行中的 World KB
- 评分基于字段与关键词匹配，不是语义裁判
