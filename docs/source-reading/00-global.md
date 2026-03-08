---
skill_version: "v3"
generated_at: "2026-03-08"
repo_commit: "a132cb6"
sources:
  - cmd/root.go
  - cmd/review.go
  - cmd/serve.go
  - internal/review/runner.go
  - internal/review/job.go
  - internal/orchestrator/orchestrator.go
  - internal/orchestrator/types.go
  - internal/orchestrator/ledger.go
  - internal/orchestrator/issueparser.go
  - internal/orchestrator/canonical.go
  - internal/provider/provider.go
  - internal/provider/factory.go
  - internal/platform/platform.go
  - internal/platform/detect/detect.go
  - internal/server/server.go
  - internal/display/display.go
  - internal/context/context.go
freshness_scope: "项目顶层结构变化：新增/删除模块、cmd 入口变化、核心接口签名变化"
---

# Hydra 项目全局源码解析

> 文件路径：`docs/source-reading/00-global.md`
> 覆盖范围：整个项目的六层分析框架
> 代码基准：`internal/orchestrator/`、`internal/provider/`、`internal/platform/`、`cmd/`、`internal/review/`、`internal/server/` 等

---

## Layer 1：场景（Problem Space）

### 一句话描述

Hydra 是一个让多个 AI 互相辩论来审查代码的命令行工具——就像请来三位专家同时看同一段代码，他们会争论、质疑彼此，最终给出比单个专家更可靠的意见。

### 核心用户与核心操作

| 用户角色 | 核心操作 |
|---|---|
| 开发者 | 在提交 PR/MR 前（或提交后）手动触发代码审查，获取结构化的审查报告 |
| 团队基础设施维护者 | 部署 Webhook 服务器，让每个新 MR 自动触发 Hydra 审查 |
| 代码仓库管理员 | 配置哪些 AI 模型参与审查、审查几轮、是否自动发布评论 |

### 主场景 vs 辅助场景

| 场景类型 | 场景描述 |
|---|---|
| 主场景 | `hydra review <PR编号>` —— 审查 GitHub PR 或 GitLab MR，自动发布行内评论 |
| 主场景 | `hydra serve` —— 后台 Webhook 服务，MR 创建/更新时自动触发审查 |
| 辅助场景 | `hydra review --local` —— 审查本地未提交的改动（无需平台账号） |
| 辅助场景 | `hydra review --branch main` —— 审查当前分支 vs 基准分支的差异 |
| 辅助场景 | `hydra init` —— 生成默认配置文件 |
| 辅助场景 | 带 `--skip-context` 跳过上下文收集的快速审查模式 |

### 用户旅程图（手动审查模式）

```
用户输入
  hydra review 42
        |
        v
[1] 配置加载
  config.LoadConfig()
  ~/.hydra/config.yaml
        |
        v
[2] 平台检测
  detect.FromRemote()
  git remote get-url origin
  --> "github.com" 或 "gitlab.com"
        |
        v
[3] 准备 Review
  review.Runner.Prepare()
  ├── 创建 AIProvider 实例 (claude/codex/openai)
  ├── 选择参与审查的 reviewers
  └── 可选: 获取 PR diff + MR metadata
        |
        v
[4] 核心辩论
  orchestrator.RunStreaming()
  ┌─────────────────────────────┐
  │ 阶段 1：并行 (errgroup)       │
  │  ├── 上下文收集 (可选)        │
  │  └── 预分析 (Analyzer)       │
  │                             │
  │ 阶段 2：多轮辩论              │
  │  for round := 1..MaxRounds  │
  │    并行调用所有 Reviewer     │
  │    可选: 共识检测            │
  │                             │
  │ 阶段 3：总结                 │
  │  ├── 各 Reviewer 提交总结    │
  │  ├── Summarizer 生成结论     │
  │  └── 提取结构化 JSON 问题    │
  └─────────────────────────────┘
        |
        v
[5] 发布评论
  plat.PostIssuesAsComments()
  三级降级：行内 → 文件级 → 全局
        |
        v
[6] 终端输出
  display.FinalConclusion()
  display.IssuesTable()
  display.TokenUsage()
```

### Q&A

> **Q**：为什么不直接调用单个 AI 模型的 API，而要搞这么复杂的多模型辩论？
>
> **Evidence**：`orchestrator.go:483 (runDebateRound)` 中，第 2 轮起 `buildMessages()` 会把前一轮所有 Reviewer 的发言注入到每个 Reviewer 的消息列表中。`orchestrator.go:425 (runDebatePhase)` 循环执行 `MaxRounds` 轮辩论，每轮后可选执行共识检测。
>
> **Inference**：单个 AI 模型的审查存在明显盲点——模型的训练数据和注意力机制决定了它倾向于关注某类问题（如安全漏洞），而忽略另一类（如性能瓶颈）。Hydra 的核心假设是：不同模型有不同的关注视角，通过让它们互相挑战，可以把单模型的盲点暴露出来。第 2 轮起每个 reviewer 都能看到其他人的发言，被迫去质疑或确认他人的观点，这会产生"群体智慧"效应。

---

## Layer 2：概念（Domain Model）

### 核心概念清单

| 概念 | 类型 | 来源 | 定义 |
|---|---|---|---|
| **Reviewer（审查者）** | 核心实体 | 作者创造 | 代表辩论中一个独立的 AI 审查角色，包含其 AI Provider 实例和系统提示词。多个 Reviewer 之间展开辩论。见 `internal/orchestrator/types.go:16 (Reviewer)` |
| **Analyzer（分析器）** | 核心实体 | 作者创造 | 在辩论开始前对代码变更进行预分析的特殊角色，提取关注点供 Reviewer 参考，不参与辩论本身。见 `internal/orchestrator/types.go:148 (AnalyzerConfig)` |
| **Summarizer（总结器）** | 核心实体 | 作者创造 | 负责收敛判断（是否达成共识）和最终结论生成的特殊角色，**必须使用 OpenAI API 模型**，因为需要严格 JSON 输出。见 `internal/config/config.go:221 (validateConfig)` |
| **AIProvider（AI提供者）** | 策略概念 | 常见模式（Strategy） | 抽象了不同 AI 后端（Claude CLI/Codex CLI/OpenAI API）的统一接口，支持同步 `Chat` 和流式 `ChatStream`。见 `internal/provider/provider.go:19 (AIProvider)` |
| **Platform（平台）** | 策略概念 | 常见模式（Strategy） | 抽象了 GitHub 和 GitLab 操作的统一接口，包含 MR 读取、评论发布、平台检测等子接口的组合。见 `internal/platform/platform.go:49 (Platform)` |
| **IssueLedger（问题账本）** | 过程概念 | 作者创造 | Ledger 模式下每个 Reviewer 独有的问题账本，跨轮次增量追踪问题的完整生命周期（新增/更新/撤回/支持/反对）。见 `internal/orchestrator/ledger.go:19 (IssueLedger)` |
| **DebateResult（辩论结果）** | 核心实体 | 作者创造 | 整个辩论过程的最终输出，包含预分析报告、全部对话历史、各 Reviewer 总结、最终结论、Token 统计和结构化问题列表。见 `internal/orchestrator/types.go:259 (DebateResult)` |

### 核心概念双层 Example

**Reviewer（审查者）**

- **场景 example**：配置文件中定义了 `model: "gpt-4o"` 和 `model: "claude-code"` 两个审查者，Hydra 启动后就会创建 2 个 Reviewer 实例，它们在辩论中互相看到对方的发言并质疑或支持。
- **结构 example**：
  ```
  Reviewer {
    ID:           "reviewer-0"              // 配置文件中的 key，用于标识发言归属
    Provider:     OpenAIProvider             // 由 model 字段决定的 AI 后端实例
    SystemPrompt: "You are a code reviewer…" // 引导审查角度的提示词，决定关注点
  }
  ```

**IssueLedger（问题账本）**

- **场景 example**：Ledger 模式下，reviewer-0 在第 1 轮提出 3 个问题，第 2 轮撤回 1 个并新增 2 个。IssueLedger 完整记录这个演变过程，最终合并时可知每个问题的"信心度"。
- **结构 example**：
  ```
  IssueLedger {
    ReviewerID: "reviewer-0"                           // 拥有此账本的 Reviewer
    Issues:     map[string]*LedgerIssue{               // 按 issueID 索引的问题集合
      "R0-1": {Status: "active", ...},                 // 仍然有效的问题
      "R0-2": {Status: "retracted", RetractedRound: 2} // 第 2 轮被 reviewer 自己撤回
    }
    nextID:     3                                      // 下一个分配的 issue 序号
  }
  ```

**DebateResult（辩论结果）**

- **场景 example**：一次 2 轮辩论结束后，RunStreaming 返回 DebateResult，包含分析报告、4 条对话消息、2 份总结、最终结论、合计 15000 tokens、以及 5 个去重后的问题。
- **结构 example**：
  ```
  DebateResult {
    PRNumber:         42                    // 被审查的 PR/MR 编号
    Analysis:         "变更涉及认证模块…"     // Analyzer 的预分析报告
    Context:          &GatheredContext{...}  // 上下文收集结果（可为 nil）
    Messages:         []DebateMessage (4条)  // 完整辩论历史，按轮次排列
    Summaries:        []DebateSummary (2份)  // 每个 Reviewer 的总结
    FinalConclusion:  "总体评价：通过…"       // Summarizer 生成的最终结论
    TokenUsage:       TokenUsage{Total: 15000} // 全部 AI 调用的 token 统计
    ConvergedAtRound: 2                     // 第 2 轮达成共识（0 = 未收敛）
    ParsedIssues:     []MergedIssue (5个)   // 去重合并后的结构化问题列表
  }
  ```

### 概念关系图

```
            ┌─────────────────────────────────┐
            │         HydraConfig             │
            │  (config.go)                    │
            └───┬─────────┬──────────┬────────┘
                │         │          │
         defines│   defines│    defines│
                v         v          v
          ┌──────────┐ ┌────────┐ ┌──────────┐
          │Reviewer  │ │Analyzer│ │Summarizer│
          │Config    │ │Config  │ │Config    │
          └────┬─────┘ └───┬────┘ └────┬─────┘
               │           │           │
     instantiates│          │           │
               v           v           v
          ┌──────────┐ ┌──────────┐ ┌──────────┐
          │ Reviewer │ │ Analyzer │ │Summarizer│
          │(+Provider│ │(+Provider│ │(+Provider│
          │ +Prompt) │ │ +Prompt) │ │ +Prompt) │
          └────┬─────┘ └────┬─────┘ └────┬─────┘
               │   feeds    │            │
               │  analysis  │            │
               │<-----------┘            │
               │    debate               │
               │<----------┐  judges     │
               │  (multi   │<------------┘
               │   round)  │
               │           │
               v           │
          ┌──────────────────────┐
          │   DebateOrchestrator │
          │   (orchestrator.go)  │
          └──────────┬───────────┘
                     │ produces
                     v
              ┌─────────────┐
              │DebateResult │    ┌──────────────┐
              │             │───>│ MergedIssue  │
              │             │    │ (去重合并的问题)|
              └─────────────┘    └──────┬───────┘
                                        │ posted to
                                        v
                                  ┌──────────┐
                                  │ Platform │
                                  │(GitHub/  │
                                  │ GitLab)  │
                                  └──────────┘
```

### 有状态概念的生命周期表

| 概念 | 初始状态 | 中间状态 | 终止状态 | 状态转换时机 |
|---|---|---|---|---|
| **LedgerIssue** | `active`（ApplyDelta 新增时） | `active`（被 update 修改） | `retracted`（reviewer 撤回） | 每轮 `extractRoundIssueDeltas` 后 |
| **ReviewerStatus** | `pending` | `thinking` | `done` | goroutine 开始/完成时更新 |
| **debateRun** | 初始化（`newRun`） | 辩论进行中 | 汇总结束（`runSummaryPhase` 返回） | `RunStreaming` 的完整调用周期 |
| **Session（CLI Provider）** | 无会话（`ShouldSendFullHistory=true`） | 有会话 ID（首条消息后） | 关闭（`EndSession`） | `startSessions` 和 `endAllSessions` |
| **inFlightEntry（Server）** | 注册到 `inFlight` map | 审查执行中 | 从 `inFlight` 移除 | Webhook 收到事件 / 审查完成 |

### Q&A

> **Q**：Analyzer 和 Summarizer 有什么区别，为什么要设计成两个不同角色？
>
> **Evidence**：`orchestrator.go:360 (runAnalysisPhase)` 在辩论开始前调用 Analyzer。`orchestrator.go:627 (runSummaryPhase)` 在辩论结束后调用 Summarizer。`config.go:221 (validateConfig)` 强制 Summarizer 使用 OpenAI API 模型。
>
> **Inference**：两者的时机和职责完全不同。Analyzer 在辩论**开始之前**运行，输出自然语言分析文本作为背景信息。Summarizer 则在每轮辩论**结束后**运行，负责共识判断和结构化 JSON 提取。Summarizer 必须使用 OpenAI API 模型，因为 CLI 模型会附加工具调用日志导致 JSON 解析失败。

---

## Layer 3：约束（Design Boundaries）

### 关键约束清单

| 约束 | 类型 | 约束内容 | 代码体现 |
|---|---|---|---|
| **无 AI SDK 依赖** | 技术约束 | 不使用 Anthropic SDK 或 OpenAI Go SDK | CLI 调用 `os/exec`（`claudecode.go`）；OpenAI 调用原生 `net/http` + SSE 手动解析（`openai.go`） |
| **Summarizer 必须用 API 模型** | 技术约束 | CLI 模型无法可靠输出纯 JSON，structurizeIssues 阶段强制校验 | `config.go:221 (validateConfig)` 的 `isOpenAIModel` 校验 |
| **平台抽象不能下沉** | 技术约束 | GitHub/GitLab 特定实现不能出现在 orchestrator 层 | `Platform` 接口在 `platform.go`，orchestrator 仅接受 `ContextGathererInterface` |
| **评论发布的三级降级** | 业务约束 | 并非所有 diff 行都可以发行内评论；需要优雅降级 | `diffparse.go` 的 `ClassifyCommentsByDiffEx`；platform 层逐级尝试 |
| **Webhook 并发上限** | 运行时约束 | 服务器模式下同时审查数量有限，防止 AI 资源耗尽 | `server.go:39 (Server)` 的 `sem chan struct{}` 信号量，默认 3 |

### 约束-影响映射表

| 约束 | 受影响的设计选择 |
|---|---|
| 无 AI SDK | ClaudeCodeProvider 通过 stdin 传递 prompt，避免命令行参数过长（E2BIG）；OpenAI 需要手动解析 SSE 流 |
| Summarizer 限制 API 模型 | 配置校验强制执行，且 structurizeIssues 最多重试 3 次并将 schema 错误反馈给 AI |
| 平台抽象不能下沉 | `ContextGathererInterface` 用接口隔离，避免循环导入；review 层作为组装层 |
| 三级降级 | diffparse.go 需要解析 unified diff 的 hunk header 来确定有效行号范围 |
| 并发上限 | Webhook Server 用 channel 实现的信号量（`sem`），而非 mutex/WaitGroup |

### Q&A

> **Q**：如果"Summarizer 必须用 API 模型"这个约束不存在，代码会怎样简化？
>
> **Evidence**：`config.go:221 (validateConfig)` 中 `isOpenAIModel` 校验强制 Summarizer 使用 API 模型。`issueparser.go:120 (ParseReviewerOutput)` 包含 3 次重试逻辑和 JSON schema 校验。
>
> **Inference**：如果 CLI 模型能可靠输出纯 JSON，Summarizer 可与其他角色共用模型类型，`validateConfig` 去掉特殊校验，`structurizeIssues` 的重试机制也可简化。但目前 CLI 模型在交互时会输出思考过程和工具调用日志，无法保证 JSON 纯净度，JSON schema 校验 + 多次重试是必要的容错层。

---

## Layer 4：拆分理由（Design Decisions）

### 主要模块的拆分动机

| 模块 | 拆分动机 | 说明 |
|---|---|---|
| `cmd/` | **生命周期边界** | CLI 解析、信号处理、输出格式是"主函数关心的事"，与核心逻辑完全分离 |
| `internal/orchestrator/` | **正交性 + 可演进性** | 辩论编排逻辑不依赖具体 AI 后端和平台，允许独立测试和演进（如新增 ledger 模式不影响其他层） |
| `internal/provider/` | **可替换性** | 新增 AI 提供者只需实现 `AIProvider` 接口，不修改任何其他模块 |
| `internal/platform/` | **可替换性 + 失败隔离** | GitHub 和 GitLab 实现互相隔离；平台操作失败不影响审查核心逻辑 |
| `internal/review/` | **组装层（组合而非继承）** | 将 config、provider、orchestrator、context 组装在一起，避免 orchestrator 直接依赖 config |
| `internal/server/` | **并发边界 + 生命周期** | Webhook 服务器有独立的请求生命周期、并发控制和优雅关闭，与 CLI 模式完全不同 |
| `internal/display/` | **演进路径** | 终端 UI 和静默日志（NoopDisplay）实现相同接口（`DisplayCallbacks`），server 模式无缝切换 |
| `internal/context/` | **失败隔离** | 上下文收集（调用链/历史/文档）失败是非致命的，隔离在独立包中，失败时 orchestrator 跳过继续 |

### 模块依赖图

```
                    main.go
                      |
                    cmd/
              ┌───────┼────────┐
              |       |        |
           review   serve    init
              |       |
              └───────┘
                  |
            internal/review/    <── 组装层
            (Runner, Job)
          ┌──────┼────────┬──────────┐
          |      |        |          |
      config  provider orchestrator context
          |      |        |
          |   ┌──┴──┐   ┌──┴──┐
          |   │claude│   │ledger│
          |   │codex │   │parser│
          |   │openai│   │canon │
          |   └──────┘   └──────┘
          |
        platform
     ┌────┴─────┐
  github      gitlab
          |
        display
     ┌────┴─────┐
  terminal   noop

  server/ ─────────────────────────> review/
```

### 责任矩阵

| 模块 | 负责什么 | 不负责什么 |
|---|---|---|
| `cmd/` | CLI 解析、信号处理、结果展示、文件输出 | 任何业务逻辑 |
| `internal/review/` | 依赖组装（Runner.Prepare）、任务构建（Job） | AI 调用、平台 API、UI 渲染 |
| `internal/orchestrator/` | 辩论流程编排、收敛检测、问题提取、Token 统计 | 具体 AI 调用、平台 API、UI 渲染 |
| `internal/provider/` | AI 后端调用（exec/HTTP）、会话管理、重试 | 任何业务逻辑 |
| `internal/platform/` | MR 读取/评论发布/平台检测、diff 解析 | AI 调用、辩论逻辑 |
| `internal/display/` | 终端彩色输出、spinner、流式渲染 | 任何业务逻辑 |
| `internal/context/` | 调用链分析（ripgrep）、历史 MR 查询、文档收集 | 辩论逻辑、评论发布 |
| `internal/server/` | Webhook 路由、并发控制、去重、优雅关闭 | 具体审查逻辑（委托给 review/） |

### Q&A

> **Q**：如果 `orchestrator` 直接依赖 `platform`，会有什么问题？
>
> **Evidence**：`orchestrator/types.go:165-178 (ContextGathererInterface)` 定义接口在 orchestrator 内部。`context/adapter.go` 实现该接口。`review/runner.go:110 (Prepare)` 负责注入依赖。
>
> **Inference**：直接依赖会引入循环导入风险（`platform` 本身需要引用 `orchestrator.MergedIssue` 来发布评论），同时 orchestrator 的测试会被迫 mock 平台 API 调用。现有设计通过依赖倒置（接口在消费方定义，实现在提供方），由 `review` 包作为组装层粘合，这是典型的六边形架构思路。

> **Q**：`internal/review/` 作为"组装层"只负责依赖注入，是否过度设计？直接在 `cmd/review.go` 中组装会怎样？
>
> **Evidence**：`review/runner.go:110 (Prepare)` 的核心逻辑是创建 provider 实例、构建 OrchestratorConfig、注入 ContextGatherer，约 200 行。`cmd/review.go:59 (runReview)` 已经在做配置加载、平台检测、Job 构建等准备工作，约 250 行。`server/reviewer.go:26 (RunServerReview)` 也通过 `review.NewRunner` 复用了同样的组装逻辑。
>
> **Inference**：如果把 `review` 包的组装逻辑搬到 `cmd/review.go`，该文件会增长到 ~450 行，仍在可维护范围内。独立 `review` 包的主要价值是让 `server/reviewer.go` 复用同样的组装逻辑。但如果未来 server 模式的组装需求与 CLI 模式分化（如 server 不需要 checkout），这层抽象可能变成障碍而非帮助。当前两个调用方的组装逻辑几乎相同，拆分尚属合理——但如果只有 CLI 模式，这层中间层就是过度设计。

---

## Layer 5：数据流（Data Transformation）

### 完整数据变换链

```
[输入层]
用户命令行参数 / Webhook JSON
        |
        | BuildMRJobFromInput / BuildLocalJob / BuildBranchJob
        v
[Job 层]
review.Job{Type, Label, Prompt(含 diff), Repo, MRNumber}
        |
        | Runner.Prepare()
        |  ├── 创建 AIProvider 实例
        |  └── 注入 ContextGatherer（可选）
        v
[组装层]
orchestrator.OrchestratorConfig
  {Reviewers[], Analyzer, Summarizer, ContextGatherer, Options}
        |
        | orchestrator.New()
        v
DebateOrchestrator
        |
        | RunStreaming(label, prompt, display)
        |
        |==== 阶段 1：并行分叉 (errgroup) ========================
        |
        |  [A] ContextGatherer.Gather(diff, prNumber, baseBranch)
        |       │  ripgrep 搜索符号引用
        |       │  git log 查历史 MR
        |       └─> GatheredContext{Summary, RawReferences, RelatedPRs}
        |
        |  [B] Analyzer.ChatStream(prompt) ──> streaming chunks ──> 终端流式输出
        |       └─> analysis string（Markdown 格式的变更分析）
        |
        |==== 阶段 2：多轮辩论 ===================================
        |
        |  for round := 1..MaxRounds:
        |    buildMessages(reviewerID)  ← 快照所有历史消息
        |    并行 ChatStream × N reviewers ──> streaming chunks
        |    ↓ 全部完成后
        |    conversationHistory = append(msg × N)
        |    [ledger 模式] extractRoundIssueDeltas()
        |      └─> Summarizer.Chat(structurize_delta prompt)
        |          └─> StructurizeDelta{Add, Retract, Update, Support, Withdraw, Contest}
        |          └─> IssueLedger.ApplyDelta()
        |    [共识检测] Summarizer.Chat(convergence prompt)
        |      └─> "CONVERGED" / "NOT_CONVERGED" + 理由
        |
        |==== 阶段 3：总结 =======================================
        |
        |  [A] 并行收集各 Reviewer 总结
        |       └─> []DebateSummary
        |
        |  [B] Summarizer.Chat(final_conclusion prompt)
        |       └─> FinalConclusion string（Markdown）
        |
        |  [C-1] Legacy 模式：Summarizer.Chat(structurize_issues prompt)
        |         ParseReviewerOutput(response) ──> ReviewerOutput{Issues[], Verdict}
        |         DeduplicateIssues(issuesByReviewer) ──> []MergedIssue
        |
        |  [C-2] Ledger 模式：
        |         mergeAllLedgers() + CanonicalizeMergedIssues()
        |         ApplyCanonicalSignals() ──> []MergedIssue
        |
        v
[输出层]
DebateResult{PRNumber, Analysis, Context, Messages, Summaries,
             FinalConclusion, TokenUsage, ConvergedAtRound, ParsedIssues}
        |
        |  分叉 (Fork)
        |
        +──> 终端彩色显示（display.FinalConclusion + IssuesTable）
        |
        +──> 文件输出（JSON / Markdown，通过 --output）
        |
        +──> 平台评论发布
              reviewpost.ConvertIssuesToPlatform(issues)
              ──> []IssueForComment
              plat.PostIssuesAsComments()
              ──> ReviewResult{Posted, Inline, FileLevel, Global, Failed}
```

### 数据变换管道图（简化）

```
git diff / MR API
      |
      | (shell exec / glab CLI)
      v
raw diff text
      |
      | BuildXxxJob() + prompt template
      v
review prompt (含 diff + 审查要求)
      |
      | Analyzer: ChatStream()
      v
analysis report (Markdown)
      |
      | Reviewer × N: ChatStream() × MaxRounds
      v
conversation history ([]DebateMessage)
      |
      | Summarizer.Chat() [structurize]
      v
raw JSON: {"issues": [...], "verdict": "...", "summary": "..."}
      |
      | ParseReviewerOutput() / ParseStructurizeDelta()
      v
[]ReviewIssue (每个 reviewer 的问题)
      |
      | DeduplicateIssues() ── Jaccard 相似度去重
      v
[]MergedIssue (合并后，按 severity 排序)
      |
      | reviewpost.ConvertIssuesToPlatform()
      v
[]IssueForComment{File, Line, Title, Severity, SuggestedFix}
      |
      | plat.PostIssuesAsComments()
      | ClassifyCommentsByDiffEx() --> 三级降级
      v
PR/MR inline comments
```

### 核心类型参考表

| 类型 | 文件:行 | 关键字段 | 用途 |
|---|---|---|---|
| `review.Job` | `internal/review/job.go:14 (Job)` | `Type, Label, Prompt, Repo, MRNumber` | 一次审查任务的完整描述，Prompt 包含 diff |
| `orchestrator.Reviewer` | `internal/orchestrator/types.go:16 (Reviewer)` | `ID, Provider, SystemPrompt` | 封装一个 AI 角色（审查者/分析器/总结器） |
| `orchestrator.DebateMessage` | `internal/orchestrator/types.go:34 (DebateMessage)` | `ReviewerID, Round, Content, Timestamp` | 辩论历史中的一条消息记录 |
| `orchestrator.ReviewIssue` | `internal/orchestrator/types.go:301 (ReviewIssue)` | `Severity, Category, File, Line, Title, Description, SuggestedFix` | 从审查者自由文本中解析出的单个结构化问题 |
| `orchestrator.MergedIssue` | `internal/orchestrator/types.go:383 (MergedIssue)` | `ReviewIssue, CanonicalID, RaisedBy, SupportedBy, ContestedBy, Descriptions` | 跨 Reviewer 去重合并后的问题，含完整归属信息 |
| `orchestrator.StructurizeDelta` | `internal/orchestrator/types.go:436 (StructurizeDelta)` | `Add, Retract, Update, Support, Withdraw, Contest` | Ledger 模式下单轮增量变更（6种操作） |
| `orchestrator.IssueLedger` | `internal/orchestrator/ledger.go:19 (IssueLedger)` | `ReviewerID, Issues map[string]*LedgerIssue, nextID` | 单个 Reviewer 的问题账本，跨轮次追踪 |
| `orchestrator.DebateResult` | `internal/orchestrator/types.go:259 (DebateResult)` | `Analysis, Messages, Summaries, FinalConclusion, TokenUsage, ParsedIssues` | 整个辩论的最终输出汇总 |
| `provider.AIProvider` | `internal/provider/provider.go:19 (AIProvider)` | `Chat(), ChatStream()` | 所有 AI 后端的统一接口 |
| `platform.Platform` | `internal/platform/platform.go:49 (Platform)` | 组合6个子接口 | 所有代码托管平台的统一接口 |

### Q&A

> **Q**：为什么需要 `MergedIssue` 这个中间格式，直接用 `ReviewIssue` 不行吗？
>
> **Evidence**：`types.go:301 (ReviewIssue)` 只包含单个问题的字段。`types.go:383 (MergedIssue)` 额外包含 `RaisedBy`、`SupportedBy`、`ContestedBy`、`Descriptions` 等跨 Reviewer 归属字段。`issueparser.go:631 (DeduplicateIssues)` 执行合并逻辑。
>
> **Inference**：`ReviewIssue` 是单个 Reviewer 的视角。当多个 Reviewer 指出同一问题时，需要记录归属信息（谁提出、谁支持、谁反对）。`MergedIssue` 保留最高严重程度、合并所有描述文本，使平台评论能注明"此问题由 claude 和 codex 共同发现"，提高可信度。

---

## Layer 6：执行流（Execution Path）

### Happy Path 调用序列

```
main.go:9  main()
  |
  v
cmd/root.go:25  Execute()
  |
  v
cmd/review.go:59  runReview()
  |
  +-- display.New()                               [display/terminal.go:42]
  +-- config.LoadConfig(configPath)               [config/config.go:152]
  +-- detect.FromRemote(platformType, host)       [platform/detect/detect.go:25]
  |     git remote get-url origin -> 正则匹配
  |
  +-- review.BuildMRJobFromInput(arg, resolver)   [review/job.go]
  |     platform.GetInfo(mrID, repo)  <- glab/gh CLI
  |     platform.GetDiff(mrID, repo)  <- glab/gh CLI
  |     返回 Job{Prompt: "diff + MR info"}
  |
  +-- review.NewRunner(cfg, checkoutMgr)          [review/runner.go:83]
  +-- runner.Prepare(job, opts)                   [review/runner.go:110]
  |     provider.CreateProvider() × (reviewers + analyzer + summarizer)
  |     [可选] checkout.Manager.Checkout()
  |     [可选] context.NewContextGathererAdapter()
  |     orchestrator.New(OrchestratorConfig{...})
  |     返回 PreparedRun
  |
  +-- prepared.Run(ctx, display)                  [review/runner.go:65]
      |
      v
  orchestrator.RunStreaming(label, prompt, display) [orchestrator/orchestrator.go:189]
      |
      +==== 阶段 1 ============================
      | newRun(prompt)
      | startSessions(label)
      | runAnalysisPhase(ctx, label, prompt, d)   [orchestrator/orchestrator.go:360]
      |   errgroup.WithContext(ctx)
      |   g.Go: contextGatherer.Gather()          <- 并行
      |   g.Go: analyzer.ChatStream()             <- 并行
      |   g.Wait()
      |
      +==== 阶段 2 ============================
      | runDebatePhase(ctx, display)              [orchestrator/orchestrator.go:425]
      |   [ledger mode] initIssueLedgers()
      |   for round := 1..MaxRounds:
      |     runDebateRound(ctx, round, display)   [orchestrator/orchestrator.go:483]
      |       buildMessages(reviewerID) × N  <-- 快照
      |       errgroup.Go × N reviewers
      |         ChatStream() <-- 并行
      |         OnMessageChunk() callback
      |       g.Wait()
      |       append to conversationHistory
      |     [ledger] extractRoundIssueDeltas()
      |       errgroup.Go × N reviewers
      |         extractIssueDelta() <- Summarizer.Chat()
      |         IssueLedger.ApplyDelta()
      |     [converge] checkConvergence()
      |       Summarizer.Chat(convergence prompt)
      |       → CONVERGED: break
      |
      +==== 阶段 3 ============================
      | runSummaryPhase(ctx, label, d, converged) [orchestrator/orchestrator.go:627]
      |   collectSummaries(ctx, display)
      |     errgroup.Go × N reviewers
      |       reviewer.ChatStream(summary prompt) <- 并行
      |   getFinalConclusion(ctx, summaries, d)
      |     summarizer.Chat(conclusion prompt)
      |   [ledger] structurizeIssuesFromLedgers()
      |     CanonicalizeMergedIssues()
      |     ApplyCanonicalSignals()
      |   [legacy] structurizeIssuesLegacy()
      |     summarizer.Chat(structurize prompt) + 重试
      |     ParseReviewerOutput()
      |     DeduplicateIssues()
      |   return DebateResult{...}
      v
  回到 cmd/review.go
  +-- display.FinalConclusion(result.FinalConclusion)
  +-- display.IssuesTable(result.ParsedIssues)
  +-- [如有 issues] plat.PostIssuesAsComments()
        ClassifyCommentsByDiffEx()  <- 三级降级分类
        plat.PostReview()           <- gh/glab CLI
  +-- [final conclusion] upsertSummaryNote()
  +-- display.TokenUsage()
  +-- [--output] saveOutput()
```

### 并发边界

| 并发区域 | 使用的原语 | 并发的内容 |
|---|---|---|
| 阶段 1（分析阶段） | `errgroup.WithContext` | 上下文收集 goroutine + 预分析 goroutine |
| 阶段 2 每轮辩论 | `errgroup.WithContext` | N 个 Reviewer 的 ChatStream，互相独立 |
| Ledger 增量提取 | `errgroup.WithContext` + `sync.Mutex` | 每个 Reviewer 的 delta 提取并行；`mu` 保护 `canonicalSignals` 写入 |
| 阶段 3 总结收集 | `errgroup.WithContext` | N 个 Reviewer 并行提交总结 |
| Server Webhook | `chan struct{}` 信号量 | 最多 `MaxConcurrent`（默认 3）个并发审查 |
| ReviewerStatus 更新 | `sync.Mutex` | 并行 goroutine 更新各自状态时加锁 |

**关键设计**：`runDebateRound` 在执行并行 ChatStream 前，先用 `buildMessages` 为所有 Reviewer **构建消息快照**（见 `orchestrator.go:484 (runDebateRound)`）。这确保所有 Reviewer 看到完全相同的辩论历史，先完成的 Reviewer 的输出不会影响后完成的 Reviewer 的输入，保证辩论的公平性。

### 关键错误处理分支

```
runReview()
  |
  +-- config.LoadConfig() 失败 --> 打印错误, 退出 (fatal)
  +-- detect.FromRemote() 失败 --> Warnf 警告, 继续 (非 fatal)
  +-- runner.Prepare() 失败 --> 返回错误, 退出 (fatal)
  |
  prepared.Run()
  |
  orchestrator.RunStreaming()
    |
    +-- runAnalysisPhase()
    |     contextGatherer.Gather() 失败 --> 忽略, gatheredContext=nil (非 fatal)
    |     analyzer.ChatStream() 失败 --> 返回 error (fatal, 中止辩论)
    |
    +-- runDebatePhase()
    |     reviewer.ChatStream() 失败 --> errgroup 传播, 取消其他 goroutine (fatal)
    |     extractRoundIssueDeltas() 失败 --> Warnf + 跳过 (非 fatal)
    |     checkConvergence() 失败 --> 视为未收敛, 继续下一轮 (非 fatal)
    |
    +-- runSummaryPhase()
          collectSummaries() 失败 --> 返回 error (fatal)
          getFinalConclusion() 失败 --> 返回 error (fatal)
          structurizeIssues (最多重试 3 次) 失败 --> parsedIssues=[] (非 fatal)
    |
  回到 cmd/review.go
    |
    +-- PostIssuesAsComments() 失败 --> SpinnerFail + Warnf (非 fatal, 审查已完成)
    +-- saveOutput() 失败 --> 返回错误 (fatal)
```

### 序列图（Happy Path，2个Reviewer，2轮）

```
cmd/review   Runner   Orchestrator  Reviewer-A  Reviewer-B  Summarizer   Platform
     |          |           |            |            |           |           |
     |--Prepare>|           |            |            |           |           |
     |          |--New()--->|            |            |           |           |
     |<-PreparedRun---------|            |            |           |           |
     |--Run()-------------->|            |            |           |           |
     |          |           |--startSessions()------->|           |           |
     |          |           |                                                 |
     |          |           |==== 阶段1：并行 =================================|
     |          |           |--ChatStream(diff)------>| (Analyzer)            |
     |          |           |--Gather(diff)---------> | (ContextGatherer)     |
     |          |           |<--analysis chunks-------|           |           |
     |          |           |<--context-----------    |           |           |
     |          |           |                                                 |
     |          |           |==== 阶段2：Round 1 ==============================|
     |          |           |--buildMessages()--->snapshot A,B               |
     |          |           |--ChatStream()------>| (A)                       |
     |          |           |--ChatStream()------>            | (B)           |
     |          |           |<--chunk, chunk-----|            |               |
     |          |           |<--chunk, chunk-----|            |               |
     |          |           |<--done A-----------|            |               |
     |          |           |<--done B-----------             |               |
     |          |           |--extractIssueDelta()-->         |    Summarizer.|
     |          |           |<--StructurizeDelta------------- |           |   |
     |          |           |--checkConvergence()------------>|           |   |
     |          |           |<--"NOT_CONVERGED"---------------|           |   |
     |          |           |                                                 |
     |          |           |==== 阶段2：Round 2 ==============================|
     |          |           |  (同上, 但 messages 包含 Round 1 历史)           |
     |          |           |--checkConvergence()------------>|           |   |
     |          |           |<--"CONVERGED" at Round 2--------|           |   |
     |          |           |                                                 |
     |          |           |==== 阶段3 ========================================|
     |          |           |--collectSummaries()->|          |               |
     |          |           |<--summary A---------|           |               |
     |          |           |<--summary B---------            |               |
     |          |           |--getFinalConclusion()---------->|               |
     |          |           |<--FinalConclusion---------------|               |
     |          |           |--structurizeIssuesFromLedgers()                 |
     |          |           |--return DebateResult                            |
     |<--DebateResult-------|            |            |           |           |
     |--PostIssuesAsComments()---------------------------------------------->|
     |<--ReviewResult---------------------------------------------------------|
```

### 错误处理流程图

```
structurizeIssues (legacy 模式)
  |
  for attempt := 1..3:
    Summarizer.Chat(structurize_prompt)
    |
    +-- ParseReviewerOutput(response)
    |     extractJSON() --> JSON not found --> attempt+1, retry
    |     json.Unmarshal() --> syntax error --> attempt+1, retry
    |     schema.ValidateIssuesJSON() --> schema error
    |       --> append errors to next prompt --> attempt+1, retry
    |     issues 为空 --> attempt+1, retry
    |
    +-- 成功 --> return []ReviewIssue
    |
  all attempts failed --> parsedIssues = [] (不 fatal, 只是无评论)
```

### 关键函数索引

| 函数 | 文件:行 | 一行描述 |
|---|---|---|
| `cmd.runReview` | `cmd/review.go:59 (runReview)` | review 命令的顶层入口，协调配置加载、平台检测、准备、执行、发布全流程 |
| `review.Runner.Prepare` | `internal/review/runner.go:110 (Prepare)` | 依赖注入组装层，将 config 变成可执行的 PreparedRun |
| `orchestrator.DebateOrchestrator.RunStreaming` | `internal/orchestrator/orchestrator.go:189 (RunStreaming)` | 三阶段辩论核心入口（分析→辩论→总结） |
| `debateRun.runAnalysisPhase` | `internal/orchestrator/orchestrator.go:360 (runAnalysisPhase)` | errgroup 并行执行上下文收集和预分析 |
| `debateRun.runDebateRound` | `internal/orchestrator/orchestrator.go:483 (runDebateRound)` | 单轮辩论：快照消息→并行 ChatStream→追加历史 |
| `debateRun.extractRoundIssueDeltas` | `internal/orchestrator/orchestrator.go:713 (extractRoundIssueDeltas)` | Ledger 模式：并行从每个 Reviewer 输出中提取增量 delta |
| `debateRun.checkConvergence` | `internal/orchestrator/orchestrator.go:221 (checkConvergence)` | 调用 Summarizer 判断是否达成共识 |
| `orchestrator.ParseReviewerOutput` | `internal/orchestrator/issueparser.go:120 (ParseReviewerOutput)` | 两级回退策略从自由文本中提取结构化 JSON 问题 |
| `orchestrator.DeduplicateIssues` | `internal/orchestrator/issueparser.go:631 (DeduplicateIssues)` | Jaccard 相似度去重合并多 Reviewer 的问题 |
| `orchestrator.isSimilarIssue` | `internal/orchestrator/issueparser.go:704 (isSimilarIssue)` | 三层过滤判断两个问题是否相似（文件→行号→Jaccard） |
| `provider.WithRetry` | `internal/provider/retry.go:53 (WithRetry)` | 泛型指数退避重试，针对暂时性错误（timeout/429/503） |
| `platform.ClassifyCommentsByDiffEx` | `internal/platform/diffparse.go:68 (ClassifyCommentsByDiffEx)` | 根据 diff 行号信息将评论分类为 inline/file/global |
| `detect.FromRemote` | `internal/platform/detect/detect.go:25 (FromRemote)` | 通过 git remote URL 正则匹配自动检测 GitHub/GitLab |
| `server.New` | `internal/server/server.go:74 (New)` | 创建 Webhook Server，初始化信号量和 runner |
| `server.RunServerReview` | `internal/server/reviewer.go:26 (RunServerReview)` | Webhook 触发的审查流程（使用 NoopDisplay） |

### Q&A

> **Q**：如果 Reviewer A 在第 2 轮的 ChatStream 执行到一半时发生错误，会发生什么？
>
> **Evidence**：`orchestrator.go:529 (runDebateRound)` 使用 `errgroup.WithContext` 管理所有 Reviewer 的 goroutine。当任一 goroutine 返回非 nil 错误时，errgroup 的 `gctx` 会被立即取消。`cmd/review.go:135 (runReview)` 处捕获并打印错误。
>
> **Inference**：Reviewer A 出错后，errgroup cancel 导致 Reviewer B 的 `ChatStream` 也中断。错误从 `runDebateRound` → `runDebatePhase` → `RunStreaming` 向上传播，最终打印错误并退出。整个审查失败，不产生部分结果——这是"fail-fast"策略，优先于产出不完整输出。

---

## Code Insights

### Insight 1：Go 泛型用于重试函数（消除样板代码）

**代码位置**：`internal/provider/retry.go:53 (WithRetry)`

**是什么**：`WithRetry[T any]` 是一个泛型函数，对任意返回类型的操作提供统一的指数退避重试。

```go
func WithRetry[T any](fn func() (T, error), opts *RetryOptions) (T, error) {
    // ...
    var zero T  // T 类型的零值，用于错误时返回
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        result, err := fn()
        if err == nil {
            return result, nil
        }
        // ...
    }
    return zero, lastErr
}
```

**设计意图**：消除多种返回类型的操作都需要单独写重试函数的样板代码。

**优点**：泛型版本既类型安全又消除了重复。`var zero T` 惯用法优雅处理值类型（struct）的零值返回，调用方返回类型自动推断。

**局限性**：Go 泛型不能用于方法（只能用于顶层函数），限制了 API 设计灵活性——无法将 `WithRetry` 设计为 provider 的方法。`isTransientError` 基于字符串匹配判断错误类型，可能误判（如错误消息中碰巧包含 "timeout" 的非暂时性错误）。

**适用场景**：项目中有多种返回类型的可重试外部调用时值得采用；如果只有一种返回类型或各操作的重试策略差异大，直接内联更简单。

---

### Insight 2：errgroup 并行 + 快照保证辩论公平性

**代码位置**：`internal/orchestrator/orchestrator.go:484-530 (runDebateRound)`

**是什么**：在每轮辩论开始前，为所有 Reviewer **先**构建好消息快照，再并行执行。

```go
// 先快照
tasks := make([]reviewerTask, len(o.reviewers))
for i, r := range o.reviewers {
    messages := o.buildMessages(r.ID)  // 快照此刻的历史
    tasks[i] = reviewerTask{reviewer: r, messages: messages, ...}
}

// 再并行执行
rg, rgctx := errgroup.WithContext(ctx)
for i, task := range tasks {
    i, task := i, task  // 闭包变量捕获
    rg.Go(func() error {
        ch, errCh := task.reviewer.Provider.ChatStream(rgctx, task.messages, ...)
        // ...
    })
}
```

**设计意图**：确保同一轮辩论中所有 Reviewer 基于完全相同的历史信息做出判断，防止先完成的 Reviewer 影响后完成的 Reviewer 的输入。

**优点**：快照隔离保证辩论公平性；`i, task := i, task` 避免经典的 goroutine 闭包变量捕获陷阱。

**局限性**：快照意味着每轮都要复制完整消息列表，当辩论轮次多、消息量大时内存开销线性增长。快照模式也牺牲了"实时反馈"能力——先完成的 Reviewer 无法立即影响后完成的 Reviewer，如果某些场景需要这种"级联触发"效果则不适用。

**适用场景**：需要基于同一状态快照并行执行时（如批量 API 请求、并行数据处理）值得采用；如果允许或需要后续操作看到先前操作的结果（流水线模式），则不适用。

---

### Insight 3：接口组合设计 Platform

**代码位置**：`internal/platform/platform.go:6-56 (Platform)`

**是什么**：`Platform` 接口通过嵌入 6 个小接口组合而成，调用方可以只依赖需要的子接口。

```go
type Platform interface {
    Named
    MRProvider
    MRCommenter
    IssueCommenter
    RepoDetector
    HistoryProvider
}

// server 层只需要部分能力
type reviewPlatform interface {
    platform.Named
    platform.MRMetadataProvider
    platform.IssueCommenter
    platform.HistoryProvider
}
```

**设计意图**：让不同调用方只依赖它实际使用的接口方法子集，遵循接口隔离原则（ISP）。

**优点**：`server/reviewer.go` 定义了自己的 `reviewPlatform` 接口，只包含实际使用的子接口，测试时可以用更轻量的 mock 实现。完整的 `Platform` 接口用于需要全部能力的场景。

**局限性**：6 个子接口的组合增加了认知负担——新开发者需要理解哪个子接口对应哪些方法。当接口频繁变化时，维护多个小接口的成本高于一个大接口。此外，Go 的隐式接口实现意味着不看具体实现文件，无法快速知道哪些结构体满足哪些子接口。

**适用场景**：调用方确实只使用部分方法且有 mock 需求时值得采用；如果所有调用方都使用全部方法，细粒度拆分只增加复杂度而无收益。

---

### Insight 4：ContextGathererInterface 解决循环依赖

**代码位置**：`internal/orchestrator/types.go:165-178 (ContextGathererInterface)`

**是什么**：在 `orchestrator` 包内定义 `ContextGathererInterface` 接口，由外部包（`internal/context`）实现，通过 `internal/review` 的适配器注入。

```go
// orchestrator 包内定义接口
type ContextGathererInterface interface {
    Gather(ctx context.Context, diff, prNumber, baseBranch string) (*GatheredContext, error)
}

// internal/context/adapter.go 实现接口
type ContextGathererAdapter struct { ... }
func (a *ContextGathererAdapter) Gather(...) (*orchestrator.GatheredContext, error) { ... }

// internal/review/runner.go 注入
contextGatherer = appctx.NewContextGathererAdapter(contextProvider, ...)
```

**设计意图**：打破 orchestrator 和 context 包之间的双向引用，让 Go 编译通过。

**优点**：接口在依赖方（orchestrator）内部定义，实现在被依赖方（context）提供，第三方（review）作为组装层——这是解决 Go 循环导入的标准依赖倒置模式。

**局限性**：增加了一层间接性——要找到 `Gather()` 的实际实现需要跟踪 adapter 和 runner 的注入链路，IDE 的"跳转到实现"不一定能直接导航。如果包结构本身就不合理（职责划分有误），依赖倒置只是绕过问题而非解决问题——更根本的做法可能是重新划分包边界。

**适用场景**：包之间确实存在合理的双向引用时值得采用；如果双向引用是因为包拆分不当，更好的做法是重新划分包边界而非添加间接层。

---

### Insight 5：Ledger 模式的增量设计（两种结构化模式对比）

**代码位置**：`internal/orchestrator/orchestrator.go:679-687 (useLedgerStructurize)` 和 `internal/orchestrator/ledger.go (IssueLedger)`

**是什么**：Hydra 支持两种问题结构化模式——Legacy（辩论结束后一次性全量提取）和 Ledger（每轮增量追踪，有兜底回退）。

```go
// 判断模式
func (o *DebateOrchestrator) useLedgerStructurize() bool {
    return strings.EqualFold(strings.TrimSpace(o.options.StructurizeMode), "ledger")
}

// Ledger 模式的兜底逻辑
func (o *debateRun) structurizeIssuesFromLedgers(ctx context.Context, display DisplayCallbacks) []MergedIssue {
    allIssues := o.collectCanonicalInputsFromLedgers()
    if len(allIssues) > 0 {
        return ApplyCanonicalSignals(CanonicalizeMergedIssues(allIssues), o.canonicalSignals)
    }
    // 兜底：增量全失败时，回退到 legacy 模式
    if o.hasReviewerMessages() {
        util.Warnf("no issues from ledgers, falling back to legacy structurizer")
        return o.structurizeIssuesLegacy(ctx, display)
    }
    return nil
}
```

**设计意图**：在辩论过程中实时追踪问题的演变（新增/撤回/支持/反对），而非仅在辩论结束后一次性提取。

**优点**：Ledger 模式能提供更精确的问题置信度。渐进增强 + 优雅降级：Ledger 失败时自动回退到 Legacy，保证审查结果不丢失。

**局限性**：两种模式并存增加了代码复杂度和测试矩阵（需要分别测试 ledger 路径、legacy 路径、以及 ledger fallback 到 legacy 的路径）。Ledger 模式依赖 Summarizer 正确解析每轮增量 delta，一旦 prompt 格式与 AI 输出不匹配，整个 ledger 链路静默失败回退到 legacy，难以察觉质量下降。如果 Ledger 已经充分验证，保留 Legacy 路径只增加维护负担。

**适用场景**：引入新功能需要兼容旧逻辑、或增量处理可能失败需要全量兜底时值得采用；如果新模式已充分验证且旧模式可以完全废弃，保留两套路径只增加维护负担。

---

*文档生成日期：2026-03-07*
*代码分析基于 commit: a132cb6（feat/gitlab 分支）*
