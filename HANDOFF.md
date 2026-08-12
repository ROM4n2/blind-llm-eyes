# 交接文档 — Vision Fallback 独立 Go 工具

> 作者从上一个会话（agent-api 工作区）交接到此工作区（D:\Code\new-api-contrib）。
> 目标：在此目录**新建会话**，继续"给纯文本模型加眼睛"的独立 Go 工具开发。

## 1. 项目一句话

本地轻量 Go 代理，架在 **Claude Code ↔ 上游 LLM** 之间：Claude Code 发请求含图片块 → 本地工具用视觉模型（MiMo `mimo-v2.5`）把图转成文字描述 → 替换图片块 → 转发给纯文本上游（DeepSeek 等）→ 流式响应原样透传。让用户**留在 DeepSeek、不切供应商、粘贴截图直接可用**地看图。

## 2. 现有产物（先读这两个，别重复调研）

| 文件 | 内容 |
|---|---|
| [vision-fallback-notes.md](./vision-fallback-notes.md) | 决策时间线 + 全部调研固化：issue 被拒经过、机制先例、两个已知死因、本机栈事实、Go 优势、下一步清单 |
| [vision-fallback-architecture.md](./vision-fallback-architecture.md) | **架构设计 v1 草案**：数据流图、Go 优势、死因对照表、组件划分（5 个 package）、三个难点解法、配置样例、v1 范围、验证方案、**第 11 节有 6 个待作者审的决策点** |

## 3. 上下文与决策记录（摘要）

- **起点**：用户主力模型 DeepSeek 纯文本无视觉；需要"不换模型看图"。
- **尝试 1**：向 new-api（Go 网关，44.9k★）提功能请求 **issue #6780**（2026-08-11，模板合规，bot 审查通过）。
- **转折**：2026-08-12 CONTRIBUTOR **somnifex** 回复"网关层不宜干预模型行为，该在应用层做"——**设计哲学软拒绝**。
- **决定**：不做网关特性，**改做独立 Go 代理**（应用层/自持形态），无需说服维护者。issue 留着不删，不再投入。
- **关键判断**：机制有成熟先例（CCR v2 做过但因多轮状态 #872 弃用；OmniRoute Vision Bridge 45k★ 活着）；小克隆全是 TS/Python，**Go 格子空**；两个已知死因（#872 历史劫持、#35223 每轮重发重复扣费）已被文档化，解法已设计（无会话状态 + 内容哈希缓存）。

## 4. 下一步（用户刚批准的方向）

1. **用户先审 `vision-fallback-architecture.md` 第 11 节的 6 个决策点**（默认建议：MiMo 后端 / 纯内存 LRU / fail-open / 只 Anthropic 格式 / 配置手动声明纯文本 / 命名用户定）
2. 决策定后：把架构拆成可执行实现计划 → 搭骨架（config/messages/proxy/vision/cache 五包）→ 先写 messages 包解析/替换 + 单测 → curl 集成测试 → 端到端验证
3. 记得处理与 CC Switch 的接缝：把该供应商的 `ANTHROPIC_BASE_URL` 指向本地工具（如 `http://127.0.0.1:8790`），工具自己持有上游 key 配置

## 5. 环境事实（本机，新会话必知）

- 全局 CLAUDE.md（`~/.claude/CLAUDE.md`）有机器环境事实：**WebSearch/WebFetch 被墙**（查文档用 context7、抓网页用 fetch MCP、GitHub 用 `api.github.com` + curl 或 GitHub MCP）；Git Bash `ln -s` 会静默复制（用 `MSYS=winsymlinks:nativestrict`）；`C:\Users\席皓宇` 是指向 `C:\Users\Haoyu` 的符号链接（正常非故障）；codegraph 索引根是 `D:\Code`。
- **Go 代码规范**：`agent-api/docs/GO-STANDARDS.md`（MUST/SHOULD 规则，写 Go 前必读）——项目约定"所有代码由用户亲手写，agent 只教练（审/提问/讲），不代写"。
- **视觉后端现成**：MiMo `mimo-v2.5`（OpenAI 兼容 `https://api.xiaomimimo.com/v1`，key 在 CC Switch 的小米供应商配置里）。注意：MiMo 供应商 **opus 等级 = mimo-v2.5（有视觉）**，sonnet = mimo-v2.5-pro（无视觉）。
- GitHub MCP 已配好可用（`github-mcp-server.exe`，`--read-only`）；issue #6780 查状态可直接用它。
- 新会话在 `D:\Code\new-api-contrib` 启动时，此目录不是 git 仓库——建议先 `git init`。

## 6. 敏感信息

本目录文件**不含任何真实 API key / token / PAT**（配置样例里的 `sk-...` 是占位符）。真实凭据在 `~/.claude/settings.json` 和 `~/.cc-switch/cc-switch.db`，**不要读取、打印、外泄**。写工具的 config 样例时用占位符。

## 7. 建议技能（新会话可调用）

- **writing-plans** — 把 architecture 第 11 节定稿后转成实现计划（下一步的主要动作）
- **test-driven-development / tdd** — 用户重视测试；先写 messages 包单测再实现
- **verification-before-completion** — 每步"完成"前先跑验证（证据优先）
- **claude-api** — 工具要对接 Anthropic Messages API 格式（image/tool_use/tool_result 块）与 OpenAI 兼容视觉端点，实现 messages/vision 包前读它
- **brainstorming** — 若要改架构方向（比如接缝、多轮缓存方案）先发散再定
- 开发规范：全局 CLAUDE.md 的"通用开发规范" 8 条 + GO-STANDARDS.md 的 Go 细则

## 8. 相关外部链接

- new-api issue #6780：https://github.com/QuantumNous/new-api/issues/6780
- 参考实现：OmniRoute Vision Bridge（45k★）、CCR ImageAgent（v2 已弃）、new-api #5593（重路由方案，挂起）
- 用户 GitHub：ROM4n2（GitHub MCP 已配，可代查）
