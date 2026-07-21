#!/usr/bin/env python3
"""pretty_log.py — 可读化查看 sim.log（JSON Lines）。

sim.log 每行是一条 JSON，单行可能上千字（perception text 完整不截断）。
本脚本提供两种查看方式：

1. HTML 报告（推荐，--html）：生成独立 HTML 文件并自动打开浏览器。
   - 长字段自然换行
   - 按方向折叠/过滤
   - 实时搜索框
   - 颜色高亮
   - 不受 Windows 终端编码问题影响
   - 可保存分享

2. 终端渲染（默认）：每条日志渲染成多行彩色文本。

用法：
    # HTML 报告（推荐）
    python scripts/pretty_log.py --html                   # 今天的日志
    python scripts/pretty_log.py --html 2026-07-20        # 指定日期
    python scripts/pretty_log.py --html -f PERCEPTION     # 只看 PERCEPTION
    python scripts/pretty_log.py --html -o report.html    # 指定输出路径
    python scripts/pretty_log.py --html --no-open         # 生成但不自动打开
    python scripts/pretty_log.py --html --hermes          # 整合 Hermes 容器日志

    # 终端渲染
    python scripts/pretty_log.py                          # 查看今天的 sim.log
    python scripts/pretty_log.py -f PERCEPTION -n 50      # 最近 50 条 PERCEPTION
    python scripts/pretty_log.py --raw                    # 原始 JSON

方向过滤器（-f）支持的简写（不区分大小写）：
    UE→MCP      UE→MCP（感知/状态/动作完成）
    MCP→UE      MCP→UE（动作命令/叙事）
    PERCEPTION  MCP→Hermes/PERCEPTION（传入 Hermes 的感知原文）
    RESPONSE    Hermes→MCP/RESPONSE（LLM 响应 + narrative）
    TOOL        Hermes→MCP/TOOL（Hermes 调用的工具）
    HERMES      Hermes/internal（Hermes 容器内部日志：LLM 调用/工具错误/Turn 结束）
    HEARTBEAT   心跳（默认隐藏，可显式过滤查看）

整合 Hermes 日志（--hermes）：
    默认读取 hermes/profiles/h01/logs/agent.log（容器内 UTC，自动转 +08:00 合并排序）。
    默认只保留 agent.conversation_loop / agent.tool_executor / run_agent /
    POST /v1/responses 行，以及任何 ERROR/WARNING；其余噪声（插件注册、健康检查、
    cron、housekeeping、gateway 生命周期）默认隐藏，加 --hermes-all 显示。

依赖：仅 Python 3 标准库。
"""
from __future__ import annotations

import argparse
import datetime as _dt
import html as _html
import io
import json
import os
import sys
import urllib.parse
import webbrowser
from pathlib import Path

# Windows 控制台默认 GBK，会破坏 UTF-8 字符（如 →、替换符、中文）。
# 强制 stdout/stderr 走 UTF-8。
if sys.stdout.encoding and sys.stdout.encoding.lower() != "utf-8":
    sys.stdout = io.TextIOWrapper(sys.stdout.buffer, encoding="utf-8", errors="replace")
    sys.stderr = io.TextIOWrapper(sys.stderr.buffer, encoding="utf-8", errors="replace")

# ── 方向简写 ───────────────────────────────────────────────────────────
_DIRECTION_ALIASES = {
    "UE→MCP": "UE→MCP",
    "MCP→UE": "MCP→UE",
    "PERCEPTION": "MCP→Hermes/PERCEPTION",
    "RESPONSE": "Hermes→MCP/RESPONSE",
    "TOOL": "Hermes→MCP/TOOL",
    "HERMES": "Hermes/internal",
    "HEARTBEAT": "heartbeat",
}

# 方向 → CSS class（用于着色）
_DIRECTION_CSS = {
    "UE→MCP": "dir-ue-mcp",
    "MCP→UE": "dir-mcp-ue",
    "MCP→Hermes/PERCEPTION": "dir-perception",
    "Hermes→MCP/RESPONSE": "dir-response",
    "Hermes→MCP/TOOL": "dir-tool",
    "Hermes/internal": "dir-hermes",
}


# ── 终端颜色 ──────────────────────────────────────────────────────────
_ANSI = {
    "reset": "\033[0m",
    "dim": "\033[2m",
    "time": "\033[36m",
    "level_info": "\033[32m",
    "level_warn": "\033[33m",
    "level_error": "\033[31m",
    "msg": "\033[1;37m",
    "key": "\033[34m",
    "value": "\033[0m",
    "marker": "\033[35m",
    "payload": "\033[90m",
}


def _color(code: str, text: str, enabled: bool) -> str:
    if not enabled:
        return text
    return f"{_ANSI[code]}{text}{_ANSI['reset']}"


def _format_time(iso: str, color: bool) -> str:
    """2026-07-20T20:24:31.7207912+08:00 → 20:24:31.720"""
    try:
        t = iso.split("T", 1)[1]
        for i, ch in enumerate(t):
            if ch in "+-Z" and i > 0:
                t = t[:i]
                break
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
    if isinstance(v, str):
        return v
    if isinstance(v, (dict, list)):
        try:
            return json.dumps(v, ensure_ascii=False, indent=indent)
        except Exception:
            return str(v)
    return str(v)


def _try_pretty_payload(s: str, color: bool) -> str:
    if not s or not s.startswith("{"):
        return s
    try:
        obj = json.loads(s)
        return _format_value(obj, color)
    except Exception:
        return s


def render_line(line: str, color: bool, show_source: bool = False) -> str:
    """把一行 JSON 渲染成多行可读文本（终端模式）。"""
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

    if _is_direction_marker(msg):
        msg_s = _color("marker", msg, color)
    else:
        msg_s = _color("msg", msg, color)

    source_s = ""
    if show_source and "source" in rec:
        src = rec["source"]
        if isinstance(src, dict):
            fname = Path(src.get("file", "")).name
            src_line = src.get("line", "")
            source_s = f"  {_color('dim', f'@{fname}:{src_line}', color)}"

    header = f"{time_s} {level} {msg_s}{source_s}"

    skip = {"time", "level", "msg", "source", "_direction", "_game_time", "_raw"}
    parts = [header]
    for k in rec:
        if k in skip:
            continue
        v = rec[k]
        if k == "payload" and isinstance(v, str):
            v = _try_pretty_payload(v, color)
            parts.append(f"  {_color('key', k, color)}:")
            for pl in _format_value(v, color).splitlines():
                parts.append(f"    {_color('payload', pl, color)}")
            continue
        if k in ("text", "narrative", "message") and isinstance(v, str):
            parts.append(f"  {_color('key', k, color)}:")
            for ln in v.split("\n"):
                parts.append(f"    {ln}")
            continue
        rendered = _format_value(v, color)
        if "\n" in rendered:
            parts.append(f"  {_color('key', k, color)}:")
            for ln in rendered.splitlines():
                parts.append(f"    {ln}")
        else:
            parts.append(f"  {_color('key', k, color)}={rendered}")

    return "\n".join(parts)


# ── 过滤与路径解析 ────────────────────────────────────────────────────
def _match_filter(rec: dict, f: str) -> bool:
    f_lower = f.lower()
    for alias, full in _DIRECTION_ALIASES.items():
        if f_lower == alias.lower():
            return full in str(rec.get("msg", ""))
    return f in str(rec.get("msg", ""))


def _hide_heartbeat(rec: dict, explicit_heartbeat: bool) -> bool:
    if explicit_heartbeat:
        return False
    msg = str(rec.get("msg", "")).lower()
    return "heartbeat" in msg


def _resolve_log_path(arg: str | None) -> Path:
    if arg is None:
        today = _dt.date.today().isoformat()
        return Path("logs") / today / "sim.log"
    p = Path(arg)
    if not p.exists() and _looks_like_date(arg):
        return Path("logs") / arg / "sim.log"
    return p


def _looks_like_date(s: str) -> bool:
    try:
        _dt.date.fromisoformat(s)
        return True
    except ValueError:
        return False


# ── 游戏时间提取 ──────────────────────────────────────────────────────
import re as _re

# perception text 中「时间HH:MM」模式
_TIME_IN_TEXT = _re.compile(r"时间(\d{1,2}:\d{2})")
# payload.environment.time_of_day（JSON 字符串里）
_TIME_IN_PAYLOAD = _re.compile(r'"time_of_day"\s*:\s*"(\d{1,2}:\d{2})"')


def _extract_game_time(rec: dict) -> str:
    """从日志记录提取游戏时间（HH:MM）。无则返回空字符串。

    两个来源：
    1. [MCP→Hermes/PERCEPTION] 的 text 字段：匹配「时间HH:MM」
    2. [UE→MCP] perception_update 的 payload.environment.time_of_day
    """
    msg = str(rec.get("msg", ""))
    # 来源 1：perception text
    if "PERCEPTION" in msg:
        text = rec.get("text", "")
        if isinstance(text, str):
            m = _TIME_IN_TEXT.search(text)
            if m:
                return m.group(1)
        return ""
    # 来源 2：UE→MCP 的 perception_update payload
    if "UE→MCP" in msg and rec.get("type") == "perception_update":
        payload = rec.get("payload", "")
        if isinstance(payload, str):
            m = _TIME_IN_PAYLOAD.search(payload)
            if m:
                return m.group(1)
        elif isinstance(payload, dict):
            env = payload.get("environment", {})
            if isinstance(env, dict):
                tod = env.get("time_of_day", "")
                if isinstance(tod, str) and tod:
                    return tod
    return ""


def _collect_records(log_path: Path, filt: str | None, tail: int | None) -> list[dict]:
    """读取日志文件，返回匹配的记录列表（已解析为 dict，含 game_time）。"""
    f_lower = (filt or "").lower()
    explicit_heartbeat = f_lower == "heartbeat"
    matched: list[dict] = []
    with log_path.open("r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                if not filt:
                    matched.append({"_raw": line, "msg": "", "level": "INFO", "time": ""})
                continue
            if filt and not _match_filter(rec, filt):
                continue
            if _hide_heartbeat(rec, explicit_heartbeat):
                continue
            # 提取游戏时间（仅记录本身有则填充，不做相邻填充）
            rec["_game_time"] = _extract_game_time(rec)
            matched.append(rec)
    if tail is not None and tail > 0:
        matched = matched[-tail:]
    return matched


# ── Hermes 容器日志解析 ───────────────────────────────────────────────
# agent.log 是 Python logging 文本格式，容器内 UTC，需转 +08:00 才能与 sim.log 对齐。
# 示例：
#   2026-07-21 08:05:31,426 INFO [sid] agent.conversation_loop: API call #1: ...
#   2026-07-21 08:05:31,495 WARNING [sid] agent.tool_executor: Tool mcp__... returned error: ...
#   2026-07-21 08:05:49,935 INFO aiohttp.access: 172.20.0.1 [...] "POST /v1/responses HTTP/1.1" 200 0 ...

_HERMES_LINE = _re.compile(
    r"^(?P<date>\d{4}-\d{2}-\d{2})\s+"
    r"(?P<time>\d{2}:\d{2}:\d{2},\d+)\s+"
    r"(?P<level>INFO|WARNING|WARN|ERROR|DEBUG)\s+"
    r"(?P<rest>.*)$"
)

# Hermes 内部高价值模块白名单（其余默认隐藏，可用 --hermes-all 显示）
_HERMES_KEEP_MODULES = (
    "agent.conversation_loop",
    "agent.tool_executor",
    "run_agent",
)
_HERMES_KEEP_MSG_PATTERNS = (
    "POST /v1/responses",
    "Turn ended",
    "API call #",
    "returned error",
    "Tool loop warning",
)

# 默认隐藏的噪声模块
_HERMES_NOISE_MODULES = (
    "hermes_cli.plugins",
    "gateway.run",
    "gateway.platforms",
    "gateway.housekeeping",
    "cron.scheduler_provider",
    "aiohttp.access",
    "tools.tirith_security",
    "mcp.client.streamable_http",
)


def _hermes_to_iso(date: str, time: str) -> str:
    """2026-07-21 + 08:05:31,426 (UTC) → 2026-07-21T16:05:31.426+08:00"""
    try:
        t = _dt.datetime.strptime(
            f"{date} {time}", "%Y-%m-%d %H:%M:%S,%f"
        ).replace(tzinfo=_dt.timezone.utc)
        local = t.astimezone(_dt.timezone(_dt.timedelta(hours=8)))
        # 2026-07-21T16:05:31.426000+08:00 — 与 sim.log 形态一致
        return local.isoformat(timespec="milliseconds")
    except ValueError:
        return ""


def _parse_hermes_line(line: str) -> dict | None:
    """解析一行 Hermes agent.log → 标准 sim.log 记录 dict（含 _direction）。"""
    m = _HERMES_LINE.match(line)
    if not m:
        return None
    date = m.group("date")
    time = m.group("time")
    level = m.group("level").replace("WARNING", "WARN")
    rest = m.group("rest")
    iso = _hermes_to_iso(date, time)
    if not iso:
        return None

    # 拆出 [session_id] 前缀（如果有）
    session = ""
    rest_stripped = rest
    if rest.startswith("["):
        end = rest.find("]")
        if end > 0:
            session = rest[1:end]
            rest_stripped = rest[end + 1 :].lstrip()

    # 拆出 module: body（模块名最后一个点号之后是消息体）
    # 兼容 "agent.conversation_loop: ..." 和 "run_agent: ..." 两种形式
    module = ""
    body = rest_stripped
    if ": " in rest_stripped:
        head, _, tail_part = rest_stripped.partition(": ")
        # head 形如 "agent.conversation_loop" / "run_agent" / "aiohttp.access"
        # 不含空格即为模块名
        if head and " " not in head:
            module = head.strip()
            body = tail_part

    msg = "[Hermes/internal]"
    fields: dict = {}
    if session:
        fields["session"] = session
    if module:
        fields["module"] = module
    fields["message"] = body

    # 对常见模式提取结构化字段（便于过滤/搜索）
    # 1) API call #N: model=... in=... out=... total=... latency=... cache=...
    api_m = _re.search(
        r"API call #(\d+):\s+model=(\S+)\s+provider=(\S+)\s+in=(\d+)\s+out=(\d+)\s+total=(\d+)\s+latency=([\d.]+s)\s+cache=(\d+)/(\d+)",
        body,
    )
    if api_m:
        fields["api_call"] = int(api_m.group(1))
        fields["model"] = api_m.group(2)
        fields["provider"] = api_m.group(3)
        fields["tokens_in"] = int(api_m.group(4))
        fields["tokens_out"] = int(api_m.group(5))
        fields["tokens_total"] = int(api_m.group(6))
        fields["latency"] = api_m.group(7)
        fields["cache"] = f"{api_m.group(8)}/{api_m.group(9)}"

    # 2) Turn ended: reason=... model=... api_calls=X/Y budget=... tool_turns=... ...
    turn_m = _re.search(
        r"Turn ended:\s+reason=(\S+)\s+model=(\S+)\s+api_calls=(\d+)/(\d+)\s+budget=(\d+)/(\d+)\s+tool_turns=(\d+)",
        body,
    )
    if turn_m:
        fields["turn_reason"] = turn_m.group(1)
        fields["model"] = turn_m.group(2)
        fields["api_calls"] = int(turn_m.group(3))
        fields["api_budget"] = int(turn_m.group(4))
        fields["tool_turns"] = int(turn_m.group(7))

    # 3) Tool ... returned error (X.XXs): {...}
    tool_err_m = _re.match(
        r"Tool\s+(?P<tool>\S+)\s+returned error\s+\((?P<dur>[\d.]+s)\):\s*(?P<detail>.+)",
        body,
    )
    if tool_err_m:
        fields["tool"] = tool_err_m.group("tool")
        fields["duration"] = tool_err_m.group("dur")
        detail = tool_err_m.group("detail")
        # 尝试把 detail 解析为 JSON
        try:
            fields["error"] = json.loads(detail)
        except json.JSONDecodeError:
            fields["error"] = detail

    rec = {
        "time": iso,
        "level": level,
        "msg": msg,
        **fields,
    }
    rec["_direction"] = "Hermes/internal"
    rec["_game_time"] = ""  # Hermes 日志无游戏时间
    return rec


def _hermes_keep(rec: dict, keep_all: bool) -> bool:
    """是否保留这条 Hermes 记录（默认白名单 + WARNING/ERROR）。"""
    if keep_all:
        return True
    level = rec.get("level", "").upper()
    if level in ("WARN", "ERROR"):
        return True
    module = rec.get("module", "")
    body = rec.get("message", "")
    if any(module.startswith(m) for m in _HERMES_KEEP_MODULES):
        return True
    if any(p in body for p in _HERMES_KEEP_MSG_PATTERNS):
        return True
    return False


def _collect_hermes_records(
    hermes_log: Path,
    keep_all: bool,
    filt: str | None,
    time_range: tuple[str, str] | None = None,
) -> list[dict]:
    """读取 Hermes agent.log，返回标准化记录列表。

    time_range: (start_iso, end_iso) — 仅保留该时间范围内的记录（含），None 表示全部。
    """
    if not hermes_log.exists():
        return []
    matched: list[dict] = []
    start_iso, end_iso = time_range if time_range else ("", "")
    with hermes_log.open("r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            line = line.rstrip("\n")
            if not line:
                continue
            rec = _parse_hermes_line(line)
            if rec is None:
                continue
            # 时间范围过滤（默认按 sim.log 范围同步）
            t = rec.get("time", "")
            if start_iso and t < start_iso:
                continue
            if end_iso and t > end_iso:
                continue
            if not _hermes_keep(rec, keep_all=keep_all):
                continue
            if filt and not _match_filter(rec, filt):
                continue
            matched.append(rec)
    return matched


# ── HTML 报告 ────────────────────────────────────────────────────────
_HTML_TEMPLATE = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<title>sim.log 报告 — {title}</title>
<style>
:root {{
  --bg: #1e1e1e;
  --bg-card: #252526;
  --bg-hover: #2a2d2e;
  --fg: #d4d4d4;
  --fg-dim: #888;
  --border: #3c3c3c;
  --accent: #569cd6;
  --time: #4ec9b0;
  --info: #6a9955;
  --warn: #d7ba7d;
  --error: #f44747;
  --ue-mcp: #c586c0;
  --mcp-ue: #9cdcfe;
  --perception: #ce9178;
  --response: #b5cea8;
  --tool: #dcdcaa;
  --hermes: #d16969;
}}
* {{ box-sizing: border-box; }}
body {{
  background: var(--bg);
  color: var(--fg);
  font-family: 'Cascadia Code', 'Consolas', 'Source Code Pro', 'Menlo', monospace;
  font-size: 13px;
  line-height: 1.5;
  margin: 0;
  padding: 0;
}}
.toolbar {{
  position: sticky; top: 0; z-index: 100;
  background: var(--bg-card);
  border-bottom: 1px solid var(--border);
  padding: 8px 16px;
  display: flex; align-items: center; gap: 12px;
  flex-wrap: wrap;
}}
.toolbar h1 {{
  margin: 0; font-size: 14px; font-weight: normal;
  color: var(--fg-dim);
}}
.toolbar input[type="text"] {{
  background: var(--bg); color: var(--fg);
  border: 1px solid var(--border);
  padding: 4px 8px; border-radius: 3px;
  font-family: inherit; font-size: 12px;
  width: 240px;
}}
.toolbar input:focus {{ outline: 1px solid var(--accent); border-color: var(--accent); }}
.toolbar select {{
  background: var(--bg); color: var(--fg);
  border: 1px solid var(--border);
  padding: 3px 8px; border-radius: 3px;
  font-family: inherit; font-size: 12px;
  cursor: pointer;
}}
.toolbar select:focus {{ outline: 1px solid var(--accent); border-color: var(--accent); }}
.filter-btn {{
  background: transparent; color: var(--fg-dim);
  border: 1px solid var(--border);
  padding: 3px 10px; border-radius: 3px;
  cursor: pointer; font-family: inherit; font-size: 12px;
  transition: all 0.15s;
}}
.filter-btn:hover {{ background: var(--bg-hover); color: var(--fg); }}
.filter-btn.active {{ background: var(--accent); color: #fff; border-color: var(--accent); }}
.stats {{
  margin-left: auto; color: var(--fg-dim); font-size: 12px;
}}
.container {{ max-width: 1400px; margin: 0 auto; padding: 12px 16px; }}
.entry {{
  background: var(--bg-card);
  border: 1px solid var(--border);
  border-radius: 4px;
  margin-bottom: 8px;
  overflow: hidden;
}}
.entry-header {{
  padding: 6px 12px;
  cursor: pointer;
  display: flex; align-items: center; gap: 10px;
  border-left: 3px solid transparent;
}}
.entry-header:hover {{ background: var(--bg-hover); }}
.entry.dir-ue-mcp .entry-header {{ border-left-color: var(--ue-mcp); }}
.entry.dir-mcp-ue .entry-header {{ border-left-color: var(--mcp-ue); }}
.entry.dir-perception .entry-header {{ border-left-color: var(--perception); }}
.entry.dir-response .entry-header {{ border-left-color: var(--response); }}
.entry.dir-tool .entry-header {{ border-left-color: var(--tool); }}
.entry.dir-hermes .entry-header {{ border-left-color: var(--hermes); }}
.entry.level-WARN .entry-header {{ border-left-color: var(--warn); }}
.entry.level-ERROR .entry-header {{ border-left-color: var(--error); }}
.entry-time {{ color: var(--time); flex-shrink: 0; }}
.entry-game-time {{
  color: var(--time); background: rgba(78, 201, 176, 0.1);
  padding: 1px 6px; border-radius: 3px;
  font-size: 11px; flex-shrink: 0;
  border: 1px solid rgba(78, 201, 176, 0.3);
}}
.entry-level {{ width: 44px; flex-shrink: 0; font-weight: bold; }}
.entry.level-INFO .entry-level {{ color: var(--info); }}
.entry.level-WARN .entry-level {{ color: var(--warn); }}
.entry.level-ERROR .entry-level {{ color: var(--error); }}
.entry.level-DEBUG .entry-level {{ color: var(--fg-dim); }}
.entry-msg {{ color: var(--fg); flex-shrink: 0; }}
.entry.dir-ue-mcp .entry-msg {{ color: var(--ue-mcp); }}
.entry.dir-mcp-ue .entry-msg {{ color: var(--mcp-ue); }}
.entry.dir-perception .entry-msg {{ color: var(--perception); }}
.entry.dir-response .entry-msg {{ color: var(--response); }}
.entry.dir-tool .entry-msg {{ color: var(--tool); }}
.entry.dir-hermes .entry-msg {{ color: var(--hermes); }}
.entry-meta {{ color: var(--fg-dim); font-size: 12px; margin-left: auto; }}
.entry-body {{
  display: none;
  padding: 8px 12px 12px 24px;
  border-top: 1px solid var(--border);
  background: var(--bg);
}}
.entry.expanded .entry-body {{ display: block; }}
.entry.expanded .entry-header {{ border-bottom: 1px solid var(--border); }}
.field {{ margin-bottom: 6px; }}
.field-key {{ color: var(--accent); }}
.field-value {{ color: var(--fg); white-space: pre-wrap; word-break: break-word; }}
.field-value.payload {{ color: var(--fg-dim); }}
.field-value.text, .field-value.narrative {{
  background: rgba(255,255,255,0.03);
  padding: 8px 12px;
  border-radius: 3px;
  border-left: 2px solid var(--border);
  margin-top: 4px;
}}
.hidden {{ display: none !important; }}
.empty {{ color: var(--fg-dim); text-align: center; padding: 60px 20px; }}
</style>
</head>
<body>
<div class="toolbar">
  <h1>sim.log — {title}</h1>
  <input type="text" id="search" placeholder="搜索（正则）..." />
  <button class="filter-btn active" data-filter="ALL">全部</button>
  <button class="filter-btn" data-filter="UE→MCP">UE→MCP</button>
  <button class="filter-btn" data-filter="MCP→UE">MCP→UE</button>
  <button class="filter-btn" data-filter="PERCEPTION">PERCEPTION</button>
  <button class="filter-btn" data-filter="RESPONSE">RESPONSE</button>
  <button class="filter-btn" data-filter="TOOL">TOOL</button>
  <button class="filter-btn" data-filter="HERMES">Hermes</button>
  <select id="game-time-filter" title="按游戏时间过滤">
    <option value="ALL">游戏时间：全部</option>
  </select>
  <span class="stats" id="stats"></span>
</div>
<div class="container" id="container">{entries}</div>
<script>
const entries = document.querySelectorAll('.entry');
const stats = document.getElementById('stats');
const search = document.getElementById('search');
const gameTimeFilter = document.getElementById('game-time-filter');
let currentFilter = 'ALL';
let currentSearch = '';
let currentGameTime = 'ALL';

// 收集所有出现过的游戏时间，填充下拉框
(function() {{
  const times = new Set();
  entries.forEach(e => {{
    const gt = e.dataset.gametime || '';
    if (gt) times.add(gt);
  }});
  const sorted = Array.from(times).sort();
  sorted.forEach(t => {{
    const opt = document.createElement('option');
    opt.value = t;
    opt.textContent = t;
    gameTimeFilter.appendChild(opt);
  }});
}})();

function applyFilters() {{
  let visible = 0;
  entries.forEach(e => {{
    const dir = e.dataset.direction || '';
    const text = e.dataset.searchtext || '';
    const gt = e.dataset.gametime || '';
    let show = true;
    if (currentFilter !== 'ALL') {{
      const filterMap = {{
        'UE→MCP': 'UE→MCP',
        'MCP→UE': 'MCP→UE',
        'PERCEPTION': 'MCP→Hermes/PERCEPTION',
        'RESPONSE': 'Hermes→MCP/RESPONSE',
        'TOOL': 'Hermes→MCP/TOOL',
        'HERMES': 'Hermes/internal',
      }};
      const full = filterMap[currentFilter] || currentFilter;
      show = dir.includes(full);
    }}
    if (show && currentGameTime !== 'ALL') {{
      show = gt === currentGameTime;
    }}
    if (show && currentSearch) {{
      try {{
        const re = new RegExp(currentSearch, 'i');
        show = re.test(text);
      }} catch (e) {{ show = text.toLowerCase().includes(currentSearch.toLowerCase()); }}
    }}
    e.classList.toggle('hidden', !show);
    if (show) visible++;
  }});
  stats.textContent = visible + ' / ' + entries.length + ' 条';
}}

document.querySelectorAll('.filter-btn').forEach(btn => {{
  btn.addEventListener('click', () => {{
    document.querySelectorAll('.filter-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    currentFilter = btn.dataset.filter;
    applyFilters();
  }});
}});

gameTimeFilter.addEventListener('change', () => {{
  currentGameTime = gameTimeFilter.value;
  applyFilters();
}});

search.addEventListener('input', () => {{
  currentSearch = search.value;
  applyFilters();
}});

entries.forEach(e => {{
  const header = e.querySelector('.entry-header');
  header.addEventListener('click', () => e.classList.toggle('expanded'));
}});

// 初始：默认展开前 3 条 PERCEPTION/RESPONSE（最有信息量）
let autoExpanded = 0;
entries.forEach(e => {{
  if (autoExpanded < 3 && (e.dataset.direction || '').match(/PERCEPTION|RESPONSE/)) {{
    e.classList.add('expanded');
    autoExpanded++;
  }}
}});

applyFilters();
</script>
</body>
</html>"""


def _direction_of(rec: dict) -> str:
    """返回消息的方向标识（用于 CSS 着色和过滤）。"""
    # Hermes/internal 记录由解析器直接标记 _direction（msg 只是 [Hermes/internal]）
    if rec.get("_direction") == "Hermes/internal":
        return "Hermes/internal"
    msg = str(rec.get("msg", ""))
    for marker in _DIRECTION_CSS:
        if marker in msg:
            return marker
    return ""


def _entry_html(rec: dict) -> str:
    """把一条记录渲染成 HTML entry。"""
    if "_raw" in rec:
        return (
            f'<div class="entry" data-direction="" data-searchtext="{_html.escape(rec["_raw"])}">'
            f'<div class="entry-header"><span class="entry-msg">[non-JSON]</span></div>'
            f'<div class="entry-body"><div class="field"><span class="field-value">{_html.escape(rec["_raw"])}</span></div></div>'
            f'</div>'
        )

    time_s = rec.get("time", "")
    try:
        t = time_s.split("T", 1)[1]
        for i, ch in enumerate(t):
            if ch in "+-Z" and i > 0:
                t = t[:i]
                break
        if "." in t:
            head, frac = t.split(".", 1)
            t = f"{head}.{frac[:3]}"
        time_s = t
    except Exception:
        pass

    level = str(rec.get("level", "INFO")).upper()
    msg = str(rec.get("msg", ""))
    direction = _direction_of(rec)
    css_dir = _DIRECTION_CSS.get(direction, "")

    # header 上的简短 meta（agent_id / seq 等）
    meta_parts = []
    for k in ("agent_id", "seq", "agent_epoch", "decision_epoch", "tokens", "tokens_total", "api_call", "module"):
        if k in rec:
            meta_parts.append(f"{k}={rec[k]}")
    meta_s = " · ".join(meta_parts)

    # 搜索文本：合并所有字段的字符串值
    search_parts = [msg]
    for k, v in rec.items():
        if k in ("time", "level", "msg", "source", "_game_time", "_direction", "_raw"):
            continue
        if isinstance(v, str):
            search_parts.append(v)
        else:
            try:
                search_parts.append(json.dumps(v, ensure_ascii=False))
            except Exception:
                search_parts.append(str(v))
    search_text = " ".join(search_parts)

    # 游戏时间（仅 perception_update / PERCEPTION 有）
    game_time = rec.get("_game_time", "")

    # body：所有字段
    skip = {"time", "level", "msg", "source", "_game_time", "_direction", "_raw"}
    body_parts = []
    for k in rec:
        if k in skip:
            continue
        v = rec[k]
        css_class = "field-value"
        if k == "payload":
            css_class += " payload"
        if k in ("text", "narrative", "message", "error"):
            css_class += " text"
        # 渲染值
        if isinstance(v, str) and k == "payload" and v.startswith("{"):
            try:
                obj = json.loads(v)
                v_str = json.dumps(obj, ensure_ascii=False, indent=2)
            except Exception:
                v_str = v
        elif isinstance(v, str):
            v_str = v
        elif isinstance(v, (dict, list)):
            v_str = json.dumps(v, ensure_ascii=False, indent=2)
        else:
            v_str = str(v)
        body_parts.append(
            f'<div class="field"><span class="field-key">{_html.escape(k)}:</span> '
            f'<span class="{css_class}">{_html.escape(v_str)}</span></div>'
        )
    body_s = "\n".join(body_parts) if body_parts else '<div class="field"><span class="field-dim">（无额外字段）</span></div>'

    # Hermes 记录：在 header 显示 message 摘要（截断 120 字）
    header_msg = msg
    if direction == "Hermes/internal":
        body_msg = str(rec.get("message", ""))
        if body_msg:
            snippet = body_msg if len(body_msg) <= 120 else body_msg[:120] + "…"
            header_msg = f"{msg} {snippet}"

    return (
        f'<div class="entry {css_dir} level-{level}" '
        f'data-direction="{_html.escape(direction)}" '
        f'data-gametime="{_html.escape(game_time)}" '
        f'data-searchtext="{_html.escape(search_text)}">'
        f'<div class="entry-header">'
        f'<span class="entry-time">{_html.escape(time_s)}</span>'
        f'<span class="entry-level">{_html.escape(level)}</span>'
        f'<span class="entry-msg">{_html.escape(header_msg)}</span>'
        + (f'<span class="entry-game-time">🎮 {_html.escape(game_time)}</span>' if game_time else '')
        + f'<span class="entry-meta">{_html.escape(meta_s)}</span>'
        f'</div>'
        f'<div class="entry-body">{body_s}</div>'
        f'</div>'
    )


def generate_html(records: list[dict], title: str, output: Path) -> Path:
    """生成 HTML 报告文件。"""
    if not records:
        entries_html = '<div class="empty">无匹配的日志记录</div>'
    else:
        entries_html = "\n".join(_entry_html(r) for r in records)
    html = _HTML_TEMPLATE.format(title=_html.escape(title), entries=entries_html)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(html, encoding="utf-8")
    return output


def _open_in_browser(path: Path) -> None:
    """跨平台打开浏览器。"""
    url = path.resolve().as_uri()
    try:
        webbrowser.open(url, new=2)
    except Exception:
        pass


# ── 主入口 ────────────────────────────────────────────────────────────
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
    ap.add_argument("--no-color", action="store_true", help="禁用颜色（终端模式）")
    ap.add_argument("--raw", action="store_true", help="原始 JSON，不渲染")
    ap.add_argument("--source", action="store_true", help="显示 source（文件:行号）")
    ap.add_argument(
        "--html",
        action="store_true",
        help="生成 HTML 报告并自动打开浏览器（推荐）",
    )
    ap.add_argument(
        "-o",
        "--output",
        help="HTML 输出路径（仅 --html 模式）；默认 logs/YYYY-MM-DD/sim_report.html",
    )
    ap.add_argument(
        "--no-open",
        action="store_true",
        help="生成 HTML 但不自动打开浏览器（仅 --html 模式）",
    )
    ap.add_argument(
        "--hermes",
        action="store_true",
        help="整合 Hermes 容器日志（hermes/profiles/h01/logs/agent.log）按时间合并",
    )
    ap.add_argument(
        "--hermes-log",
        help="Hermes 日志路径（默认 hermes/profiles/h01/logs/agent.log）",
    )
    ap.add_argument(
        "--hermes-all",
        action="store_true",
        help="显示 Hermes 日志全部条目（默认只保留 LLM 决策相关 + WARNING/ERROR）",
    )
    args = ap.parse_args()

    log_path = _resolve_log_path(args.path)
    if not log_path.exists():
        print(f"日志文件不存在：{log_path}", file=sys.stderr)
        return 1

    records = _collect_records(log_path, args.filter, args.tail)

    # 整合 Hermes 日志
    if args.hermes or args.hermes_log or args.hermes_all:
        if args.hermes_log:
            hermes_path = Path(args.hermes_log)
        else:
            # 默认位置：项目根 hermes/profiles/h01/logs/agent.log
            # 从 sim.log 路径回推项目根（logs/YYYY-MM-DD/sim.log → ../..）
            project_root = log_path.parent.parent.parent
            hermes_path = project_root / "hermes" / "profiles" / "h01" / "logs" / "agent.log"
        if not hermes_path.exists():
            print(f"警告：Hermes 日志不存在：{hermes_path}", file=sys.stderr)
        else:
            # 计算 sim.log 的时间范围，前后扩展 5 分钟
            # Hermes agent.log 是跨多天累积文件，默认只保留与 sim.log 同期条目
            sim_times = [r.get("time", "") for r in records if r.get("time")]
            time_range = None
            if sim_times and not args.hermes_all:
                start = min(sim_times)
                end = max(sim_times)
                # 前后各扩 5 分钟，捕捉 sim.log 边界外的相关条目
                try:
                    start_dt = _dt.datetime.fromisoformat(start)
                    end_dt = _dt.datetime.fromisoformat(end)
                    start = (start_dt - _dt.timedelta(minutes=5)).isoformat()
                    end = (end_dt + _dt.timedelta(minutes=5)).isoformat()
                    time_range = (start, end)
                except ValueError:
                    time_range = None
            hermes_records = _collect_hermes_records(
                hermes_path,
                keep_all=args.hermes_all,
                filt=args.filter,
                time_range=time_range,
            )
            records = records + hermes_records
            # 按时间戳升序合并排序
            records.sort(key=lambda r: r.get("time", ""))
            if args.tail is not None and args.tail > 0:
                records = records[-args.tail:]

    # ── HTML 模式 ──
    if args.html:
        if args.output:
            out_path = Path(args.output)
        else:
            # 默认放在日志同目录
            date_dir = log_path.parent.name
            out_path = log_path.parent / "sim_report.html"
        generate_html(records, title=str(log_path), output=out_path)
        print(f"HTML 报告已生成：{out_path}")
        if not args.no_open:
            _open_in_browser(out_path)
        return 0

    # ── 终端模式 ──
    color = (not args.no_color) and sys.stdout.isatty()

    if args.raw:
        for rec in records:
            if "_raw" in rec:
                print(rec["_raw"])
            else:
                try:
                    print(json.dumps(rec, ensure_ascii=False))
                except Exception:
                    print(str(rec))
        return 0

    for rec in records:
        if "_raw" in rec:
            print(rec["_raw"])
            print()
            continue
        try:
            line = json.dumps(rec, ensure_ascii=False)
        except Exception:
            line = str(rec)
        rendered = render_line(line, color, show_source=args.source)
        if rendered:
            print(rendered)
            print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
