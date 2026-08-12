#!/usr/bin/env python3
"""
test-adaptive-prod.py — 验证生产配置下 adaptive_concurrency 的升降逻辑

使用前（3 步）：
  1. 修改 config.yaml:
       vision:
         base_url: "http://127.0.0.1:9999"   # 指向本脚本的 mock vision server
  2. 重启 blind-llm-eyes.exe
  3. python test-adaptive-prod.py

三阶段验证（每阶段 5 请求 × 4 图 = 20 样本填满窗口）：
  Phase 1 (delay=3s):  P90=3s  < fast=8s   → AI increase (+1)
  Phase 2 (delay=11s): P90=11s, 8≤11≤15   → hysteresis (不降)
  Phase 3 (delay=16s): P90=16s > slow=15s → MD decrease (×0.75)

零依赖：仅使用 Python 标准库。
"""

import base64
import hashlib
import http.server
import json
import os
import struct
import sys
import threading
import time
import urllib.request
import zlib

# ============================================================
#  配置
# ============================================================
PROXY_URL = "http://127.0.0.1:8790"
MOCK_VISION_PORT = 9999
IMAGES_PER_REQUEST = 4
REQUESTS_PER_PHASE = 5  # 5 × 4 = 20 = sample_window

# 每阶段的 mock vision 延迟（秒）
PHASES = [
    {"name": "Phase 1 — Fast (3s)",   "delay": 3.0,  "expect": "increase (+1)"},
    {"name": "Phase 2 — Hysteresis (11s)", "delay": 11.0, "expect": "no change"},
    {"name": "Phase 3 — Slow (16s)",  "delay": 16.0, "expect": "decrease (×0.75)"},
]

# ============================================================
#  Mock Vision Server
# ============================================================

class MockVisionState:
    """控制 mock server 的延迟，主线程在阶段间修改。"""
    delay = 3.0

class MockVisionHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        # 消费请求体（不需要解析内容）
        content_length = int(self.headers.get("Content-Length", 0))
        _ = self.rfile.read(content_length)

        # 模拟 MiMo 处理延迟
        time.sleep(MockVisionState.delay)

        # 返回 Anthropic Messages API 兼容响应
        resp = {
            "content": [{"type": "text", "text": "Mock vision description."}],
            "stop_reason": "end_turn",
        }
        body = json.dumps(resp).encode()

        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass  # 静默访问日志


def start_mock_vision_server():
    server = http.server.ThreadingHTTPServer(
        ("127.0.0.1", MOCK_VISION_PORT), MockVisionHandler
    )
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    return server


# ============================================================
#  PNG 生成（零依赖，使用 struct + zlib）
# ============================================================

def _make_png_chunk(chunk_type: bytes, data: bytes) -> bytes:
    chunk = chunk_type + data
    crc = zlib.crc32(chunk) & 0xFFFFFFFF
    return struct.pack(">I", len(data)) + chunk + struct.pack(">I", crc)


def create_tiny_png_base64(r: int, g: int, b: int) -> str:
    """生成 1x1 像素真 PNG，返回 base64 字符串。"""
    sig = b"\x89PNG\r\n\x1a\n"
    # IHDR: 1x1, 8-bit, color type 2 (RGB)
    ihdr_data = struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0)
    ihdr = _make_png_chunk(b"IHDR", ihdr_data)
    # IDAT: filter byte (0=None) + RGB pixel
    raw = b"\x00" + bytes([r & 0xFF, g & 0xFF, b & 0xFF])
    compressed = zlib.compress(raw)
    idat = _make_png_chunk(b"IDAT", compressed)
    iend = _make_png_chunk(b"IEND", b"")
    png = sig + ihdr + idat + iend
    return base64.b64encode(png).decode()


# ============================================================
#  请求构造 & 发送
# ============================================================

def build_request_body(images: int, seed: int) -> bytes:
    """构造 Anthropic /v1/messages 请求体（含 images 张不同颜色的 1x1 PNG）。

    颜色用 SHA256(counter) 生成，确保无周期性碰撞 → 每张图 cache hash 唯一。
    """
    content = [{"type": "text", "text": "Describe these images."}]
    for i in range(images):
        counter = "%d-%d" % (seed, i)
        digest = hashlib.sha256(counter.encode()).digest()
        r, g, b = digest[0], digest[1], digest[2]
        b64 = create_tiny_png_base64(r, g, b)
        content.append({
            "type": "image",
            "source": {
                "type": "base64",
                "media_type": "image/png",
                "data": b64,
            },
        })
    body = {
        "model": "claude-3-5-sonnet-20241022",
        "max_tokens": 10,
        "stream": False,
        "messages": [{"role": "user", "content": content}],
    }
    return json.dumps(body).encode()


def send_request(body: bytes) -> dict:
    """POST /v1/messages 到代理，返回 {"status": int, "ok": bool, "ms": float}。"""
    req = urllib.request.Request(
        f"{PROXY_URL}/v1/messages",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    start = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            _ = resp.read()
            elapsed = (time.monotonic() - start) * 1000
            return {"status": resp.status, "ok": True, "ms": elapsed}
    except urllib.error.HTTPError as e:
        elapsed = (time.monotonic() - start) * 1000
        return {"status": e.code, "ok": False, "ms": elapsed}
    except Exception as e:
        elapsed = (time.monotonic() - start) * 1000
        return {"status": 0, "ok": False, "ms": elapsed, "error": str(e)}


# ============================================================
#  Metrics 读取
# ============================================================

def get_adaptive_metrics() -> dict | None:
    """解析 /metrics，返回 adaptive 相关 3 个指标。"""
    try:
        with urllib.request.urlopen(f"{PROXY_URL}/metrics", timeout=10) as resp:
            text = resp.read().decode()
    except Exception:
        return None

    result = {"current": None, "p90": None, "up": 0, "down": 0, "none": 0}
    for line in text.split("\n"):
        if line.startswith("blind_llm_eyes_adaptive_concurrency_current "):
            result["current"] = int(float(line.split()[-1]))
        elif line.startswith("blind_llm_eyes_adaptive_vision_p90_seconds "):
            result["p90"] = float(line.split()[-1])
        elif "adaptive_concurrency_adjustments_total" in line and "direction=" in line:
            if 'direction="up"' in line:
                result["up"] = int(float(line.split()[-1]))
            elif 'direction="down"' in line:
                result["down"] = int(float(line.split()[-1]))
            elif 'direction="none"' in line:
                result["none"] = int(float(line.split()[-1]))
    return result


# ============================================================
#  主流程
# ============================================================

def main():
    print("=" * 72)
    print("  adaptive_concurrency 生产配置升降逻辑验证")
    print("=" * 72)
    print(f"  Proxy:       {PROXY_URL}")
    print(f"  Mock Vision: 127.0.0.1:{MOCK_VISION_PORT}")
    print(f"  每阶段:      {REQUESTS_PER_PHASE} 请求 × {IMAGES_PER_REQUEST} 图 = {REQUESTS_PER_PHASE * IMAGES_PER_REQUEST} 样本")
    print(f"  生产配置:    fast=8s, slow=15s, window=20, cooldown=3s, ratio=0.75")
    print()
    print("  使用前请修改 config.yaml:")
    print(f'    vision.base_url: "http://127.0.0.1:{MOCK_VISION_PORT}"')
    print("  然后重启 blind-llm-eyes.exe")
    print()

    # ── 启动 mock vision server ──
    print("[1/4] 启动 mock vision server...", end=" ")
    mock_server = start_mock_vision_server()
    print(f"OK (port {MOCK_VISION_PORT})")

    # ── Pre-flight check ──
    print("[2/4] Pre-flight check...", end=" ")
    try:
        with urllib.request.urlopen(f"{PROXY_URL}/healthz", timeout=5) as resp:
            assert resp.status == 200
        print("OK (/healthz)")
    except Exception:
        print("FAIL")
        print(f"  无法连接 {PROXY_URL}/healthz — 请先启动 blind-llm-eyes.exe")
        sys.exit(1)

    m0 = get_adaptive_metrics()
    if not m0 or m0["current"] is None:
        print("  WARN: /metrics 未返回 adaptive 指标 — 检查 config.yaml adaptive_concurrency.enabled=true")
        sys.exit(2)
    print(f"  初始: limit={m0['current']}, p90={m0['p90']}s, adj(up/dn/no)=({m0['up']}/{m0['down']}/{m0['none']})")

    if m0["down"] > 0 or m0["up"] > 0:
        print(f"  WARN: 检测到累积 adjustments (up={m0['up']}/down={m0['down']}) — 建议重启进程以从 max_limit 开始")

    # ── 运行 3 个阶段 ──
    print()
    print("[3/4] 开始三阶段验证")
    print()

    report = []  # 收集每轮结果
    # 每次运行使用随机种子起点，避免跨运行 cache 命中（LRU 残留）
    run_offset = int.from_bytes(os.urandom(4), "big") & 0x7FFFFFFF
    global_counter = run_offset
    print(f"  本次运行 seed 起点: {global_counter}（避免跨运行 cache 命中）")
    print()

    for phase_idx, phase in enumerate(PHASES):
        MockVisionState.delay = phase["delay"]
        print(f"  ── {phase['name']} | expect: {phase['expect']} ──")

        for req_idx in range(REQUESTS_PER_PHASE):
            global_counter += 1
            body = build_request_body(IMAGES_PER_REQUEST, seed=global_counter)

            print(f"    req {req_idx+1}/{REQUESTS_PER_PHASE}...", end=" ", flush=True)
            result = send_request(body)
            print(f"HTTP {result['status']} | {result['ms']:.0f}ms", end="")

            # 请求间等待 cooldown（生产配置 3s）
            if req_idx < REQUESTS_PER_PHASE - 1:
                time.sleep(3.5)
            else:
                time.sleep(3.5)  # 阶段间也等 cooldown

            m = get_adaptive_metrics()
            if m:
                limit = m["current"]
                p90 = m["p90"]
                adj = f"up={m['up']}/dn={m['down']}/no={m['none']}"
                print(f" | limit={limit} p90={p90}s adj({adj})")
                report.append({
                    "phase": phase_idx + 1,
                    "req": req_idx + 1,
                    "delay": phase["delay"],
                    "status": result["status"],
                    "wall_ms": result["ms"],
                    "limit": limit,
                    "p90": p90,
                    "up": m["up"],
                    "down": m["down"],
                    "none": m["none"],
                })
            else:
                print(" | metrics unavailable")

    # ── 结果总览 ──
    print()
    print("[4/4] 结果总览")
    print()
    print(f"{'Phase':<6} {'Req':<5} {'Delay':<8} {'HTTP':<6} {'Wall(ms)':<10} {'Limit':<7} {'P90(s)':<8} {'Up':<4} {'Down':<5} {'None':<5}")
    print("-" * 75)
    for r in report:
        print(f"{r['phase']:<6} {r['req']:<5} {r['delay']:<8.1f} {r['status']:<6} {r['wall_ms']:<10.0f} {r['limit']:<7} {r['p90']:<8.2f} {r['up']:<4} {r['down']:<5} {r['none']:<5}")

    # ── 判定 ──
    print()
    if len(report) < 3:
        print("  [SKIP] 数据不足，无法判定")
        return

    first = report[0]
    last = report[-1]

    # 找每个阶段的最终 limit
    phase_limits = {}
    for r in report:
        phase_limits[r["phase"]] = r["limit"]

    print(f"  阶段 1 (fast) 最终 limit:    {phase_limits.get(1, '?')}")
    print(f"  阶段 2 (hysteresis) 最终 limit: {phase_limits.get(2, '?')}")
    print(f"  阶段 3 (slow) 最终 limit:    {phase_limits.get(3, '?')}")
    print()

    # Phase 1: 应该 increase（limit 上升）
    p1_increased = phase_limits.get(1, 0) > first["limit"]
    # Phase 2: 不应该 decrease（滞回区不降；可能因窗口过渡有小幅 increase，这是正常的）
    p2_no_decrease = phase_limits.get(2, 0) >= phase_limits.get(1, 0)
    # Phase 3: 应该 decrease（limit 下降）
    p3_decreased = phase_limits.get(3, 0) < phase_limits.get(2, 0)

    checks = [
        ("Phase 1 increase",     p1_increased,  f"limit {first['limit']}→{phase_limits.get(1,'?')}"),
        ("Phase 2 no decrease",  p2_no_decrease, f"limit {phase_limits.get(1,'?')}→{phase_limits.get(2,'?')}"),
        ("Phase 3 decrease",     p3_decreased,   f"limit {phase_limits.get(2,'?')}→{phase_limits.get(3,'?')}"),
    ]

    all_pass = True
    for name, ok, detail in checks:
        status = "PASS" if ok else "FAIL"
        if not ok:
            all_pass = False
        print(f"  [{status}] {name}: {detail}")

    print()
    if all_pass:
        print("  ✅ 所有阶段验证通过！AIMD 升降逻辑在生产配置下正确工作。")
    else:
        print("  ⚠️  部分阶段未通过，检查上方日志和 /metrics 输出。")
        print("  提示：")
        print("    - 确认 config.yaml vision.base_url 指向 127.0.0.1:9999")
        print("    - 确认进程已重启（无累积 adjustments）")
        print("    - 检查 blind-llm-eyes 终端的 adaptive Info 日志")

    # ── 提示恢复 ──
    print()
    print("  验证完成后，恢复 config.yaml:")
    print('    vision.base_url: "https://api.xiaomimimo.com/anthropic"')
    print("  然后重启 blind-llm-eyes.exe")

    mock_server.shutdown()


if __name__ == "__main__":
    main()
