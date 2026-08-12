# blind-llm-eyes

[English](README.md) | **中文**

给纯文本 LLM 一双眼睛。一个反向代理，架在 Anthropic 兼容的纯文本模型前面，把请求里的图片块透明替换为独立视觉模型生成的文字描述——让你在 Claude Code 里粘贴截图而无需切换供应商。

**主场景：** Claude Code ↔ **blind-llm-eyes** ↔ 纯文本上游（如 DeepSeek），由视觉模型（如 MiMo）描述图片。

```text
Claude Code  ──►  blind-llm-eyes  ──►  纯文本模型（DeepSeek / Anthropic 兼容）
                     │
                     └──►  视觉模型（MiMo）  ──►  图片描述
```

## 为什么

DeepSeek 没有视觉能力。在 Claude Code 里用它，粘贴截图只会得到"我看不到图片"的回复；而换到有视觉的供应商又打断工作流。

`blind-llm-eyes` 消除这个摩擦：图片留在对话里、只被描述一次，纯文本模型把描述当作自己亲眼所见。

## 特性

- **Anthropic Messages 透传** — 接收 `/v1/messages`，改写请求后转发给任意 Anthropic 兼容上游，SSE 响应逐字节流式回传。
- **图片 → 描述替换** — 图片块原位替换为 `<BLIND_LLM_EYES_IMAGE>` 包裹的文本，并追加一条 system 指令，让模型把描述当作自己的视觉观察。
- **内容哈希 LRU 缓存** — 多轮对话中同一张图重复发送时，零视觉调用（命中典型的多轮重发场景）。
- **`singleflight` 在途去重** — 并发请求携带同一张图时，合并为一次视觉调用。
- **并行图片处理** — 单请求内多图通过 `errgroup` 并发描述，受 `concurrency_limit` 限制。
- **自适应并发** *(可选)* — AIMD 风格控制器，根据真实视觉调用延迟反馈（P90 + 错误率）动态调整并发上限，保护上游免被打爆。
- **fail-open** — 视觉调用失败时替换为占位文字，不阻塞整个请求。
- **WebP → PNG 转换** — 发送前自动把 WebP 图片转为 PNG。
- **自适应超时** — 大图使用更长的超时（`large_image_timeout`）。
- **可观测性** — 结构化 JSON 日志（异步写入）、基于 `httptrace` 的分阶段耗时、`/metrics` Prometheus 指标、贯穿全链路的 request ID、优雅关闭。
- **可插拔视觉后端** — 任何实现 `vision.VisionProvider` 的后端都能接入。
- **单一静态二进制** — 无运行时依赖，约 10 MB。

## 快速开始

### 1. 构建

```bash
go build -o blind-llm-eyes .
```

### 2. 配置

```bash
cp config.example.yaml config.yaml   # 然后填入真实 key
```

最小可用配置（生产真实值）：

```yaml
listen: "127.0.0.1:8790"
upstream:
  base_url: "https://api.deepseek.com/anthropic"   # 纯文本上游（Anthropic 兼容）
  api_key: "sk-..."                                # 可选：填了则覆盖客户端 Authorization
vision:
  base_url: "https://api.xiaomimimo.com/anthropic" # 视觉模型根路径；客户端会追加 /v1/messages
  api_key: "sk-..."
  model: "mimo-v2.5"
fail_open: true
log_level: "info"
```

`config.yaml` 已被 git 忽略；`config.example.yaml` 提交的是占位符。密钥也可用环境变量提供（`BLIND_VISION_API_KEY`、`BLIND_UPSTREAM_BASE_URL`、`BLIND_UPSTREAM_API_KEY`、`BLIND_LISTEN`）。

### 3. 运行

```bash
./blind-llm-eyes -config config.yaml
```

### 4. 让 Claude Code 指向它

把供应商的 `ANTHROPIC_BASE_URL` 设为 `http://127.0.0.1:8790`（在 CC Switch 的环境变量 override 或供应商 base URL 设置里配），然后粘贴截图，纯文本模型就能回答关于图片的问题了。

单请求验证：

```bash
curl -N http://127.0.0.1:8790/v1/messages \
  -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","max_tokens":500,"stream":true,"messages":[{"role":"user","content":[
    {"type":"text","text":"这张图里有什么？"},
    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<base64>"}}]}]}'
```

响应头 `X-Blind-Llm-Eyes` 报告结果：`rewritten=1 cached=0`。

## 配置参考

| Key | 默认值 | 说明 |
| --- | --- | --- |
| `listen` | `127.0.0.1:8790` | 监听地址 |
| `upstream.base_url` | —（必填） | 纯文本上游根路径（Anthropic 兼容） |
| `upstream.api_key` | — | 若填写，转发时覆盖客户端的 `Authorization` |
| `vision.base_url` | —（必填） | 视觉模型根路径；客户端追加 `/v1/messages` |
| `vision.api_key` | — | 视觉供应商 key |
| `vision.model` | — | 视觉模型名 |
| `vision.timeout` | `30s` | 默认视觉调用超时 |
| `vision.large_image_timeout` | `120s` | ≥ `large_image_threshold` 图片的超时 |
| `vision.large_image_threshold` | `1048576` | 字节数；达到/超过该值的图片用大图超时 |
| `vision.description_cap` | `1000` | 描述的 `max_tokens` |
| `vision.supported_formats` | png/jpeg/webp/gif | 允许的媒体类型 |
| `cache.max_entries` | `500` | 内存 LRU 容量 |
| `concurrency_limit` | `4` | 单请求内最大并行视觉调用数；也是 adaptive 的初始值 |
| `adaptive_concurrency.*` | 关闭 | AIMD 控制器（见下） |
| `fail_open` | `false` | 视觉失败 → 占位文字而非 502 |
| `log_level` | `info` | `debug`/`info`/`warn`/`error` |

### 自适应并发

模仿 TCP 拥塞控制。每次真实视觉调用（仅 `singleflight` 执行者上报，保证样本反映真实上游延迟）进入滚动窗口；窗口填满时按 P90 延迟与错误率决定新上限：

- P90 < `fast_threshold_ms` 且无错误 → `+increase_step`（加性增）
- P90 > `slow_threshold_ms` 或错误率 > `error_threshold` → `×decrease_ratio`（乘性减）
- 否则 → 不变（滞回区防震荡）

默认关闭；关闭时行为与静态 `concurrency_limit` 完全一致。生产冒烟测试调优（2026-08-12）：MiMo 均值约 7.7 s、最差 20.6 s，因此默认 `concurrency_limit: 6`、`max_limit: 12`、`sample_window: 10`、`cooldown_ms: 2000`。

## 架构

```text
config      YAML + env 加载、默认值
messages    Anthropic Messages 解析、校验、图片→文本改写
cache       内容哈希（sha256）key + 线程安全 LRU
vision      VisionProvider 接口 + MiMo Anthropic 格式客户端
proxy       请求管线：解析 → 找图 → 缓存 → 描述 → 替换 → 转发
logging     结构化 JSON 日志、异步写入、request ID
metrics     Prometheus registry
```

请求路径：解析 → 扫描图片块 → LRU 哈希查询 → miss → `singleflight` 去重 → 视觉模型描述 → 图片替换为文本 → 追加 system 指令 → 转发上游 → 流式回传。

让它省钱的三个设计：

- **无状态。** 只处理当前请求；缓存是纯 hash→描述映射，绝不参与行为决策。（刻意如此：此前有实现因跨请求会话状态导致行为被历史劫持而废弃。）
- **两层去重。** LRU 消除重复发送成本；`singleflight` 把并发同图调用合并为一次。两者都以图片内容哈希为 key。
- **Go 并发模型务实使用。** channel 管数据流（异步日志、singleflight 结果交接、关闭信号），锁/原子管真实共享态（LRU、计数器）。`go test -race` 全绿。

## 可观测性

- **JSON 日志** — 每个阶段带 `stage`、`node_name`、`request_id` 与耗时字段。视觉调用拆出 `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms`，可区分去重等待与实际上游耗时。
- **`/metrics`** — Prometheus：HTTP 请求/耗时、图片处理、视觉调用/耗时、上游请求、缓存命中率、自适应上限 gauge。
- **`/healthz`** — 存活探针。
- **`X-Blind-Llm-Eyes` 头** — 每个响应 `rewritten=N cached=M`。

## 已知限制

- **仅顶层 image 块。** `tool_result` 内嵌的图片原样透传（暂不描述）——支持真实流量的嵌套 tool-result 图片是下一步计划。
- **仅 Anthropic Messages 格式**（不支持 OpenAI Chat Completions 输入）。
- **纯内存缓存** — 重启后描述丢失（个人自用可接受）。
- `/metrics`、`/healthz` 无客户端鉴权 — 建议仅本地暴露。

## 开发

```bash
go build ./...     # 编译
go vet ./...       # 静态检查
go test -race ./...  # 带竞态检测的测试
```

测试覆盖：解析/改写 round-trip（含未知字段保留）、LRU 行为、视觉客户端（mock server）、完整 handler 管线（mock 视觉 + 上游）、并发边界、跨请求 `singleflight` 去重、自适应限流行为。

## Roadmap

- 嵌套 `tool_result` 图片支持（真实 Claude Code 流量的协议正确性）
- 会话上下文感知描述（把最近消息喂给视觉模型，实现带意图的描述）
- 跨请求全局并发 / 上游限流

## License

MIT
