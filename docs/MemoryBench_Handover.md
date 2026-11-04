# 记忆评测交接文档

> 框架完整说明（参数/场景格式/评分/排查）见 [AgentTown_MemoryBench.md](AgentTown_MemoryBench.md)。

## 一、现状

待办记忆（`episodic_commitments`）评测框架与 30 场景测试集已完成并合入 master（MR !75）。跑过一次 5 场景真实冒烟基线，暴露出两个**未修复**的业务缺陷（见第四节）。

> **接下来的重点**：现有框架只是"能跑起来的管线"，现有 30 个场景和评判标准是初版，还不足以衡量记忆效果。下一步需要**先明确对记忆效果的期望，再据此仔细编写评测集和评判标准**（见第五节）。

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
scripts/run-memory-bench.sh --live --cases <id,id>  # 真实模型跑指定场景（单次最多 30 次 HTTP，全量需分批）
scripts/run-memory-promptfoo.sh --live        # 跑完导入 Promptfoo 查看器
```

### 在本地打开 Promptfoo 可视化页面

1. 首次使用先装依赖（仅一次）：

   ```bash
   cd /data/workspace/dev/eval/memory-promptfoo && npm ci
   ```

2. 导入报告（已有报告直接导入，不调用模型）：

   ```bash
   cd /data/workspace/dev
   scripts/run-memory-promptfoo.sh --report tmp/memory-bench/live-smoke-verified-config
   ```

3. 开发机上启动查看器（前台运行，Ctrl+C 停止；只监听开发机 `127.0.0.1:15500`）：

   ```bash
   scripts/run-memory-promptfoo.sh view
   ```

4. 本机另开终端建 SSH 隧道（地址换成自己容器的）：

   ```bash
   ssh -N -L 15500:127.0.0.1:15500 root@21.214.99.100.devcloud.woa.com -p 36000
   ```

5. 本机浏览器打开 `http://localhost:15500`。每行是一个场景，两列分别是 memory / no-memory；点开结果可看各阶段得分和失败的断言。

> 两组的总通过率不能直接比较（无记忆组跳过了 write/update/read），比较行动差异看 use 断言。离线回放的结果显示绿色只代表管线跑通，不代表模型能力。更多说明见 `eval/memory-promptfoo/README.md`。

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

## 五、下一步：按期望重写评测集与评判标准

现有场景集与评分规则是为验证框架而写的初版，评分以关键词匹配为主，不能代表"记忆好不好用"。后续建议按以下顺序推进：

1. **先定义期望**：明确希望记忆在仿真中带来什么效果——NPC 应该记住什么（约定、他人转告、对人印象、过往经历）、在什么时机想起来、想起来之后应该做出什么行为、哪些信息不该记住或不该泄露给别人。把期望写成可检验的条目，作为评测集的设计依据
2. **按期望编写评测集**：每条期望对应若干场景，覆盖正常路径与易错边界（时间换算、改期/取消、跨日、多人交叉、噪声对话）；场景对话要贴近仿真里 NPC 的真实说话方式，而不是人工造的标准句
3. **按期望编写评判标准**：每个场景写清楚"怎样算通过"——存储结果、检索内容、最终行为分别要满足什么；关键词评分不足以判断语义的地方，考虑引入 LLM 裁判或人工标注
4. **用新评测集重跑基线**：修复第四节两个缺陷前后各跑一次全量真实评测，对比结果，确认评测确实能反映记忆效果的变化
