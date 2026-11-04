# 记忆评测交接文档

> 框架完整说明（参数/场景格式/评分/排查）见 [AgentTown_MemoryBench.md](AgentTown_MemoryBench.md)。

## 一、现状

待办记忆（`episodic_commitments`）评测框架与 30 场景测试集已完成并合入 master（MR !75）。跑过一次 5 场景真实冒烟基线，暴露出两个**未修复**的业务缺陷（见第四节）。

## 二、已有成果

| 内容 | 位置 | 说明 |
|---|---|---|
| 评测框架 | `agenttown-mcp/cmd/agenttown-mcp/memory_bench_*_test.go` | 入口 `TestMemoryBench_Run`；直接复用生产链路（对话抽取 → 检索 → 战术决策 → compact），不启 UE |
| 场景集 | `agenttown-mcp/cmd/agenttown-mcp/testdata/memory_bench/suite.yaml` | 30 场景，六类各 5：新增 / 时间换算 / 改期 / 取消履约过期 / 记忆保持 / 隔离 |
| 运行脚本 | `scripts/run-memory-bench.sh` | 离线回放（零模型调用）或 `--live` 真实模型 |
| 可视化 | `eval/memory-promptfoo/` + `scripts/run-memory-promptfoo.sh` | Promptfoo 本地查看器，memory / no-memory 两列对比 |
| 报告 | `tmp/memory-bench/<name>/`（不入库） | `summary.md` 汇总、`results.json` 明细 |

评测设计要点：
- **双组对照**：memory 组（有记忆）vs no-memory 组（无记忆），其余输入完全相同
- **四阶段评分**：

  | 阶段 | 评什么 |
  |---|---|
  | write | 写入的记录是否正确（对象/地点/时间） |
  | update | 改期/取消/履约后状态是否正确，且不误伤其他事项 |
  | read | 检索结果是否包含/排除应有内容 |
  | use | 决策产物的工具调用是否符合约定 |

## 三、怎么跑

```bash
scripts/run-memory-bench.sh                   # 离线全量（零模型调用，验证管线）
scripts/run-memory-bench.sh --live            # 真实模型冒烟（默认 5 场景）
scripts/run-memory-bench.sh --live --cases all  # 真实模型全量 30 场景
scripts/run-memory-promptfoo.sh --live        # 跑完导入 Promptfoo 查看器
```

## 四、首次基线与已知缺陷

2026-10-28 冒烟基线（5 场景，`tmp/memory-bench/live-smoke-verified-config/`）：

| 指标 | 有记忆 | 无记忆 |
|---|---|---|
| write | 11/11 | — |
| update | 3/4 | — |
| read | 8/10 | — |
| use | 1/3 | 0/3 |

评测定位的业务缺陷（**未修复**）：

1. **抽取时没有注入已有待办清单（最关键）**：`extractConversationCommitments` 要求模型用已有事项 ID 做改期/取消，但 prompt 里没给当前 pending 清单，模型无从关联——取消场景失败（旧约定仍 pending、仍被检索到）即此原因。
   修复方向：抽取前 `LoadCommitments(owner, pending)`，把 ID + 内容 + 时间注入抽取 prompt。
2. **`UpdateCommitment` 不能改地点**：只能改状态和时间，"只改地点"的改期场景必然失败。
   修复方向：接口加 location 字段。

评测本身的局限：use 只看工具调用意图、不验证 UE 实际履约；评分基于关键词，同义表达可能误判，需人工看 `results.json` 复核。

## 五、待办（按优先级）

1. 修复缺陷 1
2. 修复缺陷 2
3. 跑全量 30 场景真实基线（`--live --cases all`），与修复前对比
4. `git stash@{0}`：suite.yaml 一处文案微调（"向小李H-03转告"→"向H-03转告"），pop 后提交即可
