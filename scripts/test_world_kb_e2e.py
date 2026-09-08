#!/usr/bin/env python3
"""端到端测试：mock_ue 自动推送 world_kb + capability_registry → MCP 生效。

验证链路（mock_ue._connection_manager 自动执行，无需手工触发）：
  连接 → _send_world_kb → _send_agent_registered → _send_capability_registry

黑盒断言（通过 MCP HTTP debug 端点）：
  - GET /debug/kb 返回异世界 yaml 的 zones/objects（world_kb merge+swap 生效）
  - GET /debug/cap 返回 system 的 9 cmd（capability_registry 注册生效）
  - MCP 日志含 "world_kb merged and persisted" + "capability_registry registered"

使用方法：
  python3 scripts/test_world_kb_e2e.py

前置条件：
  - .env 含 VENUS_API_KEY（MCP 启动需要，但握手不触发 LLM 调用）
  - Go 工具链可用（首次会编译 MCP 二进制）
  - :8770/:9091 端口空闲（dev 端口，避开 stable 的 :8760/:9090）
"""
import asyncio
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

# 复用 mock_ue 的真实握手逻辑（_send_world_kb / _send_capability_registry），
# 而不是手工拼 JSON —— 这样测的是 mock_ue 自动推送行为本身。
sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))
from agenttown.mock_ue import MockUE  # noqa: E402

REPO = Path(__file__).resolve().parents[1]
MCP_BIN = REPO / "mcp"
HTTP_PORT, WS_PORT = 8770, 9091
HTTP_URL = f"http://127.0.0.1:{HTTP_PORT}"
WS_URL = f"ws://127.0.0.1:{WS_PORT}/ws"

# 异世界 yaml（1 zone / 1 object / 1 agent T-01），与 assets/world_kb.yaml
# 完全不同，用于验证 MCP 能接受非默认 KB。bounds 用 center/extent dict
# 格式（对齐 assets/world_kb.yaml 的 schema）。
ALIEN_YAML = """
zones:
  - id: test_zone
    bounds:
      center: [5000, 5000, 0]
      extent: [5000, 5000, 500]
    entry_point: [5000, 5000, 0]
    entry_facing: [0, 0, 0]
    display_name: 测试区域
    description: e2e 专用最小世界
objects:
  - id: test_obj
    category: workbench
    zone_id: test_zone
    actor_position: [5000, 5000, 0]
    interaction_point: [5000, 5000, 0]
    interaction_facing: [0, 0, 0]
    display_name: 测试对象
    available_interactions: [inspect]
    default_state: idle
agents:
  - id: T-01
    type: humanoid
    initial_zone: test_zone
    initial_position: [1000, 1000, 0]
    display_name: 测试机器人
version: "1.0"
narrative:
  setting: e2e 测试世界
"""


def build_mcp():
    """编译 MCP 二进制到 REPO/mcp。go 增量编译，未改动的文件不重编译。"""
    print("[BUILD] compiling MCP binary...")
    subprocess.run(
        ["go", "build", "-o", str(MCP_BIN), "./cmd/agenttown-mcp"],
        cwd=REPO / "agenttown-mcp", check=True,
    )


def load_venus_key():
    """从 .env 读 VENUS_API_KEY（MCP 启动 flag 必填，但 e2e 不触发 LLM 调用）。"""
    env_file = REPO / ".env"
    for line in env_file.read_text().splitlines():
        line = line.strip()
        if line.startswith("VENUS_API_KEY="):
            val = line.split("=", 1)[1].strip()
            # 去掉可能的引号
            return val.strip('"').strip("'")
    raise SystemExit("[FATAL] VENUS_API_KEY not found in .env")


def start_mcp(world_kb_path, manifest_path, log_file):
    """启动 MCP 子进程，stdout+stderr 重定向到 log_file。"""
    env = os.environ.copy()
    log_fh = open(log_file, "wb")
    proc = subprocess.Popen(
        [
            str(MCP_BIN),
            "--llm-backend=venus",
            "--http", f":{HTTP_PORT}",
            "--ws", f":{WS_PORT}",
            "--world-kb", str(world_kb_path),
            "--world-kb-manifest", str(manifest_path),
            "--venus-api-key", load_venus_key(),
            "--log-level", "debug",
        ],
        stdout=log_fh, stderr=subprocess.STDOUT, env=env,
    )
    return proc, log_fh


def wait_status_ok(timeout=15):
    """轮询 /status 直到返回 {"ok": true} 或超时。"""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(f"{HTTP_URL}/status", timeout=1) as r:
                if json.loads(r.read()).get("ok"):
                    return
        except Exception:
            pass
        time.sleep(0.3)
    raise AssertionError(f"MCP /status not ok within {timeout}s")


def get_debug(path):
    """GET 一个 /debug/* 端点，返回解析后的 JSON。"""
    with urllib.request.urlopen(f"{HTTP_URL}{path}", timeout=2) as r:
        return json.loads(r.read())


async def run_mock_ue_handshake(world_kb_path):
    """用 MockUE 实例驱动真实握手，2 秒后 cancel。

    只跑 _connection_manager（含握手），不跑 _perception_loop，避免
    perception 循环触发 LLM 调用。握手三条消息（world_kb /
    agent_registered / capability_registry）在连接建立后背靠背发送，
    亚秒级完成。
    """
    ue = MockUE(
        mcp_ws_url=WS_URL,
        world_kb_path=str(world_kb_path),
        perception_interval=999,  # 防御性：即使跑 perception_loop 也少触发
        log_dir=tempfile.gettempdir(),
    )
    conn_task = asyncio.create_task(ue._connection_manager())
    await asyncio.sleep(2)  # 等握手 + MCP 处理完三条消息
    conn_task.cancel()
    try:
        await conn_task
    except asyncio.CancelledError:
        pass
    # 关闭可能仍打开的 WS
    if ue._ws is not None:
        try:
            await ue._ws.close()
        except Exception:
            pass


def main():
    build_mcp()
    # 日志放 /tmp 固定路径，失败后可查看（TemporaryDirectory 会被清理）
    tmp = tempfile.mkdtemp(prefix="e2e_")
    # 异世界 yaml
    alien_yaml = Path(tmp) / "alien.yaml"
    alien_yaml.write_text(ALIEN_YAML)
    # MCP 启动用的默认 yaml 副本——worldKBSwap 会把 UE 推送的 KB 写回
    # --world-kb 指定的路径，所以必须用临时副本，否则会污染 assets/world_kb.yaml。
    mcp_default_yaml = Path(tmp) / "default.yaml"
    mcp_default_yaml.write_text((REPO / "assets/world_kb.yaml").read_text())
    # manifest 也用临时路径，避免污染 assets/world_kb.manifest.json
    mcp_manifest = Path(tmp) / "world_kb.manifest.json"
    # MCP 日志
    mcp_log_path = Path("/tmp/mcp_e2e.log")
    # MCP 启动用默认 yaml 副本（模拟真实场景：MCP 先加载默认 KB，UE 连接后推送异世界覆盖）
    mcp_proc, mcp_log_fh = start_mcp(mcp_default_yaml, mcp_manifest, mcp_log_path)
    try:
        wait_status_ok()
        print(f"[OK] MCP started on :{HTTP_PORT}/:{WS_PORT}")

        # 驱动 mock_ue 握手（自动推送 world_kb + capability_registry）
        asyncio.run(run_mock_ue_handshake(alien_yaml))
        print("[OK] mock_ue handshake completed")

        # 断言 1: /debug/kb 返回异世界数据（world_kb merge+swap 生效）
        kb = get_debug("/debug/kb")
        zone_ids = [z["id"] for z in kb["zones"]]
        obj_ids = [o["id"] for o in kb["objects"]]
        if "test_zone" not in zone_ids:
            _dump_log(mcp_log_path)
            raise AssertionError(
                f"world_kb 未生效：/debug/kb zones={zone_ids}（不含 test_zone）"
            )
        if "test_obj" not in obj_ids:
            _dump_log(mcp_log_path)
            raise AssertionError(
                f"world_kb 未生效：/debug/kb objects={obj_ids}（不含 test_obj）"
            )
        print(f"[OK] /debug/kb 返回异世界数据：zones={zone_ids}, objects={obj_ids}")

        # 断言 2: /debug/cap 返回 9 cmd（capability_registry 注册生效）
        cap = get_debug("/debug/cap")
        system_cmds = [a["cmd"] for a in cap.get("agents", {}).get("system", [])]
        if len(system_cmds) != 9:
            _dump_log(mcp_log_path)
            raise AssertionError(
                f"capability_registry 未生效：/debug/cap system cmds={system_cmds}（非 9 个）"
            )
        print(f"[OK] /debug/cap 返回 {len(system_cmds)} 个 cmd：{system_cmds}")

        # 断言 3: MCP 日志含关键事件
        mcp_log_fh.flush()
        log_text = mcp_log_path.read_text(errors="replace")
        if "world_kb merged and persisted" not in log_text:
            _dump_log(mcp_log_path)
            raise AssertionError("MCP 日志缺 \"world_kb merged and persisted\" 事件")
        if "capability_registry registered" not in log_text:
            _dump_log(mcp_log_path)
            raise AssertionError("MCP 日志缺 \"capability_registry registered\" 事件")
        print("[OK] MCP 日志含 world_kb merge + capability_registry registered 事件")

        print("\nPASS: world_kb + capability_registry 端到端验证通过")
    finally:
        mcp_proc.terminate()
        try:
            mcp_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            mcp_proc.kill()
        mcp_log_fh.close()


def _dump_log(path):
    """失败时打印 MCP 日志尾部，便于诊断。"""
    print(f"\n--- MCP log tail ({path}) ---")
    try:
        text = path.read_text(errors="replace")
        lines = text.splitlines()
        for line in lines[-40:]:
            print(line)
    except Exception as e:
        print(f"(failed to read log: {e})")


if __name__ == "__main__":
    main()
