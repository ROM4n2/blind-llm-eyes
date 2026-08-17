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
- **嵌套 `tool_result` 图片** — 递归扫描 `tool_result` 内嵌图片（真实 Claude Code 截图多藏在此），与顶层图片同等处理，深度限制 16 防栈溢出。
- **会话上下文感知描述** *(可选)* — 把最近 N 轮对话（`context_rounds`，默认 3 轮 / `context_max_chars` 2000 字）注入视觉模型，让描述贴合上下文（如"这个报错怎么解决"能聚焦到报错信息）。
- **内容哈希缓存 + 可选持久化** — 多轮对话中同一张图重复发送时，零视觉调用。默认内存 LRU；可选两层缓存（LRU + SQLite）让描述跨重启存活（`cache.type: twotier`）。
- **`singleflight` 在途去重** — 并发请求携带同一张图时，合并为一次视觉调用。
- **并行图片处理** — 单请求内多图通过 `errgroup` 并发描述，受 `concurrency_limit` 限制。
- **自适应并发** *(可选)* — AIMD 风格控制器，根据真实视觉调用延迟反馈（P90 + 错误率）动态调整并发上限，保护上游免被打爆。
- **fail-open** — 视觉调用失败时替换为占位文字，不阻塞整个请求。
- **WebP → PNG 转换** — 发送前自动把 WebP 图片转为 PNG。
- **自适应超时** — 大图使用更长的超时（`large_image_timeout`）。
- **可观测性** — 结构化 JSON 日志（异步写入）、基于 `httptrace` 的分阶段耗时、`/metrics` Prometheus 指标、贯穿全链路的 request ID、优雅关闭。
- **可插拔视觉后端** — 任何实现 `vision.VisionProvider` 的后端都能接入。内置预设：MiMo（Anthropic 格式）、OpenAI 兼容、GLM-4V-Flash（免费档）、Qwen-VL（DashScope）。
- **多 provider 池 + 熔断器** — `vision_providers` 按 priority 升序定义 provider 列表，失败自动故障转移，每个 provider 配备独立三态熔断器。
- **单一静态二进制** — 无运行时依赖，约 10 MB，无需 Go 编译器。
- **模型名净化** — 转发上游前自动剥离厂商上下文长度后缀（`deepseek-chat[1m]` → `deepseek-chat`）；请求路径与 cc-switch 导入双重保险。
- **CLI 生命周期** — 9 个子命令覆盖全生命周期：`setup`（交互配置 + doctor）、`doctor`（连通性自检）、`connect`/`disconnect`（Claude Code settings.json 接线）、`start`、`status`、`stop`、`version`、`cache`（持久缓存查看/清理）。
- **cc-switch 一键导入** — 直接从 cc-switch SQLite 数据库读取 provider（尽力而为：DB 被锁时回退临时拷贝，任何错误回退手动输入）。
- **安全的 settings 管理** — `connect` 改写 Claude Code 的 `settings.json` 时先整文件备份且只备份一次（重复 `connect` 永不覆盖备份）；`disconnect` 经原子写从备份逐字节还原。

## 性能结果

对 MiMo 视觉模型的生产流量实测（2026-08-12 冒烟测试，20 个样本）。每个数字都是真实的前后对比测量，不是估算。

| 优化 | 之前 | 之后 | 提升 |
| --- | --- | --- | --- |
| 关闭 MiMo thinking 模式 | body_read 23,500 ms | 4,153 ms | **-82%** |
| 并行图片处理（`errgroup`） | 39,689 ms（2 图端到端） | 19,754 ms | **-50%** |
| 在途视觉调用去重（`singleflight`） | 5 张相同图 → 5 次调用 | → 1 次调用 | **N→1** |
| AIMD 自适应并发 | 静态 `limit=4` | 动态 `[1,12]`，自调优 | 最高 +5 / 最低降至 1 |

细节：

- **关闭 thinking 模式** —— 最大的一次提升。`body_read` 占视觉调用的大头；根因是 MiMo 默认 thinking 模式生成隐藏推理内容（2257 字符）。切到 Anthropic Messages API + `thinking.type: disabled` 后，body_read 从 23.5 s 降到 4.2 s（**-82%**），整个视觉调用从 31.7 s 降到 12.4 s（**-61%**），推理输出归零。
- **并行图片处理** —— 串行 → `errgroup` + 有界并发：2 图端到端 39.7 s → 19.8 s（**-50%**）。
- **在途去重** —— `singleflight` + 内容哈希 LRU：单请求内 5 张相同图合并为 1 次视觉调用；10 个并发请求带同一张图也合并为 1 次（**N→1**）。
- **AIMD 自适应并发** —— 用 20 个生产样本调参（MiMo 均值 7.7 s、最差 20.6 s）：默认值定为 `concurrency_limit: 6`、`max_limit: 12`、`sample_window: 10`、`cooldown_ms: 2000`。三阶段验证：上游快（P90≈3 s）→ 并发自动升至 9；正常（P90≈11 s）→ 滞回区稳住 10；上游慢（P90≈16 s）→ 并发降至 1。

## 快速开始

三个子命令完成重活：`setup`（交互配置）、`connect`（把 Claude Code 接到代理）、`start`（运行代理）。整个流程是 下载 → `setup` → `connect` → `start`。

### 1. 安装

从 [releases 页面](../../releases) 下载预编译二进制（Windows / Linux / macOS，amd64 + arm64），或从源码构建：

```bash
go install github.com/ROM4n2/blind-llm-eyes@latest
# 或在 checkout 里：
go build -o blind-llm-eyes .
```

验证安装：

```bash
blind-llm-eyes version   # blind-llm-eyes <版本> (go <运行时>)
```

### 2. 配置（`setup`）

运行交互式向导。它可以从你已有的 [cc-switch](https://github.com/farion1231/cc-switch) 数据库导入 provider，保存前还会跑一遍连通性自检（`doctor`）：

```bash
blind-llm-eyes setup
```

向导收集一个上游（纯文本）与一个视觉 provider——base URL、API key 与视觉模型——ping 两者后写出 `config.yaml`。偏好手动编辑？把 `config.example.yaml` 复制为 `config.yaml` 填入真实 key。最小可用配置：

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

### 3. 连接 Claude Code（`connect`）

通过改写 `~/.claude/settings.json` 的 `env.ANTHROPIC_BASE_URL` 把 Claude Code 指向代理：

```bash
blind-llm-eyes connect
```

会先写整文件备份到 `~/.claude/.bak-before-connect`（重复 `connect` 永不覆盖）。重启 Claude Code 使其重新读取 `settings.json`。撤销用 `blind-llm-eyes disconnect`——它从备份逐字节还原 `settings.json`。

### 4. 运行（`start`）

```bash
blind-llm-eyes            # 无参数 = start（向后兼容）
blind-llm-eyes start      # 显式
blind-llm-eyes -config /path/to/config.yaml
```

管理运行中的代理：

```bash
blind-llm-eyes status     # pidfile + GET /healthz → RUNNING / STALE
blind-llm-eyes stop       # POST /admin/shutdown（token 鉴权）→ 优雅排空
blind-llm-eyes doctor     # 全链路连通性自检（上游 + 每个视觉 provider）
```

### 5. 验证

往 Claude Code 里粘贴截图——纯文本模型现在应该能回答关于它的问题。或直接 curl：

```bash
curl -N http://127.0.0.1:8790/v1/messages \
  -H "Authorization: Bearer <key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-chat","max_tokens":500,"stream":true,"messages":[{"role":"user","content":[
    {"type":"text","text":"这张图里有什么？"},
    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<base64>"}}]}]}'
```

响应头 `X-Blind-Llm-Eyes` 报告结果：`rewritten=1 cached=0`。

> **关于 CC Switch：** 把供应商的 `ANTHROPIC_BASE_URL` 设为 `http://127.0.0.1:8790`。**不要**用 CC Switch 的代理模式——它会截断图片 body。

### 验证与故障排查

用 5 步渐进验证隔离问题，不用瞎猜：

```powershell
# L1 — 二进制与版本注入
blind-llm-eyes version
# → blind-llm-eyes 1.0.0 (go go1.26.5)

# L2 — 连通性（几乎不耗 token）
blind-llm-eyes doctor
# → upstream=PASS  vision=PASS   (exit 0)
# 若任一 FAIL：检查 base_url（无尾斜杠、正确 /anthropic vs /v1）、
#              检查 API key（环境变量或 config.yaml）

# L3 — 进程存活
# 终端 A：blind-llm-eyes start
# 终端 B：
blind-llm-eyes status
curl -s http://127.0.0.1:8790/healthz
# → status: RUNNING pid=1234 addr=127.0.0.1:8790
# → healthz: ok

# L4 — 端到端（消耗少量 API 配额）
curl -N http://127.0.0.1:8790/v1/messages `
  -H "Authorization: Bearer <upstream-key>" -H "Content-Type: application/json" `
  -d '{\"model\":\"deepseek-chat\",\"max_tokens\":500,\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"What color is this?\"},{\"type\":\"image\",\"source\":{\"type\":\"base64\",\"media_type\":\"image/png\",\"data\":\"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8/5+hHgAHggJ/PchI7wAAAABJRU5ErkJggg==\"}}]}]}' `
  -D - 2>&1 | Select-Object -First 50
# 找：HTTP/1.1 200 OK  +  X-Blind-Llm-Eyes: rewritten=1 cached=0
#     然后是包含视觉描述文本的 SSE 流

# L5 — 优雅关闭
blind-llm-eyes stop
blind-llm-eyes status
# → NOT RUNNING
```

常见坑：

| 症状 | 原因 | 解决 |
| --- | --- | --- |
| `doctor` 报 vision `PASS` 但 L4 返回 502 `"vision call failed"` | 真实 `DescribeImage`（更大 payload + 更长超时）在 `Ping`（1 token）成功的地方失败。常见于视觉超时过小。 | 调大 `vision.timeout`（默认 30s），确认视觉模型在其配置端点接受图片。 |
| `status` 返回 `NOT RUNNING` 但 `start` 在前台运行 | Windows Trae IDE 终端里，pidfile 的 `os.CreateTemp` 被沙箱拦截（sandbox error: `Not allow operate files: ...pidfile-*.tmp`）。只影响 IDE 集成终端。 | 从独立 PowerShell 窗口跑 `status` / `stop`。前台 `start` 在任何地方（含 Trae 内）都能工作。 |
| 上游返回 400 `"model: deepseek-chat[1m] not found"` | `[1m]` 后缀到达了上游（模型净化未生效）。v1.0.0 之前的旧构建不剥离后缀；或盲-llm-eyes 前面的反向代理重新注入了原始 model。 | 升级到 v1.0.0+（`blind-llm-eyes version` 确认）。确认 Claude Code / cc-switch 里的 `ANTHROPIC_MODEL` env 不会覆盖经代理发送的 model 字段——净化只发生在**代理内部**解析后的请求体上。 |
| `connect` 后 Claude Code 仍说"看不到图片" | Claude Code 只在启动时读 `settings.json`；或 `connect` 之后跑了 cc-switch 切换器，把 `ANTHROPIC_BASE_URL` 覆盖回去了。 | 重启 Claude Code。重跑 `connect`，用代理期间**别**在 cc-switch 里切供应商。 |
| 图片被截断 / 视觉 hash 不匹配 | 用了 CC Switch **代理模式**（而非 base_url 覆盖）。它会在转发前静默截断 >200 字节的请求体。 | 直接设 `ANTHROPIC_BASE_URL=http://127.0.0.1:8790`。别在 cc-switch 开代理模式。`blind-llm-eyes connect` 写的就是这个设置。 |
| `go install github.com/ROM4n2/blind-llm-eyes@latest` 报 "invalid version: unknown revision" | `@latest` 需要远端仓库至少有一个已发布的 semver tag。`v1.0.0` tag 本地存在但还没推。 | 从 releases 页下载预编译压缩包，或从 checkout 构建：`go build -ldflags "-X github.com/ROM4n2/blind-llm-eyes/buildinfo.Version=1.0.0" .` |

### 缓存管理

启用 `cache.type: twotier` 后，SQLite 冷层跨重启存活。四个 `cache` 子命令用于查看和管理：

```powershell
blind-llm-eyes cache path                  # 显示缓存类型、db 路径、文件是否存在
blind-llm-eyes cache stats                 # 条目数、总字节数、最早/最晚访问、db 文件大小
blind-llm-eyes cache list -limit 50        # 列出条目（哈希前缀 + 描述预览）
blind-llm-eyes cache clear -yes            # 删除所有条目（-yes 跳过确认）
```

每个子命令支持 `-config <路径>`（默认 `config.yaml`）。`stats` / `list` / `clear` 在 `cache.type` 为 `lru` 时退出码 1（无持久存储）。CLI 打开 SQLite 时设置 `busy_timeout=5000`，代理运行中也能查看/清理缓存。

### 安全考虑

- **Admin token** — 每次 `start` 从 `crypto/rand` 新生成，写入 `<UserConfigDir>/blind-llm-eyes/pidfile.json`，`stop` 时删除。绝不持久化到别处（无环境变量、无配置键）。
- **绑定地址** — 默认监听 `127.0.0.1:8790`（仅回环）。暴露 `0.0.0.0` 或局域网 IP 会把你的上游 API key 转发给任何能触达该端口的人——切勿在不可信网络上这样做。`/metrics` 与 `/healthz` 也无客户端鉴权。
- **config vs env 的 key** — `config.yaml` 里设的 API key 会覆盖客户端的 `Authorization` 头。配置了 `UpstreamAPIKey` 时，handler 转发上游前会剥离客户端的 `Authorization` / `Proxy-Authorization` / `Cookie` 头，避免泄露客户端凭据。
- **Pidfile 权限** — Windows 上 pidfile 目录默认 `%AppData%\blind-llm-eyes\`（继承 AppData 的用户级 ACL）。不要跨账户共享 `%AppData%` 或放在公共可读的共享盘。
- **`connect` 备份** — `~/.claude/.bak-before-connect` 是你原始 `settings.json` 的逐字副本。它包含 `settings.json` 里原有的任何秘密（如 `ANTHROPIC_API_KEY`）。请像对待真实文件一样对待它。

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
| `vision.context_rounds` | `3` | 上下文感知描述：最近 N 轮对话；`0`/`-1` 禁用 |
| `vision.context_max_chars` | `2000` | 上下文最大字符数（约 500 tokens） |
| `cache.type` | `lru` | `lru`（纯内存）或 `twotier`（LRU + SQLite，描述跨重启存活） |
| `cache.max_entries` | `500` | LRU 热层容量（`type=lru` 时为总容量） |
| `cache.db_path` | `./cache.db` | SQLite 冷层路径（仅 `type=twotier` 生效） |
| `cache.sqlite_max_entries` | `10000` | SQLite 冷层容量上限 |
| `cache.sqlite_ttl` | `0`（不限） | 冷层条目 TTL，如 `720h` 表示 30 天 |
| `concurrency_limit` | `6` | 单请求内最大并行视觉调用数；也是 adaptive 的初始值 |
| `adaptive_concurrency.*` | 关闭 | AIMD 控制器（见下） |
| `fail_open` | `true` | 视觉失败 → 占位文字而非 502 |
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
messages    Anthropic Messages 解析、校验、图片→文本改写、上下文提取
cache       内容哈希（sha256）key + 线程安全 LRU + 可选 SQLite 冷层（TwoTier）
vision      VisionProvider 接口 + MiMo / OpenAI 兼容 / GLM 免费档 / Qwen-VL 预设 + 多 provider 池 + 熔断器
proxy       请求管线：解析 → 找图 → 缓存 → 描述 → 替换 → 转发
logging     结构化 JSON 日志、异步写入、request ID
metrics     Prometheus registry
cli         子命令：setup / doctor / connect / disconnect / status / stop / version / cache
admin       /admin/shutdown 优雅关闭端点（token 鉴权）
modelutil   模型名净化（[1m] 剥离）
buildinfo   构建版本（ldflags 注入）
```

请求路径：解析 → 扫描图片块（含 tool_result 嵌套）→ LRU 哈希查询 → miss → `singleflight` 去重 → 提取对话上下文 → 视觉模型描述 → 图片替换为文本 → 追加 system 指令 → 转发上游 → 流式回传。

让它省钱的三个设计：

- **无状态。** 只处理当前请求；缓存是纯 hash→描述映射，绝不参与行为决策。（刻意如此：此前有实现因跨请求会话状态导致行为被历史劫持而废弃。）
- **两层去重。** LRU 消除重复发送成本；`singleflight` 把并发同图调用合并为一次。两者都以图片内容哈希为 key。
- **Go 并发模型务实使用。** channel 管数据流（异步日志、singleflight 结果交接、关闭信号），锁/原子管真实共享态（LRU、计数器）。`go test -race` 全绿。

## 可观测性

- **JSON 日志** — 每个阶段带 `stage`、`node_name`、`request_id` 与耗时字段。视觉调用拆出 `sf_total_ms` / `fn_exec_ms` / `sf_wait_ms`，可区分去重等待与实际上游耗时。
- **`/metrics`** — Prometheus：HTTP 请求/耗时、图片处理、视觉调用/耗时、上游请求、缓存命中率、自适应上限 gauge、per-provider 熔断器状态。
- **`/healthz`** — 存活探针。
- **`X-Blind-Llm-Eyes` 头** — 每个响应 `rewritten=N cached=M`。

## 已知限制

- **仅 Anthropic Messages 格式**（不支持 OpenAI Chat Completions 输入）。
- **`/metrics`、`/healthz` 无客户端鉴权** — 建议仅本地暴露。
- **默认纯内存缓存** — 重启后描述丢失。可选 `cache.type: twotier` 启用 SQLite 持久化。

## 开发

```bash
make test          # go test -race -count=1 ./...  （CI 门禁）
make vet           # go vet ./...
make build         # 带版本 ldflags 的本地二进制
make snapshot      # goreleaser build --snapshot —— 编译全部平台目标
make goreleaser-check  # 校验 .goreleaser.yaml
```

发布是 tag 驱动：推送 `v*` tag，`release` 工作流运行 `goreleaser release`，把压缩包 + 校验和发布到 GitHub release。维护者也可本地 `make release`（需设 `GITHUB_TOKEN`）。

测试覆盖：解析/改写 round-trip（含未知字段保留、嵌套 tool_result）、LRU 行为、视觉客户端（mock server）、完整 handler 管线（mock 视觉 + 上游）、并发边界、跨请求 `singleflight` 去重、自适应限流行为、E2E 全链路（含 FailOpen/FailClosed 网络超时场景）。

## Roadmap

- 跨请求全局并发 / 上游限流
- 加权负载均衡 + 主动健康检查（多 provider 场景）

## License

MIT
