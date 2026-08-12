# Blind LLM Eyes — Context

本地轻量 Go 代理：架在 Claude Code 与纯文本上游之间，把请求里任意深度的图片块透明替换为文字描述，让无视觉模型"看见"。只改请求、不改响应。

## 交互

**透明替换 (transparent replace)**:
请求中任意深度的 image 块被替换为一段文字描述，模型无感知、无需主动调用工具。
_Avoid_: 工具调用分叉, vision tool routing

**描述 (description)**:
视觉后端为一张图片生成的文字，原位替换该 image 块。由固定 prompt 生成，与用户请求文本无关（保证缓存可复用）。
_Avoid_: 图像标注, alt text

**改写 (rewrite)**:
把请求中的图片块原位替换为描述的动作。请求里有图才改写，无图则纯透传。
_Avoid_: intercept, transform

## 后端与缓存

**视觉后端 (vision backend)**:
把图片转成描述的视觉模型，当前为小米 MiMo mimo-v2.5（OpenAI 兼容端点）。
_Avoid_: VLM, vision model

**内容哈希缓存 (content-hash cache)**:
hash(图片字节) → 描述 的纯内存映射。同一图片多轮重发时直接复用描述，不重复调用视觉后端。不感知会话状态，是纯函数，不可能劫持行为。
_Avoid_: session cache, LRU cache

**fail-open**:
视觉后端调用失败时，原样转发未改写请求，不让辅助能力劫持主链路。
_Avoid_: fail-closed

## 接缝

**纯文本上游 (text-only upstream)**:
目标模型/供应商，无视觉能力（如 DeepSeek）。工具存在的理由。
_Avoid_: text model, upstream LLM

**多轮重发 (multi-round replay)**:
Claude Code 每轮把完整历史（含图片）重发给上游的行为。缓存存在的理由。
_Avoid_: context replay

**接缝 (seam)**:
工具与 CC Switch 的交界：供应商的 ANTHROPIC_BASE_URL 指向本地工具（127.0.0.1:8790），工具把改写后的请求转发给配置的上游。
_Avoid_: 接入点, integration point

**上游认证 (upstream auth)**:
转发给纯文本上游时用的凭据，即 Claude Code 客户端发来的 Authorization 头原样透传（接缝零额外配置，key 不落盘）。工具不持有自己的上游 key。
_Avoid_: upstream key, api_key 配置

**改写结果头 (rewrite header)**:
响应头 `X-Blind-Llm-Eyes`，标记本次请求改写了几个图片块、缓存命中几个。调试与验证"cache hit"的判据。
_Avoid_: 无（专属于本项目的观察点）

**描述生成上限 (description cap)**:
视觉后端输出描述时的 max_tokens 上限。描述会占用纯文本上游的上下文窗口，必须限长。
_Avoid_: 描述长度, description length
