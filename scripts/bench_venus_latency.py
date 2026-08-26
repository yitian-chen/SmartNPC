#!/usr/bin/env python3
"""测试 Venus 请求体长度与延迟的关联。

模拟战术层 function calling 多轮请求（system + 历史 assistant tool_calls /
tool 结果 + user），变化历史轮数来改变请求体长度，测量每次调用的端到端延迟。

用法:
  VENUS_API_KEY=xxx python3 scripts/bench_venus_latency.py            # 默认轮数档
  VENUS_API_KEY=xxx python3 scripts/bench_venus_latency.py --turns 0,8,16,24,32 --repeat 3
  python3 scripts/bench_venus_latency.py --key-from-env-file           # 从项目 .env 读 key

输出每档的请求体大小（字符 / 估算 token）与延迟（均值/中位数/最小/最大），
最后给出长度-延迟的皮尔逊相关系数，判断二者是否线性相关。
"""

import argparse
import json
import os
import statistics
import sys
import time

import requests

VENUS_URL = os.environ.get("VENUS_URL", "http://v2.open.venus.oa.com/llmproxy/v1/chat/completions")
VENUS_MODEL = os.environ.get("VENUS_MODEL", "deepseek-v4-flash")

# 每条历史消息的填充文本（固定长度，保证请求体长度随轮数线性增长）
FILLER = "这是一段用于模拟战术层多轮对话历史的填充文本，用来撑大请求体的长度。".encode().decode()


def load_key(from_env_file=False):
    key = os.environ.get("VENUS_API_KEY", "")
    if key:
        return key
    if from_env_file:
        for candidate in (".env", "../.env"):
            if os.path.exists(candidate):
                for line in open(candidate, encoding="utf-8"):
                    line = line.strip()
                    if line.startswith("VENUS_API_KEY="):
                        return line.split("=", 1)[1].strip().strip('"').strip("'")
    return ""


def build_messages(history_rounds: int):
    """构造 [system, ...历史(assistant tool_calls + tool), user]，贴近战术层形态。"""
    msgs = [{
        "role": "system",
        "content": "你是小镇居民 NPC 的战术规划模块。根据世界背景、人物背景、世界详细信息，"
                   "把当前时段目标分解为动作序列。" + FILLER * 3,
    }]
    for i in range(history_rounds):
        # 每条历史 = 1 个 assistant（带 1 个 tool_call）+ 1 个 tool 结果
        msgs.append({
            "role": "assistant",
            "content": None,
            "tool_calls": [{
                "id": f"chatcmpl-tool-{i}",
                "type": "function",
                "function": {
                    "name": "speak",
                    "arguments": json.dumps({"content": FILLER}, ensure_ascii=False),
                },
            }],
        })
        msgs.append({"role": "tool", "tool_call_id": f"chatcmpl-tool-{i}",
                     "content": "result=success duration_ms=985"})
    msgs.append({"role": "user", "content": "当前时段目标：在中央广场长椅休息。请分解。" + FILLER})
    return msgs


def call_once(messages, timeout=180):
    body = {"model": VENUS_MODEL, "messages": messages, "max_tokens": 32, "stream": False}
    data = json.dumps(body, ensure_ascii=False).encode("utf-8")
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + KEY}
    t0 = time.time()
    try:
        r = requests.post(VENUS_URL, data=data, headers=headers, timeout=timeout)
        latency = time.time() - t0
        if r.status_code == 200:
            return latency, len(data), None
        return latency, len(data), f"HTTP {r.status_code}: {r.text[:120]}"
    except requests.Timeout:
        return time.time() - t0, len(data), "TIMEOUT"
    except requests.RequestException as e:
        return time.time() - t0, len(data), f"ERR {type(e).__name__}: {e}"


def est_tokens(char_len):
    # 中文约 1 字符 ≈ 1 token 的粗略估算；这里用 0.6 折算混合中英文
    return int(char_len * 0.6)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--turns", default="0,4,8,12,16,20,24,28,32,40",
                    help="逗号分隔的历史轮数档（默认 0,4,8,12,16,20,24,28,32,40）")
    ap.add_argument("--repeat", type=int, default=3, help="每档重复次数（默认 3）")
    ap.add_argument("--key-from-env-file", action="store_true", help="从项目 .env 读 VENUS_API_KEY")
    args = ap.parse_args()

    global KEY
    KEY = load_key(args.key_from_env_file)
    if not KEY:
        print("缺少 VENUS_API_KEY：请 export VENUS_API_KEY 或加 --key-from-env-file", file=sys.stderr)
        sys.exit(1)

    turns = [int(x) for x in args.turns.split(",") if x.strip()]
    print(f"model={VENUS_MODEL}")
    print(f"url={VENUS_URL}")
    print(f"{'历史轮数':>6} {'历史条数':>6} {'请求体字符':>10} {'估算token':>10} "
          f"{'均值(s)':>8} {'中位(s)':>8} {'最小(s)':>8} {'最大(s)':>8}  备注")
    print("-" * 90)

    rows = []  # (char_len, median_latency)
    for n in turns:
        msgs = build_messages(n)
        # 预构造请求体尺寸（与 n 相关，所有 repeat 相同）
        latencies = []
        errors = []
        for _ in range(args.repeat):
            lat, size, err = call_once(msgs)
            if err:
                errors.append(err)
            else:
                latencies.append(lat)
        if latencies:
            mean = statistics.mean(latencies)
            med = statistics.median(latencies)
            mn, mx = min(latencies), max(latencies)
            rows.append((size, med))
            note = f"({len(errors)} 次失败: {errors[0]})" if errors else ""
            print(f"{n:>6} {n*2:>6} {size:>10} {est_tokens(size):>10} "
                  f"{mean:>8.2f} {med:>8.2f} {mn:>8.2f} {mx:>8.2f}  {note}")
        else:
            print(f"{n:>6} {n*2:>6} {'--':>10} {'--':>10} 全失败: {errors[0] if errors else '?'}")

    # 皮尔逊相关系数（长度 vs 中位延迟）
    if len(rows) >= 3:
        xs = [r[0] for r in rows]
        ys = [r[1] for r in rows]
        mx_, my_ = statistics.mean(xs), statistics.mean(ys)
        cov = sum((x - mx_) * (y - my_) for x, y in rows)
        vx = sum((x - mx_) ** 2 for x in xs)
        vy = sum((y - my_) ** 2 for y in ys)
        if vx > 0 and vy > 0:
            r = cov / (vx * vy) ** 0.5
            print("-" * 90)
            print(f"长度-延迟 皮尔逊相关系数 r = {r:.3f}")
            print("|r|>0.7 强相关，0.4~0.7 中等相关，<0.4 弱相关")


if __name__ == "__main__":
    main()
