# 离线待办记忆 Bench

Bench 用固定对话和虚拟游戏时间驱动生产记忆代码，不启动 UE、WebSocket 或完整 worker。它评估待办的新增、时间换算、变更、检索与后续动作选择。

## 一条命令运行

在仓库根目录运行，要求 Go 1.25+：

```bash
# 30 个场景 × 2 次 × 两组：120 次离线回放，零真实模型请求
scripts/run-memory-bench.sh

# 只运行 5 个真实冒烟场景；默认两组、重复 1 次、并发 1、最多 30 次 HTTP
scripts/run-memory-bench.sh --live

# 使用现有分层模型配置，指定输出位置
scripts/run-memory-bench.sh --live --cases smoke \
  --llm-config assets/llm_backend.yaml --output tmp/memory-bench/my-smoke

# 只重跑某个场景
scripts/run-memory-bench.sh --live --cases closure-cancel \
  --groups memory --repeats 1 --max-calls 5
```

`--live` 未提供时，使用脚本化模型响应回放，不联网。离线回放用于验证接口、存储、上下文隔离与评分链路，**不是模型能力分数**；其中行为断言可能失败，因为决策回放固定返回中性的休息动作。

脚本在真实运行时加载项目 `.env`，复用 `LLM_CONFIG` 或 `VENUS_API_KEY`、`VENUS_URL`、`VENUS_MODEL`、`VENUS_STRATEGIC_MODEL`。分层 YAML 格式沿用现有 `llmconfig`；配置中未提供 API key 时使用环境凭据，超时默认 60 秒。凭据不写入报告，也不通过命令行参数传递。

其他参数：`--suite FILE`、`--cases all|smoke|id,id`、`--groups memory,no-memory`、`--repeats N`、`--concurrency N`、`--output DIR`、`--max-calls N`。普通运行默认重复 2 次；真实运行默认只选 smoke、重复 1 次。超出模型调用预算会报告中止，不自动增加预算。

## 运行与评分方式

1. 每个场景／组／重复创建独立的内存 Store 和多个 Agent 状态。
2. 按剧本推进游戏时钟，将完整对话交给生产 `extractConversationCommitments`，复用其模型提示、Schema、时间换算和持久化逻辑。步骤串行完成，不等待真实游戏时间。
3. 在写入／更新检查点匹配记录；记录不按随机生成的数据库 ID 评分。
4. 每次决策前重建 Agent，清除会话、滚动摘要、守约 hint 和旧计划，保留本组 Store。使用生产 memoryReader 查询，再调用生产 `generateTacticalPlan`。
5. 无记忆组跳过写入并使用 NoopStore，当前任务、人设、状态和能力相同。原始对话和评分标签均不进入决策请求。

本版不启动 UE 执行动作，**use 评分只判断生成的工具调用意图，不证明游戏世界中实际履约**。能力声明、世界与人设使用固定小镇样本；不加载运行中的 World KB 或真实 NPC 会话。

| 阶段 | 规则评分 |
|---|---|
| write | 记录数量、类型、对象、地点、起止游戏时间与关键词；多条预期必须匹配不同记录 |
| update | 改期、取消、履约后的状态和时间；保留其他未变更事项 |
| read | 应出现与不得出现的文本；通过生产待办查询与格式化获得 |
| use | 至少一个允许的工具／参数组合，以及禁止的组合；不比较整段生成文本 |

全局对照使用 **use 行动断言通过率差**。完整场景通过数同时包含写入和检索，仅有记忆组适用这些阶段，不能直接当作公平的组间能力差。

## 测试集

固定套件位于 `agenttown-mcp/cmd/agenttown-mcp/testdata/memory_bench/suite.yaml`。共六类，每类 5 个场景：

- `creation`：跑步约定、传话、公开邀请、自己主持活动、多个事项。
- `time`：明天、三天后、跨午夜、绝对游戏日、深夜的明早；覆盖眼前人物相关的远期待办读取。
- `reschedule`：推迟、提前、更换地点、顺延一天、只修改其中一项。
- `closure`：取消、履约、取消其中一项、拒绝邀约、过期过滤。
- `retention`：重建 Agent、噪声 compact、跨日、重复 compact、无关新对话。
- `isolation`：未获知消息的角色、假设、他人私人事项、否定、两个拥有者。

冒烟固定选择 `creation-running`、`time-tomorrow`、`closure-cancel`、`retention-rebuild`、`isolation-owner`。收到邀请不等于承诺出席，相关场景不会要求 NPC 必须参加。未获知消息的角色也不会仅因随机选择了同一公共地点而被误判泄露；隔离主要依据记录和检索内容评分。

YAML 中 `steps` 仅包含可观察输入：时间、说话人和对话。`checks`、`probes.read`、`probes.actions` 是人工预期，只交给评分器。`replay` 是离线专用的固定抽取响应，真实模式完全忽略它；更新场景的回放也不伪造生产未提供的内部记录 ID。

步骤类型：

- `dialogue`：结束一段固定对话并提取待办。
- `advance`：推进游戏时钟。
- `restart`：重建该 NPC 状态，保留隔离存储。
- `compact`：把给定的干扰历史交给生产跨日 compact，之后清空会话。

检查点 `after` 从 1 开始。时间统一为 `{day: 1, time: '16:00'}`；排序、到期过滤和状态更新遵守现有 Store 契约。当前存储为忠实内存实现，**不验证 MySQL 的实际重启持久性或 SQL 性能**。

## 报告与失败排查

默认产物放在已被 Git 忽略的 `tmp/memory-bench/<时间-进程>/`：

- `summary.md`：分组与分类结果、各阶段断言、token、调用耗时和用例状态。
- `results.json`：存储快照、检索文本、工具动作、实际模型请求和响应；HTTP 层脱敏，不记录请求头和服务地址。
- `cases.json`：该次运行的输入与人工标签，配合套件 SHA256 复现结果。

报告中的状态：`passed`、`ability_failed`、`service_error`、`budget_stopped`、`invalid_fixture`、`offline_validated`。服务错误、预算中止和非法配置使脚本非零退出；能力失败保留报告并正常退出，便于收集真实基线，而不是为了通过测试修改业务规则。

HTTP 硬预算在生产 Venus 客户端的可选 Transport 接缝上执行，包含 agentic loop 的重试与跳转。保留的请求只有合成测试内容，仍应按内部调试产物保管，不提交完整原始报告。

## 当前已知业务限制

- 对话结束抽取要求模型输出已有事项的 ID，但当前请求没有注入已有事项清单。取消、履约和改期可能无法关联已有记录，或被重复写成新事项。Bench 不把内部 ID 塞入对话来绕过此问题。
- 当前 `UpdateCommitment` 接口只更新状态与起止时间，不能修改地点。因此“只改地点”的完整正确性预期可能失败。
- 检索有意过滤过期 pending，而不自动回写 `expired`；相应用例保留 pending 的存储预期。
- 规则评分中的关键词并非完整语义裁判；疑似同义表达导致的误判须结合请求／响应人工复核，再改进标注。

## 开发验证

```bash
cd agenttown-mcp
go test ./... -count=1
go test ./cmd/agenttown-mcp -run '^TestBench' -race -count=1
go build -o /tmp/agenttown-memory-bench-build ./cmd/agenttown-mcp
```

普通 `go test` 不会调用真实模型；显式评测入口是 `TestMemoryBench_Run`。业务修复不属于本次 Bench 建设范围，应另开提交，并保留修复前基线。

## 首次真实冒烟基线（2026-10-28）

5 个固定冒烟场景 × 两组 × 1 次，串行完成，17 次真实 HTTP 请求，无服务错误、无预算中止。此前配置接入检查消耗 10 次鉴权失败请求；修正评测配置的环境变量回退后，以剩余 20 次预算重跑，总计 27 次，没有运行全量真实评测。

| 场景 | 有记忆 | 无记忆 | 观察 |
|---|---|---|---|
| 新增约定 | 通过 | 行动失败 | 有记忆时生成前往中央广场的完整动作 |
| 明日时间换算 | 行动失败 | 行动失败 | 日期、写入和检索正确，但未生成满足约束的行动 |
| 取消 | 更新、检索失败 | 通过 | 原约定仍为 pending，随后仍被检索到 |
| 清空会话后读取 | 行动失败 | 行动失败 | 记录保留且检索正确，模型表达赴约意图，但 move_to 缺少 target_id |
| 角色隔离 | 通过 | 通过 | 未收到消息的 H-03 没有获得 H-01 的约定 |

有记忆组写入断言 11/11，更新 3/4，检索 8/10；适用的行动断言有记忆 1/3、无记忆 0/3。取消和隔离的无记忆“通过”主要表示未发生不应有的读取，不应解读为完整记忆能力。样本量小且只重复一次，以上结果仅作为缺陷定位基线。

该次报告位于 `tmp/memory-bench/live-smoke-verified-config/`，完整离线 30 场景、两组、重复 2 次的 120 次运行位于 `tmp/memory-bench/offline-30/`；原始产物均不提交 Git。
