#!/usr/bin/env python3
"""cmd_effect_summary.py — 从仿真日志推导各 cmd 对属性的影响，转成自然语言。

原理：world KB / capability_registry 中没有数值化的属性影响表，但仿真日志
同时记录了每个 agent 的物理状态时间序列（perception_update.physical_state_delta，
绝对值）与下发的动作（action_command.cmd + params.interaction）。把相邻两条
perception 之间的属性变化归因到当时在执行的动作，按 (cmd, interaction)
聚合成"每游戏小时平均变化率"，再分档转成自然语言（如"疲劳度明显提升，
余额快速增加"）。

输出写入 assets/cmd_effects.md（可通过 --out 覆盖），由 MCP 服务启动时读取
并注入战略层 prompt 的【动作对属性的影响】段（见 pkg/prompt/strategic.go）。

用法：
    python3 scripts/cmd_effect_summary.py logs-dev/2026-08-19/debug-mcp.log
    python3 scripts/cmd_effect_summary.py logs-dev/*/debug-mcp.log --out assets/cmd_effects.md

分档（每游戏小时 |rate|）：
    <1    忽略（视为噪声：待机漂移、过渡段残余等）
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
from datetime import datetime

ATTRS = ("energy", "fatigue", "joint_wear", "money")

# UE cmd → 战略层 prompt 中的工具名（与 tools.CmdToToolName 一致）
TOOL_NAME = {
    "WorkShift": "work_shift",
    "ChargeAtStation": "charge_at_station",
    "SelfMaintenance": "self_maintenance",
    "RestAtResidence": "rest_at_residence",
    "SurfInternet": "surf_internet",
    "SocialChat": "social_chat",
    "Exercise": "exercise",
    "InteractSmartObject": "InteractSmartObject",
    "MoveTo": "move_to",
    "TurnTo": "turn_to",
    "Speak": "speak",
    "Emote": "emote",
    "GenericAct": "generic_act",
    "Wait": "wait",
}

# 属性 → (显示名, 上升动词, 下降动词)；None 表示该方向不出现（如磨损只升不降）
ATTR_WORD = {
    "energy": ("能量", "恢复", "下降"),
    "fatigue": ("疲劳度", "提升", "缓解"),
    "joint_wear": ("关节磨损", "累积", None),
    "money": ("余额", "增加", "消耗"),
}

MAGNITUDES = [  # (下限, 描述词) 按 rate 绝对值从大到小匹配
    (20, "快速"),
    (10, "明显"),
    (4, "中等"),
    (1, "少量"),
]

# 短动作/无属性语义的 cmd 不进入输出（即使统计出微小漂移）
SKIP_CMDS = {"MoveTo", "TurnTo", "Speak", "Emote", "GenericAct", "Wait", "SocialChat"}


def parse_time(s):
    return datetime.fromisoformat(s)


def parse_log(path, states, acts):
    """解析单个日志，追加到 states/acts。返回处理的记录数。"""
    n = 0
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            rtype = rec.get("type")
            if rtype not in ("perception_update", "action_command"):
                continue
            payload = rec.get("payload")
            if isinstance(payload, str):
                try:
                    payload = json.loads(payload)
                except json.JSONDecodeError:
                    continue
            if not isinstance(payload, dict):
                continue
            agent = rec.get("agent_id")
            if not agent:
                continue
            if rtype == "perception_update":
                env = payload.get("environment") or {}
                ps = payload.get("physical_state_delta") or {}
                if env.get("game_time_sec") is None:
                    continue
                if not all(a in ps for a in ATTRS):
                    continue
                states[agent].append(
                    (parse_time(rec["time"]), env["game_time_sec"], ps)
                )
            else:
                params = payload.get("params") or {}
                acts[agent].append(
                    (
                        parse_time(rec["time"]),
                        payload.get("cmd", ""),
                        params.get("interaction"),
                    )
                )
            n += 1
    return n


def aggregate(states, acts):
    """相邻 perception 区间的属性变化归因到当时在执行的 (cmd, interaction)。

    返回 {key: {"hours": h, "rate": {attr: 每游戏小时变化率}, "samples": n}}。
    key 形如 "WorkShift/assemble"（无 interaction 参数则不含后缀）。
    """
    delta = defaultdict(lambda: defaultdict(float))
    hours = defaultdict(float)
    samples = defaultdict(int)
    for agent, ss in states.items():
        if len(ss) < 2:
            continue
        aa = sorted(acts.get(agent, []))
        for (w1, g1, s1), (w2, g2, s2) in zip(ss, ss[1:]):
            dg = g2 - g1
            if dg <= 0:
                continue
            # 归因：区间末时刻 w2 之前最后下发的动作（近似覆盖该区间）
            cur = None
            for (aw, cmd, itx) in aa:
                if aw <= w2:
                    cur = (cmd, itx)
                else:
                    break
            if cur is None:
                continue  # 无动作在执行的区间不参与（待机漂移基线）
            cmd, itx = cur
            key = f"{cmd}/{itx}" if itx else cmd
            for attr in ATTRS:
                delta[key][attr] += s2[attr] - s1[attr]
            hours[key] += dg / 3600.0
            samples[key] += 1
    out = {}
    for key, h in hours.items():
        if h <= 0:
            continue
        out[key] = {
            "hours": h,
            "samples": samples[key],
            "rate": {a: delta[key][a] / h for a in ATTRS},
        }
    return out


def magnitude(rate):
    for lo, word in MAGNITUDES:
        if abs(rate) >= lo:
            return word
    return None  # <1/h，视为噪声


def describe(rates):
    """把每游戏小时变化率转成自然语言短语列表，如"疲劳度明显提升"。"""
    parts = []
    for attr in ATTRS:
        r = rates[attr]
        if abs(r) < 1:
            continue
        word = magnitude(r)
        name, up, down = ATTR_WORD[attr]
        if r > 0:
            parts.append(f"{name}{word}{up}")
        else:
            verb = down or "下降"
            parts.append(f"{name}{word}{verb}")
    return parts


def build_lines(agg, min_hours):
    """生成输出行。返回 (lines, skipped)。"""
    lines, skipped = [], []
    # 排序：游戏小时数降序（观测最充分的在前）
    for key in sorted(agg, key=lambda k: -agg[k]["hours"]):
        info = agg[key]
        cmd = key.split("/", 1)[0]
        if cmd in SKIP_CMDS:
            continue
        if info["hours"] < min_hours:
            skipped.append((key, info, "样本时长不足"))
            continue
        parts = describe(info["rate"])
        if not parts:
            skipped.append((key, info, "无显著属性变化"))
            continue
        name = TOOL_NAME.get(cmd, cmd)
        display = f"{name}（{key.split('/', 1)[1]}）" if "/" in key else name
        lines.append(f"- {display}：{'、'.join(parts)}")
    return lines, skipped


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("logs", nargs="+", help="debug-mcp.log 路径（支持 glob）")
    ap.add_argument("--out", default="assets/cmd_effects.md",
                    help="输出文件（默认 assets/cmd_effects.md，传空串则只打印）")
    ap.add_argument("--min-hours", type=float, default=0.5,
                    help="每个 (cmd, interaction) 的最小观测游戏小时数（默认 0.5）")
    args = ap.parse_args()

    paths = []
    for p in args.logs:
        matched = sorted(glob.glob(p))
        paths.extend(matched if matched else [p])
    states, acts = defaultdict(list), defaultdict(list)
    total = 0
    for p in paths:
        try:
            total += parse_log(p, states, acts)
        except FileNotFoundError:
            print(f"warning: log not found: {p}", file=sys.stderr)
    if not states:
        print("error: no perception_update records parsed", file=sys.stderr)
        sys.exit(1)

    agg = aggregate(states, acts)
    lines, skipped = build_lines(agg, args.min_hours)

    header = "各活动对属性的影响（每游戏小时平均变化，由仿真日志统计）："
    body = "\n".join([header] + lines) + "\n"

    print(body)
    if skipped:
        print("跳过（未写入输出）：", file=sys.stderr)
        for key, info, why in skipped:
            print(f"  {key}: {why}（{info['hours']:.2f} 游戏小时, "
                  f"{info['samples']} 采样）", file=sys.stderr)

    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(body)
        print(f"\nwritten to {args.out}", file=sys.stderr)
    print(f"\nstats: {total} records, {len(states)} agents, "
          f"{len(agg)} (cmd,interaction) keys, {len(lines)} lines", file=sys.stderr)


if __name__ == "__main__":
    main()
