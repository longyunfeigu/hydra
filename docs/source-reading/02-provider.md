---
skill_version: "v3"
generated_at: "2026-03-08"
repo_commit: "a132cb6"
sources:
  - internal/provider/provider.go
  - internal/provider/factory.go
  - internal/provider/claudecode.go
  - internal/provider/codexcli.go
  - internal/provider/openai.go
  - internal/provider/cliprovider.go
  - internal/provider/retry.go
  - internal/provider/promptfile.go
freshness_scope: "provider 包内任何文件的结构性变化：AIProvider 接口签名变化、新增 provider 实现、factory 逻辑变化"
---

# Provider 模块源码解析

> 文件路径：`docs/source-reading/02-provider.md`
> 覆盖范围：`internal/provider/` 全部文件（L1-L4 + Code Insights）
> 模块定位：重要模块（前四层）

---

## Layer 1: 场景（Problem Space）

### 一句话描述

Provider 模块是 Hydra 与外部 AI 后端之间的适配层——它把"调用不同厂商的 AI"这件事统一成一个接口，让上层编排器不需要关心底层是 CLI 子进程还是 HTTP API。

### 核心用户与核心操作

这里的"用户"不是终端人类用户，而是 Hydra 内部的上层模块。

| 调用方 | 核心操作 |
|--------|---------|
| `review.Runner` | 通过 `CreateProvider()` 工厂函数，根据配置创建 provider 实例（`runner.go:247 (Prepare)`） |
| `orchestrator.DebateOrchestrator` | 通过 `AIProvider.Chat()` / `ChatStream()` 发送审查 prompt 并获取 AI 回复 |
| `orchestrator.DebateOrchestrator` | 通过 `SessionProvider` 接口管理多轮辩论的会话生命周期 |

### 主场景 vs 辅助场景

| 场景类型 | 描述 |
|----------|------|
| 主场景 | 编排器调用 `Chat()` 让 AI 审查代码差异，返回审查意见文本 |
| 主场景 | 编排器调用 `ChatStream()` 流式获取审查过程，实时展示给用户 |
| 主场景 | 编排器通过 `SessionProvider.StartSession()` / `EndSession()` 管理 CLI 会话，节省多轮辩论的 token 成本 |
| 辅助场景 | 工厂函数创建 `MockProvider` 用于测试（`factory.go:23-25 (CreateProvider)`） |
| 辅助场景 | `PreparePromptForCli()` 处理超大 prompt（>100KB），写临时文件避免系统限制（`promptfile.go:23 (PreparePromptForCli)`） |

### 用户旅程图

```
review.Runner                     provider 模块                          外部 AI 后端
     |                                 |                                      |
     |-- CreateProvider(model, ...) -->|                                      |
     |                                 |-- switch model {                     |
     |                                 |     "claude-code" -> ClaudeCode      |
     |                                 |     "codex-cli"   -> CodexCli        |
     |                                 |     "gpt-*"       -> OpenAI          |
     |                                 |   }                                  |
     |<-- AIProvider ------------------|                                      |
     |                                 |                                      |
     |                                 |                                      |
orchestrator                           |                                      |
     |-- StartSession("label") ------->|                                      |
     |                                 |-- session.Start()                    |
     |                                 |                                      |
     |-- Chat(ctx, msgs, sys, nil) --->|                                      |
     |                                 |-- WithRetry {                        |
     |                                 |     Snapshot()                       |
     |                                 |     BuildPrompt() / LastOnly()       |
     |                                 |     PreparePromptForCli()            |
     |                                 |     exec "claude" / "codex" -------->| CLI 子进程
     |                                 |     或 POST /chat/completions ------>| HTTP API
     |                                 |     解析 JSON/JSONL/SSE              |
     |                                 |     SetSessionID() / MarkSent()     |
     |                                 |   }                                  |
     |<-- "审查意见文本" ---------------|                                      |
     |                                 |                                      |
     |-- EndSession() ---------------->|                                      |
     |                                 |-- session.End()                      |
```

### Q&A

> **Q**: 为什么不直接用各 AI 厂商的官方 Go SDK（如 `github.com/anthropics/anthropic-sdk-go`），而是自己封装 CLI 子进程调用？
>
> **Evidence**: `claudecode.go:201 (runClaude)` 通过 `exec.CommandContext(ctx, "claude", args...)` 调用 CLI 子进程；`openai.go:132 (Chat)` 通过 `net/http` 直接调用 API。整个 `go.mod` 中没有任何 AI 厂商的 SDK 依赖。Claude Code 和 Codex 本身就是 CLI 工具，它们的核心能力（工具调用、文件读写、代码搜索）只能通过 CLI 使用，没有等价的 SDK API。
>
> **Inference**: 这是一个务实的设计选择。Claude Code CLI 和 Codex CLI 是 agentic 工具，它们内部集成了工具调用编排（读文件、搜索代码、执行命令）。如果用 SDK，Hydra 需要自己实现这些工具编排逻辑，工作量巨大且难以保持同步。对于 OpenAI 则不需要 agentic 能力，直接用 `net/http` 调 API 足够简单，不值得引入 SDK 依赖。

---

## Layer 2: 概念（Domain Model）

### 核心概念表

分为三类：核心实体 / 过程概念 / 策略概念。

#### 核心实体

**1. AIProvider（接口）** — 通用模式（Strategy Pattern）

所有 AI 后端的统一抽象，定义同步和流式两种交互方式。

- **场景 example**: 配置文件中写了 `model: "claude-code"` 和 `model: "gpt-4o"` 两个审查者，Runner 就会通过工厂创建两个不同的 `AIProvider` 实例，编排器统一调用它们的 `Chat()` 方法。
- **结构 example**:
  ```
  AIProvider (interface) {
    Name()       -> string                        // 用于日志和 UI 标识的 provider 名称
    Chat()       -> (string, error)               // 同步调用，等待完整响应后返回
    ChatStream() -> (<-chan string, <-chan error)  // 流式调用，逐块返回用于实时展示
  }
  ```

**2. SessionProvider（接口）** — 作者创造的

在 `AIProvider` 基础上扩展会话管理能力，让 CLI 类 provider 在多轮辩论中只发送增量消息。

- **场景 example**: 编排器执行 3 轮辩论，第 1 轮发送完整 prompt（~5K tokens），第 2、3 轮只发送其他审查者的新意见（~3K tokens），而非重复发送全部历史（~30K tokens）。
- **结构 example**:
  ```
  SessionProvider (interface) {
    AIProvider                          // 嵌入基础接口，继承 Chat/ChatStream 能力
    StartSession(name string)           // 开始新会话，name 用于 CLI 显示标签
    EndSession()                        // 结束当前会话，清空状态
    SessionID()       -> string         // 返回 CLI 响应中设置的会话 ID
    IsFirstMessage()  -> bool           // 判断是否首条消息，决定是否发全量历史
    MarkMessageSent()                   // 标记已发送，后续走增量模式
    ShouldSendFullHistory() -> bool     // 无 sessionID 时返回 true，强制全量发送
  }
  ```

**3. ClaudeCodeProvider（结构体）** — 作者创造的

通过 `os/exec` 调用 `claude` CLI 命令的 provider 实现，同时实现 `AIProvider` 和 `SessionProvider`。

- **场景 example**: 配置中 `model: "claude-code"` 加 `model_name: "claude-sonnet-4-5-20250514"`，工厂创建此 provider，通过 `claude -p - --model claude-sonnet-4-5-20250514` 调用 CLI。
- **结构 example**:
  ```
  ClaudeCodeProvider {
    cwd:                 "/tmp/hydra-repos/my-project"   // CLI 执行时的工作目录（被审查项目根目录）
    timeout:             15m                              // 单次 CLI 调用的超时时间
    session:             CliSessionHelper{...}            // 共享的会话状态管理器
    skipPermissions:     true                             // 跳过 CLI 交互式权限确认
    modelName:           "claude-sonnet-4-5-20250514"     // 传给 --model 参数的具体模型名
    promptSizeThreshold: 102400                           // 超过此字节数写入临时文件
  }
  ```

**4. CodexCliProvider（结构体）** — 作者创造的

通过 `os/exec` 调用 `codex` CLI 命令的 provider 实现，与 ClaudeCodeProvider 结构相似但 CLI 协议不同。

- **场景 example**: 配置中 `model: "codex-cli"` 加 `model_name: "o3-mini"`，工厂创建此 provider，通过 `codex exec --json --model o3-mini -` 调用 CLI。
- **结构 example**:
  ```
  CodexCliProvider {
    cwd:                 "/tmp/hydra-repos/my-project"   // CLI 执行时的工作目录
    timeout:             15m                              // 单次 CLI 调用的超时时间
    session:             CliSessionHelper{...}            // 共享的会话状态管理器
    sessionEnabled:      true                             // 是否启用 --thread 会话续传
    skipPermissions:     true                             // 跳过 CLI 交互式权限确认
    modelName:           "o3-mini"                        // 传给 --model 参数的具体模型名
    promptSizeThreshold: 102400                           // 超过此字节数写入临时文件
  }
  ```

**5. OpenAIProvider（结构体）** — 通用模式（HTTP Client Wrapper）

通过 `net/http` 直接调用 OpenAI Chat Completions API 的无状态 provider，不实现 `SessionProvider`。

- **场景 example**: 配置中 `model: "gpt-4o"` 加 `providers.openai.api_key: "${OPENAI_API_KEY}"`，工厂创建此 provider，通过 HTTP POST 调用 `/v1/chat/completions`。
- **结构 example**:
  ```
  OpenAIProvider {
    apiKey:          "sk-xxx..."                       // API 认证密钥，从配置读取
    model:           "gpt-4o"                          // 请求体中的 model 字段
    baseURL:         "https://api.openai.com/v1"       // 可配置的 API 基础 URL
    client:          &http.Client{}                    // 共享的 HTTP 客户端实例
    reasoningEffort: ""                                // 推理模型的深度控制（非推理模型为空）
  }
  ```

#### 过程概念

**6. CliSessionHelper（结构体）** — 作者创造的

CLI 类 provider 共享的会话状态管理器，通过 `sync.Mutex` 保护并发访问，提供 `Snapshot()` 原子快照解决 TOCTOU 竞态。

- **场景 example**: 流式读取 goroutine 调用 `SetSessionID()` 更新会话 ID 的同时，`Chat()` 方法调用 `Snapshot()` 读取状态，mutex 保证两者不会读到不一致的值。
- **结构 example**:
  ```
  CliSessionHelper {
    mu:           sync.Mutex                                // 保护并发读写的互斥锁
    sessionID:    "abc-123"                                 // 由 CLI 首次响应中提取设置
    firstMessage: false                                     // true=首次消息发全量，false=发增量
    sessionName:  "Hydra | PR #42 | reviewer:security"     // CLI 显示的会话标签
  }
  ```

#### 策略概念

**7. RetryOptions / WithRetry（泛型函数）** — 通用模式（Retry with Exponential Backoff）

泛型重试机制，对暂时性错误（超时、限流、502/503）自动指数退避重试，对非暂时性错误立即返回。

- **场景 example**: `Chat()` 调用 CLI 时遇到 429 rate limit 错误，`WithRetry` 等待 1 秒后重试，第二次成功返回结果。
- **结构 example**:
  ```
  RetryOptions {
    MaxAttempts: 3                              // 最大重试次数（含首次调用）
    BackoffMs:   [1000, 2000, 4000]             // 各次重试前的等待毫秒数
    ShouldRetry: isTransientError               // 判断错误是否值得重试的函数
  }
  ```

### 概念关系图

```
                              +------------------+
                              |  review.Runner   |  (调用者)
                              +--------+---------+
                                       |
                                       | CreateProvider()
                                       v
                              +------------------+
                              | CreateProvider() |  factory.go
                              | (工厂函数)        |
                              +--------+---------+
                                       |
                    +------------------+------------------+
                    |                  |                  |
                    v                  v                  v
          +-----------------+ +-----------------+ +---------------+
          | ClaudeCode      | | CodexCli        | | OpenAI        |
          | Provider        | | Provider        | | Provider      |
          +-----------------+ +-----------------+ +---------------+
          | implements:     | | implements:     | | implements:   |
          |  AIProvider     | |  AIProvider     | |  AIProvider   |
          |  SessionProvider| |  SessionProvider| |               |
          +--------+--------+ +--------+--------+ +-------+-------+
                   |                   |                   |
                   v                   v                   |
          +------------------+                             |
          | CliSessionHelper |  (共享，值嵌入)              |
          | cliprovider.go   |                             |
          +--------+---------+                             |
                   |                                       |
                   v                                       v
          +-----------------+                    +-----------------+
          | PreparePrompt   |                    | net/http        |
          | ForCli()        |                    | POST /chat/     |
          | promptfile.go   |                    |   completions   |
          +-----------------+                    +-----------------+
                   |
                   v
          +-----------------+
          | WithRetry[T]()  |  retry.go
          | 泛型重试         |
          +-----------------+
```

### 概念生命周期表

有状态的概念：`CliSessionHelper`

| 状态 | sessionID | firstMessage | 触发动作 |
|------|-----------|-------------|---------|
| 未初始化 | `""` | `true` | 结构体零值 |
| 会话已开始 | `""` | `true` | `Start(name)` 调用 |
| 首次响应后 | `"abc-123"` | `true` | CLI 响应中提取 `session_id` / `thread_id` |
| 消息已发送 | `"abc-123"` | `false` | `MarkMessageSent()` 调用 |
| 会话结束 | `""` | `true` | `End()` 调用 |

状态转换序列：

```
Start("label")  -->  SetSessionID("abc-123")  -->  MarkMessageSent()
     |                      |                           |
  ID=""              ID="abc-123"                 firstMessage=false
  first=true         first=true                   (后续走增量模式)
                                                       |
                                             ... (多轮重复) ...
                                                       |
                                              End()  --> 回到初始状态
```

### Q&A

> **Q**: `AIProvider` 和 `SessionProvider` 的区别是什么？为什么不把会话管理方法直接放进 `AIProvider`？
>
> **Evidence**: `provider.go:19-26 (AIProvider)` 定义 `AIProvider` 只有 `Name()`、`Chat()`、`ChatStream()` 三个方法。`provider.go:30-38 (SessionProvider)` 定义 `SessionProvider` 嵌入 `AIProvider` 并额外增加 6 个会话管理方法。`openai.go:21-27 (OpenAIProvider)` 只实现了 `AIProvider`，不实现 `SessionProvider`。`runner.go:156 (Prepare)` 通过 `provider.SetCwdIfSupported()` 使用鸭子类型检测能力。
>
> **Inference**: 这是接口隔离原则（ISP）的应用。OpenAI API 是无状态的，天然不支持会话续传，如果把会话方法放进 `AIProvider`，`OpenAIProvider` 就必须实现一堆空方法。分成两个接口后，编排器通过类型断言 `if sp, ok := p.(SessionProvider)` 按需使用会话能力，不强制所有 provider 实现。

---

## Layer 3: 约束（Design Boundaries）

### 关键约束

#### 技术约束

**C1: 无 SDK 依赖，只用 CLI 子进程和原生 HTTP**

`claudecode.go` 和 `codexcli.go` 通过 `os/exec` 调用 CLI 子进程，`openai.go` 通过 `net/http` 直接调用 API，整个 provider 包不依赖任何 AI 厂商的 Go SDK。

影响的设计：
- 必须自己解析 CLI 的 JSON/JSONL/SSE 输出格式（`claudecode.go:167-195 (parseStreamEvent)`、`codexcli.go:164-174 (parseStreamEvent)`、`openai.go:49-90 (chatStreamInternal)`）
- 必须自己管理子进程生命周期、stdin/stdout pipe、超时检测（`claudecode.go:249-369 (runClaudeStream)`）
- 必须自己实现 SSE 协议解析（`openai.go:229-258 (parseSSEStream)`）

**C2: CLI 子进程的参数长度限制（E2BIG）**

操作系统对进程参数总长度有限制（Linux 默认 ~2MB，macOS 更小）。大型代码审查的 prompt 可能包含数千行 diff，超过此限制。

影响的设计：
- prompt 通过 stdin 传入而非命令行参数（`claudecode.go:207 (runClaude)`：`cmd.Stdin = strings.NewReader(prompt)`，参数用 `-p -` 指定从 stdin 读取）
- 超大 prompt（>100KB）还需写入临时文件（`promptfile.go:23-59 (PreparePromptForCli)`）

**C3: CLI 嵌套会话冲突**

当 Hydra 在 Claude Code 终端中运行时，子进程会继承 `CLAUDECODE` 环境变量，导致 Claude CLI 报错 "cannot launch inside another session"。

影响的设计：
- `filterClaudeEnv()` 函数在启动子进程前移除 `CLAUDECODE` 环境变量（`claudecode.go:149-157 (filterClaudeEnv)`）

#### 业务约束

**C4: CLI vs API 的协议差异**

三种后端的交互协议完全不同：Claude Code 输出 JSON 数组或 stream-json JSONL，Codex CLI 只输出 JSONL，OpenAI API 使用 SSE。会话 ID 的获取方式也不同：Claude 是 `session_id` 字段，Codex 是 `thread.started` 事件的 `thread_id`。

影响的设计：
- 每个 provider 必须实现自己的事件解析逻辑
- 但会话状态管理被提取为共享的 `CliSessionHelper`（`cliprovider.go`），避免重复

#### 运行时约束

**C5: 流式读取的并发安全**

流式模式下，stdout 读取 goroutine 可能随时调用 `SetSessionID()`，而主线程可能同时调用 `Snapshot()` 读取会话状态。

影响的设计：
- `CliSessionHelper` 所有字段通过 `sync.Mutex` 保护（`cliprovider.go:15 (CliSessionHelper)`）
- `Snapshot()` 一次性在同一把锁下读取所有相关字段，解决 TOCTOU 竞态（`cliprovider.go:58-66 (Snapshot)`）

### 约束-影响映射表

```
+------+-------------------------------+--------------------------------------------+
| 约束 | 描述                          | 影响的设计决策                              |
+------+-------------------------------+--------------------------------------------+
| C1   | 无 SDK 依赖                   | 自行解析 JSON/JSONL/SSE；自行管理子进程      |
|      |                               | 生命周期和 pipe；自行实现 HTTP 请求          |
+------+-------------------------------+--------------------------------------------+
| C2   | CLI 参数长度限制 (E2BIG)       | prompt 通过 stdin 传入 (-p -)；             |
|      |                               | 超大 prompt 写临时文件 (promptfile.go)       |
+------+-------------------------------+--------------------------------------------+
| C3   | CLI 嵌套会话冲突               | filterClaudeEnv() 移除 CLAUDECODE 环境变量  |
+------+-------------------------------+--------------------------------------------+
| C4   | CLI vs API 协议差异            | 每个 provider 独立实现事件解析；             |
|      |                               | 共享 CliSessionHelper 抽取通用会话管理       |
+------+-------------------------------+--------------------------------------------+
| C5   | 流式读取的并发安全             | CliSessionHelper 用 Mutex 保护；            |
|      |                               | Snapshot() 原子快照解决 TOCTOU              |
+------+-------------------------------+--------------------------------------------+
```

### Q&A

> **Q**: 如果没有 C2（E2BIG 限制）这个约束，设计会怎样不同？
>
> **Evidence**: `claudecode.go:118 (buildArgs)` 使用 `-p -` 参数让 CLI 从 stdin 读取 prompt。`claudecode.go:207 (runClaude)` 通过 `cmd.Stdin = strings.NewReader(prompt)` 传入。`promptfile.go:23-59 (PreparePromptForCli)` 对超大 prompt 写临时文件并生成文件读取指令。
>
> **Inference**: 如果没有 E2BIG 限制，最简单的做法是直接把 prompt 放到命令行参数中（如 `claude -p "Review this diff..."`），不需要 stdin pipe，也不需要 `promptfile.go` 整个文件。但即使没有系统限制，stdin 方式仍然更优雅：它避免了 prompt 内容出现在 `ps` 进程列表中（可能泄露敏感代码），也避免了 shell 转义问题。所以 E2BIG 约束加速了一个本来就更好的设计的采用。

---

## Layer 4: 拆分理由（Design Decisions）

### 文件职责矩阵

```
+------------------+--------------------------------------------------+------------------+
| 文件             | 职责                                             | 拆分动机         |
+------------------+--------------------------------------------------+------------------+
| provider.go      | 接口定义：AIProvider、SessionProvider、Message、  | 可替换性         |
|                  | ChatOptions、SetCwdIfSupported                   | (接口与实现分离) |
+------------------+--------------------------------------------------+------------------+
| factory.go       | 工厂函数 CreateProvider + MockProvider            | 可替换性         |
|                  |                                                  | (路由集中管理)   |
+------------------+--------------------------------------------------+------------------+
| claudecode.go    | Claude Code CLI 实现：参数构建、JSON/JSONL 解析、| 正交性           |
|                  | 环境变量过滤、流式增量提取、工具调用追踪          | (不同 CLI 协议)  |
+------------------+--------------------------------------------------+------------------+
| codexcli.go      | Codex CLI 实现：参数构建、JSONL 解析、           | 正交性           |
|                  | 线程 ID 提取                                     | (不同 CLI 协议)  |
+------------------+--------------------------------------------------+------------------+
| openai.go        | OpenAI API 实现：HTTP 请求、SSE 解析、           | 正交性           |
|                  | MaxTokens 参数适配、推理深度控制                  | (API vs CLI)     |
+------------------+--------------------------------------------------+------------------+
| cliprovider.go   | 共享的 CLI 会话管理：CliSessionHelper、           | 生命周期不同     |
|                  | SessionSnapshot、prompt 构建                     | (状态 vs 协议)   |
+------------------+--------------------------------------------------+------------------+
| retry.go         | 泛型重试：WithRetry[T]、RetryOptions、           | 失败隔离         |
|                  | isTransientError                                 | (横切关注点)     |
+------------------+--------------------------------------------------+------------------+
| promptfile.go    | 大 prompt 处理：PreparePromptForCli、            | 失败隔离         |
|                  | PreparedPrompt、临时文件管理                      | (横切关注点)     |
+------------------+--------------------------------------------------+------------------+
```

### 模块依赖图

```
                    +-----------------+
                    | review.Runner   |  (外部调用者)
                    +--------+--------+
                             |
                             | 调用 CreateProvider()
                             | 调用 SetCwdIfSupported()
                             v
+--------+    +----------+   +-------------+
|provider|<---|factory.go|   |provider.go  |  (接口 + 工厂)
|  .go   |    +----+-----+   +------+------+
+--------+         |                |
                   | 创建            | 定义
         +---------+---------+------+-------+
         |                   |              |
         v                   v              v
+----------------+ +---------------+ +-------------+
| claudecode.go  | | codexcli.go   | | openai.go   |
+-------+--------+ +-------+-------+ +------+------+
        |                   |                |
        +--------+----------+                |
                 |                           |
                 v                           |
        +----------------+                   |
        |cliprovider.go  |                   |
        | (CliSession    |                   |
        |  Helper)       |                   |
        +-------+--------+                   |
                |                            |
        +-------+--------+                   |
        |                |                   |
        v                v                   |
+-------------+ +----------------+           |
| retry.go    | | promptfile.go  |           |
| WithRetry   | | PreparePrompt  |           |
+-------------+ +----------------+           |
        ^                                    |
        |                                    |
        +------------------------------------+
         openai.go 也用 WithRetry
```

### 拆分决策详解

#### D1: 接口与实现分离（provider.go vs 具体实现文件）

**动机**: 可替换性

`provider.go` 只有 48 行，纯粹定义接口和值类型，不包含任何实现逻辑。三个实现文件（`claudecode.go`、`codexcli.go`、`openai.go`）各自实现接口。

> **Q**: 如果把接口定义和实现放在同一个文件里会怎样？
>
> **Evidence**: `provider.go` 定义了 `AIProvider` 和 `SessionProvider` 两个接口（共 48 行）。`claudecode.go` 有 533 行，`codexcli.go` 有 369 行，`openai.go` 有 266 行。
>
> **Inference**: 如果合并，单个文件会超过 1200 行，可读性大幅下降。更重要的是，修改 Claude Code 的事件解析逻辑时，必须滚动跳过 Codex 和 OpenAI 的代码。分离后，每个文件有明确的边界：改 Claude 的解析不会意外影响 Codex，git blame 也能精确定位变更。

#### D2: CliSessionHelper 提取（cliprovider.go）

**动机**: 正交性 + 生命周期不同

会话管理（状态跟踪、prompt 构建、并发安全）是 CLI 类 provider 的共性需求，而事件解析协议是各 CLI 的个性需求。两个维度独立变化。

> **Q**: 如果不提取 `CliSessionHelper`，把会话管理代码分别写在 `claudecode.go` 和 `codexcli.go` 里会怎样？
>
> **Evidence**: `cliprovider.go (CliSessionHelper)` 有 133 行，提供 `Start()`、`End()`、`Snapshot()`、`BuildPrompt()`、`BuildPromptLastOnly()`、`SetSessionID()`、`MarkMessageSent()` 等方法。`claudecode.go:47-52 (StartSession)` 和 `codexcli.go:54-72 (StartSession)` 都是一行委托调用。
>
> **Inference**: 如果不提取，两个 CLI provider 各自复制一套几乎相同的 mutex 保护、Snapshot 快照、prompt 构建逻辑。当修复 TOCTOU 竞态问题时（`cliprovider.go:40-55 (Snapshot)`），需要在两处同步修改，容易遗漏。提取后，修复一处即两处受益。

#### D3: 工厂函数集中路由（factory.go）

**动机**: 可替换性 + 演进路径

`CreateProvider` 是唯一的 provider 创建入口，集中了模型名称到 provider 类型的映射规则。

> **Q**: 如果不用工厂函数，让调用方直接 `new(ClaudeCodeProvider)` 会怎样？
>
> **Evidence**: `factory.go:21-63 (CreateProvider)` 处理了 6 种分支：全局 mock、claude-code、codex-cli、gpt-*/o1-*/o3-*/o4-*、mock* 前缀、default 错误。`runner.go:247 (Prepare)` 调用 `provider.CreateProvider(rc.Model, rc.ModelName, rc.ReasoningEffort, r.cfg)`。
>
> **Inference**: 如果调用方直接构造 provider，每个调用点都需要一个 switch-case 来选择类型、设置 skipPermissions、modelName 等字段。`runner.go` 中有 3 处创建 provider 的地方（reviewers、analyzer、summarizer），还有 1 处创建 context gatherer 的。4 处重复的 switch-case 代码维护成本高，且新增 provider 时需要修改所有调用方。工厂函数把路由逻辑封装在一处。

#### D4: 重试逻辑独立（retry.go）

**动机**: 失败隔离（横切关注点）

`WithRetry` 是一个通用的泛型重试函数，不包含任何 provider 特定的逻辑，可以被任何需要重试的操作复用。

> **Q**: 如果把重试逻辑内联到每个 `Chat()` 方法中会怎样？
>
> **Evidence**: `claudecode.go:56 (Chat)`、`codexcli.go:78 (Chat)`、`openai.go:106 (Chat)` 三处调用 `WithRetry()`。`retry.go:53 (WithRetry)` 使用泛型 `WithRetry[T any]`，支持任意返回类型。
>
> **Inference**: 三个 provider 的 `Chat()` 方法都需要重试，如果内联，重试逻辑（退避计算、错误分类、最大次数控制）会重复三次。当需要调整退避策略（如增加对 HTTP 500 的重试）时，必须同步修改三处。分离后，改 `isTransientError` 一处即可。泛型设计让它不局限于 `(string, error)` 签名。

#### D5: 大 prompt 处理独立（promptfile.go）

**动机**: 失败隔离（横切关注点）

`PreparePromptForCli` 解决的是 CLI 类 provider 共有的大 prompt 问题，与具体 CLI 协议无关。

> **Q**: 如果把大 prompt 处理逻辑内联到 `claudecode.go` 和 `codexcli.go` 中会怎样？
>
> **Evidence**: `claudecode.go:66-67 (Chat)` 和 `codexcli.go:89-90 (Chat)` 都调用 `PreparePromptForCli(prompt, p.promptSizeThreshold)` 并 `defer prepared.Cleanup()`。`promptfile.go (PreparePromptForCli)` 只有 59 行。
>
> **Inference**: 逻辑重复两次，且临时文件清理策略（`Cleanup()` 函数）需要在两处保持一致。分离后，修改阈值默认值或临时文件命名策略只需改一处。

> **Q**: `promptfile.go` 只有 59 行且只被两处调用，单独成文件是否过度拆分？合并到 `cliprovider.go` 是否更合理？
>
> **Evidence**: `promptfile.go` 导出两个符号：`PreparePromptForCli()` 和 `PreparedPrompt`。调用方只有 `claudecode.go:66` 和 `codexcli.go:89` 两处。`cliprovider.go` 已有 133 行，包含所有 CLI 共享逻辑。
>
> **Inference**: 合并后 `cliprovider.go` 约 192 行，仍然合理，且"CLI 共享逻辑"的概念更内聚——会话管理和大 prompt 处理都是 CLI provider 的共性需求。独立成文件的好处是 git blame 更精确、文件名即职责标题，但对于 59 行的逻辑量，这个好处很边际。这属于个人偏好的边界区域，两种选择都可接受。

---

## Code Insights

### Insight 1: 泛型重试函数 WithRetry[T]

**代码位置**: `retry.go:53 (WithRetry)`

**是什么**: 使用 Go 1.18+ 泛型实现的通用重试函数，类型参数 `T` 让它适用于任何返回类型的操作。

**代码示例**:
```go
func WithRetry[T any](fn func() (T, error), opts *RetryOptions) (T, error) {
    maxAttempts := 3
    backoff := defaultBackoff          // [1000, 2000, 4000] ms
    shouldRetry := isTransientError

    if opts != nil {
        if opts.MaxAttempts > 0 { maxAttempts = opts.MaxAttempts }
        if len(opts.BackoffMs) > 0 { backoff = opts.BackoffMs }
        if opts.ShouldRetry != nil { shouldRetry = opts.ShouldRetry }
    }

    var lastErr error
    var zero T
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        result, err := fn()
        if err == nil { return result, nil }
        lastErr = err
        if attempt >= maxAttempts || !shouldRetry(err) { return zero, err }
        idx := attempt - 1
        if idx >= len(backoff) { idx = len(backoff) - 1 }
        time.Sleep(time.Duration(backoff[idx]) * time.Millisecond)
    }
    return zero, lastErr
}
```

**设计意图**: 消除为每种返回类型写单独重试函数的样板代码。

**优点**: 类型安全，返回类型自动推断为 `string`，零额外开销。`var zero T` 惯用法优雅地处理了错误时的零值返回。

**局限性**: `isTransientError` 通过字符串匹配判断错误类型，可能误判（如错误消息中碰巧包含 "timeout" 的非暂时性错误）。Go 泛型不支持方法级类型参数，无法将 `WithRetry` 设计为 provider 的方法。此外，固定的退避数组 `[1000, 2000, 4000]` 不含抖动（jitter），多个并发重试可能同时触发导致"惊群效应"。

**适用场景**: 项目中有多种返回类型的可重试外部调用时值得采用；只有单一返回类型或各操作的重试策略差异大时，直接内联更简单。

### Insight 2: Snapshot 原子快照解决 TOCTOU 竞态

**代码位置**: `cliprovider.go:40-66 (Snapshot)`

**是什么**: 通过一次性在同一把锁下读取所有相关字段，避免分两次读取导致的状态不一致。

**代码示例**:
```go
// 问题：分两次读取，两次锁之间状态可能被其他 goroutine 修改
id := h.SessionID()       // 加锁读 id，解锁
first := h.IsFirstMessage() // 加锁读 first，解锁
// id 和 first 可能来自不同时刻！

// 解决：Snapshot 一次性读取
type SessionSnapshot struct {
    ID           string
    FirstMessage bool
    Name         string
}

func (h *CliSessionHelper) Snapshot() SessionSnapshot {
    h.mu.Lock()
    defer h.mu.Unlock()
    return SessionSnapshot{
        ID:           h.sessionID,    // 同一把锁下
        FirstMessage: h.firstMessage, // 一次性读完
        Name:         h.sessionName,
    }
}
```

**设计意图**: 防止并发读写场景下多个字段之间出现不一致的状态（TOCTOU 竞态）。

**优点**: 值类型快照一旦取到即不可变，消除了"线程 A 读了 `id="abc"`，线程 B 清空 `id`，线程 A 再读 `first=true`"这种矛盾状态。

**局限性**: 每次读取都要拿锁，在高频读取场景下可能成为瓶颈。快照只保证读一致性，不保证写入之间的事务性——两个 goroutine 可以各自读到一致快照，但基于快照做出的写入可能冲突。此外，如果未来字段增多，Snapshot 结构体可能膨胀。

**适用场景**: 多个字段有逻辑关联（如"会话 ID"和"是否首次消息"必须一致）且存在并发读写时值得采用；如果字段之间无逻辑关联，或只有单一 goroutine 读写，Snapshot 模式增加了不必要的复杂度。

### Insight 3: stdin pipe 传递 prompt 避免参数过长

**代码位置**: `claudecode.go:118-145 (buildArgs)`, `claudecode.go:201-207 (runClaude)`

**是什么**: CLI 调用时通过 stdin 而非命令行参数传递 prompt 内容，用 `-p -` 参数告诉 CLI 从 stdin 读取。

**代码示例**:
```go
// buildArgs: 用 "-p" "-" 告诉 CLI 从 stdin 读取
args := []string{"-p", "-", "--strict-mcp-config"}

// runClaude: 通过 stdin 传入 prompt
cmd := exec.CommandContext(ctx, "claude", args...)
cmd.Stdin = strings.NewReader(prompt)  // prompt 可能有几百 KB
```

**设计意图**: 绕过操作系统对 `execve()` 参数长度的硬限制（Linux `MAX_ARG_STRLEN` 约 128KB 单参数，总长度约 2MB；macOS 约 256KB）。

**优点**: stdin 没有大小限制，且不会在 `ps` 进程列表中泄露 prompt 内容（可能包含敏感源代码）。

**局限性**: stdin 是单向管道，无法在子进程执行过程中追加输入。如果子进程同时需要大量 stdin 写入和 stdout/stderr 读取，可能遇到管道缓冲区死锁（需要用 goroutine 并发读写避免）。此外，超大 prompt 还需要额外的临时文件方案（`promptfile.go`），增加了清理逻辑的复杂度。

**适用场景**: 通过 `os/exec` 调用外部命令且输入数据量不可控时值得采用；如果数据量始终很小（<几KB），直接用命令行参数更简单直观。

### Insight 4: 流式增量文本提取 diffAssistantText

**代码位置**: `claudecode.go:426-436 (diffAssistantText)`

**是什么**: Claude CLI 的流式事件中，每条 `assistant` 消息包含的是"截至目前的完整文本"而非增量，需要通过前后对比提取新增部分。

**代码示例**:
```go
func diffAssistantText(previous, current string) string {
    current = strings.TrimSpace(current)
    previous = strings.TrimSpace(previous)
    if current == "" || current == previous {
        return ""
    }
    if previous != "" && strings.HasPrefix(current, previous) {
        return current[len(previous):]  // 只返回新增的部分
    }
    return current  // 无前缀关系时返回完整文本
}
```

**设计意图**: 将 Claude CLI 的"全量快照"流式事件转换为用户需要的"增量"输出，避免 UI 显示重复内容。

**优点**: 通过简单的 `strings.HasPrefix` 前缀比较实现全量→增量转换，代码只有 10 行，逻辑清晰。

**局限性**: `strings.HasPrefix` 假设新文本总是在旧文本后追加。如果 CLI 在某些情况下重写之前的内容（如纠正错别字、重新格式化），会导致增量提取失败，回退到返回完整文本（用户看到重复输出）。对于非 ASCII 文本，`current[len(previous):]` 的字节切片可能在多字节 UTF-8 字符中间断开。

**适用场景**: 数据源提供全量快照但消费方需要增量、且数据只追加不修改时值得采用；如果数据源可能回退或修改之前的内容，需要更复杂的 diff 算法（如 Myers diff）。

### Insight 5: 鸭子类型实现可选接口 SetCwdIfSupported

**代码位置**: `provider.go:41-48 (SetCwdIfSupported)`

**是什么**: 通过函数内定义的匿名接口和类型断言，实现"如果 provider 支持设置工作目录就设置，不支持就跳过"的可选能力。

**代码示例**:
```go
func SetCwdIfSupported(p AIProvider, cwd string) {
    type cwdSetter interface {
        SetCwd(string)
    }
    if cs, ok := p.(cwdSetter); ok {
        cs.SetCwd(cwd)
    }
}
```

**设计意图**: 让 CLI 类 provider 可以设置工作目录，同时不强制 API 类 provider 实现这个无意义的方法。

**优点**: 核心接口 `AIProvider` 保持精简（3 个方法），调用方通过 `SetCwdIfSupported()` 安全地按需使用。匿名接口定义在函数内部，不会污染包级别的命名空间。

**局限性**: 接口契约变成隐式的——IDE 无法自动提示 `SetCwd` 方法的存在，新开发者不看 `SetCwdIfSupported` 函数就不知道有这个能力。Go 标准库的 `io.WriterTo`、`http.Flusher` 等可选接口至少是包级导出的，而这里的匿名接口连类型名都没有，可发现性更差。

**适用场景**: 实现者中只有部分需要某能力、且该能力不值得扩展核心接口时值得采用；如果所有实现者都需要该能力，或可选能力数量 > 2-3 个导致代码中充斥着类型断言，应考虑拆分为多个显式接口。
