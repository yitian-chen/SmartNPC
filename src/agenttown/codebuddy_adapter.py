"""
CodeBuddy CLI → OpenAI 兼容适配层

把 OpenAI /v1/chat/completions 请求转成 CodeBuddy Code CLI 的 Run API 调用，
再把结果包装成 OpenAI 响应格式返回。复用 CLI 已有的 OAuth 认证，绕过静态 API key
不能调用 copilot.tencent.com/v2 模型端点的问题。

架构：
    Hermes (OpenAI client) → 本适配层 (http://127.0.0.1:8761)
                               ↓ POST /api/v1/runs
                           CodeBuddy CLI 本地服务 (http://127.0.0.1:<动态端口>)
                               ↓ Authorization: Bearer <OAuth token>
                           copilot.tencent.com/v2/chat/completions

CLI 端口发现：遍历 ~/.codebuddy/sessions/*.json，对每个 url 健康检查
/api/v1/health，选第一个活的。CLI 重启后端口会变，适配层每次请求前重新发现。

启动：
    python src/agenttown/codebuddy_adapter.py --port 8761
    # 或
    python -m agenttown.codebuddy_adapter --port 8761

Hermes config.yaml 配置（base_url 必须硬编码，Hermes 不展开 ${VAR}）：
    model:
      provider: custom:agenttown
      default: glm-5.2-internal-ioa
      # Docker 容器 → WSL vEthernet 网卡 → Windows 适配层
      # 172.18.16.1 是 Windows 的 vEthernet (WSL) 适配器 IP，可能在 Windows 重启后变化
      base_url: http://172.18.16.1:8761/v1
      api_mode: chat_completions
"""
from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import time
import uuid
from pathlib import Path
from typing import Any

import httpx
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse, StreamingResponse
import uvicorn

# ── 配置 ────────────────────────────────────────────────────────────────

# CLI session 文件目录（Windows 路径）
SESSIONS_DIR = Path.home() / ".codebuddy" / "sessions"
# 适配层监听端口
DEFAULT_PORT = 8761
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

app = FastAPI(title="CodeBuddy CLI OpenAI Adapter", version="0.1.0")


# ── CLI 端口发现 ────────────────────────────────────────────────────────


def _list_session_urls() -> list[str]:
    """从 ~/.codebuddy/sessions/*.json 读取所有 CLI 实例的 url"""
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


async def _check_health(client: httpx.AsyncClient, base_url: str) -> bool:
    """探测一个 CLI 实例是否活着且已认证"""
    try:
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


async def discover_cli_base_url() -> str:
    """找到一个活着的 CLI 实例的 base url"""
    urls = _list_session_urls()
    if not urls:
        raise HTTPException(
            status_code=503,
            detail="No CodeBuddy CLI session found. Start CLI with `codebuddy --serve` first.",
        )
    async with httpx.AsyncClient() as client:
        # 并行健康检查所有候选
        results = await asyncio.gather(*[_check_health(client, u) for u in urls])
        for url, ok in zip(urls, results):
            if ok:
                return url
    raise HTTPException(
        status_code=503,
        detail=f"All {len(urls)} CLI sessions are unreachable. Is `codebuddy` running?",
    )


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
    model = body.get("model", "glm-5.2-internal-ioa")
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
    parser = argparse.ArgumentParser(description="CodeBuddy CLI OpenAI Adapter")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help="Listen port")
    parser.add_argument("--host", default="0.0.0.0", help="Listen host")
    args = parser.parse_args()

    print(f"CodeBuddy CLI OpenAI Adapter", file=sys.stderr)
    print(f"  监听: http://{args.host}:{args.port}", file=sys.stderr)
    print(f"  CLI sessions dir: {SESSIONS_DIR}", file=sys.stderr)
    print(f"  OpenAI 端点: http://127.0.0.1:{args.port}/v1/chat/completions", file=sys.stderr)
    print(f"  模型列表: {', '.join(SUPPORTED_MODELS[:3])} ...", file=sys.stderr)

    uvicorn.run(app, host=args.host, port=args.port, log_level="info")


if __name__ == "__main__":
    main()
