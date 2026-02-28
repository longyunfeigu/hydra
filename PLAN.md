# Hydra Go Rewrite - Multi-Model Code Review

## Context

Hydra 是一个多模型对抗式 PR Review 工具，通过让多个 AI 模型（使用相同 prompt、不同模型）对同一段代码进行独立审查和辩论，从而产生更全面的代码审查结果。用 Go 重写，只保留多模型 review 核心功能。

**保留的功能：**
- 多模型并行审查 + 多轮辩论（Round 1 独立，Round 2+ 交叉审查）
- 收敛检测（提前停止）
- 2 个 CLI Provider：**claude-code** 和 **codex-cli**
- GitHub PR 集成（读取 diff、发布 inline comment）
- 结构化 Issue 提取 + 去重
- YAML 配置（`~/.hydra/config.yaml`）
- 指数退避重试、Token 用量追踪、流式输出
- Context 收集（可选：调用链、相关 PR、文档）

**移除的功能：** discuss、repo-review、session 持久化/resume/export、interactive 模式、post-analysis Q&A、devil's advocate、feature analyzer、state management、history tracking、API provider（anthropic/openai/gemini/minimax）、gemini-cli、qwen-code

---

## 三个核心 AI 角色

Orchestrator 中有 3 个不同的 AI 角色，各自拥有独立的 provider 实例和 system prompt：

### 1. Analyzer（预分析器）
- **职责：** 接收 PR diff，生成分析摘要 + focus areas
- **时机：** 与 context gatherer **并行执行**，在 reviewer 开始前完成
- **输出去向：** analysis 结果 + focus areas 注入到每个 reviewer 的 Round 1 prompt 中
- **配置：** `config.analyzer: { model, prompt }`

### 2. Reviewers（审查者，多个）
- **职责：** 实际执行代码审查和辩论
- **输入：** analyzer 的 analysis + context gatherer 的 context + PR diff
- **多轮辩论：** Round 1 独立审查，Round 2+ 交叉审查（看到前几轮其他人的回复）
- **配置：** `config.reviewers: { id: { model, prompt } }`

### 3. Summarizer（总结器）
- **职责（3 项）：**
  1. **收敛判断** — 每轮结束后判断 `CONVERGED / NOT_CONVERGED`
  2. **最终结论** — 基于所有 reviewer 的 summary 生成最终结论
  3. **Issue 结构化** — 从讨论文本中提取 JSON 格式的 structured issues
- **配置：** `config.summarizer: { model, prompt }`

### 完整数据流

```
                    ┌──────────────────┐
                    │   PR diff/code   │
                    └────────┬─────────┘
                             │
              ┌──────────────┼──────────────┐
              ▼              ▼              │
    ┌─────────────────┐  ┌──────────┐      │
    │ Context Gatherer │  │ Analyzer │      │
    │ (调用链/PR/文档) │  │ (预分析)  │      │
    └────────┬────────┘  └────┬─────┘      │
             │ context        │ analysis    │
             └──────┬─────────┘            │
                    ▼                      │
    ┌───────────────────────────────┐      │
    │     Reviewers (Round 1)       │◄─────┘
    │  独立审查: analysis+context+diff │     diff
    └──────────────┬────────────────┘
                   ▼
    ┌───────────────────────────────┐
    │   Summarizer: 收敛检测         │
    │   CONVERGED? → 跳到 Step 5    │
    │   NOT_CONVERGED? → 继续       │
    └──────────────┬────────────────┘
                   ▼
    ┌───────────────────────────────┐
    │     Reviewers (Round 2+)      │
    │  交叉审查: 看到前几轮所有回复    │
    │  挑战弱论点 + 指出遗漏          │
    └──────────────┬────────────────┘
                   ▼ (循环直到收敛或 maxRounds)
    ┌───────────────────────────────┐
    │  Reviewers: 各自输出 summary   │
    └──────────────┬────────────────┘
                   ▼
    ┌───────────────────────────────┐
    │  Summarizer: 最终结论          │
    │  (共识 + 分歧 + 行动项)        │
    └──────────────┬────────────────┘
                   ▼
    ┌───────────────────────────────┐
    │  Summarizer: Issue 结构化      │
    │  (JSON 提取 + 去重)            │
    └──────────────┬────────────────┘
                   ▼
    ┌───────────────────────────────┐
    │  GitHub: 发布 inline comments  │
    └───────────────────────────────┘
```

---

## 项目结构

```
hydra/
├── main.go                              # 入口
├── go.mod / go.sum
├── cmd/
│   ├── root.go                          # Cobra root command
│   ├── review.go                        # review 子命令
│   └── init.go                          # init 子命令（生成配置）
├── internal/
│   ├── config/
│   │   ├── config.go                    # HydraConfig 结构体、LoadConfig()、ValidateConfig()
│   │   └── init.go                      # 交互式配置生成
│   ├── provider/
│   │   ├── provider.go                  # AIProvider 接口 + Message 类型
│   │   ├── factory.go                   # CreateProvider() 模型路由
│   │   ├── retry.go                     # WithRetry() 指数退避
│   │   ├── claudecode.go               # Claude Code CLI provider
│   │   ├── codexcli.go                 # Codex CLI provider
│   │   └── cliprovider.go              # CLI provider 共用的 session helper
│   ├── orchestrator/
│   │   ├── orchestrator.go             # DebateOrchestrator 核心辩论循环
│   │   ├── types.go                     # Reviewer、DebateMessage、DebateResult 等
│   │   └── issueparser.go              # Issue JSON 解析 + Jaccard 去重
│   ├── github/
│   │   ├── commenter.go                # PR 评论发布（inline/file/global 三级降级）
│   │   └── diff.go                      # Diff 解析、行号提取
│   ├── context/
│   │   ├── gatherer.go                  # ContextGatherer 入口
│   │   ├── types.go                     # GatheredContext 等类型
│   │   ├── reference.go                # 调用链分析（symbol 提取 + ripgrep 搜索）
│   │   ├── history.go                   # 相关 PR 历史
│   │   ├── docs.go                      # 文档收集
│   │   └── prompt.go                    # 分析 prompt 构建
│   ├── display/
│   │   ├── terminal.go                  # 终端着色、spinner、状态显示
│   │   └── markdown.go                  # Markdown 渲染（终端 + 文件输出）
│   └── util/
│       └── logger.go                    # 分级日志（HYDRA_LOG_LEVEL）
```

---

## 核心接口设计

### AIProvider 接口

```go
// internal/provider/provider.go
type Message struct {
    Role    string // "system" | "user" | "assistant"
    Content string
}

type ChatOptions struct {
    DisableTools bool
}

type AIProvider interface {
    Name() string
    Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error)
    ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error)
}

// CLI provider 扩展接口（支持 session 复用以节省 token）
type SessionProvider interface {
    AIProvider
    StartSession(name string)
    EndSession()
    SessionID() string
}
```

### Orchestrator 核心类型

```go
// internal/orchestrator/types.go

// Reviewer 绑定 AI provider 到一个审查者身份
type Reviewer struct {
    ID           string
    Provider     provider.AIProvider
    SystemPrompt string
}

// DebateOrchestrator 构造参数 - 明确 3 个角色
type OrchestratorConfig struct {
    Reviewers       []Reviewer              // 多个审查者
    Analyzer        Reviewer                // 预分析器（1个）
    Summarizer      Reviewer                // 总结器（1个）
    ContextGatherer *context.ContextGatherer // 可选
    Options         OrchestratorOptions
}

type DebateResult struct {
    PRNumber         string
    Analysis         string                  // Analyzer 的输出
    Context          *context.GatheredContext // Context Gatherer 的输出
    Messages         []DebateMessage          // 完整辩论历史
    Summaries        []DebateSummary          // 每个 reviewer 的总结
    FinalConclusion  string                   // Summarizer 的最终结论
    TokenUsage       []TokenUsage
    ConvergedAtRound *int
    ParsedIssues     []MergedIssue            // Summarizer 提取的结构化 issues
}
```

### Config 类型

```go
// internal/config/config.go
type HydraConfig struct {
    Providers       map[string]CLIProviderConfig   `yaml:"providers"`
    Defaults        DefaultsConfig                 `yaml:"defaults"`
    Reviewers       map[string]ReviewerConfig      `yaml:"reviewers"`    // 多个审查者
    Analyzer        ReviewerConfig                 `yaml:"analyzer"`     // 预分析器
    Summarizer      ReviewerConfig                 `yaml:"summarizer"`   // 总结器
    ContextGatherer *ContextGathererConfig         `yaml:"contextGatherer,omitempty"`
}
```

---

## CLI 命令设计

```
hydra review <pr-number-or-url>     # Review PR
hydra review --local                # Review local uncommitted changes
hydra review --branch [base]        # Review current branch vs base
hydra review --files f1 f2          # Review specific files
hydra init                          # 交互式创建配置文件
```

**Review flags:**
| Flag | 说明 |
|------|------|
| `-c, --config <path>` | 配置文件路径 |
| `-r, --rounds <n>` | 最大辩论轮数 |
| `-o, --output <file>` | 输出到文件 |
| `-f, --format <fmt>` | markdown / json |
| `--no-converge` | 禁用收敛检测 |
| `-l, --local` | Review 本地未提交变更 |
| `--branch [base]` | Review 当前分支 vs base |
| `--files <f1,f2>` | Review 指定文件 |
| `--reviewers <ids>` | 逗号分隔的 reviewer ID |
| `-a, --all` | 使用所有 reviewer |
| `--skip-context` | 跳过 context 收集 |
| `--no-post` | 跳过 GitHub 评论 |

---

## Orchestrator 核心流程（详细）

```
RunStreaming(ctx, label, prompt) -> DebateResult

1. 启动 session
   - 为所有 reviewers 启动 session: "Hydra | {label} | reviewer:{id}"
   - 为 analyzer 启动 session: "Hydra | {label} | analyzer"
   - 为 summarizer 启动 session: "Hydra | {label} | summarizer"

2. 并行预处理（errgroup）
   ┌─ goroutine A: Context Gatherer
   │  - 从 prompt 提取 diff
   │  - gatherer.Gather(diff, prNumber, baseBranch)
   │  - 输出: GatheredContext (summary + rawReferences + ...)
   │
   └─ goroutine B: Analyzer 预分析（流式）
      - analyzer.ChatStream(prompt, analyzerSystemPrompt)
      - 输出: analysis 文本
      - 通过 OnMessage("analyzer", chunk) 实时输出

   两者完成后，触发 OnContextGathered callback

3. 多轮辩论: FOR round = 1 to maxRounds
   a. 构建消息（快照，所有 reviewer 看到相同信息）
      Round 1 prompt 包含:
        - Task (原始 PR diff)
        - System Context (gatheredContext.summary) ← context gatherer 输出
        - Focus hints (从 analysis 提取的 focusAreas) ← analyzer 输出
        - Call chain context (rawReferences)
        - Analysis 全文
      Round 2+ prompt 包含:
        - 前几轮所有其他 reviewer 的回复
        - 指令: 继续审查 + 指出遗漏 + 挑战弱论点

   b. 所有 reviewer 并行执行（goroutine + errgroup）
      - ChatStream → 收集完整响应
      - 更新 ReviewerStatus (pending → thinking → done)

   c. 记录到 conversationHistory

   d. 收敛检测（round >= 2 且 round < maxRounds 时）
      - Summarizer 判断 CONVERGED / NOT_CONVERGED
      - CONVERGED → 记录 convergedAtRound, break

4. 收集 summary: 每个 reviewer 提供 key points 总结

5. Summarizer 生成最终结论
   - 输入: 所有 reviewer 的匿名 summary
   - 输出: 共识 + 分歧 + 行动项

6. Summarizer 结构化 Issue 提取
   - 关闭 summarizer session（确保 clean context）
   - 从讨论文本中提取 JSON issues（最多 3 次重试）
   - Issue 去重（Jaccard 相似度 0.35 阈值）

7. 关闭所有 session, 返回 DebateResult
```

**并发模型：** `errgroup.WithContext()` 管理并行 goroutine，`context.Context` 传播 Ctrl+C。

**线程安全：** 每个 goroutine 返回自己的结果，在 `Wait()` 后聚合。

---

## Provider 实现（仅 2 个 CLI）

### claude-code provider
- 执行 `claude -p - --output-format stream-json`
- prompt 通过 stdin 传入（避免 E2BIG）
- 支持 `--resume <sessionId>` 多轮 session
- 超时 15 分钟

### codex-cli provider
- 执行 `codex --quiet --full-auto`
- prompt 通过 stdin 传入
- 超时 15 分钟

### 共用 CliSessionHelper (`cliprovider.go`)
- 管理 session ID 和名称
- 首条消息发送完整 prompt，后续仅增量发送

### Factory 路由
- `claude-code` → ClaudeCodeProvider
- `codex-cli` → CodexCliProvider
- 其他 → 返回错误

---

## GitHub 集成

- 通过 `gh` CLI 执行所有操作（`os/exec`）
- `GetPRDiff()` → `gh pr diff`
- `GetPRHeadSha()` → `gh pr view --json headRefOid`
- 评论发布三级降级：inline → file-level → global
- `parseDiffLines()` 提取 diff 中有效的右侧行号
- 去重：检查现有评论避免重复发布

---

## 外部依赖

| 包 | 用途 |
|---|------|
| `github.com/spf13/cobra` | CLI 框架 |
| `gopkg.in/yaml.v3` | YAML 解析 |
| `golang.org/x/sync/errgroup` | 并行 goroutine 管理 |
| `github.com/fatih/color` | 终端着色 |
| `github.com/briandowns/spinner` | 终端 spinner |
| `github.com/charmbracelet/glamour` | 终端 Markdown 渲染 |

**不需要任何 AI SDK**（仅通过 `os/exec` 调用 CLI）。

---

## 实现顺序

| Phase | 内容 | 文件 |
|-------|------|------|
| **1. 基础框架** | go mod、接口定义、配置加载、CLI 骨架、logger | `main.go`, `cmd/`, `internal/config/`, `internal/provider/provider.go`, `internal/util/` |
| **2. Provider 层** | retry、claude-code、codex-cli、session helper、factory | `internal/provider/*.go` |
| **3. Orchestrator** | 类型（含 3 角色）、issue 解析、核心辩论循环 | `internal/orchestrator/*.go` |
| **4. GitHub + Context** | PR diff 获取、评论发布、context 收集 | `internal/github/`, `internal/context/` |
| **5. 显示层 + 接线** | 终端输出、完整 review 命令串联 | `internal/display/`, `cmd/review.go` |
| **6. 测试** | issue parser 测试、config 测试、E2E 测试 | `*_test.go` |

---

## 验证方案

1. **单元测试：** issue parser JSON 解析 + Jaccard 去重、config 加载 + 环境变量展开、diff 解析
2. **E2E 测试：** 用本地 claude-code 和 codex-cli 对真实 PR 执行 `hydra review <pr>`，验证完整流程
3. **GitHub 集成测试：** 先用 `--no-post` 验证 diff 获取和 issue 提取，再测试实际评论发布

---

## 关键实现文件参考（TS 源码 ~/Desktop/git/magpie/）

| Go 目标 | 参考 TS 文件 | 说明 |
|---------|-------------|------|
| `internal/orchestrator/orchestrator.go` | `src/orchestrator/orchestrator.ts` | 核心: analyzer 并行 + 辩论循环 + 消息构建 + 收敛检测 + summarizer 三职责 |
| `internal/orchestrator/issueparser.go` | `src/orchestrator/issue-parser.ts` | Issue JSON 解析 + Jaccard 去重算法 |
| `internal/provider/factory.go` | `src/providers/factory.ts` | 模型路由和 provider 构造 |
| `internal/provider/claudecode.go` | `src/providers/claude-code.ts` | Claude Code CLI 调用逻辑 |
| `internal/provider/codexcli.go` | `src/providers/codex-cli.ts` | Codex CLI 调用逻辑 |
| `internal/provider/cliprovider.go` | `src/providers/session-helper.ts` | CLI session 管理 |
| `internal/github/commenter.go` | `src/github/commenter.ts` | 评论发布 + diff 解析 |
| `internal/config/config.go` | `src/config/loader.ts` | 配置加载 + 环境变量展开 |
| `cmd/review.go` | `src/commands/review.ts` | Review 命令完整流程 |
