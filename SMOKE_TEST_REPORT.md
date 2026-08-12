# 生产冒烟测试报告 — adaptive_concurrency

> **测试日期**: 2026-08-12
> **测试时间**: 21:27:00 – 21:31:12 (CST)
> **版本**: commit `f6f58dc` (feat: AIMD adaptive concurrency limiter)
> **结论**: **PASS** — adaptive 控制器生产可用，首次评估决策正确

---

## 1. 测试目标

验证 `adaptive_concurrency` 功能在生产配置下：
1. 服务正常启动，adaptive 控制器初始化
2. 真实 MiMo vision 调用成功率 100%
3. DeepSeek 上游转发正常
4. 滚动窗口满 20 样本后触发首次 AIMD 评估
5. P90 计算准确，决策符合滞回区逻辑

---

## 2. 测试环境

### 2.1 服务配置 (config.yaml)

| 配置项 | 值 |
|--------|-----|
| listen | `127.0.0.1:8790` |
| upstream.base_url | `https://api.deepseek.com/anthropic` |
| vision.base_url | `https://api.xiaomimimo.com/anthropic` |
| vision.model | `mimo-v2.5` |
| vision.timeout | 60s |
| vision.description_cap | 1000 |
| cache.max_entries | 500 |
| concurrency_limit | 4 |
| fail_open | true |

### 2.2 adaptive_concurrency 配置

```yaml
adaptive_concurrency:
  enabled: true
  min_limit: 1
  max_limit: 16
  fast_threshold_ms: 8000       # P90 < 8s → 加性增 +1
  slow_threshold_ms: 15000       # P90 > 15s → 乘性减 ×0.75
  sample_window: 20              # 20 样本触发一次评估
  cooldown_ms: 3000             # 两次调整最小间隔
  increase_step: 1
  decrease_ratio: 0.75
  error_threshold: 0.10          # 错误率 > 10% → 触发降并发
```

### 2.3 启动日志

```json
{
  "timestamp": "2026-08-12T21:27:00.7056665+08:00",
  "level": "INFO",
  "message": "blind-llm-eyes starting",
  "listen": "127.0.0.1:8790",
  "upstream": "https://api.deepseek.com/anthropic",
  "upstream_key_set": true,
  "vision_model": "mimo-v2.5",
  "vision_timeout": 60000000000,
  "vision_large_timeout": 120000000000,
  "large_image_threshold": 1048576,
  "supported_formats": ["image/png","image/jpeg","image/webp","image/gif"],
  "fail_open": true,
  "cache_max": 500,
  "concurrency_limit": 4,
  "adaptive_enabled": true,
  "adaptive_range": "[1, 16]",
  "adaptive_threshold_ms": "fast=8000 slow=15000"
}
```

---

## 3. 测试方法

### 3.1 请求构造

| 阶段 | 请求数 | 每请求图片数 | 图片来源 | 说明 |
|------|--------|-------------|----------|------|
| 阶段 1 | 1 | 2 | makoto_01.png + makoto_02.png | 真实截图 (150KB + 138KB) |
| 阶段 2 | 9 | 2 | SHA256 唯一 1×1 PNG | 填满 20 样本窗口 |

每请求 2 张图，共 10 请求 × 2 = 20 个 vision 样本，恰好填满 `sample_window=20`。

### 3.2 SHA256 唯一图片生成

为避免缓存命中（LRU max=500），阶段 2 使用 `SHA256("smoke-{seed}-{i}")` 生成唯一 RGB 值构造 1×1 PNG，确保 20 个样本全部触发真实 MiMo 调用。

---

## 4. Vision 调用延迟数据（20 样本）

### 4.1 完整样本列表

| # | request_id | 图片 | fn_exec_ms | desc_len | sf_wait_ms | 来源 |
|---|------------|------|-----------|----------|-----------|------|
| 1 | 6f9bb066 | makoto_01 | 11837 | 1201 | 0 | 真实截图 |
| 2 | 6f9bb066 | makoto_02 | 20599 | 1903 | 0 | 真实截图 |
| 3 | 79f85271 | smoke-0-0 | 4252 | 359 | 0 | 1×1 PNG |
| 4 | 79f85271 | smoke-0-1 | 4554 | 353 | 0 | 1×1 PNG |
| 5 | 9e30f37b | smoke-1-0 | 7132 | 348 | 0 | 1×1 PNG |
| 6 | 9e30f37b | smoke-1-1 | 7673 | 359 | 0 | 1×1 PNG |
| 7 | beb22485 | smoke-2-0 | 4837 | 389 | 0 | 1×1 PNG |
| 8 | beb22485 | smoke-2-1 | 8105 | 395 | 0 | 1×1 PNG |
| 9 | 5e19f085 | smoke-3-0 | 10328 | 496 | 0 | 1×1 PNG |
| 10 | 5e19f085 | smoke-3-1 | 16058 | 365 | 0 | 1×1 PNG |
| 11 | 405b54af | smoke-4-0 | 4542 | 395 | 0 | 1×1 PNG |
| 12 | 405b54af | smoke-4-1 | 9831 | 353 | 0 | 1×1 PNG |
| 13 | 9ecb0e23 | smoke-5-0 | 4323 | 420 | 0 | 1×1 PNG |
| 14 | 9ecb0e23 | smoke-5-1 | 4727 | 404 | 0 | 1×1 PNG |
| 15 | 8ab42ffd | smoke-6-0 | 4667 | 417 | 0 | 1×1 PNG |
| 16 | 8ab42ffd | smoke-6-1 | 9811 | 452 | 0 | 1×1 PNG |
| 17 | ea040d0a | smoke-7-0 | 5770 | 471 | 0 | 1×1 PNG |
| 18 | ea040d0a | smoke-7-1 | 6177 | 401 | 0 | 1×1 PNG |
| 19 | c8a37b96 | smoke-8-0 | 4569 | 410 | 0 | 1×1 PNG |
| 20 | c8a37b96 | smoke-8-1 | 4883 | 404 | 0 | 1×1 PNG |

> `sf_wait_ms=0` 表示所有调用都是 singleflight executor（非等待者），样本无重复。

### 4.2 排序后百分位计算

样本升序排列：

| 排名 | fn_exec_ms | 百分位 |
|------|-----------|--------|
| 1 | 4252 | P5 |
| 2 | 4323 | P10 |
| 3 | 4542 | P15 |
| 4 | 4554 | P20 |
| 5 | 4569 | P25 |
| 6 | 4667 | P30 |
| 7 | 4727 | P35 |
| 8 | 4837 | P40 |
| 9 | 4883 | P45 |
| 10 | 5770 | P50 (中位数) |
| 11 | 6177 | P55 |
| 12 | 7132 | P60 |
| 13 | 7673 | P65 |
| 14 | 8105 | P70 |
| 15 | 9811 | P75 |
| 16 | 9831 | P80 |
| 17 | 10328 | P85 |
| **18** | **11837** | **P90** |
| 19 | 16058 | P95 |
| 20 | 20599 | P100 (max) |

### 4.3 统计摘要

| 指标 | 值 (ms) | 值 (s) |
|------|---------|--------|
| Min | 4252 | 4.25 |
| Max | 20599 | 20.60 |
| Mean | 7734 | 7.73 |
| Median (P50) | 5970 | 5.97 |
| **P90** | **11837** | **11.84** |
| P95 | 16058 | 16.06 |
| P99 | 20599 | 20.60 |
| 样本数 | 20 | — |
| 错误数 | 0 | — |
| 错误率 | 0% | — |

### 4.4 延迟分布直方图 (Prometheus)

| 区间 | 调用数 | 占比 |
|------|--------|------|
| ≤ 5s | 9 | 45% |
| 5s – 10s | 7 | 35% |
| 10s – 30s | 4 | 20% |
| > 30s | 0 | 0% |

```
≤5s  ████████████████████░░░░░░░░░░░░░░░░░░░░░  45% (9)
5-10 ██████████████░░░░░░░░░░░░░░░░░░░░░░░░░░░  35% (7)
10-30████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░  20% (4)
>30s ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░   0% (0)
```

---

## 5. Upstream (DeepSeek) 延迟数据

### 5.1 完整请求列表

| # | request_id | total_duration_ms | http_duration_ms | stream_ms | headers_ms | rewritten | cached |
|---|------------|------------------|-----------------|-----------|-----------|-----------|--------|
| 1 | 6f9bb066 | 23099 | 2492 | 2261 | 231 | 2 | 0 |
| 2 | 79f85271 | 6402 | 1847 | 1670 | 177 | 2 | 0 |
| 3 | 9e30f37b | 9525 | 1851 | 1630 | 221 | 2 | 0 |
| 4 | beb22485 | 9997 | 1891 | 1738 | 153 | 2 | 0 |
| 5 | 5e19f085 | 17996 | 1936 | 1746 | 190 | 2 | 0 |
| 6 | 405b54af | 11700 | 1867 | 1699 | 168 | 2 | 0 |
| 7 | 9ecb0e23 | 6677 | 1949 | 1671 | 278 | 2 | 0 |
| 8 | 8ab42ffd | 11519 | 1706 | 1404 | 302 | 2 | 0 |
| 9 | ea040d0a | 7682 | 1504 | 1343 | 161 | 2 | 0 |
| 10 | c8a37b96 | 6155 | 1270 | 1056 | 214 | 2 | 0 |

> `total_duration_ms` = MiMo vision 处理 + DeepSeek HTTP 调用
> `http_duration_ms` = 纯 DeepSeek HTTP 耗时（不含 vision）

### 5.2 统计摘要

| 指标 | total_duration (ms) | http_duration (ms) |
|------|---------------------|---------------------|
| Min | 6155 | 1270 |
| Max | 23099 | 2492 |
| Mean | 11075 | 1831 |
| Sum | 110752 | 18313 |

> DeepSeek HTTP 平均仅 1.83s，占总耗时的 16.5%。MiMo vision 占比 83.5%。

---

## 6. Adaptive 并发评估

### 6.1 评估触发条件

| 条件 | 值 | 满足？ |
|------|-----|--------|
| 窗口样本数 ≥ sample_window | 20 ≥ 20 | ✅ |
| 距上次调整 ≥ cooldown_ms | N/A (首次) | ✅ |

### 6.2 评估决策日志（原文）

```json
{
  "timestamp": "2026-08-12T21:31:11.7234249+08:00",
  "level": "INFO",
  "message": "adaptive: no change (hysteresis band)",
  "limit": 4,
  "p90_ms": 11837,
  "fast_threshold_ms": 8000,
  "slow_threshold_ms": 15000,
  "err_rate": 0,
  "window_size": 20
}
```

### 6.3 决策逻辑

```
输入:
  P90 = 11837ms
  fast_threshold = 8000ms
  slow_threshold = 15000ms
  err_rate = 0%
  error_threshold = 10%

判断:
  err_rate (0%) < error_threshold (10%)        → 不触发错误降并发
  P90 (11837ms) < fast_threshold (8000ms)?      → 否
  P90 (11837ms) > slow_threshold (15000ms)?     → 否
  → 8000 ≤ 11837 ≤ 15000 → 滞回区 (hysteresis band)

输出:
  决策: no change
  limit: 保持 4
  adjustments_total{direction="none"} += 1
```

### 6.4 AIMD 决策矩阵

本测试覆盖的场景：

| P90 范围 | 决策 | 本测试是否覆盖 |
|----------|------|---------------|
| < 8000ms (fast) | increase (+1) | ✗ (P90=11837) |
| 8000–15000ms (hysteresis) | no change | **✅** |
| > 15000ms (slow) | decrease (×0.75) | ✗ (P90=11837) |
| err_rate > 10% | decrease (×0.75) | ✗ (err_rate=0%) |

> increase 和 decrease 场景已由 [test-adaptive-prod.py](test-adaptive-prod.py) 用 mock MiMo 完整验证（三阶段全 PASS）。本报告聚焦真实 MiMo 流量下的滞回区场景。

---

## 7. Prometheus 指标快照

### 7.1 Adaptive 指标

| 指标 | 值 |
|------|-----|
| `blind_llm_eyes_adaptive_concurrency_current` | 4 |
| `blind_llm_eyes_adaptive_vision_p90_seconds` | 11.837 |
| `blind_llm_eyes_adaptive_concurrency_adjustments_total{direction="none"}` | 1 |
| `blind_llm_eyes_adaptive_concurrency_adjustments_total{direction="up"}` | 0 |
| `blind_llm_eyes_adaptive_concurrency_adjustments_total{direction="down"}` | 0 |

### 7.2 Vision 指标

| 指标 | 值 |
|------|-----|
| `blind_llm_eyes_vision_calls_total{result="success"}` | 20 |
| `blind_llm_eyes_vision_calls_total{result="error"}` | 0 |
| `blind_llm_eyes_vision_call_duration_seconds_sum` | 154.69 |
| `blind_llm_eyes_vision_call_duration_seconds_count` | 20 |
| 平均 vision 延迟 | 7.73s |

### 7.3 HTTP 请求指标

| 指标 | 值 |
|------|-----|
| `blind_llm_eyes_http_requests_total{status="200"}` | 10 |
| `blind_llm_eyes_http_requests_total{status="400"}` | 1 |
| `blind_llm_eyes_http_request_duration_seconds_sum` | 110.76 |
| `blind_llm_eyes_http_request_duration_seconds_count` | 10 |
| 平均 HTTP 请求延迟 | 11.08s |

### 7.4 Upstream 指标

| 指标 | 值 |
|------|-----|
| `blind_llm_eyes_upstream_requests_total{status="200"}` | 10 |
| `blind_llm_eyes_upstream_request_duration_seconds_sum` | 2.10 |
| `blind_llm_eyes_upstream_request_duration_seconds_count` | 10 |
| 平均 upstream 延迟 | 0.21s |

### 7.5 缓存指标

| 指标 | 值 |
|------|-----|
| `blind_llm_eyes_cache_hit_ratio` | 0 |
| `blind_llm_eyes_images_processed_total{outcome="rewritten"}` | 20 |
| `blind_llm_eyes_images_processed_total{outcome="cached"}` | 0 |

---

## 8. 分析与结论

### 8.1 Adaptive 控制器验证

| 验证项 | 预期 | 实际 | 结果 |
|--------|------|------|------|
| 服务启动 | adaptive_enabled=true | true | ✅ |
| 样本采集 | 20 样本后触发评估 | 20 样本 → 1 次评估 | ✅ |
| P90 计算 | 第 18 个值（升序） | 11837ms (第 18 位) | ✅ |
| 滞回区判断 | 8s ≤ 11.8s ≤ 15s → no change | "no change (hysteresis band)" | ✅ |
| limit 不变 | 保持 4 | 4 | ✅ |
| adjustments 计数 | none += 1 | 1 | ✅ |
| 错误率计算 | 0/20 = 0% | 0% | ✅ |

### 8.2 MiMo 延迟特征分析

1. **延迟波动大**：4.25s – 20.60s，跨度 4.8 倍
2. **双峰分布**：45% 的调用 < 5s，20% 的调用 > 10s
3. **大图更慢**：makoto 真实截图 (150KB) 平均 16.2s，1×1 PNG 平均 6.7s
4. **存在超慢调用**：makoto_02 耗时 20.6s，已超过 slow_threshold (15s)
5. **P90 适合作为拥塞信号**：P90=11.8s 比均值 7.7s 更能反映尾部延迟

### 8.3 验证自适应限流的必要性

如果 P90 持续高于 15s（如 makoto 场景），AIMD 将自动降低并发：
- 4 → 3 (×0.75) → 2 (×0.75) → 1 (×0.75)
- 防止 4 路并发 vision 调用同时打爆 MiMo

如果 P90 持续低于 8s（如 1×1 PNG 场景），AIMD 将自动提升并发：
- 4 → 5 → 6 → ... → 16
- 充分利用 MiMo 吞吐能力

### 8.4 DeepSeek 上游性能

- 平均 HTTP 耗时 1.83s，占总请求 16.5%
- 延迟稳定（1.27s – 2.49s，波动小）
- 不构成瓶颈

### 8.5 结论

**adaptive_concurrency 生产可用。**

1. ✅ AIMD 控制器在真实 MiMo 流量下正确初始化、采集样本、触发评估
2. ✅ P90 计算（11837ms）与手动验证一致
3. ✅ 滞回区决策正确（8000 ≤ 11837 ≤ 15000 → no change）
4. ✅ 20 次 vision 调用 0 错误，singleflight 去重正常（sf_wait_ms=0）
5. ✅ Prometheus 指标全部上报

---

## 9. 附录

### 9.1 测试脚本

- [smoke-fill-samples.py](smoke-fill-samples.py) — 阶段 2 请求生成器（9 请求 × 2 唯一图）

### 9.2 相关 commit

| Commit | 说明 |
|--------|------|
| `f6f58dc` | feat(proxy): implement adaptive concurrency limiter with AIMD control |
| `e60e098` | test: add adaptive concurrency end-to-end validation scripts |

### 9.3 相关文件

| 文件 | 说明 |
|------|------|
| [proxy/adaptive.go](proxy/adaptive.go) | AIMD 控制器实现 |
| [proxy/adaptive_test.go](proxy/adaptive_test.go) | 单元测试（5 场景） |
| [config.yaml](config.yaml) | 生产配置 |
| [test-adaptive-prod.py](test-adaptive-prod.py) | mock MiMo 端到端验证脚本 |
