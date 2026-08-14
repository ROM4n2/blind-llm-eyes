# 发布说明 — v1.0.1

> ← 返回 [README](README.md) · [v1.0.0 发布说明](RELEASE_NOTES-v1.0.0.zh-CN.md)

[English](RELEASE_NOTES-v1.0.1.md) | **中文**

**blind-llm-eyes** —— 给纯文本 LLM 一双眼睛。

发布日期：2026-08-14
规模：自 `v1.0.0` 起 7 个提交 —— 6 项 Tier-1 修复 + 文档

---

## 总览

v1.0.1 是首个补丁版本。修复了 v1.0.0 报告的两个最紧急 bug——Claude Code
token 计数器失效、CLI 命令在 Trae IDE 沙箱下失败——并新增四项体验改进：
视觉模型白名单透传、免费 GLM-4V-Flash 引导预设、发布包内的双击启动脚本、
以及 `doctor --deep` 端到端图片自检。

无破坏性变更。既有 `config.yaml` 完全兼容；新字段均为可选，带合理默认值。

---

## Bug 修复

### 1. `count_tokens` 端点透传

[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go)

Claude Code 调用 `POST /v1/messages/count_tokens` 来填充 token 计数器。
v1.0.0 对该路径返回 `404`，导致计数器空白。handler 现已注册该路由，将请求体
原样转发至上游，响应也原样回流。不做视觉改写、不缓存——纯透明代理。

### 2. Trae IDE 沙箱：pidfile 与 settings 写入

[cli/pidfile.go](file:///d:/Code/new-api-contrib/cli/pidfile.go) ·
[cli/settings.go](file:///d:/Code/new-api-contrib/cli/settings.go)

Trae IDE 沙箱在受保护目录（`%AppData%`、`~/.claude/`）下阻止 `os.CreateTemp`，
导致 `status`、`stop`、`disconnect` 失败。原子写入辅助函数改为用
`os.WriteFile` 写入固定名临时文件（`<path>.tmp`）再 `os.Rename`——原子性不变，
不再调用 `CreateTemp`。`disconnect no-backup` 测试改用临时目录而非真实
`~/.claude/` 路径，确保确定性。

---

## 新特性

### 3. 视觉模型白名单透传

[proxy/handler.go](file:///d:/Code/new-api-contrib/proxy/handler.go)

当上游模型原生支持图片输入（如 `gpt-4o`）时，把图片改写成文字既浪费一次
视觉 API 调用、又凭空增加约 8s 延迟。新增 `vision_capable_models` 集合
（大小写不敏感）：handler 在净化后的模型名命中时跳过整个改写阶段，原样转发
请求体，响应头带 `X-Blind-Llm-Eyes` 透传标记。集合为空/nil = 永不跳过
（默认行为，不变）。

### 4. 免费 GLM-4V-Flash 预设

[vision/provider.go](file:///d:/Code/new-api-contrib/vision/provider.go) ·
[cli/setup.go](file:///d:/Code/new-api-contrib/cli/setup.go)

新增 `glm_free` provider 类型，自动填充 GLM-4V-Flash 的 base URL 与模型
（智谱 AI BigModel 平台）。该模型免费使用——只需从
`https://open.bigmodel.cn` 领取（免费）API key。`setup` 向导现将其作为默认
视觉 provider 提供，移除首次运行的付费门槛。

### 5. 发布包内置双击启动脚本

[.goreleaser.yaml](file:///d:/Code/new-api-contrib/.goreleaser.yaml) ·
[scripts/](file:///d:/Code/new-api-contrib/scripts)

每个发布包根目录现包含 `start.bat`（Windows）、`start.sh`（Linux）、
`start.command`（macOS）。双击即运行 `blind-llm-eyes start`，退出后保持
窗口打开——无需打开终端。

### 6. `doctor --deep` 端到端图片自检

[cli/doctor.go](file:///d:/Code/new-api-contrib/cli/doctor.go)

`--deep` 标志在文本 ping 通过后，发送一张真实 1×1 PNG 经 `DescribeImage`
走完整视觉管道（base64 解码、API 调用、响应解析）。要求返回非空描述才算
通过。可揪出纯文本 ping 发现不了的配置错误（错误的媒体类型处理、截断响应、
空描述 bug）。

---

## 本次发布提交

```text
103f190 feat(cli): add doctor --deep end-to-end vision pipeline test
1195927 chore(release): add start scripts to release archives
27217bb docs: add performance test results and known bugs
4485c37 feat(vision): add glm_free provider type for GLM-4V-Flash free tier
0448c7e feat(proxy): add vision-capable model whitelist passthrough
eb222a3 fix(cli): avoid trae sandbox temp file restriction in pidfile and settings
6bac77c feat(proxy): add count_tokens endpoint passthrough
```

---

## 验证

- `go test -race -count=1 ./...` —— 全部包通过
- `go vet ./...` —— 零警告
- `go build ./...` —— 通过

---

## 升级说明

v1.0.0 的直接替换版。启用新可选特性：

- **白名单透传** —— 在 `config.yaml` 中添加：
  ```yaml
  vision_capable_models: ["gpt-4o", "gpt-4-turbo"]
  ```
  （或在 `vision_providers[]` 里按 provider 设置 `vision_capable_models`）。
  省略则保持始终改写的行为。

- **GLM-4V-Flash 免费档** —— 重跑 `blind-llm-eyes setup` 选选项 1，或配置一个
  `type: glm_free` 的 provider，只需填 `api_key`。

- **doctor --deep** —— `blind-llm-eyes doctor --deep`
