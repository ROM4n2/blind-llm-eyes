#!/usr/bin/env python3
"""Smoke test: send 9 requests with unique images to fill adaptive sample window."""
import base64, hashlib, json, struct, time, urllib.request, zlib

PROXY = "http://127.0.0.1:8790"

def make_png_b64(r, g, b):
    sig = b"\x89PNG\r\n\x1a\n"
    ihdr = struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0)
    ihdr_chunk = struct.pack(">I", 13) + b"IHDR" + ihdr
    ihdr_chunk += struct.pack(">I", zlib.crc32(b"IHDR" + ihdr) & 0xFFFFFFFF)
    raw = b"\x00" + bytes([r, g, b])
    comp = zlib.compress(raw)
    idat_chunk = struct.pack(">I", len(comp)) + b"IDAT" + comp
    idat_chunk += struct.pack(">I", zlib.crc32(b"IDAT" + comp) & 0xFFFFFFFF)
    iend = struct.pack(">I", 0) + b"IEND" + struct.pack(">I", zlib.crc32(b"IEND") & 0xFFFFFFFF)
    return base64.b64encode(sig + ihdr_chunk + idat_chunk + iend).decode()

def build_body(seed):
    content = [{"type": "text", "text": "What color is this?"}]
    for i in range(2):
        h = hashlib.sha256(f"smoke-{seed}-{i}".encode()).digest()
        b64 = make_png_b64(h[0], h[1], h[2])
        content.append({"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": b64}})
    return json.dumps({
        "model": "claude-3-5-sonnet-20241022",
        "max_tokens": 50,
        "messages": [{"role": "user", "content": content}],
    }).encode()

def get_metrics():
    try:
        with urllib.request.urlopen(f"{PROXY}/metrics", timeout=5) as resp:
            text = resp.read().decode()
        result = {}
        for line in text.splitlines():
            if line.startswith("blind_llm_eyes_adaptive"):
                parts = line.split()
                if len(parts) == 2:
                    result[parts[0]] = parts[1]
            elif line.startswith("blind_llm_eyes_vision_calls_total"):
                parts = line.split()
                result[parts[0]] = parts[1]
        return result
    except Exception as e:
        return {"error": str(e)}

print("=== Before ===")
m = get_metrics()
for k, v in m.items():
    print(f"  {k} = {v}")

print("\nSending 9 requests (18 samples + 2 existing = 20)...")
for i in range(9):
    body = build_body(i)
    t0 = time.time()
    try:
        req = urllib.request.Request(
            f"{PROXY}/v1/messages",
            data=body,
            headers={
                "Content-Type": "application/json",
                "x-api-key": "REDACTED_USE_ENV_VAR",
                "anthropic-version": "2023-06-01",
            },
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=120) as resp:
            _ = resp.read()
        elapsed = time.time() - t0
        print(f"  [{i+1}/9] {elapsed:.1f}s OK")
    except Exception as e:
        elapsed = time.time() - t0
        print(f"  [{i+1}/9] {elapsed:.1f}s ERR: {e}")

print("\n=== After ===")
m = get_metrics()
for k, v in m.items():
    print(f"  {k} = {v}")
