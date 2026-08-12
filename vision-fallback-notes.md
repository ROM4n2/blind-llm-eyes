# Vision Fallback 项目笔记（调研 + 决策记录）

> 个人痛点：主力模型是 DeepSeek（纯文本无视觉），希望"不切供应商、粘贴截图直接可用"地看图。
> **当前方向（2026-08-12 定）：独立 Go 本地代理**（详见 `vision-fallback-architecture.md`）。
> 配套：`vision-fallback-architecture.md`（架构设计 v1 草案）。

## 决策时间线

1. **2026-08-11**：提交 new-api issue #6780（Vision Fallback 功能请求），模板合规，机器人审查通过。
2. **2026-08-12**：CONTRIBUTOR **somnifex** 回复："网关层不宜干预模型行为，该在应用层做"——**设计哲学层面软拒绝**（非技术否定）。
3. **pivot 决定**：不做网关特性，改做**独立 Go 代理**（应用层/用户自持形态），无需说服任何维护者。

## 核心事实

- **机制先例**（机制成熟，不是发明）：
  - CCR v2 `ImageAgent`（36k★ 项目，Node）做过完全一样的"拦截图片→替换描述"，栽在多轮状态 #872，v3 弃用改工具调用
  - OmniRoute Vision Bridge（45k★）同机制，网关内实现，**已合并、活跃、fail-open**
  - ~20 个小克隆（pi-vlm-proxy 29★、vision-bridge、deepseek-vision-proxy、image-router...）**全部 TS/Python，无 Go**——Go 格子是空的
- **两个已知死因**（我们的差异化依据）：
  - CCR v2 #872：行为被历史劫持 → 解法：**无会话状态，只处理当前请求**
  - LiteLLM #35223：每轮重发图片→每轮重复调视觉 → 解法：**内容哈希缓存**
- **维护者哲学现实**：网关"只转发不干预"是主流（LiteLLM 遇图直接 raise、opencode not_planned、new-api 拒绝）——所以别在网关里做，自己做工具。

## 本机栈事实（做工具要用的）

- **Claude Code 走 Anthropic Messages API**；CC Switch 管理 `~/.claude/settings.json` 的 env（BASE_URL/KEY/模型别名）
- **视觉后端现成**：小米 `mimo-v2.5`（有视觉，OpenAI 兼容 `https://api.xiaomimimo.com/v1`）——你的 MiMo 供应商 **opus 等级 = mimo-v2.5 有视觉**，sonnet = mimo-v2.5-pro 无视觉（小心别切错）
- 各供应商别名（settings.json env 里 `ANTHROPIC_DEFAULT_*_MODEL` 控制）
- Go 环境 OK；codegraph 索引根 `D:\Code`；GO-STANDARDS 在 `agent-api/docs/`

## Go 优势（对比前人 TS/Python）

单静态二进制（无 npx 依赖地狱）、并发原生（goroutine + sync.Map 写缓存）、net/http+io.Copy 处理 SSE 透传是强项、类型安全建模 ContentBlock、毫秒启动、交叉编译、简历契合（Go 后端）。

## 为什么我们做得成（前人死因对照）

1. 死因已被公开文档化（读过原始 issue），不是盲区
2. 范围极小且自持，无维护者博弈
3. 机制验证过多次，是在已知陷阱旁做工程
4. 栈小而确定（Claude Code → 一个上游），不是多供应商混乱

## 架构要点（详见 architecture 文件）

- 位置：Claude Code → 本地工具 → 上游；**只改请求、不改响应**（SSE 透传安全）
- 三个难点：多轮缓存（哈希 LRU）、无历史劫持（确定性变换）、流式透传（http.Flusher）
- v1 范围：Anthropic 格式 + 顶层 image 块 + MiMo 后端 + 缓存 + fail-open
- 待审决策点 6 个（见 architecture 文件第 11 节）

## 下一步

- [ ] 审架构设计的 6 个决策点
- [ ] 搭骨架：config / messages / proxy / vision / cache 五个包
- [ ] 先写 messages 包解析/替换 + 单测（喂真实 Claude Code 请求体）
- [ ] curl 集成测试（断言上游收到的 body 图片已被替换）
- [ ] 端到端：BASE_URL 指向本地工具，Claude Code 里粘贴截图验证

## 相关

- 全局 CLAUDE.md：本机环境事实（WebSearch 被墙、GitHub 走 api.github.com、符号链接等）
- GO-STANDARDS：`agent-api/docs/GO-STANDARDS.md`（写 Go 前必读）
