# orchestrator - 辩论式代码审查编排器

核心调度引擎，协调多个 AI 审查者进行多轮对抗式代码审查。完整流程：并行预处理 -> 多轮辩论 -> 收敛检测 -> 总结与问题提取。

## 文件说明

| 文件 | 职责 |
|------|------|
| `orchestrator.go` | `DebateOrchestrator` 主流程：三阶段编排算法、消息构建、会话管理、Token 追踪 |
| `types.go` | 所有数据类型和接口定义（`Reviewer`, `DebateMessage`, `DebateResult`, `ReviewIssue`, `MergedIssue` 等） |
| `issueparser.go` | JSON 问题解析（`ParseReviewerOutput`）+ Jaccard 相似度去重（`DeduplicateIssues`）+ 调用链格式化 |
| `issueparser_test.go` | 问题解析、去重、Jaccard 相似度、调用链格式化的单元测试 |
| `structurize_test.go` | `structurizeIssues` 的集成测试（mock provider + 重试逻辑验证） |

## 完整类型层级

```
DebateOrchestrator (核心编排器)
├── reviewers: []Reviewer                       // 参与辩论的审查者列表
│   └── Reviewer
│       ├── ID: string                          // e.g. "security-reviewer", "perf-reviewer"
│       ├── Provider: AIProvider                // claude-code / gpt-4o / deepseek / ...
│       └── SystemPrompt: string               // 角色系统提示词，定义审查者的专业视角
├── analyzer: Reviewer                          // 预分析器 -- 在辩论前对 diff 做初步分析
├── summarizer: Reviewer                        // 总结器 -- 负责收敛判断 + 最终结论 + 结构化提取
├── contextGatherer: ContextGathererInterface   // 上下文收集器（nil = 禁用）
│   └── Gather(diff, prNumber, baseBranch) -> *GatheredContext
│       └── GatheredContext
│           ├── Summary: string                 // 变更摘要文本
│           ├── RawReferences: []RawReference   // 符号引用关系
│           │   └── RawReference
│           │       ├── Symbol: string          // e.g. "HandleLogin"
│           │       └── FoundInFiles: []ReferenceLocation
│           │           └── ReferenceLocation
│           │               ├── File: string    // e.g. "main.go"
│           │               ├── Line: int       // e.g. 15
│           │               └── Content: string // e.g. "router.POST(\"/login\", HandleLogin)"
│           ├── AffectedModules: []AffectedModule
│           │   └── AffectedModule
│           │       ├── Name: string            // e.g. "auth"
│           │       ├── Path: string            // e.g. "internal/auth"
│           │       ├── AffectedFiles: []string // e.g. ["handler.go", "middleware.go"]
│           │       └── ImpactLevel: string     // "core" | "moderate" | "peripheral"
│           └── RelatedPRs: []RelatedPR
│               └── RelatedPR
│                   ├── Number: int             // e.g. 38
│                   └── Title: string           // e.g. "Refactor auth middleware"
├── options: OrchestratorOptions
│   ├── MaxRounds: int                          // 最大辩论轮数, e.g. 2
│   └── CheckConvergence: bool                  // 是否启用收敛检测, e.g. true
├── conversationHistory: []DebateMessage        // 全局对话历史（所有轮次、所有审查者）
├── tokenUsage: map[string]*tokenCount          // 每个审查者/角色的 Token 消耗
│   └── tokenCount
│       ├── input: int                          // 累计输入 Token 数
│       └── output: int                         // 累计输出 Token 数
├── analysis: string                            // 预分析器的输出结果
├── gatheredContext: *GatheredContext            // 上下文收集器的输出结果
├── taskPrompt: string                          // 包含 diff 的原始任务提示词
├── lastSeenIndex: map[string]int               // 每个审查者最后看到的消息偏移
└── mu: sync.Mutex                              // 保护 tokenUsage 的并发锁
```

## 核心流程：RunStreaming

```
RunStreaming(ctx, label, prompt, display) -> (*DebateResult, error)
  │
  ├─ 重置状态: conversationHistory=nil, tokenUsage={}, lastSeenIndex={}, analysis="", gatheredContext=nil
  ├─ 启动会话: 为所有 reviewer + analyzer + summarizer 启动 SessionProvider 会话
  ├─ defer endAllSessions()
  │
  ══════ 阶段 1: 并行预处理 (errgroup) ══════
  │
  ├─ goroutine A: 上下文收集 (仅当 contextGatherer != nil)
  │   ├─ display.OnWaiting("context-gatherer")
  │   ├─ extractDiffFromPrompt(prompt) -> 从 ```diff 代码块提取纯 diff
  │   ├─ contextGatherer.Gather(diff, label, "main")
  │   │   -> 调用链分析 + PR/MR 历史 + 受影响模块
  │   └─ 失败时: 静默忽略 (非致命错误), gatheredContext 保持 nil
  │
  ├─ goroutine B: 预分析
  │   ├─ display.OnWaiting("analyzer")
  │   ├─ analyzer.Provider.ChatStream(ctx, [{role:"user", content:prompt}], analyzer.SystemPrompt)
  │   │   -> 流式接收分析结果, 实时回调 display.OnMessage("analyzer", chunk)
  │   ├─ o.analysis = 完整分析文本
  │   └─ trackTokens("analyzer", input, output)
  │
  ├─ errgroup.Wait() -> 等待两个 goroutine 完成
  └─ display.OnContextGathered(gatheredContext) // 通知 UI
  │
  ══════ 阶段 2: 多轮辩论 (for round = 1..MaxRounds) ══════
  │
  ├─ 消息构建 (快照模式, 确保同一轮所有审查者看到相同信息):
  │   └─ for each reviewer: tasks[i] = { reviewer, buildMessages(reviewer.ID) }
  │
  ├─ 初始化状态追踪: statuses[i] = { ReviewerID, Status: "pending" }
  ├─ display.OnWaiting("round-N")
  ├─ display.OnParallelStatus(round, statuses)
  │
  ├─ 并行执行 (errgroup):
  │   └─ for each task (独立 goroutine):
  │       ├─ statuses[i].Status = "thinking", StartTime = now()
  │       ├─ display.OnParallelStatus(round, copyStatuses(statuses))
  │       ├─ reviewer.Provider.ChatStream(ctx, messages, systemPrompt)
  │       │   -> 流式接收, 逐块拼接到 StringBuilder
  │       ├─ statuses[i].Status = "done", EndTime = now(), Duration = (EndTime-StartTime)/1000
  │       └─ display.OnParallelStatus(round, copyStatuses(statuses))
  │
  ├─ 结果收集 (在所有审查者完成后统一处理):
  │   └─ for each result:
  │       ├─ trackTokens(reviewer.ID, input, output)
  │       ├─ conversationHistory.append(DebateMessage{reviewerID, content, timestamp})
  │       ├─ markAsSeen(reviewer.ID)
  │       └─ display.OnMessage(reviewer.ID, fullResponse)
  │
  ├─ 收敛检测 (仅当 CheckConvergence=true && round >= 2 && round < MaxRounds):
  │   ├─ display.OnWaiting("convergence-check")
  │   ├─ checkConvergence(ctx, display)
  │   │   ├─ 取最后一轮所有审查者消息
  │   │   ├─ 构建严格共识判断提示词 -> summarizer.Provider.Chat()
  │   │   ├─ 解析最后一行: "CONVERGED" 或 "NOT_CONVERGED"
  │   │   └─ display.OnConvergenceJudgment(verdict, reasoning)
  │   └─ 如果 CONVERGED: convergedAtRound = round, break
  │
  └─ display.OnRoundComplete(round, converged)
  │
  ══════ 阶段 3: 总结与问题提取 ══════
  │
  ├─ display.OnWaiting("summarizer")
  │
  ├─ collectSummaries(ctx):
  │   └─ for each reviewer:
  │       ├─ buildMessages(reviewer.ID) + append("Please summarize your key points...")
  │       └─ reviewer.Provider.Chat() -> DebateSummary{ReviewerID, Summary}
  │
  ├─ getFinalConclusion(ctx, summaries):
  │   ├─ 将所有审查者的匿名总结拼接
  │   ├─ 提示词: "Based on their anonymous summaries, provide: consensus, disagreements, action items"
  │   └─ summarizer.Provider.Chat() -> finalConclusion 文本
  │
  ├─ summarizer.EndSession() // 结束会话, 避免后续 JSON 提取受上下文干扰
  │
  ├─ structurizeIssues(ctx, display):
  │   ├─ 收集每个 reviewer 的最后一条消息 (lastMessages)
  │   ├─ 构建 JSON 提取提示词 (包含完整的 schema 说明)
  │   ├─ summarizer.Provider.Chat() -> 尝试解析 JSON
  │   ├─ 失败重试 (最多 3 次), 重试时使用更明确的格式提示
  │   ├─ ParseReviewerOutput(response) -> ReviewerOutput{Issues, Verdict, Summary}
  │   └─ DeduplicateMergedIssues(issues) -> []MergedIssue
  │
  └─ 返回 DebateResult { PRNumber, Analysis, Context, Messages, Summaries,
                          FinalConclusion, TokenUsage, ConvergedAtRound, ParsedIssues }
```

## 角色分工

```
角色              调用次数                    作用
─────────────────────────────────────────────────────────────────
analyzer          1 次                       预分析 diff，提取审查关注重点
contextGatherer   1 次                       收集调用链引用、历史 PR、项目文档
reviewers (N个)   N × 辩论轮数               多轮辩论，互相挑战对方观点
summarizer        2~4 次                     ① 共识检测 ② 最终结论 ③ 问题结构化（含重试）
```

其中 summarizer 的调用次数取决于：
- 共识检测：每次检测 1 次（从第 2 轮到倒数第 2 轮，每轮可能触发一次）
- 最终结论：1 次（`getFinalConclusion`）
- 问题结构化：1~3 次（`structurizeIssues`，JSON 解析失败时重试）

## 关键设计点

### 快照式消息构建

在每轮辩论开始时，先为**所有审查者**一次性构建好消息（`orchestrator.go:213-225`），然后再并行执行。这样保证同一轮的所有审查者看到的是**完全相同的信息**——先完成的审查者的输出不会影响后完成的审查者的输入。

```go
// 先快照，再执行
tasks := make([]reviewerTask, len(o.reviewers))
for i, r := range o.reviewers {
    tasks[i] = reviewerTask{
        reviewer: r,
        messages: o.buildMessages(r.ID),  // 快照当前历史
    }
}
// ... 然后并行执行 tasks
```

如果边执行边构建消息，审查者 A 的输出可能出现在审查者 B 的输入中，导致同一轮内信息不对称。

### 共识检测使用全量消息

`checkConvergence` 将**所有已完成轮次**（而非仅最后一轮）的完整辩论记录发给总结器（`orchestrator.go:567-576`）。这避免了仅看最后一轮导致的误判——某些分歧可能在早期轮次提出但在后续轮次中被忽略而非被解决。

### structurizeIssues 收集所有轮次

`structurizeIssues` 收集每个审查者在**所有轮次**的消息（`orchestrator.go:734-739`），而非只取最后一条。这是因为审查者可能在第 1 轮发现问题 A，第 2 轮转而讨论问题 B，如果只看最后一轮就会丢失问题 A。

## DebateMessage 示例

辩论对话中的单条消息记录：

```json
{
  "reviewerId": "security-reviewer",
  "content": "## Security Review\n\n### 1. SQL Injection in `handler.go:42`\n\nThe query uses string concatenation instead of parameterized queries, allowing SQL injection attacks.\n\n```go\n// Vulnerable code\nquery := \"SELECT * FROM users WHERE name='\" + name + \"'\"\n```\n\n### 2. Missing CSRF Protection\n\nThe POST endpoint `/api/transfer` lacks CSRF token validation...",
  "timestamp": "2024-01-15T10:30:00Z"
}
```

## buildMessages 输入/输出示例

`buildMessages` 根据当前轮次和会话模式为每个审查者构建不同的消息列表。

### Round 1 (独立审查)

每个审查者独立审查代码，不受其他审查者影响。消息中包含：任务 diff + 上下文 + 预分析结果 + 关注重点。

```
messages = [
  {
    Role: "user",
    Content: "Task: Please review https://github.com/org/repo/pull/42

              ```diff
              --- a/internal/handler/auth.go
              +++ b/internal/handler/auth.go
              @@ -40,6 +40,8 @@ func HandleLogin(w http.ResponseWriter, r *http.Request) {
                   name := r.FormValue(\"username\")
              -    rows, err := db.Query(\"SELECT * FROM users WHERE name='\" + name + \"'\")
              +    query := \"SELECT * FROM users WHERE name='\" + name + \"'\"
              +    rows, err := db.Query(query)
              ```

              ## System Context
              This PR modifies the authentication module. It affects the login handler
              and related middleware. Impact level: core.

              The analyzer suggests focusing on: SQL injection risk in handler.go;
              Missing error handling in service.go.
              These are suggestions -- also flag anything else you notice beyond these areas.

              ## Call Chain Context

              ### Callers of `HandleLogin`
              Found in 2 locations:

              1. main.go:15
                 > router.POST(\"/login\", HandleLogin)

              2. auth_test.go:42
                 > handler := http.HandlerFunc(HandleLogin)

              Here is the analysis:

              ## Changes Summary
              This PR modifies the login handler to extract query construction...

              ## Suggested Review Focus
              - SQL injection risk in handler.go
              - Missing error handling in service.go

              You are [security-reviewer]. Review EVERY changed file and EVERY changed
              function/block -- do not skip any.
              For each change, check: correctness, security, performance, error handling,
              edge cases, maintainability.
              If you reviewed a file and found no issues, say so briefly. Do not stop early."
  }
]
```

### Round 2+ (交叉审查)

审查者可以看到其他人在上一轮的反馈，进行交叉审查和辩论。

**会话模式** (支持 `SessionProvider` 的 AI 提供者，如 claude-code)：仅发送增量消息。

```
messages = [
  {
    Role: "user",
    Content: "You are [security-reviewer]. Here's what others said in the previous round:

              [perf-reviewer]: ## Performance Review

              ### 1. N+1 Query Pattern
              The login handler executes a separate query for each user permission check.
              This will cause performance degradation under load...

              ### 2. Missing Connection Pooling
              The database connection is created per-request without pooling...

              ---

              Do three things:
              1. Continue your own exhaustive review -- are there changed files or
                 functions you haven't covered yet? Cover them now.
              2. Point out what the other reviewers MISSED -- which files or changes
                 did they skip or gloss over?
              3. Respond to their points -- agree where valid, challenge where you disagree."
  }
]
```

**非会话模式** (无状态 API，如 OpenAI)：发送完整上下文，包含所有历史轮次。

```
messages = [
  {
    Role: "user",
    Content: "Task: [完整 diff]...\n\nHere is the analysis:\n[预分析结果]...\n\n
              You are [security-reviewer] in a code review debate with [perf-reviewer].\n
              Your shared goal: find ALL real issues...\n\n
              Previous rounds discussion:"
  },
  {
    Role: "user",
    Content: "[perf-reviewer]: ## Performance Review\n### 1. N+1 Query Pattern..."
  },
  {
    Role: "assistant",
    Content: "[security-reviewer 自己在 Round 1 的输出]"    // 作为 assistant 角色维持对话连贯性
  }
]
```

### 消息角色分配规则

AI 对话模型只认三种角色：`system`、`user`、`assistant`。辩论中有多个审查者，但对每个审查者的 AI 来说，世界观是：

```
system:    系统提示词（"You are a security-focused code reviewer..."）
user:      人类 + 其他所有审查者的发言 → 统一作为"外部输入"
assistant: 只有自己之前的发言 → 维持"我说过什么"的连贯性
```

**非会话模式下的消息构建**（`buildFullContextDebateMessages`，`orchestrator.go:525-545`）：

```go
// 其他审查者的消息 → user 角色
for _, msg := range previousRoundsMessages {
    messages = append(messages, provider.Message{
        Role:    "user",        // ← 其他人都是 user
        Content: fmt.Sprintf("[%s]: %s", msg.ReviewerID, msg.Content),
    })
}

// 自己之前的消息 → assistant 角色
for _, m := range o.conversationHistory {
    if m.ReviewerID == currentReviewerID {
        messages = append(messages, provider.Message{
            Role:    "assistant",   // ← 只有自己是 assistant
            Content: m.Content,
        })
    }
}
```

**会话模式下的消息构建**（`buildSessionDebateMessages`，`orchestrator.go:490-501`）：

```go
// 所有其他人的新消息拼成一条 → user 角色
var parts []string
for _, m := range newMessages {
    parts = append(parts, fmt.Sprintf("[%s]: %s", m.ReviewerID, m.Content))
}
// 整条作为 user 发送（自己的历史已在 CLI 会话中）
return []provider.Message{{Role: "user", Content: p}}
```

**为什么其他审查者不能是 assistant 角色？**

如果把 perf-reviewer 的消息也标为 `assistant`，security-reviewer 的 AI 会认为那些话是**自己说过的**，就无法形成"对方提出了什么观点，我需要回应"的辩论效果。通过 `[perf-reviewer]: ...` 前缀区分身份，用 `user` 角色表示"这是来自外部的输入"。

以第 2 轮 security-reviewer 的视角为例：

```
messages = [
  {role: "user",      content: "Task: [diff]...\nYou are [security-reviewer]..."},
  {role: "user",      content: "[perf-reviewer]: N+1 查询问题..."},    ← 对方观点，需要回应
  {role: "assistant", content: "SQL 注入漏洞..."},                     ← 我第1轮说的，保持一致
]

AI 的理解：
  "我之前说了 SQL 注入的事（assistant），现在有人（user）提出了 N+1 查询问题，
   我需要回应他的观点并补充新的发现。"
```

## ReviewIssue JSON 示例

AI 从审查文本中提取的单个结构化问题（`structurizeIssues` 输出的原始格式）：

```json
{
  "severity": "high",
  "category": "security",
  "file": "internal/handler/auth.go",
  "line": 42,
  "endLine": 45,
  "title": "SQL injection vulnerability in login handler",
  "description": "The query uses string concatenation to build SQL, allowing attackers to inject arbitrary SQL commands.\n\n**Problematic code:**\n```go\nquery := \"SELECT * FROM users WHERE name='\" + name + \"'\"\nrows, err := db.Query(query)\n```\n\n**Suggested fix:**\n```go\nquery := \"SELECT * FROM users WHERE name=$1\"\nrows, err := db.Query(query, name)\n```\n\nUsing parameterized queries prevents SQL injection by separating code from data.",
  "suggestedFix": "Use parameterized queries instead of string concatenation",
  "codeSnippet": "query := \"SELECT * FROM users WHERE name='\" + name + \"'\"",
  "raisedBy": ["security-reviewer", "perf-reviewer"]
}
```

**字段说明：**

| 字段 | 类型 | 必填 | 说明 |
|------|------|:----:|------|
| `severity` | string | 是 | 严重程度，见下方常量表 |
| `category` | string | 否 | 分类，默认 `"general"`，常见值: `security`, `performance`, `error-handling`, `style`, `correctness`, `architecture` |
| `file` | string | 是 | 文件路径 |
| `line` | *int | 否 | 起始行号 |
| `endLine` | *int | 否 | 结束行号（必须 >= line） |
| `title` | string | 是 | 一行摘要 |
| `description` | string | 是 | 详细 Markdown 描述（会作为 PR 评论发布） |
| `suggestedFix` | string | 否 | 简短修复建议 |
| `codeSnippet` | string | 否 | 问题代码片段 |
| `raisedBy` | []string | 否 | 提出该问题的审查者 ID 列表 |

## MergedIssue 示例

经过去重合并后的问题。当多个审查者提出相似问题时，合并为一个 `MergedIssue`：

```json
{
  "severity": "high",
  "category": "security",
  "file": "internal/handler/auth.go",
  "line": 42,
  "endLine": 45,
  "title": "SQL injection vulnerability in login handler",
  "description": "The query uses string concatenation to build SQL, allowing attackers to inject arbitrary SQL commands.",
  "suggestedFix": "Use parameterized queries instead of string concatenation",
  "raisedBy": ["security-reviewer", "perf-reviewer"],
  "descriptions": [
    "The query uses direct string concatenation allowing SQL injection. An attacker can pass a crafted username like `' OR 1=1 --` to bypass authentication.",
    "String concatenation in SQL query creates injection risk and also prevents query plan caching, causing both security and performance issues."
  ]
}
```

`MergedIssue` 嵌入了 `ReviewIssue` 的所有字段，并额外增加：

| 字段 | 说明 |
|------|------|
| `raisedBy` | 所有提出过此问题的审查者 ID（覆盖内嵌的同名字段） |
| `descriptions` | 所有审查者对此问题的原始描述（用于生成更全面的评论） |

## ReviewerOutput 示例

从审查者响应文本中解析出的完整结构化输出（`ParseReviewerOutput` 的返回值）：

```json
{
  "issues": [
    {
      "severity": "high",
      "category": "security",
      "file": "internal/handler/auth.go",
      "line": 42,
      "title": "SQL injection vulnerability in login handler",
      "description": "The query uses string concatenation instead of parameterized queries..."
    },
    {
      "severity": "medium",
      "category": "error-handling",
      "file": "internal/service/user.go",
      "line": 15,
      "title": "Unhandled error from database query",
      "description": "The error returned by db.QueryRow is silently discarded, which may hide database failures..."
    }
  ],
  "verdict": "request_changes",
  "summary": "Found 1 high severity security issue (SQL injection) and 1 medium error handling issue. Recommend blocking merge until SQL injection is fixed."
}
```

## DebateResult 示例

`RunStreaming` 的最终输出，汇总整个辩论过程的所有数据：

```json
{
  "prNumber": "42",
  "analysis": "## Changes Summary\nThis PR modifies the login handler to extract query construction into a separate variable.\n\n## Suggested Review Focus\n- SQL injection risk in handler.go due to string concatenation\n- Missing error handling in service.go QueryRow call",
  "context": {
    "summary": "This PR modifies the authentication module (core). Affects 2 files in internal/handler.",
    "rawReferences": [
      {
        "symbol": "HandleLogin",
        "foundInFiles": [
          {"file": "main.go", "line": 15, "content": "router.POST(\"/login\", HandleLogin)"},
          {"file": "auth_test.go", "line": 42, "content": "handler := http.HandlerFunc(HandleLogin)"}
        ]
      }
    ],
    "affectedModules": [
      {"name": "auth", "path": "internal/handler", "affectedFiles": ["auth.go"], "impactLevel": "core"}
    ],
    "relatedPRs": [
      {"number": 38, "title": "Refactor auth middleware"}
    ]
  },
  "messages": [
    {"reviewerId": "security-reviewer", "content": "## Security Review\n\n### 1. SQL Injection...", "timestamp": "2024-01-15T10:30:00Z"},
    {"reviewerId": "perf-reviewer", "content": "## Performance Review\n\n### 1. N+1 Query...", "timestamp": "2024-01-15T10:30:05Z"},
    {"reviewerId": "security-reviewer", "content": "## Round 2 Response\n\nI agree with perf-reviewer about N+1...", "timestamp": "2024-01-15T10:31:00Z"},
    {"reviewerId": "perf-reviewer", "content": "## Round 2 Response\n\nThe SQL injection issue raised by security-reviewer is critical...", "timestamp": "2024-01-15T10:31:03Z"}
  ],
  "summaries": [
    {"reviewerId": "security-reviewer", "summary": "Key findings: 1 critical SQL injection vulnerability in auth handler, 1 missing CSRF protection. Recommend blocking merge."},
    {"reviewerId": "perf-reviewer", "summary": "Key findings: N+1 query pattern in login flow, missing connection pooling. Also confirmed SQL injection risk found by other reviewer."}
  ],
  "finalConclusion": "## Points of Consensus\n- Both reviewers agree SQL injection in auth.go:42 is the highest priority issue\n- Both confirm missing error handling in service.go needs attention\n\n## Points of Disagreement\n- None significant\n\n## Recommended Actions\n1. [CRITICAL] Fix SQL injection - use parameterized queries\n2. [HIGH] Add error handling for db.QueryRow\n3. [MEDIUM] Implement connection pooling",
  "tokenUsage": [
    {"reviewerId": "analyzer", "inputTokens": 3200, "outputTokens": 800, "estimatedCost": 0.04},
    {"reviewerId": "security-reviewer", "inputTokens": 5200, "outputTokens": 1800, "estimatedCost": 0.07},
    {"reviewerId": "perf-reviewer", "inputTokens": 5200, "outputTokens": 1500, "estimatedCost": 0.067},
    {"reviewerId": "summarizer", "inputTokens": 4800, "outputTokens": 1200, "estimatedCost": 0.06}
  ],
  "convergedAtRound": 2,
  "parsedIssues": [
    {
      "severity": "high",
      "category": "security",
      "file": "internal/handler/auth.go",
      "line": 42,
      "title": "SQL injection vulnerability in login handler",
      "description": "...",
      "raisedBy": ["security-reviewer", "perf-reviewer"],
      "descriptions": ["...", "..."]
    }
  ]
}
```

## structurizeIssues 详细流程

从所有审查者的自由文本审查结果中提取结构化的 JSON 问题列表。整个流程：收集全量消息 → 构建提示词 → AI 提取 → 三层验证 → 去重 → 失败重试。

### 流程图

```
structurizeIssues(ctx, display)
  │
  ├─ 1. 收集全量消息
  │   ├─ 遍历 conversationHistory，按 reviewerID 分组
  │   ├─ 跳过 ReviewerID == "user" 的消息
  │   └─ allMessages = map[reviewerID] → []content（按轮次顺序）
  │
  ├─ 2. 构建审查文本
  │   ├─ 单轮: "[security-reviewer]:\n<内容>"
  │   ├─ 多轮: "[security-reviewer] Round 1:\n<内容>\n\n[security-reviewer] Round 2:\n<内容>"
  │   └─ 多个审查者用 "---" 分隔
  │
  ├─ 3. 渲染提示词模板
  │   ├─ structurize_issues.tmpl（首次尝试）
  │   │   包含: reviewText + reviewerIDs + JSON Schema
  │   └─ structurize_system.tmpl → "You extract structured issues from code review text. Output only valid JSON."
  │
  ├─ 4. 重试循环 (最多 3 次)
  │   │
  │   ├─ attempt 1: 使用 basePrompt（structurize_issues.tmpl）
  │   ├─ attempt 2+: 使用 retryPrompt（structurize_retry.tmpl）
  │   │   额外包含上次的 ValidationErrors
  │   │
  │   ├─ summarizer.Provider.Chat(ctx, msgs, systemPrompt, {DisableTools: true})
  │   │   注意: DisableTools=true 防止 CLI 提供者调用工具，确保只输出 JSON
  │   │
  │   ├─ ParseReviewerOutput(response) → 三层验证:
  │   │   ├─ 第1层: JSON 提取（```json 代码块 或 原始 JSON 对象）
  │   │   ├─ 第2层: JSON Schema 校验（schema.ValidateIssuesJSON）
  │   │   └─ 第3层: 逐条手动验证（severity有效? file非空? title非空? description非空?）
  │   │
  │   ├─ 验证通过 → DeduplicateMergedIssues() → return
  │   │
  │   └─ 验证失败 → 记录错误到 lastValidationErrors，用于下次重试
  │       ├─ ParseError (JSON 语法错误) → "JSON parse error: ..."
  │       ├─ SchemaErrors → schema.FormatErrorsForRetry(vr)
  │       └─ 0 issues → "JSON was valid but contained 0 issues..."
  │
  ├─ 5. bestEffort 兜底
  │   └─ 3 次都失败时，返回历次尝试中提取到最多问题的结果
  │
  └─ 6. 全部失败 → return nil
```

### 具体例子

假设 2 个审查者完成了 2 轮辩论，`conversationHistory` 有 4 条消息：

**第 1 步：收集全量消息**

```go
allMessages = {
    "perf-reviewer":     ["第1轮: N+1查询问题...", "第2轮: 同意SQL注入..."],
    "security-reviewer": ["第1轮: SQL注入漏洞...", "第2轮: 补充CSRF问题..."],
}
```

注意：收集**所有轮次**的消息，而非只取最后一轮。因为审查者可能在第 1 轮发现问题 A，第 2 轮讨论问题 B，只取最后一轮会丢失问题 A。

**第 2 步：构建审查文本**

```
[perf-reviewer] Round 1:
## Performance Review
### 1. N+1 Query Pattern
The login handler executes a separate query for each permission check...

[perf-reviewer] Round 2:
## Round 2 Response
I agree with the SQL injection finding. Additionally, the query lacks connection pooling...

---

[security-reviewer] Round 1:
## Security Review
### 1. SQL Injection in auth.go:42
The query uses string concatenation...

[security-reviewer] Round 2:
## Round 2 Response
### 2. Missing CSRF Protection
The POST endpoint /api/transfer lacks CSRF token...
```

**第 3 步：AI 提取（假设第 1 次就成功）**

AI 收到上述文本 + JSON Schema，返回：

```json
{
  "issues": [
    {
      "severity": "high",
      "file": "internal/handler/auth.go",
      "line": 42,
      "title": "SQL injection vulnerability",
      "description": "String concatenation in SQL query allows injection attacks",
      "raisedBy": ["security-reviewer", "perf-reviewer"]
    },
    {
      "severity": "medium",
      "file": "internal/handler/auth.go",
      "line": 55,
      "title": "N+1 query pattern in permission check",
      "description": "Each permission check triggers a separate database query",
      "raisedBy": ["perf-reviewer"]
    },
    {
      "severity": "medium",
      "file": "internal/handler/transfer.go",
      "line": 12,
      "title": "Missing CSRF protection",
      "description": "POST endpoint lacks CSRF token validation",
      "raisedBy": ["security-reviewer"]
    }
  ],
  "verdict": "request_changes",
  "summary": "Found 1 high and 2 medium severity issues"
}
```

**第 4 步：三层验证**

```
第1层 JSON 提取: ✅ 找到 ```json 代码块
第2层 Schema 校验: ✅ 所有必填字段存在，severity 值合法
第3层 手动验证:
  issue[0]: severity="high" ✅, file="internal/handler/auth.go" ✅, title 非空 ✅, description 非空 ✅ → 保留
  issue[1]: severity="medium" ✅, file="internal/handler/auth.go" ✅, title 非空 ✅, description 非空 ✅ → 保留
  issue[2]: severity="medium" ✅, file="internal/handler/transfer.go" ✅, title 非空 ✅, description 非空 ✅ → 保留
```

**第 5 步：去重**

3 个问题在不同文件或行号差异大，不触发合并 → 直接返回 3 个 `MergedIssue`。

### 重试场景

如果 AI 第 1 次返回了不合法的 JSON：

```
attempt 1:
  AI 返回: "Here are the issues: {issues: [...]}"  ← JSON 语法错误（key 没引号）
  ParseError: "JSON syntax error: invalid character 'i'"
  lastValidationErrors = "JSON parse error: invalid character 'i'..."

attempt 2:
  使用 structurize_retry.tmpl，额外包含:
    "Your previous attempt had these errors:
     JSON parse error: invalid character 'i'...
     Please fix these issues and try again."
  AI 修正后返回合法 JSON
  → 验证通过 → 返回结果
```

### bestEffort 兜底

```
attempt 1: 解析出 3 个 issue，但 schema 校验发现 issue[2] 缺少 description
           手动验证: 保留 2 个有效 issue → bestEffort = [2 issues]
           但因为有 schema errors → 继续重试

attempt 2: AI 返回 Chat error（API 超时）
           bestEffort 仍为 [2 issues]

attempt 3: AI 返回合法 JSON，但只有 1 个 issue
           1 < 2 → bestEffort 不更新

→ 3 次都没完美成功，返回 bestEffort（2 issues）而非 nil
```

### ParseReviewerOutput 验证管线

```
response (原始文本)
  │
  ├─ 第1层: JSON 提取
  │   ├─ 优先: 匹配 ```json ... ``` 代码块 (jsonFenceRe)
  │   ├─ 回退: 匹配包含 "issues": [...] 的 JSON 对象 (rawJSONRe)
  │   └─ 都找不到 → ParseError: "no JSON block found"
  │
  ├─ 第2层: JSON Schema 校验 (schema.ValidateIssuesJSON)
  │   ├─ 校验 issues 数组中每个元素的必填字段: severity, file, title, description
  │   ├─ 校验 severity 枚举值: critical|high|medium|low|nitpick
  │   └─ 校验失败 → SchemaErrors（但仍继续尝试手动解析）
  │
  ├─ 第3层: 逐条手动验证
  │   ├─ json.Unmarshal 为 map[string]interface{}（灵活解析，容忍额外字段）
  │   ├─ severity 必须在 validSeverities 中 → 否则跳过
  │   ├─ file 非空 → 否则跳过
  │   ├─ 从 file 中提取嵌入行号: "auth.go:42-45" → file="auth.go", line=42, endLine=45
  │   ├─ title 非空 → 否则跳过
  │   ├─ description 非空 → 否则跳过
  │   ├─ category 空 → 默认 "general"
  │   ├─ verdict 无效 → 默认 "comment"
  │   └─ 可选字段: line, endLine, suggestedFix, codeSnippet, raisedBy
  │
  └─ 返回 ParseResult { Output, RawJSON, SchemaErrors, ParseError }
```

第 2 层和第 3 层是**互补的**：Schema 校验能发现结构性错误（缺失字段、类型错误），手动验证则提供更细粒度的容错（跳过单个无效 issue 而非整体失败，从文件路径提取行号等）。即使 schema 校验失败，手动验证仍然尝试提取尽可能多的有效 issue。

## 问题去重算法

跨审查者合并相似问题，避免重复报告。

### 为什么 AI 提取后还需要去重

`structurizeIssues` 的提示词已经要求 AI "merge similar issues"，但实际中 AI 可能：
- 同一文件同一行的问题，因 title 措辞不同而分开列出
- 不同审查者用不同术语描述同一问题（如 "SQL injection" vs "unsanitized input"），AI 未能识别为同一问题
- raisedBy 字段遗漏某个审查者

因此需要一道确定性的程序化去重，确保相似问题被合并。

### 判定条件

两个问题被视为"相同"需同时满足以下三个条件：

**1. 同一文件**

```
a.File == b.File   // 完全匹配
```

**2. 行号范围重叠 (容差 5 行)**

```
aStart <= bEnd + 5  &&  bStart <= aEnd + 5
```

其中 `aStart = a.Line`, `aEnd = a.EndLine ?? a.Line`（无 EndLine 时退化为单行）。

如果任一问题缺少行号信息（`Line == nil`），则不拒绝合并（返回 true）。

示例：

```
问题 A: line=40, endLine=45    ->  aStart=40, aEnd=45
问题 B: line=48                ->  bStart=48, bEnd=48

检查: 40 <= 48+5(53) ✅  &&  48 <= 45+5(50) ✅  ->  重叠 ✅
```

```
问题 A: line=10, endLine=15    ->  aStart=10, aEnd=15
问题 B: line=30                ->  bStart=30, bEnd=30

检查: 10 <= 30+5(35) ✅  &&  30 <= 15+5(20) ❌  ->  不重叠 ❌
```

**3. 加权 Jaccard 相似度 > 0.35**

```
similarity = titleSim * 0.7 + descSim * 0.3 > 0.35
```

其中 Jaccard 相似度公式：

```
J(A, B) = |A ∩ B| / |A ∪ B|
```

- 文本先转小写，按空白分词
- 过滤英文停用词 (the, a, in, of, is, to, and, for, with, this, that, it)
- 描述文本取前 50 个词（控制计算量）
- title 权重 0.7，description 权重 0.3

示例：

```
问题 A title: "SQL injection vulnerability found"
  -> 分词过滤: ["sql", "injection", "vulnerability", "found"]

问题 B title: "SQL injection vulnerability detected"
  -> 分词过滤: ["sql", "injection", "vulnerability", "detected"]

交集: {"sql", "injection", "vulnerability"} = 3
并集: {"sql", "injection", "vulnerability", "found", "detected"} = 5
titleSim = 3/5 = 0.6

(假设 descSim = 0.5)
similarity = 0.6 * 0.7 + 0.5 * 0.3 = 0.42 + 0.15 = 0.57 > 0.35 ✅ -> 合并
```

### 合并规则

当两个问题被判定为相似时：

| 字段 | 合并策略 |
|------|----------|
| `severity` | 保留最高严重程度（critical > high > medium > low > nitpick） |
| `raisedBy` | 合并所有审查者 ID（去重） |
| `descriptions` | 保留所有原始描述 |
| `suggestedFix` | 如果已有为空，则使用新发现的 |
| 其他字段 | 保留首次出现的值 |

### 合并前后对比

**合并前 (2 个独立问题):**

```
来自 security-reviewer:
  severity: "high", file: "auth.go", line: 42
  title: "SQL injection vulnerability found"
  description: "The query uses direct string concatenation allowing SQL injection..."
  suggestedFix: ""

来自 perf-reviewer:
  severity: "medium", file: "auth.go", line: 44
  title: "SQL injection vulnerability detected"
  description: "String concatenation in SQL query creates injection risk and prevents query plan caching..."
  suggestedFix: "Use parameterized queries"
```

**合并后 (1 个 MergedIssue):**

```
  severity: "high"               // 保留最高 (high > medium)
  file: "auth.go", line: 42
  title: "SQL injection vulnerability found"
  suggestedFix: "Use parameterized queries"   // 从 perf-reviewer 补充
  raisedBy: ["security-reviewer", "perf-reviewer"]
  descriptions: [
    "The query uses direct string concatenation allowing SQL injection...",
    "String concatenation in SQL query creates injection risk and prevents query plan caching..."
  ]
```

最终结果按严重程度排序（critical 在前，nitpick 在后）。

## 严重程度与审查结论常量

### 严重程度 (severity)

```
"critical" (0) -> 阻塞合并, 必须立即修复
"high"     (1) -> 应该修复, 不修复可能导致生产问题
"medium"   (2) -> 值得修复, 改善代码质量
"low"      (3) -> 次要问题, 有时间再修
"nitpick"  (4) -> 风格建议, 非必要
```

数值越小越严重，用于排序和合并时保留最高严重程度。

### 审查结论 (verdict)

```
"approve"         -> 批准合并
"request_changes" -> 要求修改 (存在需要修复的问题)
"comment"         -> 仅评论 (默认值, 当 verdict 无效时的回退)
```

### 验证规则

- `severity` 必须是上述 5 个值之一，否则该 issue 被丢弃
- `verdict` 如果不是上述 3 个值之一，默认回退为 `"comment"`
- `file` 和 `title` 和 `description` 不能为空，否则该 issue 被丢弃

## Token 估算公式

```go
func estimateTokens(text string) int {
    cjkCount := // 统计 CJK 字符数 (汉字 + CJK 符号标点 + 全角/半角字符)
    nonCJK   := len(text) - cjkCount

    return ceil(cjkCount * 0.7 + nonCJK / 4.0)
}
```

**估算规则：**

| 字符类型 | Token 比率 | 说明 |
|----------|-----------|------|
| CJK 字符 (中日韩汉字、CJK 符号标点 U+3000-U+303F、全角/半角字符 U+FF00-U+FFEF) | 0.7 token/字符 | CJK 字符编码密度高 |
| 非 CJK 字符 | 0.25 token/字符 (约 4 字符/token) | 英文等拉丁字符 |

**费用估算：**

```
estimatedCost = (inputTokens + outputTokens) * $0.00001
```

**示例：**

```
输入: "Please review this 代码变更"

CJK: "代码变更" = 4 字符 -> 4 * 0.7 = 2.8
非CJK: "Please review this " = 20 字符 -> 20 / 4.0 = 5.0
总计: ceil(2.8 + 5.0) = 8 tokens

假设某审查者: inputTokens=5200, outputTokens=1800
estimatedCost = (5200 + 1800) * 0.00001 = $0.07
```

## 关键接口

### DisplayCallbacks

终端 UI 集成的回调接口，编排器在执行过程中通过这些回调实时更新显示：

```go
type DisplayCallbacks interface {
    OnWaiting(reviewerID string)                                  // 某角色进入等待状态
    OnMessage(reviewerID string, content string)                  // 收到消息/流式块
    OnParallelStatus(round int, statuses []ReviewerStatus)        // 并行执行状态变更
    OnRoundComplete(round int, converged bool)                    // 某轮辩论完成
    OnConvergenceJudgment(verdict string, reasoning string)       // 收敛判断结果
    OnContextGathered(ctx *GatheredContext)                       // 上下文收集完成
}
```

### ContextGathererInterface

上下文收集器抽象接口，由外部包实现（避免循环导入）：

```go
type ContextGathererInterface interface {
    Gather(diff, prNumber, baseBranch string) (*GatheredContext, error)
}
```

### AIProvider (来自 provider 包)

AI 提供者接口，支持同步和流式调用：

```go
type AIProvider interface {
    Name() string
    Chat(ctx, messages, systemPrompt, options) (string, error)
    ChatStream(ctx, messages, systemPrompt) (<-chan string, <-chan error)
}
```

支持 `SessionProvider` 的提供者可以维护对话上下文，在 Round 2+ 时只需发送增量消息。

## 会话模式 vs 非会话模式

### 为什么 startSessions 需要类型断言

`Reviewer.Provider` 的类型是 `AIProvider`（基础接口，只有 `Chat`/`ChatStream`）。`SessionProvider` 是扩展接口，增加了 `StartSession`/`EndSession`。**不是所有提供者都支持会话**：

| 提供者 | 实现 SessionProvider？ | 原因 |
|--------|:---:|------|
| ClaudeCodeProvider | 是 | `claude --resume` 支持会话续传 |
| CodexCliProvider | 是 | `codex exec resume` 同理 |
| OpenAIProvider | 否 | 无状态 HTTP API，无会话概念 |
| MockProvider | 否 | 测试用 |

类型断言 `provider.(SessionProvider)` 是 Go 的**可选能力检测**模式——有能力就用，没有就跳过：

```go
// orchestrator.go:111 — 有 StartSession 就调，没有就跳过
if sp, ok := r.Provider.(provider.SessionProvider); ok {
    sp.StartSession(...)
}
```

如果把 `StartSession` 放进 `AIProvider` 基础接口，就会强迫 OpenAI 和 Mock 实现空方法，违反接口隔离原则。

### 非会话模式（OpenAI）的行为

用 OpenAI 时，整个流程正常运行，只是消息构建策略不同：

```
startSessions()   → 类型断言失败，跳过（无会话可启动）
buildMessages()   → hasSession=false，走 buildFullContextDebateMessages 分支
endAllSessions()  → 类型断言失败，跳过（无会话可释放）
```

### 两种模式的消息对比

以第 2 轮为例（2 个审查者，security-reviewer 和 perf-reviewer）：

**会话模式 (Claude Code)**——只发增量：

```
messages = [
  {role: "user", content: "[perf-reviewer]: 第1轮发言...\n\n---\n\n请继续审查，挑战对方观点..."}
]
// 之前的 diff + 分析 + 第1轮自己的输出已在 CLI 会话中，不需要重发
```

**非会话模式 (OpenAI)**——重发完整上下文：

```
messages = [
  {role: "user",      content: "完整 diff + 分析结果 + 辩论规则"},   // 重发
  {role: "user",      content: "[perf-reviewer]: 第1轮发言..."},    // 其他人的历史
  {role: "assistant",  content: "自己第1轮的输出"},                  // 自己的历史（assistant 角色）
]
```

### Token 消耗对比

```
假设: diff + 分析 = 5000 tokens, 每轮每人输出 = 1500 tokens
2 个审查者, 3 轮辩论:

会话模式 (Claude Code):              非会话模式 (OpenAI):
  第1轮: 5000 × 2 = 10,000            第1轮: 5000 × 2  = 10,000
  第2轮: 1500 × 2 =  3,000            第2轮: 8000 × 2  = 16,000
  第3轮: 3000 × 2 =  6,000            第3轮: 11000 × 2 = 22,000
  ─────────────────────                ──────────────────────────
  总输入:           ≈ 19,000           总输入:            ≈ 48,000
```

非会话模式 token 消耗约为会话模式的 **2~3 倍**，因为每轮都重发 diff 和所有历史。但**审查质量相同**——审查者看到的信息内容一样。

### Summarizer 的 Session 生命周期

Summarizer 的 `Chat` 调用看起来是无状态的（每次都传完整的 `msgs`），但如果底层是 Claude Code / Codex，有活跃 session 时 `Chat` 实际会走 `--resume`。需要看调用时 session 是否还活着：

```
startSessions()                          ← summarizer session 启动
│
├─ 阶段 2: 辩论
│   ├─ checkConvergence()                ← summarizer.Chat() — session 活着，走 --resume
│   ├─ checkConvergence()                ← 同上，前次 check 的上下文还在 session 里
│   ...
│
├─ 阶段 3: 总结
│   ├─ collectSummaries()                ← 用的是 reviewer 的 provider，不是 summarizer
│   │
│   ├─ summarizer.EndSession()           ← ⭐ 显式结束 session（orchestrator.go:327-329）
│   │
│   ├─ 并行:
│   │   ├─ getFinalConclusion()          ← summarizer.Chat() — session 已结束，真正无状态
│   │   └─ structurizeIssues()           ← summarizer.Chat() — session 已结束，真正无状态
```

| 调用点 | session 状态 | 实际行为 |
|--------|:---:|------|
| `checkConvergence` | 活着 | 走 `--resume`，前几次 check 的上下文会累积 |
| `getFinalConclusion` | 已结束 | 真正无状态，独立请求 |
| `structurizeIssues` | 已结束 | 真正无状态，独立请求 |

**为什么 `runSummaryPhase` 要先 `EndSession`？**

```go
// orchestrator.go:327-329
if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
    sp.EndSession()
}
```

两个原因：
1. **无法并行** — 同一个 CLI session 不能同时接收两个请求，而 `getFinalConclusion` 和 `structurizeIssues` 需要并行执行
2. **上下文污染** — convergence check 的残留上下文可能干扰 JSON 提取的结果

**`checkConvergence` 走 session 有问题吗？**

没有。虽然 prompt 是自包含的（包含所有轮次的完整辩论记录），session 里会有之前 convergence check 的上下文残留，但每次 prompt 都重新发送完整辩论记录，不依赖 session 中的历史。多了一些冗余 token，但不影响正确性。而且 convergence check 频率低（最多 `MaxRounds - 2` 次）。

**如果 summarizer 用 OpenAI？** 类型断言失败，没有 session，所有 `Chat` 调用都是纯粹的无状态 HTTP 请求。

## runDebateRound 逐行解析

用一个具体例子跟踪每一行代码的执行。假设：
- 2 个审查者：`security-reviewer`（用 Claude Code）、`perf-reviewer`（用 OpenAI）
- 当前执行第 2 轮（`round=2`）
- 第 1 轮已完成，`conversationHistory` 中有 2 条消息

```go
func (o *DebateOrchestrator) runDebateRound(ctx context.Context, round int, display DisplayCallbacks) error {
```

### 第一步：快照式消息构建

```go
    type reviewerTask struct {
        reviewer Reviewer
        messages []provider.Message
    }
    tasks := make([]reviewerTask, len(o.reviewers))
    // tasks = [空task, 空task]

    for i, r := range o.reviewers {
        tasks[i] = reviewerTask{
            reviewer: r,
            messages: o.buildMessages(r.ID),
        }
    }
```

执行过程：

```
i=0, r=security-reviewer:
  buildMessages("security-reviewer")
    → hasSession=true (Claude Code 支持 SessionProvider)
    → buildSessionDebateMessages(...)
    → messages = [{role:"user", content:"[perf-reviewer]: 第1轮发言...\n请继续审查..."}]
  tasks[0] = {reviewer: security-reviewer, messages: [1条增量消息]}

i=1, r=perf-reviewer:
  buildMessages("perf-reviewer")
    → hasSession=false (OpenAI 不支持 SessionProvider)
    → buildFullContextDebateMessages(...)
    → messages = [
        {role:"user",      content:"完整diff + 分析 + 辩论规则"},
        {role:"user",      content:"[security-reviewer]: 第1轮发言..."},
        {role:"assistant", content:"perf-reviewer自己第1轮的输出"},
      ]
  tasks[1] = {reviewer: perf-reviewer, messages: [3条完整上下文消息]}
```

**关键**：两个 task 的消息都是基于当前 `conversationHistory` 构建的。此时还没有任何审查者执行，所以两人看到的历史是一致的。

### 第二步：初始化 UI 状态

```go
    statuses := make([]ReviewerStatus, len(o.reviewers))
    for i, r := range o.reviewers {
        statuses[i] = ReviewerStatus{
            ReviewerID: r.ID,
            Status:     "pending",
        }
    }
    // statuses = [
    //   {ReviewerID: "security-reviewer", Status: "pending"},
    //   {ReviewerID: "perf-reviewer",     Status: "pending"},
    // ]

    display.OnWaiting("round-2")
    // 终端显示: "⏳ round-2"

    display.OnParallelStatus(2, statuses)
    // 终端显示:
    //   security-reviewer: pending
    //   perf-reviewer:     pending
```

### 第三步：并行执行

```go
    type roundResult struct {
        reviewer     Reviewer
        fullResponse string
        inputText    string
    }
    results := make([]roundResult, len(tasks))
    // results = [空result, 空result]  预分配，每个 goroutine 写自己的索引

    rg, rgctx := errgroup.WithContext(ctx)
    // 创建 errgroup，任何一个 goroutine 失败会取消其他的

    for i, task := range tasks {
        i, task := i, task   // 捕获循环变量（Go 闭包陷阱）
        rg.Go(func() error {
```

**`i, task := i, task` 是什么？**
Go 的 `for` 循环变量在闭包中是共享的。如果不重新声明，两个 goroutine 可能都用 `i=1`（最后一次迭代的值）。`i, task := i, task` 创建局部副本，确保 goroutine 0 用 `i=0`，goroutine 1 用 `i=1`。

现在两个 goroutine 并行启动：

**goroutine 0 (security-reviewer):**

```go
            startTime := time.Now().UnixMilli()   // 1705312200000
            statuses[0] = ReviewerStatus{
                ReviewerID: "security-reviewer",
                Status:     "thinking",
                StartTime:  1705312200000,
            }
            display.OnParallelStatus(2, copyStatuses(statuses))
            // 终端显示:
            //   security-reviewer: thinking ⏱ 0s
            //   perf-reviewer:     pending
```

```go
            ch, errCh := task.reviewer.Provider.ChatStream(
                rgctx,
                task.messages,                    // [1条增量消息]
                task.reviewer.SystemPrompt,       // "You are a security-focused..."
            )
            // ch: 流式输出 channel，逐块返回文本
            // errCh: 错误 channel，完成后返回 nil 或 error

            var sb strings.Builder
            for chunk := range ch {
                sb.WriteString(chunk)
                // chunk 1: "## Round 2 Response\n\n"
                // chunk 2: "I agree with perf-reviewer about the N+1..."
                // chunk 3: "Additionally, the parameterized query fix..."
                // ... channel 关闭，循环结束
            }
            if err := <-errCh; err != nil {
                return fmt.Errorf("reviewer security-reviewer failed: %w", err)
            }
            // err == nil，继续
```

```go
            endTime := time.Now().UnixMilli()     // 1705312215000 (15秒后)
            statuses[0] = ReviewerStatus{
                ReviewerID: "security-reviewer",
                Status:     "done",
                StartTime:  1705312200000,
                EndTime:    1705312215000,
                Duration:   15.0,                 // (15000-0)/1000
            }
            display.OnParallelStatus(2, copyStatuses(statuses))
            // 终端显示:
            //   security-reviewer: done ✅ 15.0s
            //   perf-reviewer:     thinking ⏱ 12s  (另一个 goroutine 还在跑)
```

```go
            // 拼接所有消息内容 + 系统提示词，用于 token 估算
            var inputParts []string
            for _, m := range task.messages {
                inputParts = append(inputParts, m.Content)
            }
            inputText := strings.Join(inputParts, "\n") + task.reviewer.SystemPrompt
            // inputText = "[perf-reviewer]: 第1轮...\n请继续审查...\nYou are a security-focused..."

            results[0] = roundResult{
                reviewer:     security-reviewer,
                fullResponse: "## Round 2 Response\n\nI agree with perf-reviewer...",
                inputText:    inputText,
            }
            return nil
        })
```

**goroutine 1 (perf-reviewer)** 同时执行，流程相同，写入 `results[1]`。

### 第四步：等待所有 goroutine 完成

```go
    if err := rg.Wait(); err != nil {
        return err   // 任何一个审查者失败就返回错误
    }
    // 两个 goroutine 都成功完成
```

`errgroup.Wait()` 阻塞直到所有 goroutine 返回。如果某个审查者的 AI 调用失败（比如 API 超时），`errgroup` 会取消其他 goroutine 的 context（`rgctx`），并返回第一个错误。

### 第五步：统一写入历史

```go
    for _, r := range results {
        o.trackTokens(r.reviewer.ID, r.inputText, r.fullResponse)
        // 估算并累加 token 消耗（线程安全，有 mutex）

        o.conversationHistory = append(o.conversationHistory, DebateMessage{
            ReviewerID: r.reviewer.ID,
            Content:    r.fullResponse,
            Timestamp:  time.Now(),
        })
        // conversationHistory 现在有 4 条消息:
        //   [0] security-reviewer 第1轮
        //   [1] perf-reviewer 第1轮
        //   [2] security-reviewer 第2轮  ← 新增
        //   [3] perf-reviewer 第2轮      ← 新增

        o.markAsSeen(r.reviewer.ID)
        // lastSeenIndex["security-reviewer"] = 2 (消息索引)
        // lastSeenIndex["perf-reviewer"] = 3

        display.OnMessage(r.reviewer.ID, r.fullResponse)
        // 终端显示审查者的完整响应
    }

    return nil
}
```

**为什么在所有 goroutine 完成后才统一写入？** 如果在每个 goroutine 内部直接写 `conversationHistory`，需要加锁防并发写入，而且消息顺序不确定（取决于哪个 goroutine 先完成）。统一写入保证消息顺序始终是 `results[0]` 在前 `results[1]` 在后，且不需要额外加锁。

### 完整时间线

```
时间轴 (秒)
0s   ├─ 构建 tasks[0] 和 tasks[1] 的消息（快照）
     ├─ UI: "round-2"，两个审查者状态 = pending
     │
0.1s ├─ 启动 goroutine 0 (security-reviewer) ──────────────────┐
     ├─ 启动 goroutine 1 (perf-reviewer)    ──────────────┐    │
     │                                                     │    │
     │  UI: security-reviewer: thinking                    │    │
     │      perf-reviewer:     thinking                    │    │
     │                                                     │    │
     │  goroutine 1:                                       │    │
     │    ChatStream → 流式接收 chunks...                   │    │
     │                                                     │    │
     │  goroutine 0:                                       │    │
     │    ChatStream → 流式接收 chunks...                   │    │
     │                                                     │    │
12s  │  goroutine 1 完成                                   │    │
     │  UI: perf-reviewer: done ✅ 12.0s                   ├────┤
     │      security-reviewer: thinking ⏱ 12s              │    │
     │                                                     │    │
15s  │  goroutine 0 完成                                   │    │
     │  UI: security-reviewer: done ✅ 15.0s               ├────┤
     │                                                          │
     ├─ rg.Wait() 返回                                          │
     │                                                          │
15.1s├─ 统一写入 conversationHistory                             │
     │  ├─ trackTokens("security-reviewer", ...)                │
     │  ├─ append(DebateMessage{security-reviewer, ...})        │
     │  ├─ trackTokens("perf-reviewer", ...)                    │
     │  └─ append(DebateMessage{perf-reviewer, ...})            │
     │                                                          │
     └─ return nil ─────────────────────────────────────────────┘
```
