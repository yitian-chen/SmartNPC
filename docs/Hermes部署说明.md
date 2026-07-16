# Hermes Agent 部署说明

> 基于 SmartNPC 项目的 Hermes 部署实践。
> **部署方式**：WSL + Docker。Profile 文件放在 Windows 侧（仓库内），通过 volume 挂载进容器。
> 一期目标：部署 1 个 NPC（H-01 老陈），打通 Mock UE → Hermes Gateway → LLM 决策闭环。

---

## 一、部署架构

```
Windows 文件系统                           WSL (Docker)
┌─────────────────────────┐       ┌─────────────────────────────────┐
│                          │       │                                  │
│  AgentTown_v3/           │       │  Mock UE Bridge                  │
│  ├── hermes/             │       │    │                             │
│  │   └── profiles/       │ mount │    │ HTTP POST                   │
│  │       └── H-01/       │──────▶│    ▼                             │
│  │           ├── SOUL.md  │       │  ┌───────────────────────────┐  │
│  │           ├── SKILL.md │       │  │ Container: h01-gateway      │  │
│  │           ├── MEMORY.md│       │  │                            │  │
│  │           └── config/  │       │  │ Hermes Gateway :8642       │  │
│  │                        │       │  │  ├── SOUL.md (volume)      │  │
│  ├── .env                 │       │  │  ├── MEMORY.md (volume)    │  │
│  └── docker/              │       │  │  └── config.yaml (volume)  │  │
│      └── docker-compose.yml       │  └───────────┬───────────────┘  │
│                          │       │              │ HTTP              │
└─────────────────────────┘       │              ▼                   │
                                  │         LLM API                   │
                                  │     (Claude / GPT)                │
                                  └──────────────────────────────────┘
```

**关键设计**：
- Profile 源文件全部放在仓库 `hermes/profiles/H-01/` 中（Windows 侧），通过 Docker volume 挂载进容器，不复制。
- 修改 SOUL.md / SKILL.md → 重启容器即生效，无需手动同步。
- MEMORY.md 是运行时产物，挂载出来可以持久化；不进 git。

---

## 二、环境准备

### 2.1 WSL 中安装 Docker

```bash
# WSL 内（Ubuntu），安装 Docker Engine
sudo apt update
sudo apt install -y docker.io docker-compose-v2

# 将当前用户加入 docker 组（免 sudo）
sudo usermod -aG docker $USER
# 退出 WSL 重新进入生效
```

或在 Windows 侧安装 Docker Desktop 并启用 WSL 2 集成。

验证：

```bash
docker --version
docker compose version
```

### 2.2 准备 .env（LLM API Key）

在仓库根目录创建 `.env`（不进 git，已在 `.gitignore` 中）：

```env
# d:/AgentTown_v3/.env
ANTHROPIC_API_KEY=sk-ant-xxx
# 或用 OpenAI 兼容接口：
# OPENAI_API_KEY=sk-xxx
# OPENAI_BASE_URL=https://api.openai.com/v1
```

LLM 密钥通过 docker-compose 的 `env_file` 注入容器。

---

## 三、Docker 部署配置

### 3.1 Hermes 镜像构建

Hermes 自带官方 Dockerfile，使用 `docker/build-hermes.sh` 从本地 Hermes 源码构建：

```bash
# WSL 中执行，需要 Hermes 源码已 clone 到本地
bash docker/build-hermes.sh
```

> 镜像名 `hermes-agent:latest`。Hermes 源码默认路径为 Windows 侧 `C:\Users\<user>\AppData\Local\hermes\hermes-agent\`，在 WSL 中自动映射为 `/mnt/c/Users/<user>/AppData/Local/hermes/hermes-agent/`。可设置 `HERMES_SOURCE` 环境变量指向其他路径。

Hermes 官方镜像特性：
- s6-overlay 进程管理
- 非 root 用户运行（UID 10000），支持 `HERMES_UID`/`HERMES_GID` 重映射
- 数据目录 `/opt/data`（持久化卷）
- Profile 路径 `/opt/data/profiles/<name>/`

### 3.2 仓库目录结构

```
AgentTown_v3/
├── .env                          ← LLM 密钥（不进 git）
├── docker/
│   ├── docker-compose.yml        ← 一期单 Agent 编排
│   ├── docker-compose.multi.yml  ← 后续期多 Agent 编排（待创建）
│   ├── build-hermes.sh           ← 构建 Hermes 镜像脚本
│   └── setup.sh                  ← 一键部署脚本
└── hermes/
    └── profiles/
        └── H-01/
            ├── SOUL.md           ← 人格（签入 git）
            ├── SKILL.md          ← 技能（签入 git）
            ├── MEMORY.md         ← 记忆（运行时，不签入）
            └── config.yaml       ← 配置（签入 git，密钥走 .env）
```

### 3.3 docker-compose.yml（一期）

```yaml
# docker/docker-compose.yml
version: "3.8"

services:
  h01-gateway:
    image: hermes-agent:latest
    container_name: agenttown-h01
    ports:
      - "${HERMES_PORT:-8642}:8642"
    volumes:
      # Hermes 持久数据（记忆、日志等）
      - hermes-data:/opt/data
      # 挂载 Profile 文件（只读）
      - ../hermes/profiles/H-01:/opt/data/profiles/H-01:ro
    environment:
      - HERMES_AGENT_URL
      - HERMES_AGENT_API_KEY
      - HERMES_AGENT_MODEL
      - HERMES_PORT
    command:
      - "-p"
      - "H-01"
      - "gateway"
      - "run"
      - "--accept-hooks"
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8642/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 30s

volumes:
  hermes-data:
    name: agenttown-hermes-data
```

**关键说明**：
- Profile 文件通过 volume 挂载为只读（`:ro`），MEMORY.md 和日志写入 `hermes-data` 持久卷
- LLM 配置通过 `HERMES_AGENT_*` 环境变量传入，在 `config.yaml` 中通过 `${VAR}` 引用
- Hermes 的 main-wrapper 自动将 command 路由为 `hermes -p H-01 gateway run --accept-hooks`

---

## 四、一期部署流程

### 方式 A：一键部署

```bash
# WSL 中
cd /mnt/d/SmartNPC_v3
bash docker/setup.sh
```

### 方式 B：手动分步

#### 步骤 1：构建 Hermes 镜像

```bash
bash docker/build-hermes.sh
```

#### 步骤 2：启动容器

```bash
docker compose -f docker/docker-compose.yml up -d
```

#### 步骤 3：查看日志

```bash
docker compose -f docker/docker-compose.yml logs -f
```

#### 步骤 4：验证

```bash
# 健康检查
curl http://localhost:8642/health

# 测试对话
curl -s -X POST http://localhost:8642/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"message":"老陈，今天该做什么？"}'
```

---

## 五、日常操作

### 5.1 修改 profile 后重启

```bash
# 修改了仓库中 SOUL.md / SKILL.md / config.yaml 后
docker compose -f docker/docker-compose.yml restart h01-gateway
```

Profile 文件通过 volume 实时挂载，无需重新构建镜像。

### 5.2 查看日志

```bash
# 跟随日志
docker compose -f docker/docker-compose.yml logs -f h01-gateway

# 最近 50 行
docker compose -f docker/docker-compose.yml logs --tail 50 h01-gateway
```

### 5.3 停止和清理

```bash
# 停止
docker compose -f docker/docker-compose.yml down

# 停止并删除持久数据（重置 MEMORY.md）
docker compose -f docker/docker-compose.yml down -v
```

---

## 六、交互协议（一期内部）

### 6.1 Mock UE → Hermes Gateway

Mock UE Bridge 通过 HTTP POST 向 Gateway 推送感知和事件。端口映射后，Mock UE 在 Windows 侧通过 `localhost:8642` 访问容器内的 Hermes：

```bash
POST http://localhost:8642/v1/responses
Content-Type: application/json

{
  "type": "perception_update",
  "agent_id": "H-01",
  "game_time": "day1 08:00",
  "payload": {
    "position": [200, 100, 0],
    "current_zone": "main_workshop",
    "visible_agents": [],
    "nearby_objects": [
      {"id": "workbench_01", "distance": 3.0, "state": "idle"}
    ]
  }
}
```

### 6.2 每日启动协议

```
1. Mock UE 启动（Python 脚本，Windows 或 WSL 内均可）
2. Mock UE → POST localhost:8642/v1/responses
   内容："现在是 [game_time]，今日开始，请规划今天要做的事。"
3. Hermes 加载 MEMORY.md + SOUL.md
4. Hermes → LLM 生成 Daily Plan → 返回响应
5. 进入执行循环：感知推送 → 决策 → 动作输出 → 等待完成 → 下一条
```

### 6.3 Hermes 动作输出（一期回传方式）

一期无 MCP Server，Hermes 决策后的动作输出有两种方式：

**方式 A**（推荐）：Mock UE 作为 HTTP Server，Hermes Gateway 以 webhook 方式 POST 动作到 Mock UE。

**方式 B**：Hermes 将动作写入 volume 挂载的文件（`hermes/profiles/H-01/actions.json`），Mock UE 轮询读取。

> 具体方式取决于 Hermes Gateway 的 action 输出机制，需实测后确定。

---

## 七、多 NPC 扩展（后续期）

后续期使用 `docker-compose.multi.yml`，每个 NPC 一个独立容器：

```yaml
# docker/docker-compose.multi.yml
version: "3.8"

services:
  h01-gateway:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    container_name: h01-gateway
    ports:
      - "8641:8641"
    volumes:
      - ../hermes/profiles/H-01:/home/hermes/.hermes/profiles/H-01
    env_file:
      - ../.env
    environment:
      - HERMES_PROFILE=H-01
      - HERMES_PORT=8641
    restart: unless-stopped

  hermes-h02:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    container_name: hermes-h02
    ports:
      - "8642:8642"
    volumes:
      - ../hermes/profiles/H-02:/home/hermes/.hermes/profiles/H-02
    env_file:
      - ../.env
    environment:
      - HERMES_PROFILE=H-02
      - HERMES_PORT=8642
    restart: unless-stopped

  # ... H-03 到 H-10 类似
```

```bash
# 启动全部
docker compose -f docker/docker-compose.multi.yml up -d

# 启动指定几个
docker compose -f docker/docker-compose.multi.yml up -d h01-gateway hermes-h02
```

---

## 八、Profile 与版本管理

### 8.1 仓库签入策略

| 文件 | 签入 git | 说明 |
|------|:--:|------|
| `hermes/profiles/H-01/SOUL.md` | **是** | 手写人格，团队共享 |
| `hermes/profiles/H-01/SKILL.md` | **是** | 技能声明，团队共享 |
| `hermes/profiles/H-01/config.yaml` | **是** | 模板配置（不含密钥） |
| `hermes/profiles/H-01/.env.example` | **是** | 变量说明 |
| `hermes/profiles/H-01/MEMORY.md` | **否** | 运行时产物，每机独立 |
| `hermes/profiles/H-01/logs/` | **否** | 运行时日志 |
| `docker/build-hermes.sh` | **是** | 镜像构建脚本 |
| `docker/docker-compose.yml` | **是** | 部署编排 |
| `docker/setup.sh` | **是** | 一键部署脚本 |
| `.env` | **否** | 真实 API Key |

### 8.2 修改生效流程

```
修改 SOUL.md / SKILL.md / config.yaml
  → docker compose restart h01-gateway
  → 容器内 Hermes 重启，新配置生效
```

> MEMORY.md 不进仓库，每台机器独立累积。如果要分享"某个 NPC 已经经历了什么"，需单独设计记忆导出/导入机制（不在第一期范围）。

---

## 九、常见问题

### Q1：容器启动后立刻退出

查看日志定位：

```bash
docker compose -f docker/docker-compose.yml logs h01-gateway
```

常见原因：`.env` 中 `HERMES_AGENT_API_KEY` 缺失或 `HERMES_AGENT_URL` 不可达。

### Q2：`localhost:8642` 无法访问

确认容器正在运行且端口映射成功：

```bash
docker ps | grep agenttown-h01
# 应看到 0.0.0.0:8642->8642/tcp
```

如果容器正常运行但端口不通，检查 Hermes config.yaml 中 `API_SERVER_HOST` 是否为 `"0.0.0.0"`。

### Q3：修改 SOUL.md 后不生效

Volume 挂载实时同步文件内容，但 Hermes Gateway 在启动时加载 SOUL.md。修改后需重启容器：

```bash
docker compose -f docker/docker-compose.yml restart h01-gateway
```

### Q4：MEMORY.md 太大

Hermes MEMORY.md 有 2200 字符上限。超出后旧的低重要性记忆自然挤出。如需重置：

```bash
docker compose -f docker/docker-compose.yml down -v
docker compose -f docker/docker-compose.yml up -d
```

### Q5：如何更换 LLM 模型

编辑 `.env` 中的 `HERMES_AGENT_MODEL` 和 `HERMES_AGENT_URL`，重启容器：

```bash
docker compose -f docker/docker-compose.yml restart h01-gateway
```

### Q6：WSL 中 Docker 命令报 Permission Denied

```bash
# 确保当前用户在 docker 组中
groups | grep docker

# 如果没有，加进去
sudo usermod -aG docker $USER
# 退出 WSL 重新进入生效
```

---

## 十、一期核心检查清单

- [ ] WSL 中 Docker 可用：`docker --version` 正常
- [ ] 仓库根 `.env` 中 `HERMES_AGENT_*` 变量已配置且密钥正确
- [ ] `hermes/profiles/H-01/` 下 SOUL.md / SKILL.md / config.yaml 已准备
- [ ] Hermes 源码可用，`docker/build-hermes.sh` 能成功构建 `hermes-agent:latest`
- [ ] `docker compose -f docker/docker-compose.yml up -d` 容器正常运行
- [ ] `curl localhost:8642/health` 返回 200
- [ ] Mock UE Bridge 能 POST 到 `localhost:8642` 并驱动 Agent 决策

---

*本部署说明基于 SmartNPC 项目的 Hermes 部署实践整理，仅覆盖一期（单 Agent）需求。*
