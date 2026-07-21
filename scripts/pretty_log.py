#!/usr/bin/env python3
"""pretty_log.py — 可读化查看 sim.log（JSON Lines）。

sim.log 每行是一条 JSON，单行可能上千字（perception text 完整不截断）。
本脚本把每条日志渲染成多行彩色文本，便于人工浏览和排查。

用法：
    python scripts/pretty_log.py                       # 查看今天的 sim.log
    python scripts/pretty_log.py 2026-07-20            # 指定日期
    python scripts/pretty_log.py logs/2026-07-20/sim.log  # 指定文件
    python scripts/pretty_log.py -f UE→MCP             # 只看某方向的日志
    python scripts/pretty_log.py -f PERCEPTION -n 50   # 最近 50 条 PERCEPTION
    python scripts/pretty_log.py --no-color            # 禁用颜色（输出到文件时用）
    python scripts/pretty_log.py --raw                 # 原始 JSON，不渲染

方向过滤器（-f）支持的简写（不区分大小写）：
    UE→MCP      UE→MCP（感知/状态/动作完成）
    MCP→UE      MCP→UE（动作命令/叙事）
    PERCEPTION  MCP→Hermes/PERCEPTION（传入 Hermes 的感知原文）
    RESPONSE    Hermes→MCP/RESPONSE（LLM 响应 + narrative）
    TOOL        Hermes→MCP/TOOL（Hermes 调用的工具）
    HEARTBEAT   心跳（默认隐藏，可显式过滤查看）

依赖：仅 Python 3 标准库。
"""
from __future__ import annotations

import argparse
import datetime as _dt
import io
import json
import os
import sys
from pathlib import Path

# Windows 控制台默认 GBK，会破坏 UTF-8 字符（如 →、替换符、中文）。
# 强制 stdout/stderr 走 UTF-8。
if sys.stdout.encoding and sys.stdout.encoding.lower() != "utf-8":
    sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")
    sys.stderr = io.TextIOWrapper(sys.stderr.buffer, encoding="utf-8", errors="replace")

# ── 颜色 ──────────────────────────────────────────────────────────────
_ANSI = {
    "reset": "\033[0m",
    "dim": "\033[2m",
    "time": "\033[36m",      # cyan
    "level_info": "\033[32m",   # green
    "level_warn": "\033[33m",   # yellow
    "level_error": "\033[31m",  # red
    "msg": "\033[1;37m",        # bold white
    "key": "\033[34m",          # blue
    "value": "\033[0m",         # default
    "marker": "\033[35m",       # magenta — 方向标记 [UE→MCP] 等
    "payload": "\033[90m",      # bright black / gray — 长 payload
}

# 方向简写 → 完整 msg 子串
_DIRECTION_ALIASES = {
    "UE→MCP": "UE→MCP",
    "MCP→UE": "MCP→UE",
    "PERCEPTION": "MCP→Hermes/PERCEPTION",
    "RESPONSE": "Hermes→MCP/RESPONSE",
    "TOOL": "Hermes→MCP/TOOL",
    "HEARTBEAT": "heartbeat",
}


def _color(code: str, text: str, enabled: bool) -> str:
    if not enabled:
        return text
    return f"{_ANSI[code]}{text}{_ANSI['reset']}"


def _format_time(iso: str, color: bool) -> str:
    """2026-07-20T20:24:31.7207912+08:00 → 20:24:31.720"""
    try:
        # 截到毫秒
        # iso 形如 2026-07-20T20:24:31.7207912+08:00
        t = iso.split("T", 1)[1]
        # 找时区分隔（+/- 或 Z）
        for i, ch in enumerate(t):
            if ch in "+-Z" and i > 0:
                t = t[:i]
                break
        # 截到毫秒
        if "." in t:
            head, frac = t.split(".", 1)
            t = f"{head}.{frac[:3]}"
        return _color("time", t, color)
    except Exception:
        return _color("time", iso, color)


def _format_level(level: str, color: bool) -> str:
    code = {
        "INFO": "level_info",
        "WARN": "level_warn",
        "WARNING": "level_warn",
        "ERROR": "level_error",
        "DEBUG": "dim",
    }.get(level.upper(), "dim")
    return _color(code, level.upper().ljust(5), color)


def _is_direction_marker(msg: str) -> bool:
    return msg.startswith("[") and "]" in msg


def _format_value(v, color: bool, indent: str = "    ") -> str:
    """渲染一个值：字符串原样；dict/list 多行缩进；其它 str(v)。"""
    if isinstance(v, str):
        return v
    if isinstance(v, (dict, list)):
        try:
            return json.dumps(v, ensure_ascii=False, indent=indent)
        except Exception:
            return str(v)
    return str(v)


def _try_pretty_payload(s: str, color: bool) -> str:
    """对 payload 字段（JSON 字符串）尝试美化。失败则原样返回。"""
    if not s or not s.startswith("{"):
        return s
    try:
        obj = json.loads(s)
        return _format_value(obj, color)
    except Exception:
        return s


def render_line(line: str, color: bool, show_source: bool = False) -> str:
    """把一行 JSON 渲染成多行可读文本。"""
    line = line.strip()
    if not line:
        return ""
    try:
        rec = json.loads(line)
    except json.JSONDecodeError:
        return _color("dim", f"[non-JSON] {line}", color)

    time_s = _format_time(rec.get("time", ""), color)
    level = _format_level(str(rec.get("level", "")), color)
    msg = str(rec.get("msg", ""))

    # msg 若是方向标记，单独着色
    if _is_direction_marker(msg):
        msg_s = _color("marker", msg, color)
    else:
        msg_s = _color("msg", msg, color)

    # source 默认隐藏（太长）
    source_s = ""
    if show_source and "source" in rec:
        src = rec["source"]
        if isinstance(src, dict):
            fname = Path(src.get("file", "")).name
            src_line = src.get("line", "")
            source_s = f"  {_color('dim', f'@{fname}:{src_line}', color)}"

    header = f"{time_s} {level} {msg_s}{source_s}"

    # 其它字段
    skip = {"time", "level", "msg", "source"}
    parts = [header]
    for k in rec:
        if k in skip:
            continue
        v = rec[k]
        # payload 字段特殊处理：尝试解析嵌套 JSON
        if k == "payload" and isinstance(v, str):
            v = _try_pretty_payload(v, color)
            parts.append(f"  {_color('key', k, color)}:")
            for pl in _format_value(v, color).splitlines():
                parts.append(f"    {_color('payload', pl, color)}")
            continue
        # text / narrative 字段：可能含真实换行（JSON 中转义为 \n），按行渲染
        if k in ("text", "narrative") and isinstance(v, str):
            parts.append(f"  {_color('key', k, color)}:")
            for ln in v.split("\n"):
                parts.append(f"    {ln}")
            continue
        # 一般字段：单行
        rendered = _format_value(v, color)
        if "\n" in rendered:
            parts.append(f"  {_color('key', k, color)}:")
            for ln in rendered.splitlines():
                parts.append(f"    {ln}")
        else:
            parts.append(f"  {_color('key', k, color)}={rendered}")

    return "\n".join(parts)


def _match_filter(rec: dict, f: str) -> bool:
    """支持方向简写、msg 子串、以及任意字段子串。"""
    f_lower = f.lower()
    # 方向简写
    for alias, full in _DIRECTION_ALIASES.items():
        if f_lower == alias.lower():
            return full in str(rec.get("msg", ""))
    # 否则按 msg 子串
    return f in str(rec.get("msg", ""))


def _hide_heartbeat(rec: dict, explicit_heartbeat: bool) -> bool:
    """heartbeat 默认隐藏，除非用户显式 -f HEARTBEAT。"""
    if explicit_heartbeat:
        return False
    msg = str(rec.get("msg", "")).lower()
    return "heartbeat" in msg


def _resolve_log_path(arg: str | None) -> Path:
    if arg is None:
        # 默认今天
        today = _dt.date.today().isoformat()
        return Path("logs") / today / "sim.log"
    p = Path(arg)
    # 如果只给日期（如 2026-07-20）
    if not p.exists() and _looks_like_date(arg):
        return Path("logs") / arg / "sim.log"
    return p


def _looks_like_date(s: str) -> bool:
    try:
        _dt.date.fromisoformat(s)
        return True
    except ValueError:
        return False


def main() -> int:
    ap = argparse.ArgumentParser(
        description="可读化查看 sim.log（JSON Lines）",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    ap.add_argument(
        "path",
        nargs="?",
        default=None,
        help="日志文件路径或日期（如 2026-07-20）；默认今天",
    )
    ap.add_argument("-f", "--filter", help="按方向或 msg 子串过滤")
    ap.add_argument("-n", "--tail", type=int, help="只显示最后 N 条")
    ap.add_argument("--no-color", action="store_true", help="禁用颜色")
    ap.add_argument("--raw", action="store_true", help="原始 JSON，不渲染")
    ap.add_argument("--source", action="store_true", help="显示 source（文件:行号）")
    args = ap.parse_args()

    log_path = _resolve_log_path(args.path)
    if not log_path.exists():
        print(f"日志文件不存在：{log_path}", file=sys.stderr)
        return 1

    color = (not args.no_color) and sys.stdout.isatty()

    f_lower = (args.filter or "").lower()
    explicit_heartbeat = f_lower == "heartbeat"

    # 收集所有匹配行（支持 --tail）
    matched: list[str] = []
    with log_path.open("r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                # 非 JSON 行：只在无过滤时输出
                if not args.filter:
                    matched.append(line)
                continue
            if args.filter and not _match_filter(rec, args.filter):
                continue
            if _hide_heartbeat(rec, explicit_heartbeat):
                continue
            matched.append(line)

    if args.tail is not None and args.tail > 0:
        matched = matched[-args.tail:]

    if args.raw:
        for line in matched:
            print(line)
        return 0

    for line in matched:
        rendered = render_line(line, color, show_source=args.source)
        if rendered:
            print(rendered)
            print()  # 条目间空行
    return 0


if __name__ == "__main__":
    sys.exit(main())
