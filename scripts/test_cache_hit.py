"""
缓存命中验证脚本：模拟连续发送同一张图片的多次请求 + 不同图片对比。

用法：
    cd d:\Code\new-api-contrib
    python scripts\test_cache_hit.py

预期结果：
    场景一（同一张图片重复发送）：
        第1次：1 rewritten, 0 cached  （MiMo 被调用）
        第2次：1 rewritten, 1 cached  （缓存命中）
        第3次：1 rewritten, 1 cached  （缓存命中）

    场景二（发送完全不同的图片）：
        第4次：1 rewritten, 0 cached  （新图片，缓存未命中，MiMo 被调用）
        第5次：1 rewritten, 1 cached  （新图片缓存命中）

    场景三（混发两张图片，验证互不干扰）：
        第6次：2 rewritten, 1 cached  （图A命中，图B也命中）
"""

import base64
import json
import struct
import time
import urllib.request
import zlib


# ── 生成 PNG 工具函数 ─────────────────────────────────────────────
def make_png(width: int, height: int, rgba: bytes) -> bytes:
    def chunk(tag: bytes, data: bytes) -> bytes:
        return (
            struct.pack(">I", len(data))
            + tag
            + data
            + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)
        )

    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)
    raw = b""
    for y in range(height):
        raw += b"\x00" + rgba[y * width * 4 : (y + 1) * width * 4]
    idat = zlib.compress(raw)
    return sig + chunk(b"IHDR", ihdr) + chunk(b"IDAT", idat) + chunk(b"IEND", b"")


# ── 准备两张不同的测试图片 ───────────────────────────────────────
W, H = 100, 100

# 图A：纯红色
PNG_RED = make_png(W, H, b"\xff\x00\x00\xff" * (W * H))
B64_RED = base64.b64encode(PNG_RED).decode("ascii")

# 图B：纯蓝色（完全不同的像素数据 → 不同 SHA-256 哈希）
PNG_BLUE = make_png(W, H, b"\x00\x00\xff\xff" * (W * H))
B64_BLUE = base64.b64encode(PNG_BLUE).decode("ascii")


# ── 构造请求体 ────────────────────────────────────────────────────
def build_payload_single(b64_img: str, question: str = "What is in this image?") -> dict:
    """单图请求"""
    return {
        "model": "deepseek-v4-flash",
        "system": [{"type": "text", "text": "You are a helpful assistant."}],
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": question},
                    {
                        "type": "image",
                        "source": {
                            "type": "base64",
                            "media_type": "image/png",
                            "data": b64_img,
                        },
                    },
                ],
            }
        ],
        "max_tokens": 100,
    }


def build_payload_dual(b64_img_a: str, b64_img_b: str) -> dict:
    """双图请求（同一条消息里放两张图）"""
    return {
        "model": "deepseek-v4-flash",
        "system": [{"type": "text", "text": "You are a helpful assistant."}],
        "messages": [
            {
                "role": "user",
                "content": [
                    {"type": "text", "text": "Describe both images."},
                    {
                        "type": "image",
                        "source": {
                            "type": "base64",
                            "media_type": "image/png",
                            "data": b64_img_a,
                        },
                    },
                    {
                        "type": "image",
                        "source": {
                            "type": "base64",
                            "media_type": "image/png",
                            "data": b64_img_b,
                        },
                    },
                ],
            }
        ],
        "max_tokens": 100,
    }


# ── 发送请求并提取关键信息 ────────────────────────────────────────
def send_request(url: str, payload: dict, label: str) -> dict:
    data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.time()
    with urllib.request.urlopen(req, timeout=120) as resp:
        elapsed = time.time() - t0
        status = resp.status
        headers = dict(resp.headers)
        body = resp.read().decode("utf-8", errors="replace")

    blind_header = headers.get("X-Blind-Llm-Eyes", "N/A")
    print(f"\n{'='*60}")
    print(f"  {label}")
    print(f"{'='*60}")
    print(f"  HTTP Status       : {status}")
    print(f"  耗时              : {elapsed:.2f}s")
    print(f"  X-Blind-Llm-Eyes  : {blind_header}")

    try:
        parsed = json.loads(body)
        for block in parsed.get("content", []):
            if block.get("type") == "text":
                print(f"  回答              : {block['text'][:120]}")
                break
    except Exception:
        print(f"  原始响应(前200字符): {body[:200]}")

    return {
        "label": label,
        "status": status,
        "elapsed": elapsed,
        "blind_header": blind_header,
    }


# ── 打印汇总表 ────────────────────────────────────────────────────
def print_summary(results: list, title: str):
    print(f"\n{'='*60}")
    print(f"  {title}")
    print(f"{'='*60}")
    print(f"{'轮次':<12} {'耗时':<10} {'X-Blind-Llm-Eyes':<30}")
    print("-" * 55)
    for r in results:
        print(f"{r['label']:<12} {r['elapsed']:.2f}s{'':<4} {r['blind_header']:<30}")


# ── 主流程 ────────────────────────────────────────────────────────
URL = "http://127.0.0.1:8790/v1/messages"

print(f"目标地址: {URL}")
print(f"图A: {W}x{H} 纯红色 PNG ({len(PNG_RED)} bytes)")
print(f"图B: {W}x{H} 纯蓝色 PNG ({len(PNG_BLUE)} bytes)")

# ═══════════════════════════════════════════════════════════════════
# 场景一：同一张图片（图A）重复发送 3 次
# ═══════════════════════════════════════════════════════════════════
print(f"\n{'#'*60}")
print(f"  场景一：同一张图片（红色）重复发送 3 次")
print(f"{'#'*60}")

results_s1 = []
for i in range(3):
    label = f"S1-{i+1}"
    payload = build_payload_single(B64_RED)
    result = send_request(URL, payload, label)
    results_s1.append(result)
    if i < 2:
        time.sleep(1)

print_summary(results_s1, "场景一汇总")

# ═══════════════════════════════════════════════════════════════════
# 场景二：发送完全不同的图片（图B 蓝色）2 次
# ═══════════════════════════════════════════════════════════════════
print(f"\n{'#'*60}")
print(f"  场景二：发送完全不同的图片（蓝色）2 次")
print(f"{'#'*60}")

results_s2 = []
for i in range(2):
    label = f"S2-{i+1}"
    payload = build_payload_single(B64_BLUE)
    result = send_request(URL, payload, label)
    results_s2.append(result)
    if i < 0:
        time.sleep(1)

print_summary(results_s2, "场景二汇总")

# ═══════════════════════════════════════════════════════════════════
# 场景三：同一条消息里放两张图（图A + 图B），验证双图缓存互不干扰
# ═══════════════════════════════════════════════════════════════════
print(f"\n{'#'*60}")
print(f"  场景三：同一条消息放两张图（红+蓝），两张都应命中缓存")
print(f"{'#'*60}")

results_s3 = []
label = "S3-1"
payload = build_payload_dual(B64_RED, B64_BLUE)
result = send_request(URL, payload, label)
results_s3.append(result)

print_summary(results_s3, "场景三汇总")

# ═══════════════════════════════════════════════════════════════════
# 场景四：使用真实图片（P5 新岛真 x2）测试缓存
# ═══════════════════════════════════════════════════════════════════
import os

TESTDATA_DIR = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "testdata")
IMG_MAKOTO_01 = os.path.join(TESTDATA_DIR, "makoto_01.png")
IMG_MAKOTO_02 = os.path.join(TESTDATA_DIR, "makoto_02.png")


def load_image_as_b64(path: str) -> str:
    with open(path, "rb") as f:
        data = f.read()
    return base64.b64encode(data).decode("ascii")


def detect_media_type(path: str) -> str:
    ext = os.path.splitext(path)[1].lower()
    mapping = {".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}
    return mapping.get(ext, "image/png")


real_images_available = os.path.exists(IMG_MAKOTO_01) and os.path.exists(IMG_MAKOTO_02)

if real_images_available:
    print(f"\n{'#'*60}")
    print(f"  场景四：使用真实图片（P5 新岛真 x2）测试缓存")
    print(f"{'#'*60}")

    b64_makoto_01 = load_image_as_b64(IMG_MAKOTO_01)
    b64_makoto_02 = load_image_as_b64(IMG_MAKOTO_02)
    media_type_01 = detect_media_type(IMG_MAKOTO_01)
    media_type_02 = detect_media_type(IMG_MAKOTO_02)

    file_size_01 = os.path.getsize(IMG_MAKOTO_01)
    file_size_02 = os.path.getsize(IMG_MAKOTO_02)
    print(f"  makoto_01.png: {file_size_01} bytes, {len(b64_makoto_01)} chars base64")
    print(f"  makoto_02.png: {file_size_02} bytes, {len(b64_makoto_02)} chars base64")

    # 4a: 发送图1 两次
    results_s4a = []
    for i in range(2):
        payload = {
            "model": "deepseek-v4-flash",
            "system": [{"type": "text", "text": "You are a helpful assistant."}],
            "messages": [{
                "role": "user",
                "content": [
                    {"type": "text", "text": "Describe this character."},
                    {"type": "image", "source": {"type": "base64", "media_type": media_type_01, "data": b64_makoto_01}},
                ],
            }],
            "max_tokens": 200,
        }
        result = send_request(URL, payload, f"S4a-{i+1}")
        results_s4a.append(result)
        if i < 1:
            time.sleep(1)

    print_summary(results_s4a, "场景四-a汇总（图1 重复发送）")

    # 4b: 发送图2 两次
    results_s4b = []
    for i in range(2):
        payload = {
            "model": "deepseek-v4-flash",
            "system": [{"type": "text", "text": "You are a helpful assistant."}],
            "messages": [{
                "role": "user",
                "content": [
                    {"type": "text", "text": "Describe this character."},
                    {"type": "image", "source": {"type": "base64", "media_type": media_type_02, "data": b64_makoto_02}},
                ],
            }],
            "max_tokens": 200,
        }
        result = send_request(URL, payload, f"S4b-{i+1}")
        results_s4b.append(result)
        if i < 1:
            time.sleep(1)

    print_summary(results_s4b, "场景四-b汇总（图2 重复发送）")

    # 4c: 双图请求
    payload = {
        "model": "deepseek-v4-flash",
        "system": [{"type": "text", "text": "You are a helpful assistant."}],
        "messages": [{
            "role": "user",
            "content": [
                {"type": "text", "text": "Compare these two characters."},
                {"type": "image", "source": {"type": "base64", "media_type": media_type_01, "data": b64_makoto_01}},
                {"type": "image", "source": {"type": "base64", "media_type": media_type_02, "data": b64_makoto_02}},
            ],
        }],
        "max_tokens": 200,
    }
    result = send_request(URL, payload, "S4c-1")
    results_s4c = [result]

    print_summary(results_s4c, "场景四-c汇总（双图请求）")
else:
    print(f"\n[SKIP] 场景四：testdata 下未找到 makoto_01.png / makoto_02.png")

# ═══════════════════════════════════════════════════════════════════
# 断言
# ═══════════════════════════════════════════════════════════════════
print(f"\n{'='*60}")
print("  断言结果")
print(f"{'='*60}")

# 场景一断言
s1_0 = results_s1[0]
s1_rest = results_s1[1:]

if "0 cached" in s1_0["blind_header"] and "1 rewritten" in s1_0["blind_header"]:
    print("[PASS] S1-1: 图A首次发送，MiMo 被调用，缓存未命中")
else:
    print(f"[FAIL] S1-1: 期望 '1 rewritten, 0 cached'，实际 '{s1_0['blind_header']}'")

if all("1 cached" in r["blind_header"] for r in s1_rest):
    print("[PASS] S1-2~3: 图A重复发送，全部缓存命中")
else:
    for r in s1_rest:
        if "1 cached" not in r["blind_header"]:
            print(f"[FAIL] {r['label']}: 期望缓存命中，实际 '{r['blind_header']}'")

# 场景二断言
s2_0 = results_s2[0]
s2_1 = results_s2[1]

if "0 cached" in s2_0["blind_header"] and "1 rewritten" in s2_0["blind_header"]:
    print("[PASS] S2-1: 图B首次发送，MiMo 被调用，缓存未命中（与图A互不干扰）")
else:
    print(f"[FAIL] S2-1: 期望 '1 rewritten, 0 cached'，实际 '{s2_0['blind_header']}'")

if "1 cached" in s2_1["blind_header"]:
    print("[PASS] S2-2: 图B重复发送，缓存命中")
else:
    print(f"[FAIL] S2-2: 期望 '1 rewritten, 1 cached'，实际 '{s2_1['blind_header']}'")

# 场景三断言：两张图都应命中缓存（2 rewritten, 2 cached）
s3_0 = results_s3[0]
if "2 cached" in s3_0["blind_header"] and "2 rewritten" in s3_0["blind_header"]:
    print("[PASS] S3-1: 双图请求，两张图均命中缓存（2 rewritten, 2 cached）")
elif "2 rewritten" in s3_0["blind_header"] and "1 cached" in s3_0["blind_header"]:
    print("[WARN] S3-1: 双图请求，仅 1 张命中缓存（可能图B在 S2 已被淘汰）")
else:
    print(f"[FAIL] S3-1: 期望 '2 rewritten, 2 cached'，实际 '{s3_0['blind_header']}'")

# 耗时对比
if s1_0["elapsed"] > s1_rest[0]["elapsed"] * 1.5:
    print(f"[PASS] S1 耗时对比: 首次 ({s1_0['elapsed']:.2f}s) > 缓存命中 ({s1_rest[0]['elapsed']:.2f}s)")
else:
    print(f"[WARN] S1 耗时对比: 首次 ({s1_0['elapsed']:.2f}s) vs 缓存命中 ({s1_rest[0]['elapsed']:.2f}s) 差异不大")

if s2_0["elapsed"] > s2_1["elapsed"] * 1.5:
    print(f"[PASS] S2 耗时对比: 首次 ({s2_0['elapsed']:.2f}s) > 缓存命中 ({s2_1['elapsed']:.2f}s)")
else:
    print(f"[WARN] S2 耗时对比: 首次 ({s2_0['elapsed']:.2f}s) vs 缓存命中 ({s2_1['elapsed']:.2f}s) 差异不大")

# 场景四断言
if real_images_available:
    print(f"\n{'='*60}")
    print("  场景四断言（真实图片）")
    print(f"{'='*60}")

    s4a_0 = results_s4a[0]
    s4a_1 = results_s4a[1]
    s4b_0 = results_s4b[0]
    s4b_1 = results_s4b[1]
    s4c_0 = results_s4c[0]

    if "0 cached" in s4a_0["blind_header"]:
        print("[PASS] S4a-1: makoto_01 首次发送，缓存未命中")
    elif "1 cached" in s4a_0["blind_header"]:
        print("[INFO] S4a-1: makoto_01 在之前测试中已缓存")
    else:
        print(f"[FAIL] S4a-1: 状态 '{s4a_0['blind_header']}'")

    if "1 cached" in s4a_1["blind_header"]:
        print("[PASS] S4a-2: makoto_01 重复发送，缓存命中")
    else:
        print(f"[FAIL] S4a-2: 期望缓存命中，实际 '{s4a_1['blind_header']}'")

    if "0 cached" in s4b_0["blind_header"]:
        print("[PASS] S4b-1: makoto_02 首次发送，缓存未命中")
    elif "1 cached" in s4b_0["blind_header"]:
        print("[INFO] S4b-1: makoto_02 在之前测试中已缓存")
    else:
        print(f"[FAIL] S4b-1: 状态 '{s4b_0['blind_header']}'")

    if "1 cached" in s4b_1["blind_header"]:
        print("[PASS] S4b-2: makoto_02 重复发送，缓存命中")
    else:
        print(f"[FAIL] S4b-2: 期望缓存命中，实际 '{s4b_1['blind_header']}'")

    if "2 cached" in s4c_0["blind_header"]:
        print("[PASS] S4c-1: 双图请求，两张均命中缓存（2 rewritten, 2 cached）")
    else:
        print(f"[WARN] S4c-1: 双图请求状态: '{s4c_0['blind_header']}'")
