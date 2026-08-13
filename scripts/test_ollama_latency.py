#!/usr/bin/env python3
"""测试两个 Ollama 端点的推理延迟并对比。

默认对比：
  - 云开发容器本地 Ollama  http://localhost:11434
  - SSH 反向隧道 Ollama     http://localhost:11435

使用与反应层（reactive layer）一致的真实 prompt + 调用参数
（model=qwen2.5:7b-instruct-q4_K_M，num_predict=80，num_thread=16，
非流式 /api/chat），测量：
  - 冷启动延迟（先 keep_alive=0 卸载模型，再首次调用）
  - 热调用延迟分布（连续 N 次）
  - 吞吐（tokens/sec，按 prompt_eval_count + eval_count 估算）

用法：
  python scripts/test_ollama_latency.py                 # 默认 10 次热调用 + 跳过冷启动
  python scripts/test_ollama_latency.py --cold          # 含冷启动测试
  python scripts/test_ollama_latency.py --warm 20       # 20 次热调用
  python scripts/test_ollama_latency.py --prompt short  # 用短 prompt
  python scripts/test_ollama_latency.py --endpoint cloud        # 只测 11434
  python scripts/test_ollama_latency.py --endpoint tunnel       # 只测 11435
  python scripts/test_ollama_latency.py --num-thread 16         # 覆盖线程数
"""

import argparse
import json
import statistics
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from typing import Optional

# ─── 默认配置 ──────────────────────────────────────────────
DEFAULT_MODEL = "qwen2.5:7b-instruct-q4_K_M"
DEFAULT_NUM_PREDICT = 80
DEFAULT_NUM_THREAD = 16
DEFAULT_WARM_CALLS = 10
HTTP_TIMEOUT = 30  # 冷启动可能很久，给足超时

ENDPOINTS = {
    "cloud":   "http://localhost:11434",  # 云开发容器本地 Ollama
    "tunnel":  "http://localhost:11435",  # SSH 反向隧道到远端 Windows 主机
}

# ─── 真实反应层 prompt（取自 pkg/prompt/reactive.go 模板填充） ───
REACTIVE_PROMPT = """你是 NPC 老陈 的反应决策模块。当前情况需要你判断是否打断当前行动进行重规划。

【你的角色】
名字：老陈
职业：车间主管、装配工人、维护技师
背景：车间主管机器人，在工厂工作多年，对每台设备和每道工序都了如指掌。
性格特质：沉稳、念旧，耐久省电、磨损慢，可长期工作。
说话风格：简短有力，多用行业术语，偶尔提起过去的旧事。

【当前状态】
游戏时间：10:54
位置：main_workshop

【在途动作】
WorkShift(semantic_group=workbench, interaction=assemble)
来源：tactical（战术层规划的动作为深思熟虑的结果，非必要不打断）

【战术层上下文】
当前时段：09:00-12:00
每日计划摘要：
07:00-09:00: 去中央广场充电桩晨间补电
09:00-12:00: 在主生产车间工作台进行装配作业
12:00-14:00: 在住所休息并短时补电
14:00-18:00: 在主生产车间继续装配作业
18:00-22:00: 去机械维修厂修理台做自我维护保养
22:00-07:00: 夜间在休眠舱休息

【触发原因】
result=failed reason=claim_queued progress=0.00

【可选反应】
- continue：不打断，让当前行动继续
- observe：不打断，记录这个事件供后续参考
- replan：当前时段的整个战术规划已不合理（如物理状态无法支撑剩余 action、感知到新的 object / agent 希望改变原来的计划转而与之互动），请求战术层基于当前状态重新分解本时段 goal

判断要点：
- 战术层规划的动作通常是合理的，除非有明确理由，否则 continue
- replan 是"重大"决策：当你认为整个 action 队列都应作废、重新规划时使用。

请输出 JSON，格式严格如下，不要输出 JSON 以外的任何内容：
{"reaction": "continue|observe|replan", "reason": "简短理由"}

不要输出 JSON 以外的任何内容。"""

SHORT_PROMPT = "请用一句话回答：1+1 等于几？只输出数字。"

PROMPTS = {
    "reactive": REACTIVE_PROMPT,
    "short":    SHORT_PROMPT,
}


@dataclass
class CallResult:
    ok: bool
    latency_ms: float
    tokens_total: int = 0       # prompt_eval_count + eval_count
    tokens_eval: int = 0        # eval_count (output tokens)
    eval_duration_ms: float = 0  # 单次 eval 阶段耗时（用于算 tokens/sec）
    error: str = ""
    content: str = ""


@dataclass
class EndpointStats:
    name: str
    url: str
    cold: Optional[CallResult] = None
    warm: list[CallResult] = field(default_factory=list)

    @property
    def warm_ok(self) -> list[CallResult]:
        return [r for r in self.warm if r.ok]

    @property
    def warm_latencies_ms(self) -> list[float]:
        return [r.latency_ms for r in self.warm_ok]


def http_post_json(url: str, payload: dict, timeout: float = HTTP_TIMEOUT) -> tuple[int, dict, str]:
    """POST JSON，返回 (status, parsed_json_or_empty, raw_text_or_error)."""
    body = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8")
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        return e.code, {}, raw
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        return 0, {}, f"{type(e).__name__}: {e}"

    try:
        parsed = json.loads(raw)
    except json.JSONDecodeError:
        return status, {}, raw
    return status, parsed, raw


def unload_model(base_url: str, model: str) -> None:
    """通过 keep_alive=0 卸载模型，强制下次调用重新加载（模拟冷启动）。"""
    # /api/generate 对同一 model 设 keep_alive=0 即卸载
    payload = {
        "model": model,
        "prompt": "",
        "stream": False,
        "keep_alive": 0,
    }
    http_post_json(f"{base_url}/api/generate", payload, timeout=10)


def call_chat(base_url: str, model: str, prompt: str,
              num_predict: int, num_thread: int) -> CallResult:
    """调用 /api/chat（非流式），测量延迟并解析 token 统计。"""
    options = {"num_predict": num_predict}
    if num_thread >= 0:
        options["num_thread"] = num_thread

    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "stream": False,
        "options": options,
    }

    t0 = time.perf_counter()
    status, resp, err = http_post_json(
        f"{base_url}/api/chat", payload, timeout=HTTP_TIMEOUT
    )
    elapsed_ms = (time.perf_counter() - t0) * 1000.0

    if status != 200 or not resp:
        return CallResult(
            ok=False,
            latency_ms=elapsed_ms,
            error=f"HTTP {status}: {err[:200]}",
        )

    content = resp.get("message", {}).get("content", "")
    # token 统计字段（Ollama /api/chat 返回）
    prompt_eval_count = resp.get("prompt_eval_count", 0) or 0
    eval_count = resp.get("eval_count", 0) or 0
    eval_duration_ns = resp.get("eval_duration", 0) or 0
    eval_duration_ms = eval_duration_ns / 1e6

    return CallResult(
        ok=True,
        latency_ms=elapsed_ms,
        tokens_total=prompt_eval_count + eval_count,
        tokens_eval=eval_count,
        eval_duration_ms=eval_duration_ms,
        content=content,
    )


def percentile(values: list[float], p: float) -> float:
    """线性插值分位数，与 numpy.percentile 默认一致。"""
    if not values:
        return 0.0
    xs = sorted(values)
    if len(xs) == 1:
        return xs[0]
    k = (len(xs) - 1) * (p / 100.0)
    f = int(k)
    c = min(f + 1, len(xs) - 1)
    if f == c:
        return xs[f]
    return xs[f] + (xs[c] - xs[f]) * (k - f)


def fmt_ms(v: float) -> str:
    return f"{v:7.1f} ms"


def fmt_tok(v: float) -> str:
    if v <= 0:
        return "  -  "
    return f"{v:6.1f} t/s"


def test_endpoint(name: str, url: str, *, model: str, prompt: str,
                  num_predict: int, num_thread: int,
                  do_cold: bool, warm_calls: int) -> EndpointStats:
    print(f"\n{'='*70}")
    print(f"测试端点：{name}  ({url})")
    print(f"模型：{model}  num_predict={num_predict}  num_thread={num_thread}")
    print(f"{'='*70}")

    stats = EndpointStats(name=name, url=url)

    # ─── 冷启动测试 ───
    if do_cold:
        print(f"\n[冷启动] 卸载模型后首次调用 ...")
        unload_model(url, model)
        time.sleep(0.5)  # 让卸载生效
        print(f"  调用中（最长等待 {HTTP_TIMEOUT}s）...", end="", flush=True)
        result = call_chat(url, model, prompt, num_predict, num_thread)
        print(f" done.")
        stats.cold = result
        if result.ok:
            print(f"  冷启动延迟：{fmt_ms(result.latency_ms)}  "
                  f"输出 {result.tokens_eval} tokens  "
                  f"{fmt_tok(result.tokens_eval / (result.eval_duration_ms/1000) if result.eval_duration_ms > 0 else 0)}")
        else:
            print(f"  冷启动失败：{result.error}")

    # ─── 热调用测试 ───
    print(f"\n[热调用] 连续 {warm_calls} 次 ...")
    for i in range(1, warm_calls + 1):
        result = call_chat(url, model, prompt, num_predict, num_thread)
        stats.warm.append(result)
        if result.ok:
            tps = result.tokens_eval / (result.eval_duration_ms/1000) if result.eval_duration_ms > 0 else 0
            print(f"  #{i:2d}: {fmt_ms(result.latency_ms)}  "
                  f"eval {result.tokens_eval} tok / {result.eval_duration_ms:.0f} ms  "
                  f"{fmt_tok(tps)}  "
                  f"out={result.content[:40]!r}")
        else:
            print(f"  #{i:2d}: FAILED  {result.error[:100]}")
        # 热调用之间不加间隔——测的就是连续推理吞吐

    return stats


def summarize(stats: EndpointStats) -> None:
    print(f"\n--- {stats.name} ({stats.url}) 统计 ---")
    if stats.cold is not None:
        if stats.cold.ok:
            print(f"  冷启动：       {fmt_ms(stats.cold.latency_ms)}")
        else:
            print(f"  冷启动：       FAILED ({stats.cold.error[:60]})")

    lats = stats.warm_latencies_ms
    n_ok = len(lats)
    n_fail = len(stats.warm) - n_ok
    if n_ok == 0:
        print(f"  热调用：       全部失败 ({n_fail} 次)")
        return

    print(f"  热调用样本：   {n_ok} 成功 / {n_fail} 失败")
    print(f"  延迟 min：     {fmt_ms(min(lats))}")
    print(f"  延迟 max：     {fmt_ms(max(lats))}")
    print(f"  延迟 mean：    {fmt_ms(statistics.mean(lats))}")
    if n_ok >= 2:
        print(f"  延迟 stdev：   {fmt_ms(statistics.stdev(lats))}")
    print(f"  延迟 p50：     {fmt_ms(percentile(lats, 50))}")
    print(f"  延迟 p95：     {fmt_ms(percentile(lats, 95))}")

    # 吞吐统计（仅 eval 阶段，反映纯推理速度）
    tps_samples = [
        r.tokens_eval / (r.eval_duration_ms / 1000)
        for r in stats.warm_ok
        if r.eval_duration_ms > 0
    ]
    if tps_samples:
        print(f"  吞吐 mean：    {fmt_tok(statistics.mean(tps_samples))}")
        print(f"  吞吐 p50：     {fmt_tok(percentile(tps_samples, 50))}")

    # 输出 token 数（应稳定为 num_predict 上限附近）
    tok_samples = [r.tokens_eval for r in stats.warm_ok]
    if tok_samples:
        print(f"  输出 tokens：  mean={statistics.mean(tok_samples):.1f}  "
              f"min={min(tok_samples)}  max={max(tok_samples)}")


def compare(all_stats: list[EndpointStats]) -> None:
    """多端点横向对比表。"""
    if len(all_stats) < 2:
        return
    print(f"\n{'='*70}")
    print("横向对比")
    print(f"{'='*70}")
    header = f"{'指标':<14}" + "".join(f"{s.name:>18}" for s in all_stats)
    print(header)
    print("-" * len(header))

    def row(label: str, fmt_fn) -> str:
        cells = []
        for s in all_stats:
            cells.append(fmt_fn(s))
        return f"{label:<14}" + "".join(f"{c:>18}" for c in cells)

    def cold_str(s: EndpointStats) -> str:
        if s.cold is None:
            return "-"
        if s.cold.ok:
            return f"{s.cold.latency_ms:.1f} ms"
        return "FAILED"

    def warm_mean_str(s: EndpointStats) -> str:
        l = s.warm_latencies_ms
        return f"{statistics.mean(l):.1f} ms" if l else "-"

    def warm_p50_str(s: EndpointStats) -> str:
        l = s.warm_latencies_ms
        return f"{percentile(l, 50):.1f} ms" if l else "-"

    def warm_p95_str(s: EndpointStats) -> str:
        l = s.warm_latencies_ms
        return f"{percentile(l, 95):.1f} ms" if l else "-"

    def tps_str(s: EndpointStats) -> str:
        tps = [r.tokens_eval / (r.eval_duration_ms/1000)
               for r in s.warm_ok if r.eval_duration_ms > 0]
        return f"{statistics.mean(tps):.1f} t/s" if tps else "-"

    def success_str(s: EndpointStats) -> str:
        return f"{len(s.warm_ok)}/{len(s.warm)}"

    print(row("冷启动", cold_str))
    print(row("热调用 mean", warm_mean_str))
    print(row("热调用 p50", warm_p50_str))
    print(row("热调用 p95", warm_p95_str))
    print(row("吞吐 t/s", tps_str))
    print(row("成功率", success_str))

    # 结论性提示
    if all(len(s.warm_latencies_ms) >= 3 for s in all_stats):
        means = {s.name: statistics.mean(s.warm_latencies_ms) for s in all_stats}
        fastest = min(means, key=means.get)
        slowest = max(means, key=means.get)
        ratio = means[slowest] / means[fastest] if means[fastest] > 0 else 0
        print(f"\n  → {fastest} 端点平均延迟最低 ({means[fastest]:.1f} ms)，"
              f"{slowest} 比它慢 {ratio:.2f}x ({means[slowest]:.1f} ms)")


def main() -> int:
    parser = argparse.ArgumentParser(
        description="测试并对比两个 Ollama 端点的推理延迟。",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument(
        "--endpoint",
        choices=["cloud", "tunnel", "both"],
        default="both",
        help="选择测试端点（cloud=11434, tunnel=11435, both=两个都测）",
    )
    parser.add_argument(
        "--cold",
        action="store_true",
        help="包含冷启动测试（先卸载模型再首次调用，可能需要 10-60s）",
    )
    parser.add_argument(
        "--warm",
        type=int,
        default=DEFAULT_WARM_CALLS,
        help=f"热调用次数（默认 {DEFAULT_WARM_CALLS}）",
    )
    parser.add_argument(
        "--prompt",
        choices=list(PROMPTS.keys()),
        default="reactive",
        help="使用的 prompt：reactive=反应层真实 prompt，short=一句话短 prompt",
    )
    parser.add_argument(
        "--model",
        default=DEFAULT_MODEL,
        help=f"模型名（默认 {DEFAULT_MODEL}）",
    )
    parser.add_argument(
        "--num-predict",
        type=int,
        default=DEFAULT_NUM_PREDICT,
        help=f"最大输出 token 数（默认 {DEFAULT_NUM_PREDICT}，与反应层一致）",
    )
    parser.add_argument(
        "--num-thread",
        type=int,
        default=DEFAULT_NUM_THREAD,
        help=f"推理线程数（默认 {DEFAULT_NUM_THREAD}，0=让 Ollama 自决，-1=省略字段）",
    )
    args = parser.parse_args()

    prompt = PROMPTS[args.prompt]
    selected = ["cloud", "tunnel"] if args.endpoint == "both" else [args.endpoint]

    print(f"测试配置：")
    print(f"  端点：{', '.join(f'{n}={ENDPOINTS[n]}' for n in selected)}")
    print(f"  模型：{args.model}")
    print(f"  Prompt：{args.prompt} ({len(prompt)} 字符)")
    print(f"  num_predict={args.num_predict}  num_thread={args.num_thread}")
    print(f"  冷启动测试：{'是' if args.cold else '否'}")
    print(f"  热调用次数：{args.warm}")

    all_stats: list[EndpointStats] = []
    for name in selected:
        url = ENDPOINTS[name]
        stats = test_endpoint(
            name, url,
            model=args.model,
            prompt=prompt,
            num_predict=args.num_predict,
            num_thread=args.num_thread,
            do_cold=args.cold,
            warm_calls=args.warm,
        )
        all_stats.append(stats)
        summarize(stats)

    compare(all_stats)
    return 0


if __name__ == "__main__":
    sys.exit(main())
