"""
CodeBuddy CLI → OpenAI 兼容适配层

把 OpenAI /v1/chat/completions 请求转成 CodeBuddy Code CLI 的 Run API 调用，
再把结果包装成 OpenAI 响应格式返回。复用 CLI 已有的 OAuth 认证，绕过静态 API key
不能调用 copilot.tencent.com/v2 模型端点的问题。

架构：
    Hermes (OpenAI client) → 本适配层 (http://127.0.0.1:8761)
                               ↓ POST /api/v1/runs
                           CodeBuddy CLI 子进程 (http://127.0.0.1:52001, --serve 模式)
                               ↓ Authorization: Bearer <OAuth token>
                           copilot.tencent.com/v2/chat/completions

模型隔离：适配层自己启动一个独立的 CLI 子进程（带 --model 参数），不依赖用户日常
用的 CodeBuddy CN 桌面版 GUI。这样换模型时只影响适配层，不影响 GUI。
模型和端口配置在 src/agenttown/adapter_config.yaml。

启动：
    python src/agenttown/codebuddy_adapter.py --port 8761
    # 或
    python -m agenttown.codebuddy_adapter --port 8761

Hermes config.yaml 配置（base_url 必须硬编码，Hermes 不展开 ${VAR}）：
    model:
      provider: custom:agenttown
      default: deepseek-v4-flash-ioa
      # Docker 容器 → WSL vEthernet 网卡 → Windows 适配层
      # 172.18.16.1 是 Windows 的 vEthernet (WSL) 适配器 IP，可能在 Windows 重启后变化
      base_url: http://172.18.16.1:8761/v1
      api_mode: chat_completions

换模型：改 adapter_config.yaml 的 model 字段 + hermes/profiles/h01/config.yaml 的
default / default_model 字段（两处保持一致），然后 bash start.sh 重启。
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import subprocess
import sys
import time
import uuid
from contextlib import asynccontextmanager
from datetime import datetime
from pathlib import Path
from typing import Any

import httpx
import yaml
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse
import uvicorn

# ── 配置 ────────────────────────────────────────────────────────────────

# 适配层自身的配置文件（模型和 CLI 端口在这里改）
ADAPTER_CONFIG_FILE = Path(__file__).parent / "adapter_config.yaml"

# CLI session 文件目录（Windows 路径，仅用于 fallback 端口发现）
SESSIONS_DIR = Path.home() / ".codebuddy" / "sessions"
# 适配层监听端口
DEFAULT_PORT = 8761
# 适配层管理的 CLI 子进程默认端口（避开 GUI 动态端口和适配层自身端口）
DEFAULT_CLI_PORT = 52001
# 默认模型（adapter_config.yaml 不存在或字段缺失时用）
DEFAULT_CLI_MODEL = "deepseek-v4-flash-ioa"
# CLI 子进程启动超时（秒）—— codebuddy --serve 启动需要几秒
CLI_STARTUP_TIMEOUT = 30.0
# 请求 CLI 的超时（Run API 可能要等 Agent 完整执行）
CLI_TIMEOUT = 180.0
# 健康检查超时
HEALTH_TIMEOUT = 2.0
# CLI 必需的请求头
CLI_HEADERS = {"X-CodeBuddy-Request": "1"}

# 可上报给 OpenAI 客户端的模型列表（从 product.ioa.json 提取的 iOA 版可用模型）
SUPPORTED_MODELS = [
    "glm-5.2-internal-ioa",
    "deepseek-v4-pro-ioa",
    "deepseek-v4-flash-ioa",
    "claude-sonnet-5",
    "claude-sonnet-5-1m",
    "gpt-5.6-sol",
    "gpt-5.6-terra",
    "gemini-3.1-pro",
]


def _load_adapter_config() -> dict[str, Any]:
    """读 adapter_config.yaml，文件不存在或字段缺失时返回空 dict（用代码默认值）"""
    if not ADAPTER_CONFIG_FILE.exists():
        return {}
    try:
        return yaml.safe_load(ADAPTER_CONFIG_FILE.read_text(encoding="utf-8")) or {}
    except (yaml.YAMLError, OSError):
        return {}


_ADAPTER_CONFIG = _load_adapter_config()

# CLI 子进程配置（命令行参数可覆盖）
CLI_PORT = int(_ADAPTER_CONFIG.get("cli_port", DEFAULT_CLI_PORT))
CLI_MODEL = str(_ADAPTER_CONFIG.get("model", DEFAULT_CLI_MODEL))

# 运行时状态：CLI 子进程句柄（None 表示适配层没启动子进程，可能在用外部 CLI）
_cli_process: subprocess.Popen | None = None

# 运行时配置（main() 启动时设置，lifespan 和请求处理用）
_runtime_cli_port: int = CLI_PORT
_runtime_cli_model: str = CLI_MODEL
_runtime_manage_cli: bool = True  # True = 适配层自动启动/管理 CLI 子进程
_runtime_log_dir: Path | None = None  # CLI 子进程日志目录


# ── CLI 子进程管理 ──────────────────────────────────────────────────────


def _is_port_listening(host: str, port: int) -> bool:
    """检查端口是否已被监听（可能是上次没干净退出的 CLI，或外部已启动的 CLI）"""
    import socket
    try:
        with socket.create_connection((host, port), timeout=1.0):
            return True
    except OSError:
        return False


async def _check_cli_health(base_url: str) -> bool:
    """探测 CLI 实例是否活着且已认证"""
    try:
        async with httpx.AsyncClient() as client:
            r = await client.get(
                f"{base_url}/api/v1/auth/status",
                headers=CLI_HEADERS,
                timeout=HEALTH_TIMEOUT,
            )
            if r.status_code != 200:
                return False
            data = r.json()
            # authEnabled:false（本地模式）或 authenticated:true 都可用
            return data.get("authenticated", False) or not data.get("authEnabled", True)
    except (httpx.HTTPError, json.JSONDecodeError):
        return False


def start_cli_process(cli_port: int, model: str, log_dir: Path | None = None) -> subprocess.Popen | None:
    """
    启动一个独立的 CLI 子进程（--serve 模式，固定端口，指定模型）。

    如果端口已被占用且健康检查通过，说明已有 CLI 在跑（可能是上次没干净退出，
    或外部启动的），直接返回 None 复用现有进程。

    返回：Popen 句柄（如果是本次启动的），或 None（复用现有进程 / 启动失败）
    """
    base_url = f"http://127.0.0.1:{cli_port}"

    # 端口已占用 —— 检查是否是健康的 CLI，是则复用
    if _is_port_listening("127.0.0.1", cli_port):
        # 同步检查：用 httpx 同步客户端（启动阶段，不在 async 上下文里）
        try:
            r = httpx.get(f"{base_url}/api/v1/auth/status", headers=CLI_HEADERS, timeout=2.0)
            if r.status_code == 200 and (
                r.json().get("authenticated", False) or not r.json().get("authEnabled", True)
            ):
                print(f"[adapter] CLI already running on :{cli_port}, reusing", file=sys.stderr)
                return None
        except (httpx.HTTPError, json.JSONDecodeError):
            pass
        # 端口被占但不是健康 CLI —— 报错让用户处理
        print(
            f"[adapter] WARNING: port {cli_port} is occupied but not a healthy CLI. "
            f"Either free the port or change cli_port in adapter_config.yaml.",
            file=sys.stderr,
        )
        return None

    # 定位 codebuddy 可执行文件
    cb_cmd = _find_codebuddy_binary()
    if cb_cmd is None:
        print(
            "[adapter] ERROR: codebuddy command not found in PATH. "
            "Install CodeBuddy CLI or add it to PATH.",
            file=sys.stderr,
        )
        return None

    # 日志文件
    log_file = None
    if log_dir is not None:
        log_dir.mkdir(parents=True, exist_ok=True)
        log_file = open(log_dir / "cli.log", "ab", buffering=0)

    # 启动 CLI 子进程
    # --serve: 长驻服务模式（暴露 HTTP API，不进入交互式 REPL）
    # --port: 固定端口，避免动态发现
    # --host 127.0.0.1: 只监听本机（适配层在本机访问）
    # --model: 指定模型（这是换模型的唯一可靠方式，Run API 请求体里的 model 字段会被忽略）
    args = [cb_cmd, "--serve", "--port", str(cli_port), "--host", "127.0.0.1", "--model", model]
    print(f"[adapter] Starting CLI subprocess: {' '.join(args)}", file=sys.stderr)
    try:
        proc = subprocess.Popen(
            args,
            stdout=log_file if log_file else subprocess.DEVNULL,
            stderr=log_file if log_file else subprocess.DEVNULL,
            # 不继承适配层的 stdin（CLI 不需要交互输入）
            stdin=subprocess.DEVNULL,
            # Windows 上创建新的进程组，便于整组终止
            creationflags=subprocess.CREATE_NEW_PROCESS_GROUP if sys.platform == "win32" else 0,
        )
    except OSError as e:
        print(f"[adapter] ERROR: failed to start CLI subprocess: {e}", file=sys.stderr)
        return None
    print(f"[adapter] CLI subprocess started (PID {proc.pid})", file=sys.stderr)
    return proc


def _find_codebuddy_binary() -> str | None:
    """定位 codebuddy 可执行文件"""
    # 1. PATH 查找
    for name in ("codebuddy", "codebuddy.exe", "cbc", "cbc.exe"):
        path = _which(name)
        if path:
            return path
    # 2. npm 全局安装常见位置（Windows）
    npm_global = Path.home() / "AppData" / "Roaming" / "npm"
    for name in ("codebuddy.cmd", "codebuddy.exe"):
        p = npm_global / name
        if p.exists():
            return str(p)
    return None


def _which(name: str) -> str | None:
    """简易 which，避免依赖 shutil.which 在某些 Windows 环境的怪异行为"""
    # 优先用 shutil.which
    import shutil
    found = shutil.which(name)
    if found:
        return found
    return None


def stop_cli_process() -> None:
    """终止适配层启动的 CLI 子进程（如果有）"""
    global _cli_process
    if _cli_process is None:
        return
    if _cli_process.poll() is not None:
        # 进程已退出
        _cli_process = None
        return
    print(f"[adapter] Stopping CLI subprocess (PID {_cli_process.pid})", file=sys.stderr)
    try:
        _cli_process.terminate()
        try:
            _cli_process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            _cli_process.kill()
            _cli_process.wait(timeout=3)
    except OSError:
        pass
    _cli_process = None


async def ensure_cli_running(cli_port: int, model: str, log_dir: Path | None) -> str:
    """
    确保 CLI 在指定端口运行，返回 base_url。
    如果端口没活着的 CLI，启动一个子进程并等它就绪。
    """
    base_url = f"http://127.0.0.1:{cli_port}"

    # 已有健康 CLI 在跑（可能是上次没干净退出，或外部启动的）
    if await _check_cli_health(base_url):
        return base_url

    # 启动子进程
    global _cli_process
    _cli_process = start_cli_process(cli_port, model, log_dir)

    # 等子进程就绪（轮询健康检查）
    print(f"[adapter] Waiting for CLI to be ready on :{cli_port} (max {CLI_STARTUP_TIMEOUT}s)...", file=sys.stderr)
    start = time.monotonic()
    while time.monotonic() - start < CLI_STARTUP_TIMEOUT:
        # 子进程意外退出则停止等待
        if _cli_process is not None and _cli_process.poll() is not None:
            raise RuntimeError(
                f"CLI subprocess exited prematurely (code={_cli_process.returncode}). "
                f"Check logs/{datetime.now().strftime('%Y-%m-%d')}/cli.log"
            )
        if await _check_cli_health(base_url):
            print(f"[adapter] CLI ready on :{cli_port}", file=sys.stderr)
            return base_url
        await asyncio.sleep(1.0)

    raise RuntimeError(
        f"CLI did not become ready on :{cli_port} within {CLI_STARTUP_TIMEOUT}s. "
        f"Check logs/{datetime.now().strftime('%Y-%m-%d')}/cli.log"
    )


# ── 端口发现（fallback：适配层不管理 CLI 时用） ──────────────────────────


def _list_session_urls() -> list[str]:
    """从 ~/.codebuddy/sessions/*.json 读取所有 CLI 实例的 url（fallback 用）"""
    urls: list[str] = []
    if not SESSIONS_DIR.exists():
        return urls
    for f in SESSIONS_DIR.glob("*.json"):
        try:
            data = json.loads(f.read_text(encoding="utf-8"))
            url = data.get("url")
            if url:
                urls.append(url)
        except (json.JSONDecodeError, OSError):
            continue
    return urls


async def discover_cli_base_url() -> str:
    """
    找到一个活着的 CLI 实例的 base url。

    优先用 adapter_config.yaml 配置的固定端口（CLI_PORT）—— 适配层启动时
    会自动拉起子进程监听这个端口。如果固定端口的 CLI 没起来（比如 --no-manage-cli
    模式），回退到扫描 ~/.codebuddy/sessions/ 找其他活着的 CLI 实例（比如用户
    手动启动的桌面版 GUI）。
    """
    # 1. 优先检查固定端口的 CLI
    fixed_url = f"http://127.0.0.1:{CLI_PORT}"
    if await _check_cli_health(fixed_url):
        return fixed_url

    # 2. Fallback：扫描 sessions 目录找其他活着的 CLI
    urls = _list_session_urls()
    if urls:
        async with httpx.AsyncClient() as client:
            results = await asyncio.gather(
                *[_check_cli_health_url(client, u) for u in urls]
            )
            for url, ok in zip(urls, results):
                if ok:
                    return url

    raise HTTPException(
        status_code=503,
        detail=(
            f"No healthy CodeBuddy CLI found. Tried fixed port :{CLI_PORT} and "
            f"{len(urls)} session(s) in ~/.codebuddy/sessions/. "
            f"Start CLI with `codebuddy --serve --port {CLI_PORT} --model {CLI_MODEL}` "
            f"or run adapter without --no-manage-cli to auto-start one."
        ),
    )


async def _check_cli_health_url(client: httpx.AsyncClient, base_url: str) -> bool:
    """探测一个 CLI 实例是否活着且已认证（接受 client 参数的版本，用于并发扫描）"""
    try:
        r = await client.get(
            f"{base_url}/api/v1/auth/status",
            headers=CLI_HEADERS,
            timeout=HEALTH_TIMEOUT,
        )
        if r.status_code != 200:
            return False
        data = r.json()
        return data.get("authenticated", False) or not data.get("authEnabled", True)
    except (httpx.HTTPError, json.JSONDecodeError):
        return False


# ── FastAPI 应用与生命周期 ──────────────────────────────────────────────


@asynccontextmanager
async def lifespan(app: FastAPI):
    """
    适配层启动时：拉起 CLI 子进程（如果 --manage-cli 模式）
    适配层关闭时：终止 CLI 子进程
    """
    if _runtime_manage_cli:
        try:
            await ensure_cli_running(_runtime_cli_port, _runtime_cli_model, _runtime_log_dir)
        except RuntimeError as e:
            print(f"[adapter] WARNING: failed to start CLI subprocess: {e}", file=sys.stderr)
            print(
                "[adapter] Adapter will start anyway and fall back to discovering external CLI. "
                "If no CLI is available, requests will return 503.",
                file=sys.stderr,
            )
    yield
    # 关闭：终止 CLI 子进程
    stop_cli_process()


app = FastAPI(title="CodeBuddy CLI OpenAI Adapter", version="0.2.0", lifespan=lifespan)


# ── 消息格式转换 ────────────────────────────────────────────────────────


def messages_to_prompt(messages: list[dict[str, Any]]) -> str:
    """
    把 OpenAI messages 数组拼成 CLI Run API 的 payload.text。

    CLI Run API 是面向 Agent 的，只接收一个 text 字段，没有多轮消息概念。
    我们把历史消息拼成上下文，最后一条 user 消息作为主 prompt。

    Hermes 的调用模式：每次 perception 都是把完整上下文塞进一条 user 消息，
    所以实际上 messages 通常只有 1-2 条，拼接不会失真。
    """
    if not messages:
        return ""

    parts: list[str] = []
    for msg in messages:
        role = msg.get("role", "user")
        content = msg.get("content", "")
        # content 可能是 string 或 list（多模态），只取文本
        if isinstance(content, list):
            text_chunks = [
                c.get("text", "") for c in content if isinstance(c, dict) and c.get("type") in ("text", "input_text")
            ]
            content = "\n".join(text_chunks)
        if not isinstance(content, str) or not content.strip():
            continue
        if role == "system":
            parts.append(f"[系统指令]\n{content}")
        elif role == "user":
            parts.append(f"[用户]\n{content}")
        elif role == "assistant":
            parts.append(f"[助手]\n{content}")
        else:
            parts.append(f"[{role}]\n{content}")

    return "\n\n".join(parts)


def build_run_request(text: str) -> dict[str, Any]:
    """构造 CLI Run API 的 Gateway Protocol 请求体"""
    msg_id = f"adapter-{uuid.uuid4().hex[:12]}"
    return {
        "id": msg_id,
        "type": "message",
        "source": {
            "platform": "generic",
            "sender": {"id": "hermes-adapter", "name": "Hermes Adapter"},
            "conversation": {"id": msg_id, "type": "direct"},
        },
        "payload": {"text": text},
    }


def parse_run_sse_stream(raw_lines: list[bytes]) -> dict[str, Any] | None:
    """
    解析 Run API SSE 流，提取最终结果事件。

    SSE 格式：
        event: message
        data: {"version":"1.0","replyTo":"...","status":"completed","content":{"markdown":"..."}}

        event: done
        data: {}

    返回 message 事件的 JSON（含 content.markdown），如果没有则 None。
    """
    current_event_type = None
    for line in raw_lines:
        line = line.strip()
        if not line:
            current_event_type = None
            continue
        if line.startswith(b"event:"):
            current_event_type = line[6:].strip().decode("utf-8", errors="replace")
        elif line.startswith(b"data:"):
            payload = line[5:].strip()
            if not payload:
                continue
            try:
                evt = json.loads(payload)
            except json.JSONDecodeError:
                continue
            if current_event_type == "message" and "content" in evt:
                return evt
    return None


# ── OpenAI 响应构造 ────────────────────────────────────────────────────


def make_openai_response(
    text: str,
    model: str,
    *,
    stream: bool = False,
    finish_reason: str = "stop",
):
    """构造 OpenAI 兼容响应"""
    resp_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
    created = int(time.time())

    if not stream:
        return {
            "id": resp_id,
            "object": "chat.completion",
            "created": created,
            "model": model,
            "choices": [
                {
                    "index": 0,
                    "message": {"role": "assistant", "content": text},
                    "finish_reason": finish_reason,
                }
            ],
            "usage": {
                "prompt_tokens": 0,
                "completion_tokens": 0,
                "total_tokens": 0,
            },
        }

    # 流式模式：把完整文本切成 chunk 模拟流式
    chunks = []
    # 第一个 chunk：role
    chunks.append(
        {
            "id": resp_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
            "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}],
        }
    )
    # 文本 chunk（按字符切，模拟逐字输出）
    for ch in text:
        chunks.append(
            {
                "id": resp_id,
                "object": "chat.completion.chunk",
                "created": created,
                "model": model,
                "choices": [{"index": 0, "delta": {"content": ch}, "finish_reason": None}],
            }
        )
    # 结束 chunk
    chunks.append(
        {
            "id": resp_id,
            "object": "chat.completion.chunk",
            "created": created,
            "model": model,
            "choices": [{"index": 0, "delta": {}, "finish_reason": finish_reason}],
        }
    )
    return chunks


def make_error_response(message: str, status: int = 500, err_type: str = "api_error") -> dict[str, Any]:
    """构造 OpenAI 风格的错误响应"""
    return {
        "error": {
            "message": message,
            "type": err_type,
            "code": status,
        }
    }


# ── 调用 CLI Run API ──────────────────────────────────────────────────


async def call_cli_run(base_url: str, text: str) -> str:
    """
    调用 CLI Run API，阻塞等待结果，返回最终 markdown 文本。

    Run API 是异步的：先 POST 创建 run 拿 runId，再 GET stream 订阅 SSE 流。
    对于简单 prompt，结果会在几秒内一次性返回（不是真正的流式）。
    """
    run_body = build_run_request(text)
    async with httpx.AsyncClient(timeout=CLI_TIMEOUT) as client:
        # 1. 创建 run
        r = await client.post(
            f"{base_url}/api/v1/runs",
            headers={**CLI_HEADERS, "Content-Type": "application/json"},
            json=run_body,
        )
        if r.status_code == 429:
            raise HTTPException(status_code=429, detail="CLI rate limited")
        if r.status_code != 202 and r.status_code != 200:
            raise HTTPException(
                status_code=502,
                detail=f"CLI run creation failed: HTTP {r.status_code} {r.text[:200]}",
            )
        run_id = r.json()["data"]["runId"]

        # 2. 订阅 SSE 流拿结果
        async with client.stream(
            "GET",
            f"{base_url}/api/v1/runs/{run_id}/stream",
            headers=CLI_HEADERS,
        ) as resp:
            if resp.status_code == 404:
                # Run 已完成（简单 prompt 跑得快），流已关闭
                raise HTTPException(
                    status_code=502,
                    detail="Run completed before stream subscription. Prompt may be too simple or CLI too fast.",
                )
            if resp.status_code != 200:
                body = await resp.aread()
                raise HTTPException(
                    status_code=502,
                    detail=f"CLI stream failed: HTTP {resp.status_code} {body[:200]}",
                )

            # 收集所有行，解析 SSE
            raw_lines: list[bytes] = []
            async for line in resp.aiter_lines():
                raw_lines.append(line.encode("utf-8") if isinstance(line, str) else line)

    result = parse_run_sse_stream(raw_lines)
    if result is None:
        raise HTTPException(
            status_code=502,
            detail="No message event in CLI SSE stream",
        )

    status = result.get("status", "")
    if status != "completed":
        raise HTTPException(
            status_code=502,
            detail=f"CLI run did not complete: status={status}",
        )

    content = result.get("content", {})
    markdown = content.get("markdown", "")
    if not markdown:
        # 兜底：尝试其他字段
        markdown = content.get("text", "") or json.dumps(content, ensure_ascii=False)
    return markdown


# ── FastAPI 端点 ────────────────────────────────────────────────────────


@app.get("/health")
async def health():
    """适配层自身健康检查"""
    try:
        cli_url = await discover_cli_base_url()
        return {"status": "ok", "cli_url": cli_url}
    except HTTPException:
        return {"status": "degraded", "cli_url": None}


@app.get("/v1/models")
async def list_models():
    """返回支持的模型列表（OpenAI 兼容格式）"""
    return {
        "object": "list",
        "data": [
            {"id": m, "object": "model", "created": 0, "owned_by": "codebuddy"}
            for m in SUPPORTED_MODELS
        ],
    }


async def _parse_body(request: Request) -> dict[str, Any]:
    """读取请求体，兼容 UTF-8 / GBK 编码（Windows curl 默认 GBK）"""
    raw = await request.body()
    if not raw:
        return {}
    for encoding in ("utf-8", "utf-8-sig", "gbk", "gb18030"):
        try:
            return json.loads(raw.decode(encoding))
        except (UnicodeDecodeError, json.JSONDecodeError):
            continue
    raise HTTPException(status_code=400, detail="Invalid JSON body (unsupported encoding)")


@app.post("/v1/chat/completions")
async def chat_completions(request: Request):
    """OpenAI 兼容的 chat completions 端点"""
    try:
        body = await _parse_body(request)
    except HTTPException as e:
        return JSONResponse(
            status_code=e.status_code,
            content=make_error_response(str(e.detail), e.status_code, "invalid_request_error"),
        )

    messages = body.get("messages", [])
    # 请求里的 model 字段被 CLI 忽略（CLI 用启动时 --model 指定的模型），
    # 这里回写 _runtime_cli_model 保持响应与实际使用的模型一致，避免客户端误判。
    model = body.get("model", _runtime_cli_model)
    stream = body.get("stream", False)

    if not messages:
        return JSONResponse(
            status_code=400,
            content=make_error_response("messages is required", 400, "invalid_request_error"),
        )

    # 发现 CLI 端口
    try:
        cli_base_url = await discover_cli_base_url()
    except HTTPException as e:
        return JSONResponse(
            status_code=e.status_code,
            content=make_error_response(e.detail, e.status_code, "api_error"),
        )

    # 转换消息并调用 CLI
    prompt_text = messages_to_prompt(messages)
    try:
        result_text = await call_cli_run(cli_base_url, prompt_text)
    except HTTPException as e:
        return JSONResponse(
            status_code=e.status_code,
            content=make_error_response(str(e.detail), e.status_code, "api_error"),
        )

    # 构造 OpenAI 响应
    if stream:
        chunks = make_openai_response(result_text, model, stream=True)

        async def chunk_streamer():
            for chunk in chunks:
                yield f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n"
            yield "data: [DONE]\n\n"

        return StreamingResponse(
            chunk_streamer(),
            media_type="text/event-stream",
            headers={"Cache-Control": "no-cache", "X-Accel-Buffering": "no"},
        )
    else:
        return make_openai_response(result_text, model, stream=False)


@app.post("/v1/responses")
async def responses_fallback(request: Request):
    """
    兜底：某些 OpenAI 客户端会调 /v1/responses（Responses API）。
    简单转发到 /v1/chat/completions 逻辑。
    """
    return await chat_completions(request)


# ── 入口 ────────────────────────────────────────────────────────────────


def main():
    global _runtime_cli_port, _runtime_cli_model, _runtime_manage_cli, _runtime_log_dir

    parser = argparse.ArgumentParser(description="CodeBuddy CLI OpenAI Adapter")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help="Adapter listen port")
    parser.add_argument("--host", default="0.0.0.0", help="Adapter listen host")
    parser.add_argument(
        "--cli-port",
        type=int,
        default=CLI_PORT,
        help=f"CLI subprocess port (default: {CLI_PORT} from adapter_config.yaml)",
    )
    parser.add_argument(
        "--cli-model",
        type=str,
        default=CLI_MODEL,
        help=f"Model for CLI subprocess (default: {CLI_MODEL} from adapter_config.yaml)",
    )
    parser.add_argument(
        "--no-manage-cli",
        action="store_true",
        help="Don't auto-start CLI subprocess; fall back to discovering external CLI",
    )
    parser.add_argument(
        "--log-dir",
        type=str,
        default=None,
        help="Log directory for CLI subprocess (default: logs/YYYY-MM-DD/ next to this file)",
    )
    args = parser.parse_args()

    # 设置运行时配置（lifespan 会用）
    _runtime_cli_port = args.cli_port
    _runtime_cli_model = args.cli_model
    _runtime_manage_cli = not args.no_manage_cli

    # 日志目录：默认用项目根目录下的 logs/YYYY-MM-DD/
    if args.log_dir:
        _runtime_log_dir = Path(args.log_dir)
    else:
        # 适配层脚本在 src/agenttown/，项目根是上两级
        project_root = Path(__file__).resolve().parent.parent.parent
        _runtime_log_dir = project_root / "logs" / datetime.now().strftime("%Y-%m-%d")

    print("CodeBuddy CLI OpenAI Adapter", file=sys.stderr)
    print(f"  监听: http://{args.host}:{args.port}", file=sys.stderr)
    print(f"  CLI 子进程: http://127.0.0.1:{_runtime_cli_port} (model: {_runtime_cli_model})", file=sys.stderr)
    print(f"  管理 CLI 子进程: {'是' if _runtime_manage_cli else '否（回退到外部 CLI 发现）'}", file=sys.stderr)
    if _runtime_log_dir:
        print(f"  CLI 日志: {_runtime_log_dir / 'cli.log'}", file=sys.stderr)
    print(f"  OpenAI 端点: http://127.0.0.1:{args.port}/v1/chat/completions", file=sys.stderr)
    print(f"  模型列表: {', '.join(SUPPORTED_MODELS[:3])} ...", file=sys.stderr)
    print(f"  配置文件: {ADAPTER_CONFIG_FILE}", file=sys.stderr)

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
