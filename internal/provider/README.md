# provider - AI 提供者抽象层

Hydra 是一个多模型对抗式代码审查工具，`provider` 包定义了统一的 AI 提供者接口，并提供三种具体实现：Claude Code CLI、Codex CLI、OpenAI API。通过该抽象层，Hydra 的编排器（orchestrator）可以同时调度多个不同的 AI 模型，对同一份代码变更进行独立审查，再交叉辩论，最终汇总出高质量的审查意见。

## 文件说明

| 文件 | 说明 |
|------|------|
| `provider.go` | 核心接口定义：`AIProvider`、`SessionProvider`、`Message`、`ChatOptions` |
| `cliprovider.go` | CLI 提供者共享的会话管理器 `CliSessionHelper`，含原子快照和 prompt 构建 |
| `claudecode.go` | Claude Code CLI 实现：通过 `os/exec` 调用 `claude` 命令，支持 JSON/stream-json 输出 |
| `codexcli.go` | Codex CLI 实现：通过 `os/exec` 调用 `codex` 命令，支持 JSONL 事件流 |
| `openai.go` | OpenAI API 实现：直接 `net/http` 调用 Chat Completions API，支持 SSE 流式 |
| `retry.go` | 泛型指数退避重试机制，自动识别瞬时错误 |
| `factory.go` | 工厂函数 `CreateProvider` + `MockProvider` 测试桩 |

## 核心接口

```go
// Message 表示一条对话消息。
type Message struct {
    Role    string // "system" | "user" | "assistant"
    Content string
}

// ChatOptions 控制单次调用的行为选项。
type ChatOptions struct {
    DisableTools bool // 禁用 CLI 工具调用（用于纯 JSON 输出场景）
}

// AIProvider 是所有 AI 提供者的核心接口。
// Hydra 通过此接口与不同的 AI 后端交互。
type AIProvider interface {
    Name() string
    // Chat 发送消息并等待完整响应（同步）。
    Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error)
    // ChatStream 发送消息并以流式方式返回响应片段（异步）。
    // 返回两个 channel：chunks 接收文本片段，errs 接收错误。
    ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error)
}

// SessionProvider 在 AIProvider 基础上扩展了会话管理能力。
// CLI 提供者通过会话复用来避免每次调用都发送完整历史，节省 token。
type SessionProvider interface {
    AIProvider
    StartSession(name string)        // 开始新会话
    EndSession()                     // 结束当前会话
    SessionID() string               // 获取当前会话 ID（由 CLI 响应返回）
    IsFirstMessage() bool            // 是否为会话中的第一条消息
    MarkMessageSent()                // 标记已发送一条消息
    ShouldSendFullHistory() bool     // 是否需要发送完整历史（首条消息或无会话时）
}
```

## Message 数据结构示例

```go
// 对话消息示例 — Hydra 审查场景下的典型消息序列
messages := []Message{
    {Role: "system", Content: "You are a security-focused code reviewer..."},
    {Role: "user", Content: "Please review this diff:\n```diff\n+func handler(w http.ResponseWriter, r *http.Request) {\n+    query := r.URL.Query().Get(\"id\")\n+    db.Exec(\"SELECT * FROM users WHERE id = \" + query)\n..."},
    {Role: "assistant", Content: "I've identified several security concerns:\n1. SQL injection vulnerability..."},
}

// 在多轮辩论中，消息会不断追加：
// orchestrator 将其他 reviewer 的意见作为新的 user 消息加入
messages = append(messages, Message{
    Role:    "user",
    Content: "Other reviewer's opinion:\n> The SQL injection is critical...\nDo you agree?",
})
```

## ChatOptions 使用示例

```go
// 标准调用（默认行为）
response, err := provider.Chat(ctx, msgs, systemPrompt, nil)

// 禁用工具调用（用于获取纯 JSON 输出，如结构化问题提取）
opts := &ChatOptions{DisableTools: true}
response, err := provider.Chat(ctx, msgs, systemPrompt, opts)
```

## 工厂路由

`CreateProvider` 根据模型名称匹配规则，创建对应的 `AIProvider` 实例：

```
CreateProvider(model, cfg)
  |
  |-  model == "claude-code"              -> ClaudeCodeProvider  (claude CLI 子进程)
  |-  model == "codex-cli"                -> CodexCliProvider    (codex CLI 子进程)
  |-  strings.HasPrefix(model, "gpt-")    -> OpenAIProvider      // gpt-4o, gpt-4o-mini, ...
  |-  strings.HasPrefix(model, "o1-")     -> OpenAIProvider      // o1-preview, o1-mini
  |-  strings.HasPrefix(model, "o3-")     -> OpenAIProvider      // o3-mini
  |-  strings.HasPrefix(model, "mock")    -> MockProvider        // 测试桩
  '-- cfg.Mock == true                    -> MockProvider        (全局 mock 模式，所有模型)
```

**配置读取**：对于 OpenAI 类模型，工厂函数从 `cfg.Providers["openai"]` 中读取 `api_key` 和可选的 `base_url`（支持 Azure OpenAI / Ollama 等兼容 API）。对于 CLI 类模型，读取 `cfg.Defaults.SkipPermissions` 控制是否跳过权限确认。

## Claude Code Provider

### CLI 调用方式

```bash
# 首次调用（新会话）— 传入 system prompt，通过 stdin 发送完整 prompt
echo "Please review this diff..." | claude -p - \
  --output-format stream-json \
  --system-prompt "You are a security reviewer..." \
  --dangerously-skip-permissions

# 后续调用（复用会话）— 使用 --resume 续传，只发送增量消息
echo "Here are other reviewers' opinions..." | claude -p - \
  --output-format stream-json \
  --resume <session-id> \
  --dangerously-skip-permissions
```

**参数说明**：
- `-p -`：pipe 模式，从 stdin 读取 prompt（避免参数过长导致 E2BIG 错误）
- `--output-format json`：非流式模式，输出完整 JSON 数组
- `--output-format stream-json`：流式模式，输出 JSONL（每行一个事件）
- `--resume <id>`：复用已有会话，CLI 记住之前的上下文
- `--system-prompt`：系统提示词，仅首次调用时传入
- `--dangerously-skip-permissions`：跳过交互式权限确认（非交互模式必须）

### JSON 响应格式

```
非流式模式（--output-format json）：输出为 JSON 数组

  [
    {"type":"system", ...},
    {"type":"assistant", "message":{"content":[{"type":"text","text":"Review..."}]}, "session_id":"abc-123"},
    {"type":"result", "subtype":"success", "result":"Complete review text...", "session_id":"abc-123"}
  ]

流式模式（--output-format stream-json）：逐行 JSONL 输出

  {"type":"assistant","message":{"content":[{"type":"text","text":"Looking at the diff..."}]},"session_id":"abc-123"}
  {"type":"result","subtype":"success","result":"Complete review...","session_id":"abc-123"}
```

**事件类型**：
- `assistant`：AI 生成的消息，文本在 `message.content[].text` 中
- `result`：最终结果，完整文本在 `result` 字段中
- 所有事件都可能携带 `session_id`，Provider 从中提取并保存

### 核心流程

```
Chat() / ChatStream()
  |
  |- 1. Snapshot() 原子读取会话状态（解决 TOCTOU 竞态）
  |- 2. 构建 prompt
  |     |- ShouldSendFull() == true  -> BuildPrompt()（系统提示 + 全部消息历史）
  |     '- ShouldSendFull() == false -> BuildPromptLastOnly()（仅最后一条 user 消息）
  |- 3. buildArgs() 构建 CLI 参数
  |     |- 首次：--system-prompt "..." --output-format stream-json
  |     '- 续传：--resume <session-id> --output-format stream-json
  |- 4. os/exec 执行 claude 命令，stdin 传入 prompt
  |- 5. 解析输出
  |     |- Chat:       json.Unmarshal JSON 数组 -> 提取 session_id + result text
  |     '- ChatStream: 逐行读取 JSONL -> 解析 assistant/result 事件 -> 发送到 channel
  |- 6. SetSessionID() 保存会话 ID，MarkMessageSent() 标记非首次
  '- 7. WithRetry 包装（仅 Chat 同步模式）
```

## OpenAI Provider

### API 请求/响应格式

```
POST {baseURL}/chat/completions

请求体（非流式）:
  {
    "model": "gpt-4o",
    "messages": [
      {"role": "system", "content": "You are a code reviewer..."},
      {"role": "user", "content": "Review this diff:\n```diff\n+func..."}
    ],
    "stream": false
  }

响应体（非流式）:
  {
    "choices": [
      {"message": {"role": "assistant", "content": "I found a bug..."}}
    ]
  }
```

```
请求体（流式）:
  {
    "model": "gpt-4o",
    "messages": [...],
    "stream": true
  }

SSE 流式响应（每行一个 Server-Sent Event）:
  data: {"choices":[{"delta":{"content":"I found"}}]}
  data: {"choices":[{"delta":{"content":" a potential"}}]}
  data: {"choices":[{"delta":{"content":" SQL injection"}}]}
  data: [DONE]
```

### 核心流程

```
Chat() / ChatStream()
  |
  |- 1. buildMessages()：将 systemPrompt 前置 + 合并 messages
  |     [system prompt, user msg1, assistant msg1, user msg2, ...]
  |- 2. POST /chat/completions
  |     |- Chat:       stream: false -> json.Unmarshal 完整响应
  |     '- ChatStream: stream: true  -> bufio.Scanner 逐行解析 SSE data: 事件
  |- 3. 支持自定义 baseURL（Azure OpenAI / Ollama 等兼容 API）
  '- 4. WithRetry 包装（仅 Chat 同步模式）
```

**与 CLI Provider 的区别**：OpenAI Provider 是无状态的，每次调用发送完整消息历史，不维护会话。因此它只实现 `AIProvider` 接口，不实现 `SessionProvider`。

## SessionProvider 生命周期

```
StartSession("Hydra | PR #42 | reviewer:security")
  |
  |-  session.Start(name)
  |     '- sessionID = "", firstMessage = true, sessionName = name
  |
  |-  第 1 轮调用: Chat(ctx, messages, systemPrompt, nil)
  |     |- Snapshot() -> {ID:"", FirstMessage:true}
  |     |- ShouldSendFull() == true  -> 发送完整历史 + 系统提示
  |     |- CLI 响应包含 session_id -> SetSessionID("abc-123")
  |     '- MarkMessageSent()        -> firstMessage = false
  |
  |-  第 2 轮调用: Chat(ctx, newMessages, systemPrompt, nil)
  |     |- Snapshot() -> {ID:"abc-123", FirstMessage:false}
  |     |- ShouldSendFull() == false -> 仅发送最后一条 user 消息
  |     '- CLI 使用 --resume abc-123 续传会话
  |
  |-  第 N 轮调用: ...（同第 2 轮）
  |
  '-- EndSession()
        '- session.End()
             '- sessionID = "", firstMessage = true, sessionName = ""
```

## 会话快照与 TOCTOU 竞态问题

`CliSessionHelper` 使用 `Snapshot()` 方法解决 TOCTOU（Time Of Check To Time Of Use）竞态问题。

### 问题：分两次读取会话状态

如果分别调用 `SessionID()` 和 `IsFirstMessage()`，两次加锁之间可能有其他 goroutine 修改了状态：

```
goroutine A (Chat)          goroutine B (辩论结束，清理会话)
-----------------           -----------------------------------
SessionID()
  lock()
  读 id = "abc123"
  unlock()
                            End()
                              lock()
                              id = ""
                              firstMessage = true
                              unlock()
IsFirstMessage()
  lock()
  读 first = true
  unlock()

结果：id="abc123" + first=true -> 不一致！
id 说"有旧会话"，first 说"是新会话"，两个值来自不同时刻。
后续用 --resume abc123 续传会报错，因为会话已经被 End() 清掉了。
```

### 解决：Snapshot() 一次性读取

```go
func (h *CliSessionHelper) Snapshot() SessionSnapshot {
    h.mu.Lock()
    defer h.mu.Unlock()
    return SessionSnapshot{
        ID:           h.sessionID,    // 在同一把锁下
        FirstMessage: h.firstMessage, // 一次性读完
        Name:         h.sessionName,
    }
}
```

```
goroutine A (Chat)          goroutine B
-----------------           ----------
Snapshot()
  lock()
  读 id = "abc123"          End() -> 想加锁？被 A 占着，等！
  读 first = false
  unlock()                  -> 现在才能执行 End()

结果：id="abc123" + first=false -> 一致！
```

**原则：如果多个字段之间有逻辑关联，必须在同一把锁内一次性读取，不能分开读。**

## 重试机制

`WithRetry` 是一个泛型函数，对暂时性错误自动重试并采用指数退避策略：

```go
func WithRetry[T any](fn func() (T, error), opts *RetryOptions) (T, error)

type RetryOptions struct {
    MaxAttempts int              // 最大尝试次数（默认 3）
    BackoffMs   []int            // 退避间隔序列，毫秒（默认 [1000, 2000, 4000]）
    ShouldRetry func(error) bool // 自定义错误判断函数
}
```

### 重试流程示例

```
WithRetry(fn, nil)  // 使用默认配置：最多 3 次，退避 1s -> 2s -> 4s
  |
  |- 第 1 次失败 (connection reset) -> 瞬时错误，等待 1s 后重试
  |- 第 2 次失败 (429 rate limit)   -> 瞬时错误，等待 2s 后重试
  |- 第 3 次成功                    -> 返回结果

WithRetry(fn, nil)  // 遇到非瞬时错误
  |
  |- 第 1 次失败 (401 unauthorized) -> 非瞬时错误，立即返回错误，不再重试
```

### 瞬时错误判定规则

```
isTransientError(err) 通过错误消息的字符串匹配来判断：

  可重试（瞬时错误）：
    "timeout"          - 网络超时
    "connection reset" - 连接被重置
    "econnreset"       - POSIX 连接重置错误码
    "rate limit"       - 速率限制
    "429"              - HTTP 429 Too Many Requests
    "502"              - HTTP 502 Bad Gateway
    "503"              - HTTP 503 Service Unavailable

  不可重试（立即返回）：
    HTTP 400 Bad Request     - 请求参数错误
    HTTP 401 Unauthorized    - 认证失败
    HTTP 404 Not Found       - 资源不存在
    其他非瞬时错误           - 直接返回，不浪费重试次数
```
