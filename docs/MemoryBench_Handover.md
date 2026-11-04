# 记忆评测交接文档

> 待办记忆（episodic_commitments）评测体系的建设成果、已发现的业务缺陷与未完成事项。
> 评测框架的完整使用说明见 [AgentTown_MemoryBench.md](AgentTown_MemoryBench.md)（框架/场景/评分/报告）与 [eval/memory-promptfoo/README.md](../eval/memory-promptfoo/README.md)（查看器），本文不重复，只讲清楚"现在到哪了、接下来做什么"。

---

## 一、现状一句话

评测框架与 30 场景测试集**已建完**（`feature/memory-bench` 分支，3 个 commit），跑过一次 5 场景真实冒烟基线；**业务侧存在两个评测已定位但未修复的记忆缺陷**（改期/取消无法关联旧记录、不能改地点），全量 30 场景真实基线未跑，**分支未推送远端、未提 MR**。

## 二、现有成果

### 1. Go 离线评测框架（`cmd/agenttown-mcp/memory_bench_*_test.go`，显式入口 `TestMemoryBench_Run`）

- **复用生产代码**：`extractConversationCommitments`（对话结束抽取）、`memoryReader`（检索）、`generateTacticalPlan`（决策）、跨日 compact——评测的就是生产链路本身，不是仿真替身。不启动 UE/WebSocket/worker
- **双组对照**：memory 组（真实内存 Store）vs no-memory 组（NoopStore + 跳过写入），人设/任务/状态/能力声明完全相同，隔离地对比"有记忆带来多少行为差异"
- **四阶段规则评分**：
  | 阶段 | 评什么 |
  |---|---|
  | write | 记录数量/类型/对象/地点/起止游戏时间/关键词 |
  | update | 改期/取消/履约后的状态与时间，且不得误伤其他事项 |
  | read | 生产待办查询结果中应出现/不得出现的文本 |
  | use | 决策产物的工具调用意图（允许组合 + 禁止组合），不比较整段文本 |
- **两种运行模式**：默认离线（`replay` 脚本化模型响应回放，零模型调用，验证管线）与 `--live` 真实模型（凭据走 `.env`/`LLM_CONFIG`，与仿真同源）
- **HTTP 硬预算**：生产 Venus 客户端加了可选 `Transport` 接缝（`pkg/venus/client.go`，生产默认 nil 零影响），预算在 Transport 层强制，含 agentic loop 重试
- **报告**：`tmp/memory-bench/<name>/` 下 `summary.md`（分组/分类/阶段断言/token/耗时）、`results.json`（存储快照/检索文本/动作/完整模型请求响应，HTTP 层脱敏）、`cases.json`（输入与标签，配套件 SHA256 可复现）。均不入库

### 2. 30 场景测试集（`testdata/memory_bench/suite.yaml`，2302 行）

六类各 5 个：`creation`（新增）/ `time`（时间换算：明天、三天后、跨午夜、绝对游戏日、深夜的明早）/ `reschedule`（改期）/ `closure`（取消/履约/过期）/ `retention`（重建 Agent/噪声 compact/跨日/重复 compact 后的记忆保持）/ `isolation`（未获知者不泄露：假设、他人私事、否定、双拥有者）。5 个固定冒烟场景（`--cases smoke`）。

场景结构上 `steps` 只含可观察输入（时间/说话人/对话文本），`checks`/`probes` 是人工预期只交给评分器——**标签不进模型请求，无泄漏**。

### 3. Promptfoo 查看与对比（`eval/memory-promptfoo/`）

- 固定版本 Promptfoo + 独立 Node 运行时（`npm ci`，不动系统 Node），本地实验数据库、关闭遥测、不向云端分享
- `scripts/run-memory-promptfoo.sh`：跑 Bench → 自动导入查看器；`--report` 导入既有报告**零模型调用**；`view` 起本地查看器（仅监听 127.0.0.1，`bind-local.cjs` 预加载强制回环），本机 SSH 隧道 15500 端口浏览
- 界面按场景×重复分行，memory/no-memory 两列对比，可看 stage_scores/失败 checks/probes/存储快照

### 4. 首次真实冒烟基线（2026-10-28，`tmp/memory-bench/live-smoke-verified-config/`）

5 冒烟场景 × 2 组 × 1 次，17 次真实 HTTP，无服务错误：

| 指标 | 有记忆组 | 无记忆组 |
|---|---|---|
| 写入断言 | 11/11 | —（跳过） |
| 更新断言 | 3/4 | — |
| 检索断言 | 8/10 | — |
| use 行动断言 | 1/3 | 0/3 |

场景级：新增约定 ✅（无记忆组行动失败——去了错误地方）；时间换算 ❌（写入检索对、行动不满足约束）；**取消 ❌（旧约定仍 pending 仍被检索到）**；会话清空后读取 ❌（move_to 缺 target_id）；角色隔离 ✅。

## 三、怎么跑（速查）

```bash
# 离线全量（30 场景 × 2 组 × 2 次 = 120 次回放，零模型调用）
scripts/run-memory-bench.sh

# 真实冒烟（默认 5 场景、2 组、1 次、最多 30 次 HTTP）
scripts/run-memory-bench.sh --live

# Promptfoo：跑 + 导入查看器
scripts/run-memory-promptfoo.sh --live

# 开发验证（框架自身测试）
cd agenttown-mcp && go test ./cmd/agenttown-mcp -run '^TestBench' -race -count=1
```

## 四、评测已定位的业务缺陷（未修复，接手后按此排障）

1. **抽取请求缺已有事项清单（最关键）**：`extractConversationCommitments` 的模型输出 Schema 要求携带已有事项的 ID 来核销改期/取消，但请求 prompt 里**没有注入该 NPC 当前的 pending commitments 清单**——模型无从得知 ID，导致 update 类操作无法关联旧记录、或把同一事项重复写成新记录。冒烟基线"取消"场景失败（旧约定仍 pending 仍被检索到）即此根因。**修复方向：抽取前查 `LoadCommitments(owner, pending)`，把清单（ID+内容+时间）注入抽取 prompt**
2. **`UpdateCommitment` 不支持改地点**：Store 接口只更新 status 与起止时间，"只改地点"场景（reschedule-location）的完整预期必然失败。修复方向：接口加 location 字段
3. 过期 pending 只在读时动态过滤、不回写 `expired` 状态（设计取舍，非 bug，但相关场景的存储预期按 pending 写）
4. use 评分只验证**工具调用意图**，不证明 UE 世界实际履约（框架未接 UE）
5. 评分关键词非完整语义裁判——同义表达误判需人工看 results.json 复核标注

另有仿真链路侧的守约缺陷（与评测框架无关但同属记忆闭环）：履约后二次赴约、未获应答被当接受等，已在 master（4f00f48/2becca2）修复，评测场景未覆盖这些路径。

## 五、尚未完成（行动项，按优先级）

1. **修复缺陷 1**（抽取注入已有事项清单）——收益最大：update 3/4 → 预期全绿，改期/取消/履约核销整条链路才真正闭环
2. **修复缺陷 2**（UpdateCommitment 支持改地点）——小改动，reschedule-location 场景解锁
3. **跑全量 30 场景真实基线**：缺陷修复后各跑一次（`--live --cases all`），形成"修复前 vs 修复后"对比；修复前基线只有 5 场景冒烟
4. **推送分支 + 提 MR**：`feature/memory-bench` 目前只在本地（基于 9a2fd67/master@!73），远端无此分支；合入前需 rebase 到最新 master（其间 master 已进 MR !74 及对话链路系列修复）
5. **处理 stash**：`stash@{0}` 是 suite.yaml 一处文案微调（场景对话"向小李H-03转告"→"向H-03转告"，去掉名字混写），pop 后并入分支即可
6. （可选，后续迭代）use 阶段接 UE 动作执行验证实际履约；MySQL Store 替换内存实现验证真实持久化/SQL 性能；语义评分（当前纯关键词）

## 六、资产清单

| 资产 | 位置 | 状态 |
|---|---|---|
| 评测框架 | `feature/memory-bench` 分支 `cmd/agenttown-mcp/memory_bench_*_test.go` | 完成，未推送 |
| 场景集 | 同分支 `testdata/memory_bench/suite.yaml` | 30 场景完成 |
| Promptfoo | 同分支 `eval/memory-promptfoo/`（node_modules 本地 npm ci，当前工作区已有残留） | 完成 |
| 脚本 | 同分支 `scripts/run-memory-bench.sh` / `run-memory-promptfoo.sh` | 完成 |
| 框架文档 | 同分支 `docs/AgentTown_MemoryBench.md` | 完成（含完整参数与排查） |
| 真实冒烟报告 | `tmp/memory-bench/live-smoke-verified-config/`（不入库） | 2026-10-28 基线 |
| 离线全量报告 | `tmp/memory-bench/offline-30/`（不入库） | 120 次回放 |
| WIP | `git stash@{0}`（suite.yaml 文案微调） | 未提交 |
