# Blind LLM Eyes — 项目实施计划（v1 MVP）

> **For agentic workers:** 步骤使用复选框 (`- [ ]`) 语法跟踪进度。

**Goal:** 在 1-3 天内交付可用的本地 Go 代理单二进制，架在 Claude Code 与纯文本上游（DeepSeek）之间，把请求中的图片块透明替换为 MiMo 生成的文字描述，实现「不切供应商、粘贴截图直接可用」。

**Architecture:** 五层包结构 (`config` / `messages` / `cache` / `vision` / `proxy`) + `main.go` 入口。请求路径：解析 Anthropic Messages → 扫描 image 块 → 内容哈希查 LRU 缓存 → 未命中调 MiMo → 原位替换图片为描述 → 转发上游；SSE 响应原样透传，零修改。跨请求唯一状态是纯函数式缓存，无会话状态避免历史劫持。

**Tech Stack:** Go 1.22+、标准库 `net/http`/`encoding/json`/`crypto/sha256`、`gopkg.in/yaml.v3`、`slog` 结构化日志、MiMo `mimo-v2.5`（OpenAI 兼容 `/v1/chat/completions` 视觉端点）。

***

## 1. 项目背景

### 1.1 痛点

用户主力模型是 **DeepSeek（纯文本，无视觉能力）**。日常使用 Claude Code 粘贴截图时，DeepSeek 无法理解图片内容，必须切换到有视觉能力的供应商（如 MiMo、GPT-4o），打断工作流。

### 1.2 机制先例与已知陷阱

| 先例                           | 结果    | 死因                             | 我们的规避                                    |
| ---------------------------- | ----- | ------------------------------ | ---------------------------------------- |
| CCR v2 ImageAgent（Node，36k★） | v3 弃用 | 跨请求会话状态导致行为被历史劫持（#872）         | **无会话状态**：只处理当前请求，缓存是纯 hash→描述映射，不参与行为决策 |
| LiteLLM 透传图                  | 重复扣费  | 客户端每轮重发完整历史（含图），每轮都调视觉（#35223） | **内容哈希 LRU**：同一张图多轮重发零视觉调用               |
| new-api #6780 功能请求           | 软拒绝   | 维护者哲学：「网关层不宜干预模型行为」            | **独立自持工具**：自己显式运行显式配置，无需说服任何人            |

### 1.3 为什么是 Go（对比 TS/Python 前人）

* **单静态二进制**：拷一个 `blind-llm-eyes.exe` 就能跑，无 npx/Node 运行时依赖

* **并发原生**：goroutine + `sync.Mutex` 写 LRU，无事件循环回调坑

* **SSE 透传强项**：`net/http` + `http.Flusher` + `io.Copy`，标准库搞定

* **类型安全**：ContentBlock 用 struct 建模（image/text/tool\_use/tool\_result），编译期报错

* **本地工具无感**：毫秒启动、几 MB 内存占用

***

## 2. 项目目标

### 2.1 MVP 目标（必须达成，1-3 天内）

1. **端到端可用**：`ANTHROPIC_BASE_URL=http://127.0.0.1:8790` 启动 Claude Code → 粘贴截图 → DeepSeek 正确描述图片内容
2. **多轮缓存命中**：连续两轮同一张截图 → 日志/响应头显示第二轮 `cache_hit`，无第二次 MiMo 调用
3. **fail-open 验证**：故意填错 MiMo key → 请求仍能到达 DeepSeek（图片原样转发，DeepSeek 自行报错或忽略）
4. **流式不卡**：SSE token 到达即 flush 给 Claude Code，不攒整包

### 2.2 v1 明确不做（YAGNI）

* ❌ tool\_result 块里嵌套的图片（Claude Code 传图走顶层 image 块，少见）

* ❌ OpenAI Chat Completions 格式兼容（v1 只 Anthropic Messages）

* ❌ TLS / 客户端鉴权 / 多供应商切换 UI

* ❌ 配置热重载

* ❌ 会话级语义去重（哈希级够了）

* ❌ 缓存落盘（v1 纯内存，重启丢失可接受）

### 2.3 成功度量（验收标准）

| 验收项       | 验证方式                                       | 通过标准                                                                   |
| --------- | ------------------------------------------ | ---------------------------------------------------------------------- |
| 图片替换      | curl 发 Anthropic 请求含 base64 图 → 抓包看上游 body | image 块消失，出现对应 text 描述块                                                |
| 缓存命中      | 连续两轮同图                                     | 日志：第 1 轮 `vision_call=1 cache_hit=0`；第 2 轮 `vision_call=0 cache_hit=1` |
| fail-open | 配置错 vision.api\_key 后发请求                   | 请求到达上游，不阻塞（上游对图的处理由它自己决定）                                              |
| 流式透传      | 发长请求看 Claude Code 侧打字效果                    | token 逐个出现，无明显卡顿（上游 flush 到代理 ≤ 200ms 内到客户端）                           |
| 改写结果头     | 任意响应看 headers                              | `X-Blind-Llm-Eyes: rewritten=N cached=M` 两个数字准确                        |

***

## 3. 主要阶段划分

```
Phase 0: 项目初始化  (≈ 0.5 天)
    │
    ▼
Phase 1: messages 包（解析 + 替换，TDD 先行）  (≈ 0.5 天)  ← 最核心，先搞对
    │
    ▼
Phase 2: cache 包 + vision 包  (≈ 0.5 天)
    │
    ▼
Phase 3: proxy 包（请求管线 + SSE 透传）  (≈ 0.5 天)
    │
    ▼
Phase 4: config + main.go 串起来  (≈ 0.3 天)
    │
    ▼
Phase 5: 集成 + 端到端验证  (≈ 0.5 天)
```

***

## 4. 关键任务清单（按阶段，bite-sized）

> **协作模式提醒**：每一步「写代码」是您手写；我负责**出单测样例、做代码审查、解释设计取舍、报坑提预案**。每完成一个 task 请把代码贴给我审，审过后再进下一个。

### Phase 0: 项目初始化

**Files:**

* Create: `go.mod`、`go.sum`、`main.go`（空骨架）、`config.example.yaml`

* Create: `.gitignore`

* [ ] **Step 0.1: git init + go mod init**

  ```bash
  cd D:\Code\new-api-contrib
  git init
  go mod init github.com/ROM4n2/blind-llm-eyes
  go get gopkg.in/yaml.v3
  ```

  预期：`go.mod` 出现，module 名正确

* [ ] **Step 0.2: 写** **`.gitignore`**

  ```
  # Binaries
  /blind-llm-eyes
  /blind-llm-eyes.exe

  # Config with secrets
  config.yaml
  config.*.yaml
  !config.example.yaml

  # Go workspace
  *.test
  *.out
  vendor/
  ```

* [ ] **Step 0.3: 写** **`config.example.yaml`（真实凭据用占位符）**

  ```yaml
  listen: "127.0.0.1:8790"
  upstream:
    base_url: "https://api.deepseek.com/anthropic"   # 纯文本上游 Anthropic 兼容端点
    api_key: "sk-deepseek-placeholder"                # Claude Code 发来的 Authorization 头会覆盖此字段（透传）
  vision:
    base_url: "https://api.xiaomimimo.com/v1"         # MiMo OpenAI 兼容端点
    api_key: "sk-mimo-placeholder"
    model: "mimo-v2.5"                                # 有视觉的那个（opus 等级）
    timeout: "30s"
    description_cap: 500                              # max_tokens，描述占用纯文本上游上下文，必须限长
  cache:
    max_entries: 500                                  # 纯内存 LRU 容量
  fail_open: true                                     # 视觉失败 → 原样转发，不阻塞主链路
  log_level: "info"                                   # debug / info / warn / error
  ```

* [ ] **Step 0.4: 空** **`main.go`** **骨架（能编译通过）**

  ```go
  package main

  import "fmt"

  func main() {
      fmt.Println("blind-llm-eyes: starting...")
      // TODO: load config, start server
  }
  ```

  运行：`go run .` → 预期打印字符串后退出，无编译错误

* [ ] **Step 0.5: Commit（P0 完成）**

  ```bash
  git add go.mod go.sum .gitignore config.example.yaml main.go
  git commit -m "chore: init blind-llm-eyes project skeleton"
  ```

***

### Phase 1: messages 包 — Anthropic Messages 解析 + 图片替换（核心中的核心，TDD 先行）

**Files:**

* Create: `messages/content.go`（ContentBlock struct 建模）

* Create: `messages/parse.go`（解析请求体 → 找出所有 image 块）

* Create: `messages/rewrite.go`（把 image 块原位替换成 text 描述块）

* Create: `messages/parse_test.go`、`messages/rewrite_test.go`

**设计约束（必须遵守）**：

* 只改请求体中 `content` 数组里 **顶层 user message 的 image 块**（v1 scope）

* 不碰 `tool_use` / `tool_result` 块里嵌套的图（v1 不做，别顺手加）

* 替换是**原位**的：替换后 `content` 数组长度不变，元素顺序不变，只把第 i 个元素从 `type:"image"` 换成 `type:"text"`

* `source.data` 的 base64 解码失败时：跳过该块，不报错（fail-open 精神，由上游自行处理）

* [ ] **Step 1.1: 先写** **`messages/content.go`（struct 定义）**
  对应 Anthropic Messages API 规范：

  ```go
  package messages

  // Request 是 /v1/messages 的请求体（Claude Code 发来的格式）
  type Request struct {
      Model     string    `json:"model"`
      Messages  []Message `json:"messages"`
      MaxTokens int       `json:"max_tokens"`
      System    string    `json:"system,omitempty"`
      Stream    bool      `json:"stream,omitempty"`
  }

  type Message struct {
      Role    string         `json:"role"` // "user" | "assistant"
      Content []ContentBlock `json:"content"`
  }

  type ContentBlock struct {
      Type   string       `json:"type"` // "text" | "image" | "tool_use" | "tool_result"
      Text   string       `json:"text,omitempty"`
      Source *ImageSource `json:"source,omitempty"` // image 块专用
  }

  type ImageSource struct {
      Type      string `json:"type"`      // "base64"
      MediaType string `json:"media_type"` // "image/png" | "image/jpeg" | "image/webp"
      Data      string `json:"data"`      // base64 字符串，不带 data: 前缀
  }
  ```

  运行：`go build ./messages` → 预期编译通过

* [ ] **Step 1.2: 写** **`messages/parse_test.go`（TDD 先写 failing test）**
  喂一个真实 Claude Code 请求体样例（含一张图 + 一段文字）：

  ```go
  package messages

  import (
      "encoding/json"
      "strings"
      "testing"
  )

  func TestFindImageBlocks_UserMessageOnly(t *testing.T) {
      body := `{
          "model": "deepseek-chat",
          "max_tokens": 1024,
          "messages": [
              {
                  "role": "user",
                  "content": [
                      {"type": "text", "text": "描述这张图"},
                      {
                          "type": "image",
                          "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}
                      }
                  ]
              }
          ],
          "stream": true
      }`

      var req Request
      if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
          t.Fatalf("decode: %v", err)
      }

      imgs := FindImageBlocks(&req)
      if len(imgs) != 1 {
          t.Fatalf("want 1 image, got %d", len(imgs))
      }
      want := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
      if imgs[0].Source.Data != want {
          t.Errorf("data mismatch")
      }
      if imgs[0].Source.MediaType != "image/png" {
          t.Errorf("media_type mismatch: %s", imgs[0].Source.MediaType)
      }
  }
  ```

  运行：`go test ./messages -run TestFindImageBlocks -v` → 预期 FAIL：`FindImageBlocks` 未定义

* [ ] **Step 1.3: 实现** **`messages/parse.go`** **让 test 变绿**

  ```go
  package messages

  // FindImageBlocks 返回请求体中所有顶层 user message 里的 image 块指针。
  // 返回的指针直接指向 req.Messages[i].Content[j]，修改 *block 即可原位替换。
  func FindImageBlocks(req *Request) []*ContentBlock {
      var out []*ContentBlock
      for i := range req.Messages {
          if req.Messages[i].Role != "user" {
              continue
          }
          for j := range req.Messages[i].Content {
              blk := &req.Messages[i].Content[j]
              if blk.Type == "image" && blk.Source != nil && blk.Source.Data != "" {
                  out = append(out, blk)
              }
          }
      }
      return out
  }
  ```

  运行：`go test ./messages -run TestFindImageBlocks -v` → 预期 PASS

* [ ] **Step 1.4: 写** **`messages/rewrite_test.go`（TDD 先 failing）**

  ```go
  package messages

  import (
      "encoding/json"
      "strings"
      "testing"
  )

  func TestReplaceImageWithDescription(t *testing.T) {
      body := `{
          "messages": [
              {"role": "user", "content": [
                  {"type": "text", "text": "描述这张图"},
                  {"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "abc"}}
              ]}
          ]
      }`
      var req Request
      json.NewDecoder(strings.NewReader(body)).Decode(&req)

      imgs := FindImageBlocks(&req)
      ReplaceImageWithDescription(imgs[0], "这是一张 1x1 红色 PNG 图片")

      // 断言：content 长度没变（原位替换）
      if len(req.Messages[0].Content) != 2 {
          t.Fatalf("content len changed: %d", len(req.Messages[0].Content))
      }
      // 断言：image 块变成了 text 块
      blk := req.Messages[0].Content[1]
      if blk.Type != "text" {
          t.Errorf("want type=text, got %s", blk.Type)
      }
      if blk.Text != "这是一张 1x1 红色 PNG 图片" {
          t.Errorf("text mismatch: %s", blk.Text)
      }
      // 断言：source 被清空（避免序列化时带冗余字段）
      if blk.Source != nil {
          t.Errorf("source should be nil after replace")
      }
  }
  ```

  运行：`go test ./messages -run TestReplaceImageWithDescription -v` → FAIL：`ReplaceImageWithDescription` 未定义

* [ ] **Step 1.5: 实现** **`messages/rewrite.go`** **变绿**

  ```go
  package messages

  // ReplaceImageWithDescription 把一个 image ContentBlock 原位替换为 text 描述块。
  // 调用方需保证 blk 来自 FindImageBlocks（即其 Type == "image" 且 Source != nil）。
  func ReplaceImageWithDescription(blk *ContentBlock, description string) {
      blk.Type = "text"
      blk.Text = "[Image Description: " + description + "]"
      blk.Source = nil
  }
  ```

  运行：`go test ./messages -v` → 两个 test 全 PASS

* [ ] **Step 1.6: 补一个「无图请求纯透传」的回归 test（重要，保证无图时 body 一字不动）**

  ```go
  func TestFindImageBlocks_NoImages(t *testing.T) {
      body := `{"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`
      var req Request
      json.NewDecoder(strings.NewReader(body)).Decode(&req)
      if len(FindImageBlocks(&req)) != 0 {
          t.Errorf("want 0, got %d", len(FindImageBlocks(&req)))
      }
  }
  ```

  运行：`go test ./messages -v` → 3/3 PASS

* [ ] **Step 1.7: Commit Phase 1**

  ```bash
  git add messages/
  git commit -m "feat(messages): parse Anthropic request and replace image blocks with descriptions"
  ```

***

### Phase 2: cache 包 + vision 包

**Files:**

* Create: `cache/hash.go`、`cache/lru.go`、`cache/lru_test.go`

* Create: `vision/client.go`、`vision/client_test.go`（mock server）

#### 2A: cache 包 — 内容哈希 + LRU

* [ ] **Step 2A.1: 写** **`cache/hash.go`**

  ```go
  package cache

  import (
      "crypto/sha256"
      "encoding/base64"
      "fmt"
  )

  // HashFromBase64Data 接收 base64 字符串（不带前缀），解码后 sha256，返回 URL-safe base64 前 16 字节作为 key。
  // 错误：解码失败返回空串 + error，调用方按 fail-open 处理（当缓存 miss 处理，调视觉时也会失败，然后原样转发）
  func HashFromBase64Data(base64Data string) (string, error) {
      raw, err := base64.StdEncoding.DecodeString(base64Data)
      if err != nil {
          return "", fmt.Errorf("decode base64: %w", err)
      }
      sum := sha256.Sum256(raw)
      // 取前 16 字节够了，碰撞概率可忽略
      return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(sum[:16]), nil
  }
  ```

* [ ] **Step 2A.2: 写** **`cache/lru_test.go`（TDD failing）**

  ```go
  package cache

  import "testing"

  func TestLRU_GetPut(t *testing.T) {
      c := NewLRU(2) // 容量 2
      c.Put("k1", "desc1")
      if v, ok := c.Get("k1"); !ok || v != "desc1" {
          t.Fatalf("k1 miss: ok=%v v=%q", ok, v)
      }

      // 塞满容量
      c.Put("k2", "desc2")
      c.Put("k3", "desc3") // 应该踢掉 k1

      if _, ok := c.Get("k1"); ok {
          t.Errorf("k1 should be evicted")
      }
      if v, ok := c.Get("k2"); !ok || v != "desc2" {
          t.Errorf("k2 wrong: ok=%v v=%q", ok, v)
      }
  }

  func TestLRU_GetPromotes(t *testing.T) {
      c := NewLRU(2)
      c.Put("a", "1")
      c.Put("b", "2")
      _, _ = c.Get("a") // 提升 a 为最近使用
      c.Put("c", "3")   // 应踢掉 b，不是 a
      if _, ok := c.Get("b"); ok {
          t.Errorf("b should be evicted")
      }
      if _, ok := c.Get("a"); !ok {
          t.Errorf("a should survive")
      }
  }
  ```

  运行：`go test ./cache -v` → FAIL：`NewLRU` 未定义

* [ ] **Step 2A.3: 实现** **`cache/lru.go`** **变绿**
  用 `container/list` + `sync.Mutex`（标准库 LRU，无第三方依赖）：

  ```go
  package cache

  import (
      "container/list"
      "sync"
  )

  // LRU 是线程安全的 hash→描述 缓存。零值不可用，用 NewLRU。
  type LRU struct {
      mu       sync.Mutex
      cap      int
      ll       *list.List                  // 最近用的在 front
      items    map[string]*list.Element
  }

  type entry struct {
      key   string
      value string // description
  }

  func NewLRU(capacity int) *LRU {
      return &LRU{
          cap:   capacity,
          ll:    list.New(),
          items: make(map[string]*list.Element),
      }
  }

  func (c *LRU) Get(key string) (string, bool) {
      c.mu.Lock()
      defer c.mu.Unlock()
      if e, ok := c.items[key]; ok {
          c.ll.MoveToFront(e)
          return e.Value.(*entry).value, true
      }
      return "", false
  }

  func (c *LRU) Put(key, value string) {
      c.mu.Lock()
      defer c.mu.Unlock()
      if e, ok := c.items[key]; ok {
          c.ll.MoveToFront(e)
          e.Value.(*entry).value = value
          return
      }
      e := c.ll.PushFront(&entry{key: key, value: value})
      c.items[key] = e
      for c.ll.Len() > c.cap {
          c.removeOldest()
      }
  }

  // removeOldest 必须在锁内调用
  func (c *LRU) removeOldest() {
      e := c.ll.Back()
      if e == nil {
          return
      }
      c.ll.Remove(e)
      ent := e.Value.(*entry)
      delete(c.items, ent.key)
  }
  ```

  运行：`go test ./cache -v` → 2/2 PASS

* [ ] **Step 2A.4: Commit cache 包**

  ```bash
  git add cache/
  git commit -m "feat(cache): content-hash sha256 key + thread-safe LRU eviction"
  ```

#### 2B: vision 包 — MiMo 视觉后端客户端

* [ ] **Step 2B.1: 写** **`vision/client.go`**
  调 OpenAI 兼容 `/v1/chat/completions`，发单图消息，提取 `choices[0].message.content`（string，v1 不处理 content array 响应）：

  ```go
  package vision

  import (
      "bytes"
      "context"
      "encoding/json"
      "fmt"
      "io"
      "net/http"
      "time"
  )

  // Client 调视觉后端（OpenAI 兼容 /v1/chat/completions）。
  type Client struct {
      BaseURL        string
      APIKey         string
      Model          string
      DescriptionCap int            // max_tokens
      Timeout        time.Duration
      HTTPClient     *http.Client
  }

  // DescribeImage 把一张 base64 图片变成文字描述。
  // 返回值：描述字符串；出错时返回空串 + error（调用方按 fail-open 决定是否原样转发）
  func (c *Client) DescribeImage(ctx context.Context, base64Data, mediaType string) (string, error) {
      if c.BaseURL == "" || c.APIKey == "" || c.Model == "" {
          return "", fmt.Errorf("vision client not configured")
      }

      // 固定 prompt（与用户请求无关，保证缓存可复用）
      systemPrompt := "You are a visual description assistant. Describe the provided image in detail in Chinese. Focus on objects, layout, colors, text visible in the image, and any code snippets or UI elements. Keep the description under 400 words but be precise."

      reqBody := map[string]any{
          "model": c.Model,
          "messages": []map[string]any{
              {
                  "role":    "system",
                  "content": systemPrompt,
              },
              {
                  "role": "user",
                  "content": []map[string]any{
                      {
                          "type": "image_url",
                          "image_url": map[string]string{
                              "url": fmt.Sprintf("data:%s;base64,%s", mediaType, base64Data),
                          },
                      },
                  },
              },
          },
          "max_tokens":  c.DescriptionCap,
          "temperature": 0.0,
      }

      bodyBytes, err := json.Marshal(reqBody)
      if err != nil {
          return "", fmt.Errorf("marshal vision req: %w", err)
      }

      httpClient := c.HTTPClient
      if httpClient == nil {
          httpClient = &http.Client{Timeout: c.Timeout}
      }

      req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(bodyBytes))
      if err != nil {
          return "", fmt.Errorf("build vision req: %w", err)
      }
      req.Header.Set("Content-Type", "application/json")
      req.Header.Set("Authorization", "Bearer "+c.APIKey)

      resp, err := httpClient.Do(req)
      if err != nil {
          return "", fmt.Errorf("vision http do: %w", err)
      }
      defer resp.Body.Close()

      respBytes, err := io.ReadAll(resp.Body)
      if err != nil {
          return "", fmt.Errorf("read vision resp: %w", err)
      }
      if resp.StatusCode >= 400 {
          return "", fmt.Errorf("vision resp %d: %s", resp.StatusCode, truncate(string(respBytes), 500))
      }

      var parsed struct {
          Choices []struct {
              Message struct {
                  Content string `json:"content"`
              } `json:"message"`
          } `json:"choices"`
      }
      if err := json.Unmarshal(respBytes, &parsed); err != nil {
          return "", fmt.Errorf("unmarshal vision resp: %w (raw=%s)", err, truncate(string(respBytes), 300))
      }
      if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
          return "", fmt.Errorf("vision resp empty choices")
      }
      return parsed.Choices[0].Message.Content, nil
  }

  func truncate(s string, n int) string {
      if len(s) <= n {
          return s
      }
      return s[:n] + "..."
  }
  ```

* [ ] **Step 2B.2: 写** **`vision/client_test.go`（用** **`net/http/httptest`** **起 mock server，不打真实 MiMo）**

  ```go
  package vision

  import (
      "context"
      "encoding/json"
      "net/http"
      "net/http/httptest"
      "testing"
      "time"
  )

  func TestDescribeImage_OK(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if r.Header.Get("Authorization") != "Bearer test-key" {
              t.Errorf("auth header missing: %s", r.Header.Get("Authorization"))
              w.WriteHeader(401)
              return
          }
          var req map[string]any
          json.NewDecoder(r.Body).Decode(&req)
          if req["model"] != "mimo-v2.5" {
              t.Errorf("model wrong: %v", req["model"])
          }
          json.NewEncoder(w).Encode(map[string]any{
              "choices": []map[string]any{
                  {"message": map[string]any{"content": "一张蓝色背景的测试图片，左上角有 Logo"}},
              },
          })
      }))
      defer srv.Close()

      c := &Client{
          BaseURL:        srv.URL,
          APIKey:         "test-key",
          Model:          "mimo-v2.5",
          DescriptionCap: 300,
          Timeout:        5 * time.Second,
      }

      desc, err := c.DescribeImage(context.Background(), "iVBORw0K...", "image/png")
      if err != nil {
          t.Fatalf("err: %v", err)
      }
      if desc != "一张蓝色背景的测试图片，左上角有 Logo" {
          t.Errorf("desc mismatch: %q", desc)
      }
  }

  func TestDescribeImage_500Fail(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          w.WriteHeader(500)
          w.Write([]byte("internal server error"))
      }))
      defer srv.Close()

      c := &Client{BaseURL: srv.URL, APIKey: "x", Model: "m", DescriptionCap: 100, Timeout: 2 * time.Second}
      _, err := c.DescribeImage(context.Background(), "abc", "image/png")
      if err == nil {
          t.Errorf("expected error on 500")
      }
  }
  ```

  运行：`go test ./vision -v` → 2/2 PASS

* [ ] **Step 2B.3: Commit vision 包**

  ```bash
  git add vision/
  git commit -m "feat(vision): MiMo OpenAI-compatible client with mock tests"
  ```

***

### Phase 3: proxy 包 — 请求管线 + SSE 流式透传

**Files:**

* Create: `proxy/handler.go`（单请求管线 orchestrator）

* Create: `proxy/passthrough.go`（SSE 透传）

* Create: `proxy/handler_test.go`（用 httptest 模拟下游）

**设计约束**：

* **只改请求体，不改响应体**（SSE 字节流原样转发，连空白行都不增删）

* 响应头加 `X-Blind-Llm-Eyes: rewritten=N cached=M`（调试观察点）

* `Authorization` 头**原样透传**（用户发来的 key 直接给上游用，工具自己不持有上游 key 也不落盘）

* fail-open：`vision.DescribeImage` 报错 → 该 image 块**不替换**，保持原样转发（让上游自己决定怎么办）

* [ ] **Step 3.1: 写** **`proxy/passthrough.go`（SSE 流式转发）**

  ```go
  package proxy

  import (
      "io"
      "net/http"
  )

  // CopyResponse 把上游响应（含 headers + status code + body）原样写给 w。
  // SSE 场景下：body 每读到一些就 Flush 一次，保证客户端及时拿到 token。
  func CopyResponse(w http.ResponseWriter, resp *http.Response) error {
      // 先拷贝 headers（注意：Header() map 是引用，得逐个设）
      dst := w.Header()
      for k, vs := range resp.Header {
          for _, v := range vs {
              dst.Add(k, v)
          }
      }
      w.WriteHeader(resp.StatusCode)

      flusher, canFlush := w.(http.Flusher)

      buf := make([]byte, 8*1024) // 8KB 缓冲
      for {
          n, rerr := resp.Body.Read(buf)
          if n > 0 {
              if _, werr := w.Write(buf[:n]); werr != nil {
                  return werr
              }
              if canFlush {
                  flusher.Flush()
              }
          }
          if rerr == io.EOF {
              break
          }
          if rerr != nil {
              return rerr
          }
      }
      return nil
  }
  ```

* [ ] **Step 3.2: 写** **`proxy/handler.go`（核心管线）**

  ```go
  package proxy

  import (
      "bytes"
      "encoding/json"
      "io"
      "log/slog"
      "net/http"
      "sync/atomic"

      "github.com/ROM4n2/blind-llm-eyes/cache"
      "github.com/ROM4n2/blind-llm-eyes/messages"
      "github.com/ROM4n2/blind-llm-eyes/vision"
  )

  // HandlerDeps 是 Handler 的依赖（用 struct 注入，方便测试替换 mock）。
  type HandlerDeps struct {
      UpstreamBaseURL string
      VisionClient    *vision.Client
      Cache           *cache.LRU
      FailOpen        bool
      Log             *slog.Logger
  }

  // NewHandler 返回一个标准 http.Handler，处理 /v1/messages 所有请求。
  func NewHandler(deps HandlerDeps) http.Handler {
      mux := http.NewServeMux()
      h := &requestHandler{deps: deps}
      mux.HandleFunc("/v1/messages", h.handleMessages)
      return mux
  }

  type requestHandler struct {
      deps HandlerDeps
  }

  func (h *requestHandler) handleMessages(w http.ResponseWriter, r *http.Request) {
      if r.Method != http.MethodPost {
          http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
          return
      }

      // 1) 读原始 body（读完要还，后面转发也需要）
      rawBody, err := io.ReadAll(r.Body)
      if err != nil {
          http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
          return
      }

      // 2) 解析 JSON
      var req messages.Request
      parseErr := json.Unmarshal(rawBody, &req)

      var rewritten atomic.Int64
      var cached atomic.Int64

      if parseErr == nil {
          // 3) 找图
          imgs := messages.FindImageBlocks(&req)

          // 4) 逐个图：查缓存 → 未命中调视觉
          for _, blk := range imgs {
              hash, herr := cache.HashFromBase64Data(blk.Source.Data)
              if herr == nil {
                  if desc, ok := h.deps.Cache.Get(hash); ok {
                      messages.ReplaceImageWithDescription(blk, desc)
                      rewritten.Add(1)
                      cached.Add(1)
                      continue
                  }
              }

              // 缓存 miss / hash 失败 → 调视觉
              desc, verr := h.deps.VisionClient.DescribeImage(r.Context(), blk.Source.Data, blk.Source.MediaType)
              if verr != nil {
                  // 视觉失败：fail-open 则不替换，跳过此图
                  if !h.deps.FailOpen {
                      http.Error(w, "vision call failed: "+verr.Error(), http.StatusBadGateway)
                      return
                  }
                  h.deps.Log.Warn("vision call failed, fail-open keeping original image", "err", verr)
                  continue
              }

              // 成功：写缓存 + 替换
              if herr == nil {
                  h.deps.Cache.Put(hash, desc)
              }
              messages.ReplaceImageWithDescription(blk, desc)
              rewritten.Add(1)
          }

          // 5) 重新序列化请求体（如果有替换或解析成功就重编；否则用原始 rawBody）
          if rewritten.Load() > 0 {
              newBody, merr := json.Marshal(&req)
              if merr != nil {
                  h.deps.Log.Error("re-marshal request failed", "err", merr)
                  // 重编失败用原始 body（fail-open 精神）
                  rawBody = rawBody
              } else {
                  rawBody = newBody
              }
          }
      } else {
          // 解析失败：不处理，原样转发（fail-open 精神）
          h.deps.Log.Warn("parse anthropic request failed, passthrough raw", "err", parseErr)
      }

      // 6) 转发给上游
      upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.deps.UpstreamBaseURL+"/v1/messages", bytes.NewReader(rawBody))
      if err != nil {
          http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
          return
      }

      // Header 透传：除 Host/Content-Length 外全拷贝（Authorization 最关键）
      for k, vs := range r.Header {
          if k == "Host" {
              continue
          }
          for _, v := range vs {
              upstreamReq.Header.Add(k, v)
          }
      }
      upstreamReq.ContentLength = int64(len(rawBody))

      upstreamResp, err := http.DefaultClient.Do(upstreamReq)
      if err != nil {
          http.Error(w, "upstream do: "+err.Error(), http.StatusBadGateway)
          return
      }
      defer upstreamResp.Body.Close()

      // 7) 加改写结果头（调试观察点）
      upstreamResp.Header.Set("X-Blind-Llm-Eyes",
          formatCountHeader(rewritten.Load(), cached.Load()))

      // 8) SSE 原样透传
      if err := CopyResponse(w, upstreamResp); err != nil {
          h.deps.Log.Error("copy response", "err", err)
      }
  }

  func formatCountHeader(rewritten, cached int64) string {
      return formatInt(rewritten) + " rewritten, " + formatInt(cached) + " cached"
  }

  func formatInt(n int64) string {
      // 手搓避免 strconv 依赖增加（其实 strconv 也行，随手写的）
      if n == 0 {
          return "0"
      }
      neg := n < 0
      if neg {
          n = -n
      }
      var buf [20]byte
      i := len(buf)
      for n > 0 {
          i--
          buf[i] = byte('0' + n%10)
          n /= 10
      }
      if neg {
          i--
          buf[i] = '-'
      }
      return string(buf[i:])
  }
  ```

* [ ] **Step 3.3: 写** **`proxy/handler_test.go`**（用 mock 上游 + mock vision，断言改写数 + 缓存命中数）

  ```go
  package proxy

  import (
      "bytes"
      "context"
      "encoding/json"
      "log/slog"
      "net/http"
      "net/http/httptest"
      "os"
      "strings"
      "testing"
      "time"

      "github.com/ROM4n2/blind-llm-eyes/cache"
      "github.com/ROM4n2/blind-llm-eyes/vision"
  )

  // 假上游：收到请求后把 body 原样回显（方便我们断言图片被替换了没有）
  func fakeUpstream(t *testing.T, gotBody *[]byte) *httptest.Server {
      return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          b, _ := io.ReadAll(r.Body)
          *gotBody = b
          w.Header().Set("Content-Type", "text/event-stream")
          w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
          w.Write([]byte("data: [DONE]\n\n"))
      }))
  }

  func TestHandler_ImageReplaceAndCache(t *testing.T) {
      var upstreamGot []byte
      up := fakeUpstream(t, &upstreamGot)
      defer up.Close()

      // mock vision：固定返回 "MockDesc-A"
      visionCalls := 0
      vis := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          visionCalls++
          json.NewEncoder(w).Encode(map[string]any{
              "choices": []map[string]any{{"message": map[string]any{"content": "MockDesc-A"}}},
          })
      }))
      defer vis.Close()

      deps := HandlerDeps{
          UpstreamBaseURL: strings.TrimSuffix(up.URL, "/"),
          VisionClient: &vision.Client{
              BaseURL:        strings.TrimSuffix(vis.URL, "/"),
              APIKey:         "x",
              Model:          "mimo-v2.5",
              DescriptionCap: 300,
              Timeout:        5 * time.Second,
          },
          Cache:    cache.NewLRU(10),
          FailOpen: true,
          Log:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
      }
      h := NewHandler(deps)

      reqBody := `{"model":"deepseek-chat","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}}]}],"stream":true}`

      // ==== 第 1 次请求：缓存 miss，应该调 vision ====
      rr1 := httptest.NewRecorder()
      req1, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
      req1.Header.Set("Content-Type", "application/json")
      req1.Header.Set("Authorization", "Bearer sk-test-upstream")
      h.ServeHTTP(rr1, req1)

      if rr1.Code != 200 {
          t.Fatalf("1st status: %d body=%s", rr1.Code, rr1.Body.String())
      }
      if visionCalls != 1 {
          t.Errorf("1st vision calls: want 1, got %d", visionCalls)
      }
      if hdr := rr1.Header().Get("X-Blind-Llm-Eyes"); !strings.Contains(hdr, "1 rewritten") {
          t.Errorf("1st header wrong: %q", hdr)
      }
      // 断言上游收到的 body 里：image 块没了，出现 MockDesc-A
      var upstreamReq map[string]any
      json.Unmarshal(upstreamGot, &upstreamReq)
      msgs := upstreamReq["messages"].([]any)
      firstMsg := msgs[0].(map[string]any)
      content := firstMsg["content"].([]any)
      secondBlock := content[1].(map[string]any)
      if secondBlock["type"] != "text" {
          t.Errorf("1st upstream: 2nd block not text, type=%v", secondBlock["type"])
      }
      if !strings.Contains(secondBlock["text"].(string), "MockDesc-A") {
          t.Errorf("1st upstream: desc missing in text=%v", secondBlock["text"])
      }

      // ==== 第 2 次请求：同一图，缓存 hit，visionCalls 不应增加 ====
      rr2 := httptest.NewRecorder()
      req2, _ := http.NewRequest("POST", "/v1/messages", bytes.NewBufferString(reqBody))
      req2.Header.Set("Content-Type", "application/json")
      h.ServeHTTP(rr2, req2)

      if visionCalls != 1 {
          t.Errorf("2nd vision calls: still want 1, got %d (cache not hit!)", visionCalls)
      }
      hdr := rr2.Header().Get("X-Blind-Llm-Eyes")
      if !strings.Contains(hdr, "1 cached") {
          t.Errorf("2nd header want 1 cached, got %q", hdr)
      }
  }
  ```

  运行：`go test ./proxy -v` → PASS

* [ ] **Step 3.4: Commit proxy 包**

  ```bash
  git add proxy/
  git commit -m "feat(proxy): request pipeline (parse→cache→vision→replace→forward) + SSE passthrough"
  ```

***

### Phase 4: config 包 + main.go 串起来

**Files:**

* Create: `config/loader.go`

* Modify: `main.go`（写真实逻辑）

* [ ] **Step 4.1: 写** **`config/loader.go`（YAML + env 覆盖）**

  ```go
  package config

  import (
      "fmt"
      "os"
      "time"

      "gopkg.in/yaml.v3"
  )

  type Config struct {
      Listen   string      `yaml:"listen"`
      Upstream UpstreamCfg `yaml:"upstream"`
      Vision   VisionCfg   `yaml:"vision"`
      Cache    CacheCfg    `yaml:"cache"`
      FailOpen bool        `yaml:"fail_open"`
      LogLevel string      `yaml:"log_level"` // debug|info|warn|error
  }

  type UpstreamCfg struct {
      BaseURL string `yaml:"base_url"`
  }

  type VisionCfg struct {
      BaseURL        string `yaml:"base_url"`
      APIKey         string `yaml:"api_key"`
      Model          string `yaml:"model"`
      TimeoutStr     string `yaml:"timeout"`
      Timeout        time.Duration
      DescriptionCap int    `yaml:"description_cap"`
  }

  type CacheCfg struct {
      MaxEntries int `yaml:"max_entries"`
  }

  // Load 从路径加载 YAML；env 覆盖对应字段（BLIND_ 前缀）。
  func Load(path string) (*Config, error) {
      raw, err := os.ReadFile(path)
      if err != nil {
          return nil, fmt.Errorf("read config: %w", err)
      }
      var c Config
      if err := yaml.Unmarshal(raw, &c); err != nil {
          return nil, fmt.Errorf("parse yaml: %w", err)
      }
      // 默认值
      if c.Listen == "" {
          c.Listen = "127.0.0.1:8790"
      }
      if c.Vision.TimeoutStr == "" {
          c.Vision.TimeoutStr = "30s"
      }
      d, err := time.ParseDuration(c.Vision.TimeoutStr)
      if err != nil {
          return nil, fmt.Errorf("vision.timeout: %w", err)
      }
      c.Vision.Timeout = d
      if c.Vision.DescriptionCap <= 0 {
          c.Vision.DescriptionCap = 500
      }
      if c.Cache.MaxEntries <= 0 {
          c.Cache.MaxEntries = 500
      }
      if c.LogLevel == "" {
          c.LogLevel = "info"
      }
      // env 覆盖（BLIND_ 前缀）
      if v := os.Getenv("BLIND_LISTEN"); v != "" {
          c.Listen = v
      }
      if v := os.Getenv("BLIND_VISION_API_KEY"); v != "" {
          c.Vision.APIKey = v
      }
      if v := os.Getenv("BLIND_UPSTREAM_BASE_URL"); v != "" {
          c.Upstream.BaseURL = v
      }
      if c.Upstream.BaseURL == "" || c.Vision.BaseURL == "" {
          return nil, fmt.Errorf("upstream.base_url and vision.base_url are required")
      }
      return &c, nil
  }
  ```

* [ ] **Step 4.2: 重写** **`main.go`** **把所有东西串起来**

  ```go
  package main

  import (
      "context"
      "flag"
      "fmt"
      "log/slog"
      "net/http"
      "os"
      "os/signal"
      "strings"
      "syscall"
      "time"

      "github.com/ROM4n2/blind-llm-eyes/cache"
      "github.com/ROM4n2/blind-llm-eyes/config"
      "github.com/ROM4n2/blind-llm-eyes/proxy"
      "github.com/ROM4n2/blind-llm-eyes/vision"
  )

  func main() {
      configPath := flag.String("config", "config.yaml", "path to config yaml")
      flag.Parse()

      cfg, err := config.Load(*configPath)
      if err != nil {
          fmt.Fprintf(os.Stderr, "load config: %v\n", err)
          os.Exit(1)
      }

      logger := buildLogger(cfg.LogLevel)
      logger.Info("blind-llm-eyes starting",
          "listen", cfg.Listen,
          "upstream", cfg.Upstream.BaseURL,
          "vision_model", cfg.Vision.Model,
          "fail_open", cfg.FailOpen,
          "cache_max", cfg.Cache.MaxEntries,
      )

      deps := proxy.HandlerDeps{
          UpstreamBaseURL: strings.TrimRight(cfg.Upstream.BaseURL, "/"),
          VisionClient: &vision.Client{
              BaseURL:        strings.TrimRight(cfg.Vision.BaseURL, "/"),
              APIKey:         cfg.Vision.APIKey,
              Model:          cfg.Vision.Model,
              DescriptionCap: cfg.Vision.DescriptionCap,
              Timeout:        cfg.Vision.Timeout,
          },
          Cache:    cache.NewLRU(cfg.Cache.MaxEntries),
          FailOpen: cfg.FailOpen,
          Log:      logger,
      }

      srv := &http.Server{
          Addr:         cfg.Listen,
          Handler:      proxy.NewHandler(deps),
          ReadTimeout:  60 * time.Second,
          WriteTimeout: 10 * time.Minute, // 长 SSE 响应
      }

      // 优雅关闭
      errCh := make(chan error, 1)
      go func() {
          logger.Info("listening", "addr", cfg.Listen)
          if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
              errCh <- err
          }
      }()

      sigCh := make(chan os.Signal, 1)
      signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

      select {
      case err := <-errCh:
          logger.Error("server failed", "err", err)
          os.Exit(1)
      case sig := <-sigCh:
          logger.Info("shutting down", "signal", sig)
          ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
          defer cancel()
          if err := srv.Shutdown(ctx); err != nil {
              logger.Error("shutdown error", "err", err)
          }
      }
  }

  func buildLogger(level string) *slog.Logger {
      var lvl slog.Level
      switch strings.ToLower(level) {
      case "debug":
          lvl = slog.LevelDebug
      case "warn":
          lvl = slog.LevelWarn
      case "error":
          lvl = slog.LevelError
      default:
          lvl = slog.LevelInfo
      }
      return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
  }
  ```

* [ ] **Step 4.3: 编译验证 + 空跑验证**

  ```bash
  # 先 copy config.example.yaml → config.yaml，填真实 key（不在仓库里）
  go build -o blind-llm-eyes.exe .
  # 空跑（不启动 Claude Code）：先看报啥错（应该是 config.yaml 不存在，或字段缺失）
  ./blind-llm-eyes.exe -config config.example.yaml
  ```

  预期：如果 `config.example.yaml` 里 `upstream.base_url` 填了占位 URL 但 vision.api\_key 是 placeholder，程序会正常启动并打印 `listening addr=127.0.0.1:8790`（因为 vision key 检查是延迟到真实调用时才做的）。

* [ ] **Step 4.4: Commit Phase 4**

  ```bash
  git add config/ main.go
  git commit -m "feat(main): config loader + wire up all packages; graceful shutdown"
  ```

***

### Phase 5: 集成 + 端到端验证

* [ ] **Step 5.1: curl 集成测试（单测级手动验证，不需要 Claude Code）**

  1. 准备：一份真实 base64 小图（比如 1x1 红色 PNG）
  2. 起工具：`./blind-llm-eyes.exe -config config.yaml`（config.yaml 填了真实 key）
  3. 另起终端 curl：

     ```bash
     curl -N http://127.0.0.1:8790/v1/messages \
       -H "Authorization: Bearer <你的 DeepSeek key>" \
       -H "Content-Type: application/json" \
       -d '{
         "model": "deepseek-chat",
         "max_tokens": 500,
         "stream": true,
         "messages": [{
           "role": "user",
           "content": [
             {"type":"text","text":"这张图里有什么？用一句话回答。"},
             {"type":"image","source":{"type":"base64","media_type":"image/png","data":"<真实base64>"}}
           ]
         }]
       }' -D /tmp/headers.txt
     ```
  4. 看 `/tmp/headers.txt`：应有 `X-Blind-Llm-Eyes: 1 rewritten, 0 cached`
  5. 看工具日志：有 `vision_call` 记录
  6. **再 curl 一次同样的 body**：

     * headers 里应为 `X-Blind-Llm-Eyes: 1 rewritten, 1 cached`

     * 工具日志：**没有**第二次 vision\_call

* [ ] **Step 5.2: fail-open 验证**

  1. 改 config.yaml 的 `vision.api_key` 为 `sk-fake-fake`
  2. 重启工具，curl 同样的请求
  3. 预期：

     * 工具日志打印 `vision call failed, fail-open keeping original image` 带具体错误

     * `X-Blind-Llm-Eyes: 0 rewritten, 0 cached`

     * 请求到达 DeepSeek，DeepSeek 对无法识别 image 块自行处理（可能报错也可能忽略）——**但请求没卡在工具这层**

* [ ] **Step 5.3: 端到端（Claude Code 实机）**

  1. 打开 Claude Code → Settings → 把当前供应商（DeepSeek）的 `ANTHROPIC_BASE_URL` 改成 `http://127.0.0.1:8790`

     * （CC Switch 的话：在对应供应商的 env override 里加 `ANTHROPIC_BASE_URL=http://127.0.0.1:8790`）
  2. 起本地工具：`./blind-llm-eyes.exe -config config.yaml`
  3. 在 Claude Code 里粘贴一张**代码截图**（最贴近日常使用场景），问：「这张截图里的代码在做什么？有 bug 吗？」
  4. 预期：

     * DeepSeek 能输出图片内容相关的回答（不是瞎编，是真的能描述代码结构）

     * 工具日志显示 `rewritten=1 cached=0`

     * 紧接着（同一轮里不用重发）再问一句「把刚才那段代码重写得更简洁」——**不用再调视觉**，这一轮图片没出现在新请求里（Claude Code 只有多轮跨请求才重发历史）
  5. 新会话再次粘贴**同一张**截图 → 预期 `cached=1`，日志无 vision\_call

* [ ] **Step 5.4: 无图请求纯透传（保证没破坏正常对话）**

  1. Claude Code 里纯文字问：「写个 Go 快排」
  2. 预期：正常回答，不调视觉，`X-Blind-Llm-Eyes: 0 rewritten, 0 cached`
  3. 对比不走代理时的回答：**完全一致**（字可能因采样不同略有差别，但长度、质量不应下降）

* [ ] **Step 5.5: 交叉编译（可选，MVP 完成后做）**

  ```bash
  # Windows → macOS arm64
  GOOS=darwin GOARCH=arm64 go build -o blind-llm-eyes-darwin-arm64 .
  # Windows → linux amd64
  GOOS=linux GOARCH=amd64 go build -o blind-llm-eyes-linux-amd64 .
  ```

***

## 5. 时间节点预估（MVP 速通：1-3 天）

| 阶段                 | 理想（快的一天）          | 正常（2 天）     | 保守（3 天，含踩坑） | 关键路径依赖            |
| ------------------ | ----------------- | ----------- | ----------- | ----------------- |
| P0 初始化             | 30 分钟             | 30 分钟       | 1 小时        | 无                 |
| P1 messages（TDD）   | 2 小时              | 3 小时        | 半天          | 无，先做最核心           |
| P2 cache + vision  | 1.5 小时            | 2.5 小时      | 半天          | P1 struct 定义好     |
| P3 proxy（管线 + SSE） | 2 小时              | 半天          | 半天          | P1 + P2 完成        |
| P4 config + main   | 1 小时              | 1.5 小时      | 2 小时        | P1-P3             |
| P5 集成验证            | 2 小时              | 半天          | 半天          | P4 编译通过           |
| **合计**             | **\~11h（1 天能干完）** | **\~1.5 天** | **\~3 天**   | P1→P2→P3→P4→P5 串行 |

> **每天的节奏建议**：
>
> * 上午：TDD 写包 + 单测
>
> * 下午：串 proxy + 集成验证
>
> * 晚上：实机端到端（如果白天没搞定）

***

## 6. 资源需求

### 6.1 人员

| 角色              | 人                | 职责                         |
| --------------- | ---------------- | -------------------------- |
| 开发者（主笔）         | 您（用户）            | 手写所有 Go 代码                 |
| 教练 / 审查者（Agent） | 我（Trae / Claude） | 出单测样例、代码审查、解释设计取舍、报坑、出验证脚本 |
| 端到端测试者          | 您                | 在 Claude Code 里粘贴截图实机验证    |

### 6.2 计算资源

* **开发机**：您当前 Windows 机器即可，Go 开发环境已 OK

* **内存**：工具运行期预计 < 50MB（LRU 500 条描述 × \~2KB 每条 ≈ 1MB，加上 Go runtime 和 goroutine 栈，撑死 50MB）

* **磁盘**：单二进制 \~10MB（不 strip 的话），strip 后 \~6MB

### 6.3 外部服务 / API

| 服务                          | 用途           | 凭据来源                                              |
| --------------------------- | ------------ | ------------------------------------------------- |
| **MiMo** **`mimo-v2.5`**    | 生成图片描述（视觉后端） | CC Switch 的小米供应商配置里的 key（opus 等级 = mimo-v2.5 有视觉） |
| **DeepSeek Anthropic 兼容端点** | 纯文本上游（目标模型）  | `ANTHROPIC_BASE_URL` 覆写后的转发目标                     |

### 6.4 预算预估（个人自用，无硬预算）

| 项目                                 | 单次成本估算                           | 日均估算（中度使用）          | 月估算          |
| ---------------------------------- | -------------------------------- | ------------------- | ------------ |
| MiMo 视觉调用（mimo-v2.5，假设 ≈ ¥0.01/张图） | ¥0.01/张                          | ¥0.1-0.5（10-50 张新图） | ¥3-15        |
| LRU 缓存命中节省                         | ≈ 70-90% 图是重复的（多轮重发占大头）          | 净省 ¥0.3-5/天         | 净省 ¥10-150/月 |
| DeepSeek 文本部分（描述占上下文）              | 描述约 300-500 token ≈ ¥0.001-0.003 | 可忽略                 | 可忽略          |

> **结论**：月度成本约 **几元钱到十几元**，完全在个人可接受范围内。

***

## 7. 潜在风险分析与应对

| 风险                                                                                                    | 概率     | 影响                                 | 触发条件                                    | 应对预案                                                                                                                                                                                      |
| ----------------------------------------------------------------------------------------------------- | ------ | ---------------------------------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **R1: MiMo 端点格式和标准 OpenAI 视觉不完全一致（文档说兼容但有偏差）**                                                        | 中（30%） | 高：调不通视觉，P2 vision 包要改              | 发第一次真实请求时 400 / 空响应 / content 不是 string | 1) 先拿 MiMo playground 的 curl 样例对字段名；2) `vision.Client` 的请求体改成用 struct 而非 `map[string]any`，方便逐字段调；3) 最坏情况换 `gpt-4o-mini` 做视觉后端（接口完全确定）                                                     |
| **R2: DeepSeek 的 Anthropic 兼容端点对「图片块变文字块后的 content 数组」有校验问题**                                         | 低（10%） | 中：转发后上游报 400                       | 集成测试第一次发请求时                             | 1) 先 curl 直接打 DeepSeek 端点发「替换后的 body」看能不能过；2) 若不过：把描述拼进前一个 text 块的末尾，content 数组长度减 1（即把 `[text, image→text]` 合并成单个长 text 块）                                                               |
| **R3: Claude Code 实际发送的 image 块结构与假设不一致（比如用** **`image_url`** **而非** **`source`** **+** **`base64`）** | 中（25%） | 高：FindImageBlocks 一张都找不到           | 端到端第一次实际粘贴截图时日志 0 rewritten             | 1) 先在 proxy.Handler 里加一条 debug 日志：`log.Debug("raw body head", "head", truncate(string(rawBody[:min(2000,len(rawBody))])))` 把真实请求体前 2KB 打出来看；2) messages.ContentBlock 按实际字段加 JSON tag 兼容分支 |
| **R4: LRU 并发 bug（死锁 / 数据竞争）**                                                                         | 低（15%） | 中：偶发卡 / 偶发 panic                   | 高并发多轮对话时                                | 1) Phase 2 写完 LRU 立刻跑 `go test ./cache -race`（加 `-race` 开数据竞争检测）；2) 出 bug 直接切 `sync.Map` + 单独写一个简单计数器做容量（精度差点但不死锁，MVP 够用）                                                                 |
| **R5: SSE 透传时，上游** **`Transfer-Encoding: chunked`** **+ Flush 不及时导致 Claude Code 觉得卡住**                | 中（30%） | 中：体感慢，打字一卡一卡的                      | 端到端长回答时                                 | 1) 加 `w.(http.Flusher).Flush()` 在每次 Write 后（代码里已写，但要验证真的触发了）；2) 把 `buf` 从 8KB 调到 2KB 更频繁 flush；3) 必要时包一层 `bufio.WriterSize(2048)`                                                         |
| **R6: Authorization 头透传时，CC Switch 发的 key 不是直接给 DeepSeek 用的（经过了 CC Switch 自己的中间层）**                   | 低（10%） | 高：转发后全是 401                        | 端到端第一次请求时                               | 1) 把 `BLIND_UPSTREAM_BASE_URL` 指向 **CC Switch 的代理端口**（不是直接 DeepSeek），让 CC Switch 处理 key 映射，工具只负责改 body 不碰 auth；2) 工具完全不处理 Authorization，全量透传所有头（代码里已这么写）                                  |
| **R7: 描述 prompt 选得差，MiMo 输出太长 / 太啰嗦 / 语焉不详，导致 DeepSeek 误解图片内容**                                       | 中（40%） | 低-中：用户体验差，不是故障                     | 端到端第一次描述截图时                             | 1) 调小 `description_cap`（300 就够了，别 500）；2) system prompt 改成「用简洁中文分点描述，优先识别代码片段、UI 元素、错误信息」；3) 保留 prompt 为可配置项（config.yaml 里加 `vision.prompt` 字段，默认值硬编码）                                    |
| **R8: fail-open 语义下，视觉失败但上游（DeepSeek）对「含未识别 image 块」直接报错而非忽略**                                        | 中（35%） | 中：用户看到的是 DeepSeek 错误，不是 MiMo 错误，误导 | 第一次故意坏 key 测试时                          | 1) 日志里同时警告「图像未替换，上游可能报错」；2) 响应头 `X-Blind-Llm-Eyes` 加 `failed=K` 字段明示有几张图调视觉失败；3) 可选配置项：fail-open 时也把 image 块移除换「\[图片视觉调用失败，无法描述]」占位文字（保证上游永远收不到 image 块）——默认关，用户遇到问题再开                    |

***

## 8. 决策回顾（已与用户确认的关键决策）

| #  | 决策点    | 选项                                       | 最终                       | 依据                   |
| -- | ------ | ---------------------------------------- | ------------------------ | -------------------- |
| 1  | 视觉后端   | MiMo / GLM-4V / Qwen-VL                  | **MiMo mimo-v2.5**       | 现成 key，OpenAI 兼容端点   |
| 2  | 缓存落盘   | 纯内存 / 持久化                                | **纯内存 LRU**              | v1 简单够用，重启不致命        |
| 3  | 失败语义   | fail-open / fail-closed                  | **fail-open**            | 辅助能力不阻塞主链路           |
| 4  | API 格式 | 仅 Anthropic / 双格式                        | **仅 Anthropic Messages** | v1 先最小化 scope        |
| 5  | 纯文本判断  | 手动声明 / 内置清单                              | **配置手动声明**               | 简单直接，不维护模型注册表        |
| 6  | 项目命名   | vision-fallback / blind-llm-eyes         | **blind-llm-eyes**       | 用户定，更有辨识度            |
| 7  | 开发模式   | 用户手写/我代笔/混合                              | **用户手写，我教练**             | 符合 GO-STANDARDS 约定   |
| 8  | 开发周期   | 1-3 天 / 1 周 / 2 周                        | **1-3 天 MVP**            | 范围小，核心机制验证过          |
| 9  | 技术库    | 标准库 / gopkg.in/yaml.v3 / slog / net/http | **全选**                   | 零框架依赖，全标准库 + 一个 YAML |
| 10 | 预算场景   | 个人 / 团队 / 有限                             | **个人自用，无硬预算**            | 月度几元钱可忽略             |

***

## 9. 下一步（计划批准后立即执行）

1. **您批准本计划**
2. 我切换到执行模式，按 Phase 0 → P1 → P2 → P3 → P4 → P5 **逐阶段推进**
3. 每阶段结束时：您贴代码，我做 **代码审查** + **跑 test 命令** 验证
4. P5 结束后：我们一起做端到端实机验证，验收通过即交付

> **提醒（来自 HANDOFF.md §3.4、§5）**：
>
> * 写 Go 前请再扫一眼 `agent-api/docs/GO-STANDARDS.md`（MUST/SHOULD 规则）
>
> * 本目录不是 git 仓库——Phase 0 第一步先 `git init`
>
> * 真实 key 放 `config.yaml`（已在 `.gitignore`），**不要提交到仓库**

