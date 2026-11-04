# AgentTown 部署启动指南

## 双仓库结构简介

`/data/workspace` 下有 dev 和 stable 两个目录，是**同一工蜂仓库的两个独立 clone**，端口、数据库、日志全隔离，可同时运行互不干扰，目的是随时保留一个稳定版本运行的服务以供联调。

| | dev | stable |
|---|---|---|
| 目录 | `/data/workspace/dev` | `/data/workspace/stable` |
| 分支 | `dev-working` | `master` |
| 端口 | 8770（HTTP）/ 9093（WS） | 8760（HTTP）/ 9092（WS） |
| 数据库 | `agenttown_dev` | `agenttown_stable` |
| 日志 | `logs-dev/` | `logs/` |

dev 工作区的主分支是 `dev-working`，新任务另开 feature/fix 分支，完成后合回 dev-working。

**使用方式**：日常开发在 dev 下进行（改代码、`--drop-tables` 重启、看日志调试）；代码稳定后通过工蜂 MR 从 feature 分支合入 `master`，再到 stable 下 pull 更新并重启，可以进行联调验证。

**git 提交与拉取**（在 dev 目录下）：

```bash
cd /data/workspace/dev

# 提交：新任务建议开分支（feature/xxx 或 fix/xxx）
git checkout -b feature/xxx

git add <文件>
git commit -m "fix(mcp): 一句话说明改了什么"    # 格式 <type>(<scope>): <中文描述>
git push -u origin HEAD          # 开了 feature/fix 分支时：推当前分支（-u 建立跟踪，之后 git push 即可）
# 推送后到工蜂网页（git.woa.com → smartnpc → 合并请求 → 新建）提 MR：dev-working（或 feature 分支）→ master
```

**更新 stable 供联调**（master 有新合入时）：

```bash
cd /data/workspace/stable
git pull
bash start-debug.sh --drop-tables               # 重启生效
```

## 步骤 1：进入云开发容器

通过 AnyDev 打开本项目工作区 smartnpc，用SSH连接，用合适的IDE打开。
如果已打开，则可以跳过步骤2，项目已经放在开发容器的 /data/workspace/dev 和 /data/workspace/stable下。
此工作区当前的IP域名：`ssh root@21.214.99.100.devcloud.woa.com -p 36000`

如果需要新建云开发容器并在其中进行开发，则需要进行下面的步骤2。

## 步骤 2：克隆两个仓库（仅首次）

```bash
cd /data/workspace
git clone https://git.woa.com/yitianchen/smartnpc.git stable
cd stable && git checkout master && cd ..
git clone https://git.woa.com/yitianchen/smartnpc.git dev
cd dev && git checkout dev-working && cd ..
```

**新建的容器还需安装 MySQL**（rpm 包装在容器系统层，不随数据盘保留；验证：`command -v mysqld`，已有输出则跳过）：

```bash
dnf install -y mysql-server
```

装完零配置：start 脚本自动初始化数据目录、拉起 mysqld、按实例建库；**表也不用建**——MCP 启动时自动执行内嵌 migrations 建表（`schema_migrations` 跟踪增量，`--drop-tables` 重置后同样自动重建）。数据在 `/data/mysql-data`（持久化盘），同一工作区重建容器不丢；全新工作区从零开始。

## 步骤 3：配置 .env（仅首次，两个目录各一份）

```bash
cp .env.example /data/workspace/stable/.env
cp .env.example /data/workspace/dev/.env
```
编辑两个 .env，填入 VENUS_API_KEY=sk-xxx（必需，缺失 MCP 启动即退）

可以访问 https://ai.woa.com/#/manager/api-account 获取API。

## 步骤 4：启动 stable 实例（端口 8760/9092）

```bash
cd /data/workspace/stable
bash start-debug.sh --drop-tables
```

## 步骤 5：启动 dev 实例（端口 8770/9093）

```bash
cd /data/workspace/dev
bash start-dev.sh --drop-tables
```

> `--drop-tables`：重置数据库（DROP + CREATE）,日常联调都用它启动；不带则保留上次数据。

启动脚本自动完成：停旧进程 → 起 MySQL → （--drop-tables 时）重置库 → 编译 MCP → 启动并等健康检查。

## 步骤 6：验证启动成功

```bash
curl -s http://localhost:8760/healthz   # stable
curl -s http://localhost:8770/healthz   # dev
```

## 步骤 7：UE 连接

在 UE 打开 AgentTown 地图，在项目设置中配置 WS 地址指向容器：

- stable：`ws://<容器IP>:9092/ws`
- dev：`ws://<容器IP>:9093/ws`

当前工作区IP是ws://21.214.99.100:9093/ws （dev端）。

容器 IP 查法（启动完成的 summary 里也会打印）：

```bash
hostname -I
```

连接成功的标志（日志里出现 UE 连接消息序列）：

```bash
grep 'agent_registered' /data/workspace/dev/logs-dev/$(date +%Y-%m-%d)/debug-mcp.log
```

## 步骤 8：打开 debug 页面

浏览器直接访问：

- dev：`http://21.214.99.100:8770/debug/`
- stable：`http://21.214.99.100:8760/debug/`

如果重新新建了开发容器，则换成对应容器的IP。

## 步骤 9：查看日志

日志按启动日期分目录，路径为 `<实例目录>/logs(-dev)/YYYY-MM-DD/debug-mcp.log`：

- dev 实例：`/data/workspace/dev/logs-dev/2026-11-04/debug-mcp.log`
- stable 实例：`/data/workspace/stable/logs/2026-11-04/debug-mcp.log`

## 步骤 10：查看 actual_prompts 与 LLM 指标

- LLM 请求体留存（H-01 最新一次完整请求：战略/战术/对话/路由）：
  - dev 实例：`/data/workspace/dev/docs/actual_prompts.md`
  - stable 实例：`/data/workspace/stable/docs/actual_prompts.md`
- LLM 调用指标（延迟分位/重试率/错误分布）：
  - dev 实例：`/data/workspace/dev/docs/llm_metrics.md`
  - stable 实例：`/data/workspace/stable/docs/llm_metrics.md`

> 两者随仿真运行实时覆盖写入（不入库）。

## 步骤 11：停止

```bash
bash /data/workspace/dev/start-dev.sh --stop
bash /data/workspace/stable/start-debug.sh --stop
```

> 停止不影响 MySQL；要重置数据下次启动加 `--drop-tables` 即可。

## 附：MySQL 直连（查记忆/调度数据）

```bash
mysql --socket=/data/mysql-run/mysql.sock -uroot agenttown_dev    # dev 库
mysql --socket=/data/mysql-run/mysql.sock -uroot agenttown_stable # stable 库
```

## 附：配置 LLM 后端切换（可选，llm_backend）

默认不需要配置：不设 `LLM_CONFIG` 时 MCP 直连 Venus（读 `.env` 的 `VENUS_API_KEY`）。只有要把某一层切到自建 vLLM/SGLang 或其他 OpenAI 兼容服务时才配置。

1. 复制模板（实际配置文件已 gitignore，不入库）：

```bash
cp assets/llm_backend.example.yaml assets/llm_backend.yaml
```

2. 编辑 `assets/llm_backend.yaml`，按层填后端——战略层（每日计划，一天一次，可用较重模型）与战术层+对话层（高频，复用同一客户端）可分别指向不同服务：

```yaml
strategic:
  base_url: "http://v2.open.venus.oa.com/llmproxy"   # 服务根地址，不带 /v1 后缀（客户端自动拼 /v1/chat/completions）
  api_key: ""          # 留空则读 VENUS_API_KEY 环境变量
  model: "deepseek-v4-pro"
  timeout_sec: 120

tactical:
  base_url: "http://<自建服务host>:<port>"            # 自建服务也可用 host+port 两个字段
  api_key: ""          # 自建服务无鉴权可留空
  model: "Qwen3-8B-AWQ"
  timeout_sec: 120
```

3. 在 `.env` 里启用（start 脚本读取并透传 `--llm-config`）：

```
LLM_CONFIG=assets/llm_backend.yaml
```

4. 重启实例生效：

```bash
bash start-dev.sh --drop-tables
```

注意：
- DeepSeek 官方 thinking 模型（`deepseek-flash`/`deepseek-reasoner`）不支持强制 `tool_choice=required`，给该层加 `tool_choice: "auto"`
- 真实 api_key 不要提交入库（`llm_backend.yaml` 已 gitignore）
- 不想用时删掉 `.env` 里的 `LLM_CONFIG` 行即可回到 Venus 默认路径
