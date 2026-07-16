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
│  │           ├── SKILL.md │       │  │ Container: hermes-h01      │  │
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

### 3.1 docker-compose.yml

仓库目录结构：

```
AgentTown_v3/
├── .env                          ← LLM 密钥（不进 git）
├── docker/
│   ├── docker-compose.yml        ← 一期单 Agent 编排
│   ├── docker-compose.multi.yml  ← 后续期多 Agent 编排
│   └── Dockerfile                ← Hermes 容器镜像（如需自构建）
└── hermes/
    └── profiles/
        └── H-01/
            ├── config.yaml       ← Hermes 运行配置
            └── .env.example      ← 变量说明
```

**docker-compose.yml（一期）**：

```yaml
# docker/docker-compose.yml
version: "3.8"

services:
  hermes-h01:
    # 如果 Hermes 有官方镜像，直接用
    # image: hermes/hermes:latest
    # 否则用自构建镜像
    build:
      context: ..
      dockerfile: docker/Dockerfile
    container_name: hermes-h01
    ports:
      - "8642:8642"
    volumes:
      # 挂载 profile 目录（SOUL.md / SKILL.md / MEMORY.md 均在此）
      - ../hermes/profiles/H-01:/home/hermes/.hermes/profiles/H-01
    env_file:
      - ../.env
    environment:
      - HERMES_PROFILE=H-01
      - HERMES_PORT=8642
    restart: unless-stopped
```

### 3.2 config.yaml（Hermes 运行配置）

放在 `hermes/profiles/H-01/config.yaml`，签入 git（不含密钥）：

```yaml
# hermes/profiles/H-01/config.yaml

# Agent 模型
agent:
  model: claude-3.5-sonnet

# LLM provider（密钥从环境变量读取）
hermes_agent:
  url: "https://api.anthropic.com/v1"
  api_key: "${ANTHROPIC_API_KEY}"
  model: claude-3.5-sonnet

# API 服务器（Gateway 模式）
api_server:
  enabled: true
  port: 8642
  host: "0.0.0.0"      # 容器内必须绑定 0.0.0.0

# MCP 服务器（一期为空）
mcp_servers: []
```

### 3.3 Dockerfile（如需自构建）

```dockerfile
# docker/Dockerfile
FROM python:3.12-slim

# 安装系统依赖
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    git \
    && rm -rf /var/lib/apt/lists/*

# 创建 hermes 用户
RUN useradd -m -s /bin/bash hermes

# 安装 Hermes（替换为实际安装方式）
# 方式 A：官方安装脚本
# RUN curl -fsSL https://hermes.example.com/install.sh | bash

# 方式 B：pip 安装（如果 Hermes 发布到 PyPI）
# RUN pip install hermes-agent

# 方式 C：从 GitHub 安装
# RUN pip install git+https://github.com/hermes/hermes-agent.git

# 安装 MCP 支持
RUN pip install mcp

# 创建 profile 目录结构
RUN mkdir -p /home/hermes/.hermes/profiles && \
    chown -R hermes:hermes /home/hermes/.hermes

USER hermes
WORKDIR /home/hermes

# 入口：启动指定 profile 的 gateway
ENTRYPOINT ["hermes", "-p", "${HERMES_PROFILE}", "gateway", "run", "--accept-hooks"]
```

> **注意**：Dockerfile 内容取决于 Hermes 的实际发布形式。请根据 Hermes 官方文档调整安装步骤。

---

## 四、一期部署流程

### 步骤 1：准备 profile 文件

在 `hermes/profiles/H-01/` 下准备以下文件：

```
hermes/profiles/H-01/
├── SOUL.md        ← 手写人格
├── SKILL.md       ← 技能声明
├── config.yaml    ← Hermes 配置
└── .env.example   ← 变量说明（参考用）
```

> MEMORY.md 首次启动时自动生成（容器内），会自动持久化到挂载的 volume 中。

### 步骤 2：配置 .env

仓库根 `.env` 中填入 LLM 密钥：

```env
ANTHROPIC_API_KEY=sk-ant-xxx
```

### 步骤 3：构建并启动容器

```bash
# 在 WSL 中进入仓库目录
cd /mnt/d/AgentTown_v3

# 构建镜像（仅首次）
docker compose -f docker/docker-compose.yml build

# 启动
docker compose -f docker/docker-compose.yml up -d

# 查看日志
docker compose -f docker/docker-compose.yml logs -f hermes-h01
```

### 步骤 4：验证容器状态

```bash
# 容器运行中
docker ps | grep hermes-h01

# 健康检查（Gateway 启动约 5-10 秒后可用）
curl -s http://localhost:8642/health
```

### 步骤 5：验证 LLM 对话

```bash
# 测试对话
curl -s -X POST http://localhost:8642/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"message": "现在是什么时间？你该做什么？"}'
```

预期收到 Hermes 的 LLM 回复。

---

## 五、日常操作

### 5.1 修改 profile 后重启

```bash
# 修改了仓库中的 SOUL.md / SKILL.md / config.yaml 后
docker compose -f docker/docker-compose.yml restart hermes-h01
```

无需重新构建镜像，profile 文件通过 volume 实时同步。

### 5.2 查看 Agent 决策日志

```bash
# 进入容器
docker exec -it hermes-h01 bash

# 日志位置
cat ~/.hermes/profiles/H-01/logs/latest.log
```

或直接在宿主机（WSL）查看 volume 挂载目录：

```bash
# 日志也挂载到了 Windows 侧
cat /mnt/d/AgentTown_v3/hermes/profiles/H-01/logs/latest.log
```

### 5.3 停止和清理

```bash
# 停止
docker compose -f docker/docker-compose.yml down

# 停止并删除 volume（重置 MEMORY.md）
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
  hermes-h01:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    container_name: hermes-h01
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
docker compose -f docker/docker-compose.multi.yml up -d hermes-h01 hermes-h02
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
| `docker/Dockerfile` | **是** | 容器镜像定义 |
| `docker/docker-compose.yml` | **是** | 部署编排 |
| `.env` | **否** | 真实 API Key |

### 8.2 修改生效流程

```
修改 SOUL.md / SKILL.md / config.yaml
  → docker compose restart hermes-h01
  → 容器内 Hermes 重启，新配置生效
```

> MEMORY.md 不进仓库，每台机器独立累积。如果要分享"某个 NPC 已经经历了什么"，需单独设计记忆导出/导入机制（不在第一期范围）。

---

## 九、常见问题

### Q1：容器启动后立刻退出

查看日志定位：

```bash
docker compose -f docker/docker-compose.yml logs hermes-h01
```

常见原因：`.env` 中 API Key 缺失或格式错误。

### Q2：`localhost:8642` 无法访问

确认容器正在运行且端口映射成功：

```bash
docker ps | grep hermes-h01
# 应看到 0.0.0.0:8642->8642/tcp
```

如果容器正常运行但端口不通，检查 Hermes config.yaml 中 `api_server.host` 是否为 `"0.0.0.0"`。

### Q3：修改 SOUL.md 后不生效

Volume 挂载实时同步文件内容，但 Hermes Gateway 在启动时加载 SOUL.md。修改后需重启容器：

```bash
docker compose -f docker/docker-compose.yml restart hermes-h01
```

### Q4：MEMORY.md 太大

Hermes MEMORY.md 有 2200 字符上限。超出后旧的低重要性记忆自然挤出。如需重置：

```bash
# 方式 1：删文件重启
rm /mnt/d/AgentTown_v3/hermes/profiles/H-01/MEMORY.md
docker compose -f docker/docker-compose.yml restart hermes-h01

# 方式 2：重建容器（删除 volume）
docker compose -f docker/docker-compose.yml down -v
docker compose -f docker/docker-compose.yml up -d
```

### Q5：如何更换 LLM 模型

编辑 `hermes/profiles/H-01/config.yaml` 和 `.env`，重启容器：

```bash
docker compose -f docker/docker-compose.yml restart hermes-h01
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
- [ ] 仓库根 `.env` 中 LLM API Key 正确
- [ ] `hermes/profiles/H-01/` 下 SOUL.md / SKILL.md / config.yaml 已准备
- [ ] `docker/Dockerfile` 和 `docker/docker-compose.yml` 已创建
- [ ] `docker compose -f docker/docker-compose.yml build` 成功
- [ ] `docker compose -f docker/docker-compose.yml up -d` 容器正常运行
- [ ] `curl localhost:8642/health` 返回 200
- [ ] Mock UE Bridge 能 POST 到 `localhost:8642` 并驱动 Agent 决策

---

*本部署说明基于 SmartNPC 项目的 Hermes 部署实践整理，仅覆盖一期（单 Agent）需求。*
