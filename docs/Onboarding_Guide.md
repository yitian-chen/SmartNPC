# AgentTown 部署启动指南

## 步骤 1：进入云开发容器

通过 AnyDev 打开本项目工作区 smartnpc，用SSH连接，用合适的IDE打开。
如果已打开，则可以跳过步骤2，项目已经放在开发容器的 /data/workspace/dev 和 /data/workspace/stable下。
此工作区当前的IP域名：`ssh root@21.214.99.100.devcloud.woa.com -p 36000`

如果需要新建容器并在其中进行开发，则需要进行下面的步骤2。

## 步骤 2：克隆两个仓库（仅首次）

```bash
cd /data/workspace
git clone https://git.woa.com/yitianchen/smartnpc.git stable
cd stable && git checkout master && cd ..
git clone https://git.woa.com/yitianchen/smartnpc.git dev
cd dev && git checkout dev-working && cd ..
```

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

```bash
# H-01 最新一次完整 LLM 请求体（战略/战术/对话/路由）
less /data/workspace/dev/docs/actual_prompts.md

# LLM 调用表现（延迟分位/重试率/错误分布）
less /data/workspace/dev/docs/llm_metrics.md
```

> 两者随仿真运行实时覆盖写入；是 git 跟踪的运行时产物，提交前注意别把仿真残留带进无关 commit。

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
