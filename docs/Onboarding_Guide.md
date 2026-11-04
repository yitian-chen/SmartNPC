# AgentTown 部署启动指南

> FIXME（交接时补充）：AnyDev 工作区的申请/打开入口 URL 与账号流程。

## 步骤 1：进入云开发容器

通过 AnyDev 打开本项目工作区（远程 Linux 容器）。

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
# 编辑两个 .env，填入 VENUS_API_KEY=sk-xxx（必需，缺失 MCP 启动即退）
```

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

> `--drop-tables`：重置数据库（DROP + CREATE），migrations 从零重建——日常联调都用它启动；不带则保留上次数据。

启动脚本自动完成：停旧进程 → 起 MySQL → （--drop-tables 时）重置库 → 编译 MCP → 启动并等健康检查。

## 步骤 6：验证启动成功

```bash
curl -s http://localhost:8760/healthz   # stable
curl -s http://localhost:8770/healthz   # dev
```

## 步骤 7：UE 连接

UE 同事打开 AgentTown 地图工程，WS 地址指向容器：

- stable：`ws://<容器IP>:9092/ws`
- dev：`ws://<容器IP>:9093/ws`

容器 IP 查法（启动完成的 summary 里也会打印）：

```bash
hostname -I
```

连接成功的标志（日志里出现 UE 连接消息序列）：

```bash
grep 'agent_registered' /data/workspace/dev/logs-dev/$(date +%Y-%m-%d)/debug-mcp.log
```

## 步骤 8：打开 debug 页面

本地浏览器需先能访问容器端口（任选其一）：

```bash
# 方式一：AnyDev 自带端口转发/预览（优先）
# 方式二：SSH 隧道
ssh -L 8770:localhost:8770 -L 8760:localhost:8760 <容器>
```

然后打开：

- dev：`http://localhost:8770/debug/`
- stable：`http://localhost:8760/debug/`

## 步骤 9：查看日志

```bash
# dev 实例（stable 换成 logs/ 目录）
LOG=/data/workspace/dev/logs-dev/$(date +%Y-%m-%d)/debug-mcp.log

# HTML 报告（可折叠/搜索/过滤）
cd /data/workspace/dev && python scripts/pretty_log.py --html

# 裸 grep
grep '队列已填充' $LOG            # 战术层每次分解的动作序列
grep '下发 action' $LOG           # 实际下发给 UE 的动作
grep 'TACTICAL-PROMPT' $LOG       # 战术层发给 LLM 的 prompt
grep 'speech' $LOG                # 对话链路
grep '事件路由' $LOG              # 事件判决
grep 'error' $LOG                 # 错误
```

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
