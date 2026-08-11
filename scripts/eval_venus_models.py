#!/usr/bin/env python3
"""评测 Venus 平台多个模型的延迟与输出情况。

从真实仿真日志中取一条战略层 prompt 和一条战术层 prompt，逐个调用
Venus /v1/chat/completions 端点，测量每个模型的：
  - 总耗时（wall-clock）
  - 首 token 耗时（TTFB，仅 streaming 模式可测）
  - 输出文本长度（字符数）
  - token usage（prompt_tokens / completion_tokens / total）
  - HTTP 状态码与错误信息

用法：
    python scripts/eval_venus_models.py
    python scripts/eval_venus_models.py --log /path/to/debug-mcp.log
    python scripts/eval_venus_models.py --models qwen3.6-35b-a3b,deepseek-v4-flash
    python scripts/eval_venus_models.py --runs 3        # 每个模型跑 3 轮取平均
    python scripts/eval_venus_models.py --stream         # 用 streaming 测 TTFB
    python scripts/eval_venus_models.py --out report.json

环境变量：
    VENUS_API_KEY   必填（从 .env 加载或 shell 环境读取）
    VENUS_URL       可选，默认 http://v2.open.venus.oa.com/llmproxy

依赖：仅 Python 3 标准库（urllib），无需 pip install。
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# ── 默认配置 ──────────────────────────────────────────────────────

DEFAULT_VENUS_URL = "http://v2.open.venus.oa.com/llmproxy"
DEFAULT_MODELS = [
    "qwen3.6-35b-a3b",
    "deepseek-v4-flash",
    "deepseek-v4-pro",
    "kimi-k2-light",
    "hy3-external",
]
DEFAULT_TIMEOUT = 120  # 单次调用最长 120s
DEFAULT_MAX_TOKENS = 4096

# 日志中 prompt 文本所在 JSON 字段（slog JSON handler 输出）
LOG_MSG_STRATEGIC = "[MCP→LLM/STRATEGIC-PROMPT]"
LOG_MSG_TACTICAL = "[MCP→LLM/TACTICAL-PROMPT]"


# ── 工具函数 ──────────────────────────────────────────────────────

def load_env_file(env_path: Path) -> dict[str, str]:
    """从 .env 文件加载键值对（不覆盖已存在的环境变量）。"""
    env = {}
    if not env_path.exists():
        return env
    for line in env_path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        env[k.strip()] = v.strip().strip('"').strip("'")
    return env


def extract_prompt_from_log(log_path: Path, marker: str) -> str | None:
    """从日志中提取第一条匹配 marker 的 prompt 文本。

    日志是 JSON Lines 格式，每行一个 JSON 对象，prompt 文本在 `text` 字段。
    """
    if not log_path.exists():
        return None
    with log_path.open("r", encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or marker not in line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                continue
            if obj.get("msg") == marker:
                text = obj.get("text", "")
                if text:
                    return text
    return None


def find_latest_log(root: Path) -> Path | None:
    """在 logs/ 目录下找最新包含 strategic + tactical prompt 的日志文件。"""
    if not root.exists():
        return None
    # 按日期倒序遍历
    date_dirs = sorted(
        [d for d in root.iterdir() if d.is_dir() and d.name[:4].isdigit()],
        reverse=True,
    )
    for d in date_dirs:
        log = d / "debug-mcp.log"
        if log.exists():
            # 检查是否包含两种 prompt
            has_strategic = has_tactical = False
            with log.open("r", encoding="utf-8") as f:
                for line in f:
                    if not has_strategic and LOG_MSG_STRATEGIC in line:
                        has_strategic = True
                    if not has_tactical and LOG_MSG_TACTICAL in line:
                        has_tactical = True
                    if has_strategic and has_tactical:
                        return log
    return None


# ── Venus 调用 ────────────────────────────────────────────────────

def call_venus_nonstream(
    base_url: str,
    api_key: str,
    model: str,
    prompt: str,
    max_tokens: int,
    timeout: int,
) -> dict:
    """非流式调用 Venus，返回测量结果 dict。"""
    url = base_url.rstrip("/") + "/v1/chat/completions"
    body = json.dumps({
        "model": model,
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
        "stream": False,
    }).encode("utf-8")
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", f"Bearer {api_key}")
    req.add_header("Venus-Sticky-Routing", "token")

    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            raw = resp.read().decode("utf-8")
        elapsed = time.monotonic() - t0
        if status != 200:
            return {
                "ok": False, "status": status, "error": raw[:500],
                "elapsed_sec": elapsed, "output_len": 0,
            }
        data = json.loads(raw)
        text = ""
        for ch in data.get("choices", []):
            text += ch.get("message", {}).get("content", "")
        usage = data.get("usage", {})
        return {
            "ok": True, "status": status, "elapsed_sec": elapsed,
            "output_len": len(text), "output": text,
            "prompt_tokens": usage.get("prompt_tokens", 0),
            "completion_tokens": usage.get("completion_tokens", 0),
            "total_tokens": usage.get("total_tokens", 0),
            "model_returned": data.get("model", ""),
        }
    except urllib.error.HTTPError as e:
        elapsed = time.monotonic() - t0
        body = ""
        try:
            body = e.read().decode("utf-8", errors="replace")[:500]
        except Exception:
            pass
        return {
            "ok": False, "status": e.code, "error": body,
            "elapsed_sec": elapsed, "output_len": 0,
        }
    except Exception as e:
        return {
            "ok": False, "status": 0, "error": f"{type(e).__name__}: {e}",
            "elapsed_sec": time.monotonic() - t0, "output_len": 0,
        }


def call_venus_stream(
    base_url: str,
    api_key: str,
    model: str,
    prompt: str,
    max_tokens: int,
    timeout: int,
) -> dict:
    """流式调用 Venus，测量 TTFB 和总耗时。"""
    url = base_url.rstrip("/") + "/v1/chat/completions"
    body = json.dumps({
        "model": model,
        "max_tokens": max_tokens,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
    }).encode("utf-8")
    req = urllib.request.Request(url, data=body, method="POST")
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", f"Bearer {api_key}")
    req.add_header("Venus-Sticky-Routing", "token")
    req.add_header("Accept", "text/event-stream")

    t0 = time.monotonic()
    ttfb = None
    text_parts: list[str] = []
    usage: dict = {}
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            if status != 200:
                raw = resp.read().decode("utf-8", errors="replace")
                return {
                    "ok": False, "status": status, "error": raw[:500],
                    "elapsed_sec": time.monotonic() - t0, "output_len": 0,
                }
            # 逐行读 SSE
            buf = b""
            while True:
                chunk = resp.read(4096)
                if not chunk:
                    break
                buf += chunk
                while b"\n" in buf:
                    line, buf = buf.split(b"\n", 1)
                    line = line.decode("utf-8", errors="replace").strip()
                    if not line or line.startswith(":"):
                        continue
                    if not line.startswith("data:"):
                        continue
                    data_str = line[len("data:"):].lstrip()
                    if data_str == "[DONE]":
                        continue
                    try:
                        chunk_obj = json.loads(data_str)
                    except json.JSONDecodeError:
                        continue
                    if ttfb is None and chunk_obj.get("choices"):
                        delta = chunk_obj["choices"][0].get("delta", {}).get("content", "")
                        if delta:
                            ttfb = time.monotonic() - t0
                            text_parts.append(delta)
                        elif chunk_obj["choices"][0].get("finish_reason"):
                            ttfb = time.monotonic() - t0
                    if chunk_obj.get("choices"):
                        delta = chunk_obj["choices"][0].get("delta", {}).get("content", "")
                        if delta and ttfb is not None:
                            text_parts.append(delta)
                    if chunk_obj.get("usage"):
                        usage = chunk_obj["usage"]
        elapsed = time.monotonic() - t0
        text = "".join(text_parts)
        return {
            "ok": True, "status": status, "elapsed_sec": elapsed,
            "ttfb_sec": ttfb if ttfb is not None else 0.0,
            "output_len": len(text), "output": text,
            "prompt_tokens": usage.get("prompt_tokens", 0),
            "completion_tokens": usage.get("completion_tokens", 0),
            "total_tokens": usage.get("total_tokens", 0),
            "model_returned": "",
        }
    except urllib.error.HTTPError as e:
        elapsed = time.monotonic() - t0
        body = ""
        try:
            body = e.read().decode("utf-8", errors="replace")[:500]
        except Exception:
            pass
        return {
            "ok": False, "status": e.code, "error": body,
            "elapsed_sec": elapsed, "output_len": 0,
        }
    except Exception as e:
        return {
            "ok": False, "status": 0, "error": f"{type(e).__name__}: {e}",
            "elapsed_sec": time.monotonic() - t0, "output_len": 0,
        }


def call_venus(base_url, api_key, model, prompt, max_tokens, timeout, stream):
    return call_venus_stream(base_url, api_key, model, prompt, max_tokens, timeout) \
        if stream else call_venus_nonstream(base_url, api_key, model, prompt, max_tokens, timeout)


# ── 主流程 ────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--log", type=Path, default=None,
                    help="指定 debug-mcp.log 路径；默认自动搜索 logs/ 下最新日志")
    ap.add_argument("--models", default=",".join(DEFAULT_MODELS),
                    help=f"逗号分隔的模型列表；默认 {','.join(DEFAULT_MODELS)}")
    ap.add_argument("--runs", type=int, default=1,
                    help="每个模型重复调用次数（取平均延迟）")
    ap.add_argument("--stream", action="store_true",
                    help="使用 streaming 模式（可测 TTFB）")
    ap.add_argument("--timeout", type=int, default=DEFAULT_TIMEOUT,
                    help=f"单次调用超时秒数；默认 {DEFAULT_TIMEOUT}")
    ap.add_argument("--max-tokens", type=int, default=DEFAULT_MAX_TOKENS,
                    help=f"max_tokens 参数；默认 {DEFAULT_MAX_TOKENS}")
    ap.add_argument("--out", type=Path, default=None,
                    help="将完整结果写入 JSON 文件")
    ap.add_argument("--save-output", action="store_true",
                    help="在 JSON 输出里保留每个调用的完整文本输出（默认省略）")
    args = ap.parse_args()

    # 加载 API key
    env = load_env_file(Path(".env"))
    api_key = os.environ.get("VENUS_API_KEY") or env.get("VENUS_API_KEY")
    if not api_key:
        print("ERROR: VENUS_API_KEY 未设置（请 export VENUS_API_KEY=... 或写入 .env）", file=sys.stderr)
        return 1
    base_url = os.environ.get("VENUS_URL") or env.get("VENUS_URL") or DEFAULT_VENUS_URL

    # 定位日志文件
    log_path = args.log
    if log_path is None:
        log_path = find_latest_log(Path("logs"))
        if log_path is None:
            # 也尝试 stable 目录
            log_path = find_latest_log(Path("/data/workspace/stable/logs"))
    if log_path is None or not log_path.exists():
        print(f"ERROR: 找不到包含 strategic/tactical prompt 的日志文件", file=sys.stderr)
        print(f"  请用 --log /path/to/debug-mcp.log 显式指定", file=sys.stderr)
        return 1
    print(f"[info] 使用日志: {log_path}")

    # 提取 prompt
    strategic_prompt = extract_prompt_from_log(log_path, LOG_MSG_STRATEGIC)
    tactical_prompt = extract_prompt_from_log(log_path, LOG_MSG_TACTICAL)
    if not strategic_prompt:
        print(f"ERROR: 日志中未找到 {LOG_MSG_STRATEGIC}", file=sys.stderr)
        return 1
    if not tactical_prompt:
        print(f"ERROR: 日志中未找到 {LOG_MSG_TACTICAL}", file=sys.stderr)
        return 1
    print(f"[info] 战略层 prompt: {len(strategic_prompt)} 字符")
    print(f"[info] 战术层 prompt: {len(tactical_prompt)} 字符")
    print(f"[info] Venus URL: {base_url}")
    print(f"[info] 模型列表: {args.models}")
    print(f"[info] 每模型重复: {args.runs} 次；streaming: {args.stream}")
    print()

    models = [m.strip() for m in args.models.split(",") if m.strip()]
    tasks = [
        ("strategic", strategic_prompt),
        ("tactical", tactical_prompt),
    ]

    all_results: list[dict] = []
    for layer_name, prompt in tasks:
        print(f"━━━ {layer_name} 层 (prompt {len(prompt)} 字符) ━━━")
        for model in models:
            print(f"  ▶ {model:<28} ...", end="", flush=True)
            run_results = []
            for run_idx in range(args.runs):
                res = call_venus(
                    base_url, api_key, model, prompt,
                    args.max_tokens, args.timeout, args.stream,
                )
                res["layer"] = layer_name
                res["model"] = model
                res["run"] = run_idx + 1
                run_results.append(res)
                all_results.append(res)
            # 汇总该 model×layer 的多轮结果
            ok_runs = [r for r in run_results if r.get("ok")]
            if ok_runs:
                avg_elapsed = sum(r["elapsed_sec"] for r in ok_runs) / len(ok_runs)
                avg_tokens = sum(r.get("completion_tokens", 0) for r in ok_runs) / len(ok_runs)
                avg_out_len = sum(r["output_len"] for r in ok_runs) / len(ok_runs)
                ttfb_str = ""
                if args.stream and ok_runs[0].get("ttfb_sec") is not None:
                    avg_ttfb = sum(r.get("ttfb_sec", 0) for r in ok_runs) / len(ok_runs)
                    ttfb_str = f" TTFB={avg_ttfb:.2f}s"
                print(f" ✓ avg={avg_elapsed:.2f}s{ttfb_str} out={avg_out_len:.0f}chars tok={avg_tokens:.0f}")
            else:
                err = run_results[0].get("error", "unknown")[:80] if run_results else "no result"
                print(f" ✗ {err}")
        print()

    # 打印汇总表
    print("━━━ 汇总 ━━━")
    print(f"{'模型':<28} {'层':<10} {'状态':<6} {'耗时(s)':<10} {'输出字符':<10} {'completion_tokens':<18}")
    print("-" * 90)
    for r in all_results:
        status = "OK" if r.get("ok") else f"ERR{r.get('status','')}"
        elapsed = r.get("elapsed_sec", 0)
        out_len = r.get("output_len", 0)
        comp_tok = r.get("completion_tokens", 0)
        print(f"{r['model']:<28} {r['layer']:<10} {status:<6} {elapsed:<10.2f} {out_len:<10} {comp_tok:<18}")

    # 输出完整结果到 JSON
    if args.out:
        # 默认省略完整 output 文本（避免文件过大）
        out_data = []
        for r in all_results:
            item = dict(r)
            if not args.save_output:
                item.pop("output", None)
                item.pop("error", None)
            out_data.append(item)
        args.out.write_text(
            json.dumps(out_data, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"\n[info] 完整结果已写入: {args.out}")

    # 保存样本输出到独立文件（便于人工对比模型输出质量）
    if args.save_output:
        for r in all_results:
            if not r.get("ok"):
                continue
            fname = f"eval_output_{r['layer']}_{r['model']}_run{r['run']}.txt"
            with open(fname, "w", encoding="utf-8") as f:
                f.write(r.get("output", ""))
        print(f"[info] 各模型输出样本已保存为 eval_output_*.txt")

    return 0


if __name__ == "__main__":
    sys.exit(main())
