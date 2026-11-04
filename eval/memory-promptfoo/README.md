# Promptfoo 记忆评测入口

本目录提供固定版本 Promptfoo 和独立 Node 运行时，不修改系统 Node。Go Bench 仍负责场景执行、生产记忆调用、规则评分和全局 HTTP 硬预算；Promptfoo 负责本地存档、浏览和实验对比。

## 安装与运行

在开发机仓库根目录 `/data/workspace/dev`：

```bash
cd /data/workspace/dev/eval/memory-promptfoo
npm ci
cd /data/workspace/dev

# 默认：完整离线回放，然后导入 Promptfoo；不调用真实模型
scripts/run-memory-promptfoo.sh

# 导入已存在的真实报告，不重新运行 Agent、不调用模型
scripts/run-memory-promptfoo.sh --report tmp/memory-bench/my-smoke

# 重新跑五场景真实冒烟，然后导入（显式调用模型）
scripts/run-memory-promptfoo.sh --live

# 单场景真实回归，原 Go Bench 参数继续有效
scripts/run-memory-promptfoo.sh --live --cases closure-cancel --max-calls 5
```

新运行使用自动生成的独立输出目录，不能额外指定 `--output`。其他参数沿用 `scripts/run-memory-bench.sh`。`--report` 接受报告目录或 `results.json`，不能与运行参数混用。

Promptfoo 导入不触发任何 Agent 重试或额外模型调用；真实模型请求在导入前由一次 Go 运行统一执行，预算不会因可视化而增加。不自动启动服务，不向云端分享，关闭遥测。凭据仍只由 Go 读取现有环境配置，不写入 Promptfoo 配置。

## 在本机浏览

开发机上启动查看器（前台运行，Ctrl+C 停止）：

```bash
cd /data/workspace/dev
scripts/run-memory-promptfoo.sh view
```

本机另开 PowerShell 终端，建立隧道：

```powershell
ssh -N -L 15500:127.0.0.1:15500 npc-devcloud
```

本机浏览器打开 `http://localhost:15500`。查看器仅监听开发机 `127.0.0.1`，不需要对外开放端口。

固定版本的 viewer 没有 host 参数，因此入口仅在 viewer 进程预加载 `bind-local.cjs`，把 TCP listen 限定为回环地址；不修改 Promptfoo 依赖，也不影响 Go 或 eval 进程。

界面中：

- 每行是一个场景的一次重复，两列分别是 memory / no-memory。
- 在评测选择器切换不同运行；用 Failures、Errors 筛选能力失败与执行错误。
- 点开结果可查看 `stage_scores`、失败 `checks`、检索与动作 `probes`、存储快照 `snapshots`。
- 真实完整请求与响应保留在原始 Go `results.json`，避免在两个系统复制全部调试数据。
- 原模型 token、模型名称、耗时与调用数放在结果 metadata 的 `original*` 字段；界面的导入耗时不是模型延迟，导入不计为新模型 token 消耗。

默认 Promptfoo 总分是完整场景的二值通过结果。**两组总通过率不能直接比较**，因为无记忆组跳过写入、更新、检索。行动差异仍应看 Go `summary.md` 中适用的 use 断言，以及每条结果的阶段计数。N/A 阶段显示 total=0，不冒充能力通过。

离线运行的名称包含 OFFLINE，指标为 `OfflinePipelineOnly`，表示执行链路完成；即使回放行为断言失败也可能呈绿色，不代表真实模型能力。真实运行指标为 `ScenarioPass`。服务错误、预算中止、非法输入和缺失组合显示为 Error，不计为通过。

业务失败会使 Promptfoo 命令返回非零退出码，但结果仍保存，可正常打开查看器。缺失组合不会自动补跑，防止无意调用模型。

## 产物与测试

- 原 Bench 报告：`tmp/memory-bench/promptfoo-run-*/`。
- 生成的 Promptfoo 配置与 JSON：`tmp/memory-bench/promptfoo-import-*/`。
- 本地实验数据库：`tmp/memory-bench/promptfoo-state/`。
- 所有原始产物和 node_modules 均忽略，不提交 Git。删除 state 会删除界面历史。

```bash
cd eval/memory-promptfoo
npm test
```

接入不改变现有记忆业务规则，也不增加多轮 UE 仿真。模型版本、代码版本、场景内容或评分规则变化时，应结合原套件哈希与报告一起比较，避免把口径变化当作效果变化。

参考：[自定义 Provider](https://www.promptfoo.dev/docs/providers/custom-api/)、[规则断言](https://www.promptfoo.dev/docs/configuration/expected-outputs/javascript/)、[结果查看器](https://www.promptfoo.dev/docs/usage/web-ui/)。
