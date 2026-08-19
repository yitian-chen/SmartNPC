#!/usr/bin/env python3
"""cmd_effect_summary.py — 从日志中的 world_kb 消息提取各互动对属性的影响，
转成自然语言。

原理：UE 推送的 world_kb 消息里，每个 object 的 available_interactions
直接声明了属性影响速率（energy/fatigue/joint_wear/money_delta_per_hour，
另有 money_one_shot 一次性变动）。本脚本取最后一条 world_kb 消息，
按 (semantic_group, interaction) 去重聚合并分档转成自然语言
（如"疲劳度明显提升、余额快速增加"）。

输出写入 assets/cmd_effects.md（可通过 --out 覆盖），由 MCP 服务启动时读取
并注入战略层 prompt 的【动作对属性的影响】段（见 pkg/prompt/strategic.go）。

用法：
    python3 scripts/cmd_effect_summary.py logs-dev/2026-08-19/debug-mcp.log
    python3 scripts/cmd_effect_summary.py logs-dev/*/debug-mcp.log --out assets/cmd_effects.md

分档（每游戏小时 |速率|）：
    <1    忽略（视为无显著影响）
    1-4   少量
    4-10  中等
    10-20 明显
    >=20  快速
"""

import argparse
import glob
import json
import sys
from collections import defaultdict

RATE_ATTRS = ("energy", "fatigue", "joint_wear", "money")

# 属性 → (显示名, 上升动词, 下降动词)
ATTR_WORD = {
    "energy": ("能量", "恢复", "下降"),
    "fatigue": ("疲劳度", "提升", "缓解"),
    "joint_wear": ("关节磨损", "累积", "修复"),
    "money": ("余额", "增加", "消耗"),
}

MAGNITUDES = [  # (下限, 描述词) 按 |速率| 从大到小匹配
    (20, "快速"),
    (10, "明显"),
    (4, "中等"),
    (1, "少量"),
]

# 影响速率字段名（world_kb available_interactions 内）
RATE_FIELDS = {a: f"{a}_delta_per_hour" for a in RATE_ATTRS}
ONE_SHOT_FIELD = "money_one_shot"


def parse_world_kb(paths):
    """依序解析日志文件，返回最后一条 world_kb 消息的 payload（dict）。
    无 world_kb 消息返回 None。"""
    last = None
    for path in paths:
        try:
            f = open(path, encoding="utf-8")
        except FileNotFoundError:
            print(f"warning: log not found: {path}", file=sys.stderr)
            continue
        with f:
            for line in f:
                line = line.strip()
                if not line or '"world_kb"' not in line:
                    continue
                try:
                    rec = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if rec.get("type") != "world_kb":
                    continue
                payload = rec.get("payload")
                if isinstance(payload, str):
                    try:
                        payload = json.loads(payload)
                    except json.JSONDecodeError:
                        continue
                if isinstance(payload, dict) and "generated" in payload:
                    last = payload
    return last


def collect_interactions(payload):
    """从 world_kb payload 提取 (semantic_group, interaction) → 影响声明。

    返回 {key: {"display": 物体显示名, "rates": {attr: 每小时速率},
    "one_shot": 一次性余额变动}}。同一 key 出现多种速率时取首个并在
    stderr 提示。字符串形态的 available_interactions（旧格式无速率）跳过。
    """
    out = {}
    conflicts = []
    for obj in payload.get("generated", {}).get("objects", []):
        sg = obj.get("semantic_group") or obj.get("id", "")
        display = obj.get("display_name") or sg
        for itx in obj.get("available_interactions", []):
            if not isinstance(itx, dict):
                continue  # 旧格式：仅字符串动词，无速率声明
            name = itx.get("name")
            if not name:
                continue
            key = (sg, name)
            rates = {a: float(itx.get(f, 0) or 0) for a, f in RATE_FIELDS.items()}
            one_shot = float(itx.get(ONE_SHOT_FIELD, 0) or 0)
            if key in out and out[key]["rates"] != rates:
                conflicts.append(key)
                continue
            out[key] = {"display": display, "rates": rates, "one_shot": one_shot}
    for key in conflicts:
        print(f"warning: {key[0]}/{key[1]} 存在多种速率声明，取首个", file=sys.stderr)
    return out


def magnitude(rate):
    for lo, word in MAGNITUDES:
        if abs(rate) >= lo:
            return word
    return None  # <1/h，无显著影响


def describe(rates, one_shot):
    """把每小时速率 + 一次性变动转成自然语言短语列表。"""
    parts = []
    for attr in RATE_ATTRS:
        r = rates[attr]
        if abs(r) < 1:
            continue
        word = magnitude(r)
        name, up, down = ATTR_WORD[attr]
        parts.append(f"{name}{word}{up if r > 0 else down}")
    if one_shot:
        verb = "增加" if one_shot > 0 else "消耗"
        parts.append(f"一次性{verb}余额 {abs(one_shot):g} 点")
    return parts


def build_lines(interactions):
    lines = []
    for (sg, name), info in sorted(interactions.items()):
        parts = describe(info["rates"], info["one_shot"])
        desc = "、".join(parts) if parts else "无属性影响"
        lines.append(f"- {info['display']}（{sg}/{name}）：{desc}")
    return lines


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("logs", nargs="+", help="debug-mcp.log 路径（支持 glob，取最后一条 world_kb 消息）")
    ap.add_argument("--out", default="assets/cmd_effects.md",
                    help="输出文件（默认 assets/cmd_effects.md，传空串则只打印）")
    args = ap.parse_args()

    paths = []
    for p in args.logs:
        matched = sorted(glob.glob(p))
        paths.extend(matched if matched else [p])

    payload = parse_world_kb(paths)
    if payload is None:
        print("error: no world_kb message found in given logs", file=sys.stderr)
        sys.exit(1)

    interactions = collect_interactions(payload)
    if not interactions:
        print("error: world_kb carries no rate-declared interactions "
              "(available_interactions without *_delta_per_hour fields?)", file=sys.stderr)
        sys.exit(1)

    lines = build_lines(interactions)
    header = "各活动对属性的影响（每游戏小时变化率，来自 world KB 声明）："
    body = "\n".join([header] + lines) + "\n"

    print(body)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(body)
        print(f"written to {args.out}", file=sys.stderr)
    print(f"stats: {len(payload.get('generated', {}).get('objects', []))} objects, "
          f"{len(interactions)} (semantic_group, interaction) keys", file=sys.stderr)


if __name__ == "__main__":
    main()
