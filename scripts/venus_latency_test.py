#!/usr/bin/env python3
"""Venus deepseek-v4-flash 延迟与稳定性测试。

模拟 AgentTown 战略层真实调用形态（system+user、json_schema strict、
相近的 prompt 规模），分两组测量：
  1. 串行：N 次顺序调用，测基线延迟分布
  2. 并发：M 路同时调用（模拟 07:00 五个 NPC 同时生成每日计划），
     复现"超时-超时"占位内容等网关不稳定问题

用法：
  python3 venus_latency_test.py [--rounds 10] [--concurrency 5] [--timeout 60]

凭据从 /data/workspace/dev/.env 的 VENUS_API_KEY 读取。
"""
import argparse
import concurrent.futures
import json
import os
import re
import statistics
import sys
import time
import urllib.request
import urllib.error

ENV_PATH = os.path.join(os.path.dirname(__file__), "..", ".env")
BASE_URL = os.environ.get("VENUS_URL", "http://v2.open.venus.oa.com/llmproxy")
MODEL = os.environ.get("VENUS_MODEL", "deepseek-v4-flash")

# 与 pkg/prompt/strategic.go 的 StrategicSystemPrompt 同源（精简版，规模相当）
SYSTEM_PROMPT = """你是小镇居民 NPC 的战略规划模块。每天清晨 07:00，你根据用户信息中提供的角色身份、今日日程、物理状态、世界知识与可用能力，规划当天 07:00 到次日 07:00 的活动安排。

【动作对状态的影响】
- work_shift（装配/分拣等）：消耗能量、积累疲劳与少量关节磨损，赚取余额
- charge_at_station（充电）：恢复能量、缓解疲劳，消耗余额
- self_maintenance（维护）：缓解关节磨损，大量消耗余额
- rest_at_residence（休息）：缓解疲劳
- surf_internet（上网）：少量消耗能量与余额、缓解疲劳

要求：
1. 输出 JSON 数组（6-8 条），每条只含 "time"（"HH:MM-HH:MM"）和 "goal"（一句话，必须是纯文本字符串），以 [ 开头 ] 结尾，不要其他文字
2. 每个时段 ≥120 分钟，仅安排一项主要任务；连续两个任务相同的时段合并为一个长时段
3. 规划每个时段时，先想清楚这个时段的活动要用用户信息中【可用能力】里哪个 cmd 实现
4. goal 中提到的地点、人物、设备必须是用户信息中【你的角色】和【世界知识】里存在的，不得编造
5. 首段（07:00 起）必须是日间活动，不得安排休眠；末段跨午夜时结束时间表示次日时刻
6. 充电仅在能量为"低电量"或疲劳为"非常疲劳"时安排；能量充足时优先产出性活动"""

USER_PROMPT = """[战略层/每日规划] 现在是仿真时间 07:00，新的一天开始了。

【你的角色】
名字：老陈
职业：装配工人（专做工作台装配作业）
背景：资深装配工人，常驻主生产车间工作台，专精半成品装配工序。
性格特质：沉稳、念旧，耐久省电、磨损慢
说话风格：简短有力，多用行业术语

【物理状态】
物理状态：能量 充足、疲劳 精神饱满、关节磨损 良好、余额 200。

【世界知识】
- 主生产车间（id=main_workshop）：工作台（workbench）、调试台（debug_station）、质检台（inspection_table）
- 中央广场（id=central_plaza）：长椅（bench）、充电桩（charger）
- 休眠舱居住区（id=residential_quarters）：睡眠舱（sleep_pod）

【可用能力】
- 工作班次（work_shift）
- 在充电站充电（charge_at_station）
- 自我维护保养（self_maintenance）
- 在住所休息（rest_at_residence）
- 上网浏览（surf_internet）

昨日总结：昨天按计划完成了车间装配。

请基于你的角色身份和性格，规划今天一天的活动安排。
只输出 JSON 数组，每条形如 {"time":"HH:MM-HH:MM","goal":"纯文本一句话"}，"goal" 必须是字符串，不要输出任何其他文字。"""

SCHEMA = {
    "type": "array",
    "items": {
        "type": "object",
        "properties": {"time": {"type": "string"}, "goal": {"type": "string"}},
        "required": ["time", "goal"],
        "additionalProperties": False,
    },
}

# 占位/垃圾内容特征（实际日志中观测到的网关超时占位输出）
PLACEHOLDER_PATTERNS = [
    re.compile(r"超时"),
    re.compile(r"此条无效"),
    re.compile(r"未安排-未安排"),
    re.compile(r'"time"\s*:\s*"-"\s*,\s*"goal"\s*:\s*"-"'),
]
TIME_RE = re.compile(r"\d{1,2}:\d{2}\s*-\s*\d{1,2}:\d{2}")


def load_api_key():
    with open(ENV_PATH, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if line.startswith("VENUS_API_KEY="):
                return line.split("=", 1)[1].strip()
    sys.exit(f"VENUS_API_KEY not found in {ENV_PATH}")


def classify(raw_text: str):
    """返回 (ok, reason)：ok=输出是可用计划；reason 描述失败类型。"""
    if not raw_text or not raw_text.strip():
        return False, "empty"
    for pat in PLACEHOLDER_PATTERNS:
        if pat.search(raw_text):
            return False, "placeholder"
    s = raw_text.strip()
    if s.startswith("```"):
        s = s.strip("`")
    start, end = s.find("["), s.rfind("]")
    if start < 0 or end <= start:
        return False, "no-json-array"
    try:
        items = json.loads(s[start : end + 1])
    except json.JSONDecodeError:
        return False, "bad-json"
    if not isinstance(items, list) or not items:
        return False, "empty-plan"
    valid = sum(
        1
        for it in items
        if isinstance(it, dict)
        and isinstance(it.get("time"), str)
        and isinstance(it.get("goal"), str)
        and TIME_RE.fullmatch(it["time"].strip())
    )
    if valid == 0:
        return False, "no-valid-slot"
    if valid < len(items):
        return True, f"partial({valid}/{len(items)})"
    return True, "ok"


def call_once(api_key: str, timeout: float, seq: int):
    body = {
        "model": MODEL,
        "max_tokens": 2048,
        "messages": [
            {"role": "system", "content": SYSTEM_PROMPT},
            {"role": "user", "content": USER_PROMPT},
        ],
        "response_format": {
            "type": "json_schema",
            "json_schema": {"name": "daily_plan", "strict": True, "schema": SCHEMA},
        },
    }
    req = urllib.request.Request(
        BASE_URL.rstrip("/") + "/v1/chat/completions",
        data=json.dumps(body).encode(),
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
            "Venus-Sticky-Routing": "token",
        },
        method="POST",
    )
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            payload = json.loads(resp.read())
        latency = time.monotonic() - t0
        text = ""
        for ch in payload.get("choices", []):
            text += ch.get("message", {}).get("content", "") or ""
        usage = payload.get("usage", {})
        ok, reason = classify(text)
        return {
            "seq": seq,
            "latency": latency,
            "ok": ok,
            "reason": reason,
            "text_head": text[:80].replace("\n", " "),
            "completion_tokens": usage.get("completion_tokens", -1),
        }
    except urllib.error.HTTPError as e:
        return {"seq": seq, "latency": time.monotonic() - t0, "ok": False,
                "reason": f"http-{e.code}", "text_head": e.read()[:80].decode("utf-8", "replace"),
                "completion_tokens": -1}
    except Exception as e:  # noqa: BLE001
        return {"seq": seq, "latency": time.monotonic() - t0, "ok": False,
                "reason": f"{type(e).__name__}: {e}"[:60], "text_head": "",
                "completion_tokens": -1}


def pct(sorted_vals, p):
    if not sorted_vals:
        return 0.0
    idx = min(len(sorted_vals) - 1, int(round(p / 100 * (len(sorted_vals) - 1))))
    return sorted_vals[idx]


def report(tag, results):
    ok_results = [r for r in results if r["ok"]]
    lat = sorted(r["latency"] for r in results)
    ok_lat = sorted(r["latency"] for r in ok_results)
    fails = [r for r in results if not r["ok"]]
    print(f"\n===== {tag} =====")
    print(f"调用数: {len(results)}  成功: {len(ok_results)}  失败: {len(fails)}  "
          f"成功率: {len(ok_results)/len(results)*100:.0f}%")
    if lat:
        print(f"延迟(全部):  min={lat[0]:.2f}s  p50={pct(lat,50):.2f}s  "
              f"p95={pct(lat,95):.2f}s  max={lat[-1]:.2f}s")
    if ok_lat:
        print(f"延迟(成功):  min={ok_lat[0]:.2f}s  p50={pct(ok_lat,50):.2f}s  "
              f"p95={pct(ok_lat,95):.2f}s  max={ok_lat[-1]:.2f}s")
    toks = [r["completion_tokens"] for r in ok_results if r["completion_tokens"] > 0]
    if toks:
        print(f"completion_tokens: min={min(toks)} avg={statistics.mean(toks):.0f} max={max(toks)}")
    if fails:
        print("失败明细:")
        by_reason = {}
        for r in fails:
            by_reason.setdefault(r["reason"], []).append(r)
        for reason, rs in sorted(by_reason.items(), key=lambda kv: -len(kv[1])):
            lats = "/".join(f"{r['latency']:.1f}s" for r in rs[:5])
            print(f"  [{reason}] x{len(rs)}  延迟: {lats}")
            head = rs[0]["text_head"]
            if head:
                print(f"    输出头: {head}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--rounds", type=int, default=10, help="串行调用次数")
    ap.add_argument("--concurrency", type=int, default=5, help="并发路数（模拟 NPC 数）")
    ap.add_argument("--batches", type=int, default=2, help="并发批次数")
    ap.add_argument("--timeout", type=float, default=60.0)
    args = ap.parse_args()

    api_key = load_api_key()
    print(f"目标: {BASE_URL}  模型: {MODEL}  超时: {args.timeout}s")
    print(f"计划: 串行 {args.rounds} 次；并发 {args.batches} 批 x {args.concurrency} 路")

    # 串行组
    serial = []
    for i in range(args.rounds):
        r = call_once(api_key, args.timeout, i + 1)
        serial.append(r)
        mark = "OK " if r["ok"] else "FAIL"
        print(f"  [串行 {i+1:02d}] {mark} {r['latency']:6.2f}s  {r['reason']}")

    # 并发组
    concurrent_results = []
    for b in range(args.batches):
        with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as ex:
            futures = [ex.submit(call_once, api_key, args.timeout, b + 1)
                       for b in range(args.concurrency)]
            batch = [f.result() for f in futures]
        concurrent_results.extend(batch)
        marks = " ".join(("OK" if r["ok"] else "F") + f"({r['latency']:.1f}s)" for r in batch)
        print(f"  [并发批次 {b+1}] {marks}")

    report("串行", serial)
    report("并发", concurrent_results)
    report("汇总", serial + concurrent_results)


if __name__ == "__main__":
    main()
