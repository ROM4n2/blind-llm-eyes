# 发布说明 — v1.0.0

> ← 返回 [README](README.md) · [HANDOFF](HANDOFF.md)

[English](RELEASE_NOTES-v1.0.0.md) | **中文**

**blind-llm-eyes** —— 给纯文本 LLM 一双眼睛。

发布日期：2026-08-13
分支：已合并到 `master`（合并提交 `58c1ee8`，tag：`v1.0.0`）
规模：13 个提交 · 44 个文件变更 · +5379 / −83 行（含文档）

---

## 总览

v1.0.0 让 blind-llm-eyes 从开发原型过渡为**产品化、可直接安装**的工具。代理核心（图片→文字描述改写、缓存、并发、可观测性）在之前里程碑中已就绪；本次发布补齐非开发者所需的一切——**安装、配置、运行、管理**——无需碰 Go 工具链。

核心变化：**单一静态二进制** + 引导式 `setup` 向导 + 一键 `connect` 到 Claude Code + `start`/`stop`/`status`/`doctor` 生命周期命令。每次推送 `v*` tag，goreleaser 自动发布 Windows / Linux / macOS（amd64 + arm64）预编译二进制。

### 用户流程（v1.0.0 新增）

```text
下载二进制 → blind-llm-eyes setup → blind-llm-eyes connect → blind-llm-eyes start
                                                              ↳ status / stop / doctor
```

---

## 新特性

### 1. CLI 子命令系统（`blind-llm-eyes <子命令>`）

[main.go](file:///d:/Code/new-api-contrib/main.go) 中的薄分发层把请求路由到新 [cli/](file:///d:/Code/new-api-contrib/cli) 包实现的各子命令。无参数运行仍启动服务器（与 v1.0.0 之前的调用方式向后兼容）。

| 子命令 | 用途 |
| --- | --- |
| `version` | 打印版本 + Go 运行时（`buildinfo.Version`，发布时经 ldflags 注入） |
| `setup` | 交互式配置向导（见 §2） |
| `connect` | 把 Claude Code 的 `settings.json` 接到代理（见 §3） |
| `disconnect` | 从备份还原 `settings.json` |
| `start` | 运行代理（无子命令时的默认行为） |
| `stop` | 经 admin 端点优雅关闭（见 §4） |
| `status` | 检查 pidfile + `/healthz` → `RUNNING` / `STALE` / `NOT RUNNING` |
| `doctor` | 连通性自检：ping 上游 + 每个 vision provider（见 §5） |

### 2. 交互式 setup 向导（`setup`）

[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go) 编排完整的首次运行体验：

1. **cc-switch 导入** *(可选)* —— 读取 `~/.cc-switch/cc-switch.db`（SQLite），列出全部 Claude Code provider，让你挑选谁当上游、谁当视觉。导入时模型名会被净化（见 §6）。
2. **手填 + 默认值** —— 上游与视觉的 base URL / API key / model，带合理默认值（`https://api.deepseek.com/anthropic`、`mimo-v2.5` 等）。
3. **doctor 自检** —— 保存前先 ping 两个端点；失败时询问"是否仍要保存"。
4. **生成配置** —— 写出 `config.yaml`。
5. **可选 connect** —— 询问是否立即执行 `connect`。
6. **启动指引** —— 打印接下来要跑的准确命令。

### 3. Claude Code 接线（`connect` / `disconnect`）

[cli/connect.go](file:///d:/Code/new-api-contrib/cli/connect.go) + [cli/settings.go](file:///d:/Code/new-api-contrib/cli/settings.go)

- `connect` 把 `~/.claude/settings.json` 的 `env.ANTHROPIC_BASE_URL` 改写为 `http://127.0.0.1:8790`。
- **任何修改前**先整文件备份到 `~/.claude/.bak-before-connect`。重复 `connect` 只更新 URL，**绝不覆盖备份**（原始状态永远可恢复）。
- 原子写：新 `settings.json` 先写临时文件再 rename，写入中途崩溃不会损坏文件。
- `disconnect` 从备份**逐字节**还原 `settings.json` 并移除备份标记。
- 两个命令都按 OS 自动探测 settings 路径（Windows `%USERPROFILE%\.claude`，其他 `~/.claude`）。

### 4. 进程生命周期：admin 关闭 + pidfile

[admin/admin.go](file:///d:/Code/new-api-contrib/admin/admin.go) + [cli/pidfile.go](file:///d:/Code/new-api-contrib/cli/pidfile.go) + [cli/status.go](file:///d:/Code/new-api-contrib/cli/status.go) + [cli/stop.go](file:///d:/Code/new-api-contrib/cli/stop.go)

- `start` 时服务器写 pidfile（`pidfile.json`），含 PID、监听地址、关闭 token、启动时间。
- `POST /admin/shutdown` 携带 `X-Admin-Token: <token>` 触发优雅排空（经现有 `WaitGroup` 等在途请求），随后通知 `main` 退出。token 缺失/错误 → `403 Forbidden`；正确 → `202 Accepted`。
- `stop` 读 pidfile、发带鉴权的关闭请求、删除 pidfile。
- `status` 把 pidfile 与 `/healthz` 交叉核对：`RUNNING`（pidfile + healthz 均 OK）、`STALE`（pidfile 在但 healthz 不通）、`NOT RUNNING`（无 pidfile）。

### 5. 连通性自检（`doctor`）

[cli/doctor.go](file:///d:/Code/new-api-contrib/cli/doctor.go) + [vision/ping.go](file:///d:/Code/new-api-contrib/vision/ping.go) + [cli/ping_upstream.go](file:///d:/Code/new-api-contrib/cli/ping_upstream.go)

- **Vision ping**（`vision.Ping`）：轻量 `POST /v1/messages`，带 1×1 PNG 与 `max_tokens=1`，不发真实图片即可同时验证连通性与鉴权。实现在 client、单 provider 与 pool 三层（pool 逐个 ping 各 provider，逐 provider 报告状态）。
- **上游 ping**（`cli.PingUpstream`）：`POST /v1/messages` 带一条平凡文本消息；检查 HTTP 状态与可解析的响应。
- `doctor` 同时跑两者，打印通过/失败表格，任一检查失败即以非零码退出——可放进脚本。

### 6. 模型名净化（`[1m]` 剥离）

[modelutil/modelutil.go](file:///d:/Code/new-api-contrib/modelutil/modelutil.go)

部分厂商 UI（如 cc-switch）会给模型名追加上下文长度后缀——`deepseek-chat[1m]`、`deepseek-chat[1M]`。上游 API 会把它们当未知模型拒绝。`modelutil.SanitizeModel` 在 handler 转发请求前剥掉尾部 `[<数字><单位>]` 后缀（单位大小写不敏感）。已集成进代理 handler、cc-switch 导入路径与 setup 向导。

### 7. cc-switch SQLite 导入

[cli/ccswitch.go](file:///d:/Code/new-api-contrib/cli/ccswitch.go)

- 从 `~/.cc-switch/cc-switch.db` 读取 `providers` 表（`app_type = 'claude'`）。
- **只读**打开数据库；若文件被锁（cc-switch GUI 运行中），回退为把 DB 拷贝到临时文件再读。
- 解析每个 provider 的 `settings_config` JSON，抽取 `env.ANTHROPIC_BASE_URL` / `ANTHROPIC_API_KEY` / `ANTHROPIC_MODEL`，净化模型名。
- 损坏行静默跳过；导入永远不会让整个 setup 失败。

---

## 改进

### 重构 provider 构造

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go)

把 `main.go` 里内联的 provider 构造抽成可复用、可测试的函数：
- `BuildProvider(cfg)` —— 从配置构建单个 `vision.Client`。
- `BuildSingleProvider(cfg)` —— 包一层 `*SingleProvider` 带熔断器统计。
- `BuildPool(cfg)` —— 构建带故障转移的多 provider 池。

这把 `main.go` 从约 200 行接线砍成薄分发 + 服务器生命周期，并让 provider 构造可单元测试。

### README 快速开始重写

[README.md](file:///d:/Code/new-api-contrib/README.md) 的 Quick start 改为产品化流程（下载 → `setup` → `connect` → `start` → `status`/`stop`/`doctor`），替代旧的 `go build` + 手工 `cp config.example.yaml` 路径。Development 一节记录了新的 `make` 目标。

---

## 测试

### 新增 E2E 集成测试套件

[test/e2e_test.go](file:///d:/Code/new-api-contrib/test/e2e_test.go) —— 572 行，5 个用例，`-race` 下全部通过。

| 测试 | 验证内容 |
| --- | --- |
| `TestE2E_FullPipeline` | 真实 `vision.Client` + 真实 `proxy.NewHandler` 对 httptest 假服务器。发 `deepseek-chat[1m]` + image 块；断言转发上游前 `[1m]` 已剥离、恰好 1 次 vision 调用、vision 收到 `mimo-v2.5`、图片被描述替换、SSE 透传、`X-Blind-Llm-Eyes` 头、第 2 次请求缓存命中。 |
| `TestE2E_AdminShutdown_PidfileCleanup` | 真实 `admin.ShutdownHandler` + 真实 `cli.WritePidfile`/`ReadPidfile`。错误 token → 403（不关闭）；正确 token → 202 + `Done()` 关闭 + pidfile 删除。 |
| `TestE2E_AdminShutdown_RejectsMissingToken` | 无 token 的 POST → 403，handler 仍保持待命。 |
| `TestE2E_VisionTimeout_FailOpen` | 慢视觉服务器（2s 延迟）+ 200ms 客户端超时 + `FailOpen=true` → 200 响应，上游收到占位文字 `[Image could not be described by vision model]`，而非图片或延迟响应。 |
| `TestE2E_VisionTimeout_FailClosed` | 同一慢服务器 + `FailOpen=false` → 502 响应，上游未被触达，body 提到 `vision call failed`。 |

超时用例用 `select`+`done` 通道模式（而非裸 `time.Sleep`），使 `httptest.Server.Close()` 不会被挂起的 handler 阻塞——这是先前集成测试工作得出的 goroutine 生命周期教训。

### TDD 覆盖

每个新子命令与包都测试先行开发。新测试文件：`admin/admin_test.go`、`buildinfo/buildinfo_test.go`、`cli/*_test.go`（8 个文件）、`modelutil/modelutil_test.go`、`proxy/handler_modelutil_test.go`、`vision/ping_test.go`、`vision/provider_test.go`、`test/e2e_test.go`。

**全量套件：`go test -race -count=1 ./...` 在全部 13 个包上全绿。**

---

## 发布基础设施

### goreleaser

[.goreleaser.yaml](file:///d:/Code/new-api-contrib/.goreleaser.yaml)

- 交叉编译 6 个目标：`linux/darwin/windows × amd64/arm64`。
- `CGO_ENABLED=0` —— 全部依赖均为纯 Go（含 cc-switch 导入用的 `modernc.org/sqlite`），交叉编译在任何 runner 上都可复现。
- 经 ldflags 注入 `buildinfo.Version`（`-X ...buildinfo.Version={{.Version}}`）。
- 产出 `.tar.gz`（linux/darwin）与 `.zip`（windows）压缩包 + `checksums.txt`。
- 基于 git 的变更日志，排除 `docs:`/`test:`/`chore:`/merge 提交。

本机已验证：`goreleaser check` 通过；`goreleaser build --snapshot` 产出全部 6 个二进制（每个约 15 MB）且版本注入正确。

### GitHub 发布工作流

[.github/workflows/release.yml](file:///d:/Code/new-api-contrib/.github/workflows/release.yml)

Tag 驱动：推送 `v*` tag → 工作流带完整历史 checkout、按 `go.mod` 装 Go、跑 `goreleaser release --clean`。把压缩包 + 校验和发布到 GitHub release。`permissions: contents: write` 限定在该工作流内。

### Makefile

[Makefile](file:///d:/Code/new-api-contrib/Makefile)

开发便利目标：

| 目标 | 动作 |
| --- | --- |
| `make test` | `go test -race -count=1 ./...`（CI 门禁） |
| `make vet` | `go vet ./...` |
| `make build` | 带版本 ldflags 的本地二进制（`VERSION ?= dev`） |
| `make snapshot` | `goreleaser build --snapshot --clean`（全部 6 个目标，不发布） |
| `make goreleaser-check` | 校验 `.goreleaser.yaml` |
| `make release` | `goreleaser release --clean`（仅维护者，需 `GITHUB_TOKEN`） |
| `make clean` | 删除 `dist/` 与本地二进制 |

---

## 升级 / 迁移说明

这是首个打 tag 的发布，无原地升级路径。对既有开发版 checkout 用户：

1. 拉取 `feat/onboarding-productize` 分支（或打好的 `v1.0.0` tag）。

2. 从源码构建（`make build`）或从 releases 页下载预编译二进制。
3. 运行 `blind-llm-eyes setup` —— 它会识别你现有的 `config.yaml` 默认值，但仍引导你完成校验。既有 `config.yaml` 完全兼容，无 schema 变更。
4. 如果你之前在 Claude Code 的 settings 或 cc-switch 里手工设过 `ANTHROPIC_BASE_URL`，`blind-llm-eyes connect` 现在会替你管理它（带备份）。

**破坏性调用变更：无。** `blind-llm-eyes -config config.yaml` 仍启动服务器（无子命令路径保留为 `start`）。

---

## 本次发布提交

```text
3f059f8  test(e2e): add network timeout scenarios for fail-open and fail-closed paths
29c5104  test: add e2e integration test for full pipeline and admin shutdown
9f46a2b  chore: add goreleaser, release workflow, and makefile
1508234  feat(cli): add interactive setup wizard with cc-switch import and doctor
252d06e  feat(cli): add cc-switch SQLite provider import
5565bd6  feat(cli): add connect/disconnect for Claude Code settings.json wiring
7a26701  feat(proxy): strip [1m] model suffix before forwarding to upstream
e2cbf8b  feat(cli): add doctor subcommand with vision and upstream ping
405b4b2  feat(admin): add shutdown endpoint, pidfile, and status/stop subcommands
77f8ee8  feat(cli): add subcommand dispatch skeleton and thin main.go
ad70a9f  refactor(vision): extract provider builders from main.go
3d9f9eb  feat(cli): add buildinfo package and version subcommand
```

---

## 已知限制

沿用自 v1.0.0 之前（非本次发布引入）：

- **仅 Anthropic Messages 格式**（不接受 OpenAI Chat Completions 输入）。
- **内存缓存** —— 描述在重启后丢失。
- `/metrics` 与 `/healthz` 无客户端鉴权——请只在本地暴露。
- **CC Switch 代理模式会截断图片 body**；用户必须直接设 `ANTHROPIC_BASE_URL`（`connect` 子命令正是这样做的）。

---

## 验证

- `go test -race -count=1 ./...` —— 13 个包，全绿
- `go vet ./...` —— 零警告
- `goreleaser check` —— 配置合法
- `goreleaser build --snapshot --clean` —— 6 个二进制构建成功，版本 ldflags 确认（`blind-llm-eyes 0.0.1-next (go go1.26.5)`）
- `blind-llm-eyes version` —— 打印注入的版本 + Go 运行时
