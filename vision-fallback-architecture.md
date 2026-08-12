# Vision Fallback 独立 Go 工具 — 架构设计（v1 草案，待审）

> 定位：本地轻量代理，架在 Claude Code 与上游 LLM 之间，给纯文本模型（DeepSeek 等）透明"看图"能力。
> 状态：**设计草案，等待作者（用户）审阅**。配套见 `vision-fallback-notes.md`（调研与决策记录）。

---

## 0. 一句话

Claude Code 发的请求里有图片块 → 本地工具用视觉模型（MiMo `mimo-v2.5`）把图变成文字描述 → 替换图片块 → 转发给纯文本上游 → 流式响应原样透传。

## 1. 为什么是独立工具（回应 new-api 的拒绝）

new-api CONTRIBUTOR somnifex 说"网关层不宜干预模型行为，该在应用层做"。

- **狭义反驳**：应用层（Claude Code）**不知道目标模型是纯文本**——它见到图就发图。唯一同时握着"图片字节"和"目标模型能力"两样信息的地方，就是中间这层。
- **广义化解**：网关特性是强加给所有用户的；独立工具是**你自己显式运行、显式配置**的，哲学分歧自动消解，也不用说服任何维护者。

## 2. 数据流

```
Claude Code
   │  ANTHROPIC_BASE_URL = http://127.0.0.1:8790
   ▼
┌──────────────────────────────────────────────┐
│  vision-fallback (Go 单二进制)                │
│                                              │
│  请求路径:                                    │
│  1. 解析 Anthropic Messages 请求体            │
│  2. 扫描 content 里的 image 块                │
│  3. 每个图片：contentHash → 查缓存            │
│        ┌─ 命中 → 用缓存里的描述（零视觉调用）  │
│        └─ 未命中 → 调视觉模型 → 存缓存         │
│  4. 图片块 → 文本描述块（原位替换）            │
│  5. 转发给上游（读配置的 upstream）            │
│  6. 响应 SSE 流原样透传（不改响应！）          │
└──────────────────────────────────────────────┘
   │
   ▼
  真实上游（DeepSeek / 火山 / 小米，读工具自己的配置）
```

## 3. Go 的优势（对比前人的 Node/TS、Python）

| 维度 | 为什么对"本地工具"重要 |
|---|---|
| **单静态二进制** | 无 Node 运行时、无 npx 依赖地狱（本会话 markdownify/github-mcp 全栽在 npx 上）。拷一个 exe 就能跑 |
| **并发是语言特性** | 每个请求/视觉调用一个 goroutine；`sync.Map`/mutex 就能写多轮缓存；无事件循环回调用坑 |
| **net/http + io.Copy** | SSE 透传是标准库强项，`http.Flusher` 即可；零框架依赖 |
| **类型安全** | ContentBlock 用 struct 建模（image/text/tool_use/tool_result），格式错在编译期暴露 |
| **快速启动/低内存** | 本地代理应该"无感"，Go 二进制毫秒级启动、几 MB 内存 |
| **交叉编译** | Windows 开发、交叉编出 mac/linux 一行命令 |
| **简历契合** | 你在学 Go 后端——这是 Go 真正重要的项目 |

## 4. 为什么能解决前人"死掉"的问题（三个死因对照）

| 死因 | 出处 | 我们的解法 |
|---|---|---|
| **行为被历史劫持**：激活逻辑看会话历史，处理过图后劫持后续所有工具调用 | CCR v2 #872 | **无会话状态**：只处理当前请求的图片块，确定性变换；跨请求唯一状态是"纯函数式缓存"（hash→描述），不可能劫持行为 |
| **多轮重复扣费/毒化**：客户端每轮重发图片，每轮都重调视觉模型 | LiteLLM #35223 | **内容哈希缓存**：`hash(图片字节) → 描述`，命中直接复用，零视觉调用；LRU 限界 |
| **维护者不认可/哲学之争**：功能嵌在巨型网关里，要过维护者这关 | new-api #6780、opencode not_planned | **自持自控**：独立小工具，自己是产品所有者，无组织摩擦 |

**凭什么你能做成他们没做成的**：
1. 死因已经被公开文档化了（我们读过原始 issue），不是盲区
2. 范围极小且自持——不用在 1200+ issue 的仓库里跟维护者博弈
3. 机制被验证过多次，我们是在**已知陷阱旁做工程**，不是发明
4. 你的栈小而确定（Claude Code → 一个上游），不是网关那套多供应商混乱

## 5. 借鉴活的（不是重造轮子）

| 活项目 | 借鉴点 |
|---|---|
| **OmniRoute Vision Bridge**（45k★，合并） | ① **fail-open**：视觉调用失败就原样转发，不阻塞主请求；② 可观测性：日志/响应头标明"哪里被变换了什么"；③ 策略收敛方向：只做 describe-then-forward，不做整请求 reroute |
| **CCR v3 Fusion vision**（36k★，活） | 工具调用 vs 透明替换的分叉——对我们（"粘贴截图直接可用"）透明替换是正解；但记住 CCR 的教训：多轮状态必须干净 |
| **new-api #5593**（挂着但设计可取） | "只看当前请求，不扫历史"——我们直接采纳 |
| **CC Switch media_sanitizer**（你的日常工具） | 模型能力注册表概念——但我们简化：v1 不维护模型清单，配置里声明"这个上游是纯文本"即可 |

## 6. 核心组件（Go package 划分）

```
vision-fallback/
├── main.go            # 入口：加载配置、启动 server
├── config/            # YAML/env 配置加载、校验
├── proxy/             # net/http 服务器：请求处理、响应透传
│   ├── handler.go     # 单请求处理管线
│   └── passthrough.go # SSE 流式透传（http.Flusher + io.Copy）
├── messages/          # Anthropic Messages API 结构建模
│   ├── content.go     # ContentBlock: text / image / tool_use / tool_result
│   ├── parse.go       # 解析请求体 → 找图片块
│   └── rewrite.go     # 图片块 → 描述块原位替换
├── vision/            # 视觉后端客户端（OpenAI 兼容 /v1）
│   └── client.go      # 调 MiMo mimo-v2.5 等，返回文字描述
└── cache/             # 多轮缓存
    ├── hash.go        # 图片内容哈希
    └── lru.go         # LRU: hash → 描述（限界 + 可选持久化）
```

## 7. 三个难点 + 解法（详细）

### 难点 A：多轮重发图片（LiteLLM 死因）
Claude Code 每轮重发完整历史（含图片）。不做缓存 → 每轮都调视觉模型 → 烧钱且慢。

**解法**：`hash(图片原始字节) → 描述` 的 LRU 缓存。命中直接复用描述，不调视觉。缓存是**纯的**（同样的图永远得同样的描述），不会受会话状态影响。v1 内存 LRU（如 500 条），可选落盘。

### 难点 B：历史劫持（CCR 死因）
CCR v2 的激活逻辑被历史触发，导致处理后劫持后续所有调用。

**解法**：**不做任何"是否处理过"之类的跨请求标记**。每个请求独立、确定性地把图片块换成描述。缓存只做"图片→描述"的纯映射，不参与行为决策。

### 难点 C：流式正确性
好消息：**我们不改响应**，所以 SSE 透传是安全的。请求变换在转发前全部完成，流里只有上游自己的 token。

**要处理的边缘**：① `http.Flusher` 及时 flush；② 客户端断开时停止转发；③ 上游中途报错 → 透传错误事件。

## 8. 配置（v1 极简，YAML + env 覆盖）

```yaml
listen: "127.0.0.1:8790"
upstream:
  base_url: "https://api.deepseek.com/anthropic"   # 真实上游
  api_key: "sk-..."                                 # 上游 key
vision:
  base_url: "https://api.xiaomimimo.com/v1"         # OpenAI 兼容端点
  api_key: "sk-..."                                 # MiMo key
  model: "mimo-v2.5"                                # 有视觉的那个
  timeout: "30s"
cache:
  max_entries: 500
  persist_file: ""                                  # 空 = 不落盘
fail_open: true                                     # 视觉失败 → 原样转发
log_level: "info"
```

## 9. v1 范围（YAGNI——明确不做）

**做**：
- Anthropic Messages API（Claude Code 原生格式）
- 顶层 user content 里的 image 块
- MiMo/OpenAI 兼容视觉后端
- SSE 流式透传
- 内容哈希 LRU 缓存
- fail-open

**明确不做**（v2+ 再说）：
- tool_result 里嵌套图片（少见，Claude Code 传图主要走顶层 image 块）
- 多供应商切换 UI
- TLS / 客户端鉴权
- 配置热重载
- 会话级语义去重（哈希级就够了）

## 10. 验证方案（怎么证明可用）

1. **单元**：messages 包的解析/替换 round-trip 测试（喂真实 Claude Code 请求体样例）
2. **集成**：起本地工具，用 curl 发一个带 `image_url`/base64 的 Anthropic 请求 → 断言上游收到的 body 里图片被替换成描述
3. **端到端**：`ANTHROPIC_BASE_URL=http://127.0.0.1:8790` 启动 Claude Code，粘贴截图问"描述这张图"，DeepSeek 能答出内容
4. **多轮**：连续两轮同一截图 → 断言第二轮缓存命中（日志显示 cache hit，无第二次视觉调用）
5. **fail-open**：故意给错 vision key → 请求仍能到达上游

## 11. 留给作者审的点（决策）

1. **视觉后端默认用 MiMo `mimo-v2.5`**？还是 GLM-4V / Qwen-VL？（MiMo 你有现成 key，最省事）
2. **缓存是否落盘**？纯内存 LRU 够用吗？（v1 建议纯内存）
3. **fail-open 语义**：视觉失败时"原样转发图片"（上游可能报错）vs"直接返回错误"？（我建议 fail-open）
4. **只支持 Claude Code（Anthropic 格式）**还是 v1 就兼容 OpenAI 格式？（建议 v1 只 Anthropic）
5. **模型能力判断**：配置里手动声明"这个上游是纯文本"（简单）vs 内置模型清单（像 CC Switch 那样）？（建议 v1 手动声明）
6. **二进制名/仓库名**：`vision-fallback`? `blind-llm-eyes`? 你定

---

*本文件与 `vision-fallback-notes.md` 配套。审完请批注 11 节决策点。*
