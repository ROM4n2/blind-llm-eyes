#!/usr/bin/env python3
"""Send unique test images to trigger vision provider failover + circuit breaker."""
import base64, json, os, struct, zlib, urllib.request, time

PROXY = "http://127.0.0.1:8790"
API_KEY = os.environ.get("BLIND_LLM_EYES_API_KEY", "")
if not API_KEY.startswith("sk-"):
    print("ERROR: set BLIND_LLM_EYES_API_KEY env var")
    raise SystemExit(1)

def make_png(r, g, b):
    width, height = 8, 8
    raw = b""
    for _ in range(height):
        raw += b"\x00" + bytes([r, g, b]) * width
    compressed = zlib.compress(raw)
    def chunk(ctype, data):
        c = ctype + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c) & 0xffffffff)
    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr) + chunk(b"IDAT", compressed) + chunk(b"IEND", b"")

colors = [(255,0,0), (0,255,0), (0,0,255), (128,128,0), (255,128,0)]
color_names = ["red", "green", "blue", "yellow", "orange"]

print(f"Sending {len(colors)} unique images to trigger circuit breaker (threshold=3)...")
print()

for i, ((r, g, b), name) in enumerate(zip(colors, color_names)):
    img_data = make_png(r, g, b)
    img_b64 = base64.b64encode(img_data).decode()

    body = {
        "model": "claude-3-5-sonnet-20241022",
        "max_tokens": 256,
        "messages": [{
            "role": "user",
            "content": [
                {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": img_b64}},
                {"type": "text", "text": f"What color is this image? Answer in one word."}
            ]
        }]
    }

    t0 = time.time()
    req = urllib.request.Request(
        f"{PROXY}/v1/messages",
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "x-api-key": API_KEY, "anthropic-version": "2023-06-01"},
        method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            elapsed = time.time() - t0
            print(f"  Req {i+1} ({name:6s}): {resp.status} in {elapsed:.1f}s")
    except urllib.error.HTTPError as e:
        elapsed = time.time() - t0
        err_body = e.read().decode()[:120]
        print(f"  Req {i+1} ({name:6s}): {e.code} in {elapsed:.1f}s - {err_body}")
    except Exception as e:
        elapsed = time.time() - t0
        print(f"  Req {i+1} ({name:6s}): ERROR in {elapsed:.1f}s - {e}")

print()
print("=== Metrics ===")
try:
    with urllib.request.urlopen(f"{PROXY}/metrics", timeout=5) as resp:
        for line in resp.read().decode().splitlines():
            if any(k in line for k in ["provider_calls", "circuit_breaker_state", "failover_events"]) and not line.startswith("#"):
                print(f"  {line}")
except Exception as e:
    print(f"  Metrics error: {e}")
