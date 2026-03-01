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

## 问题去重算法

跨审查者合并相似问题，避免重复报告。

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
