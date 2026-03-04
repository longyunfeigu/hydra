# Structurize Issues 重构计划

## 一句话目标

把"辩论结束后一次性提取所有 issues"改为"每轮增量提取 + 最后本地合并"，解决输入过长导致的失败问题。

## 当前问题

`structurizeIssues()`（`orchestrator.go:744`）的做法：

```
reviewer-A Round1 文本 + reviewer-A Round2 文本 + reviewer-B Round1 文本 + ...
→ 全部拼成一个 reviewText
→ 一次性发给 LLM 提取 issues
```

**问题**：`reviewText` 随 `reviewers数 × 轮次数 × 每轮输出长度` 线性增长，经常超出模型上下文限制。

## 改后的流程

```
                        ┌─────────────────────────────────────────────┐
  辩论进行中             │  每轮结束后，立即对每个 reviewer 做增量提取    │
                        └─────────────────────────────────────────────┘

  Round 1 结束:
    reviewer-A 的输出 ──→ LLM 提取 ──→ ledger-A = {I1, I2, I3}
    reviewer-B 的输出 ──→ LLM 提取 ──→ ledger-B = {I1, I2}

  Round 2 结束:
    reviewer-A 的输出 + ledger-A 摘要 ──→ LLM 提取 delta ──→ 更新 ledger-A
    reviewer-B 的输出 + ledger-B 摘要 ──→ LLM 提取 delta ──→ 更新 ledger-B
    （delta 可以是：新增 issue / 撤回 issue / 更新字段）

  ...每轮重复...

                        ┌─────────────────────────────────────────────┐
  辩论结束后             │  本地合并所有 ledger，不再调 LLM               │
                        └─────────────────────────────────────────────┘

    ledger-A 的有效 issues ─┐
    ledger-B 的有效 issues ─┼──→ 本地 DeduplicateMergedIssues() ──→ 最终 []MergedIssue
    ledger-C 的有效 issues ─┘
```

**关键**：每次 LLM 调用的输入 = **本轮单个 reviewer 的输出** + **简短的 ledger 摘要表格**，不再包含历史全文。

## 分阶段交付

### P0：增量提取 + 本地合并（解决核心的"太长"问题）

这是最小可用版本，改完就能解决 prompt 太长的问题。

#### 新增数据结构

```go
// internal/orchestrator/ledger.go（新文件）

// IssueLedger 维护单个 reviewer 跨轮次的 issue 账本
type IssueLedger struct {
    ReviewerID string
    Issues     map[string]*LedgerIssue  // issueID -> issue
    nextID     int                       // 自增 ID 计数器
}

// LedgerIssue 是 ledger 中的一条记录
type LedgerIssue struct {
    ID           string   // "I1", "I2", ...（本地生成，不让模型编）
    Status       string   // "active" | "retracted"
    Severity     string
    Category     string
    File         string
    Line         *int
    Title        string
    Description  string   // 简短描述（非完整 description）
    SuggestedFix string
    Round        int      // 首次提出的轮次
}
```

#### 执行流程（伪代码）

```
func (o *DebateOrchestrator) RunDebate():
    // 现有逻辑不变
    runAnalysisPhase()

    // 为每个 reviewer 创建空 ledger
    ledgers = map[reviewerID]*IssueLedger{}

    for round = 1..maxRounds:
        runDebateRound(round)  // 现有逻辑，不改

        // ★ 新增：每轮结束后，并行对每个 reviewer 做增量提取
        g, ctx := errgroup.WithContext(ctx)
        for each reviewer:
            g.Go(func() error {
                content = 该 reviewer 本轮的输出
                summary = ledgers[reviewer].BuildSummary()  // 简短表格（第1轮为空）
                delta   = callLLM(content, summary)         // 提取增量
                if delta != nil {
                    ledgers[reviewer].ApplyDelta(delta)     // 更新 ledger
                }
                // 解析失败不报 error，跳过该轮即可
                return nil
            })
        g.Wait()

        if converged: break

    // ★ 新增：本地合并替代原来的 structurizeIssues()
    allIssues = mergeAllLedgers(ledgers)

    // 兜底：所有增量提取都失败时，fallback 到 legacy 一次性提取
    if len(allIssues) == 0 && len(o.conversationHistory) > 0 {
        allIssues = o.structurizeIssuesLegacy(ctx, display)
    }

    result.ParsedIssues = DeduplicateMergedIssues(allIssues)
```

#### LLM 调用的输入输出

**输入**（发给 LLM 的 prompt）：

```
以下是 reviewer "security-hawk" 在第 2 轮的输出：
---
{本轮输出文本，通常 1-3KB}
---

该 reviewer 之前已发现的 issues：
| ID | Severity | File:Line        | Title                    |
|----|----------|------------------|--------------------------|
| I1 | high     | auth.go:42       | Missing token validation |
| I2 | medium   | handler.go:15    | Unchecked error return   |

请提取本轮新增的 issues，以及对已有 issues 的更新或撤回。
输出 JSON 格式：
{
  "add": [{ "severity": "...", "file": "...", "line": ..., "title": "...", "description": "..." }],
  "retract": ["I1"],
  "update": [{ "id": "I2", "severity": "high", "description": "..." }]
}
```

**输出**（LLM 返回）：

```json
{
  "add": [
    { "severity": "high", "file": "db.go", "line": 88, "title": "SQL injection risk", "description": "..." }
  ],
  "retract": [],
  "update": [
    { "id": "I2", "severity": "high", "description": "Actually this is worse than medium..." }
  ]
}
```

#### 需要新增/修改的文件

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/orchestrator/ledger.go` | 新增 | IssueLedger 数据结构和方法 |
| `internal/orchestrator/ledger_test.go` | 新增 | ledger 单元测试 |
| `internal/schema/schemas/issues_delta.json` | 新增 | delta 输出的 JSON Schema |
| `internal/prompt/templates/structurize_delta.tmpl` | 新增 | 增量提取的 prompt 模板 |
| `internal/prompt/templates/structurize_delta_system.tmpl` | 新增 | 增量提取的 system prompt |
| `internal/orchestrator/orchestrator.go` | 修改 | 在 `runDebateRound` 后调用增量提取；替换 `structurizeIssues` 调用 |
| `internal/orchestrator/types.go` | 修改 | 如需新增类型 |

#### 不改动的部分

- `runDebateRound()`：辩论逻辑完全不变
- `DeduplicateMergedIssues()`：最后一步仍然复用现有去重
- `ParseReviewerOutput()`：delta 解析用新的 parser，不影响旧代码
- `issues.json`：最终输出 schema 不变

#### 回退策略

- 用配置项控制：`structurize_mode: legacy | ledger`（默认 `legacy`）
- legacy 模式调用原来的 `structurizeIssues()`
- 稳定后再切默认值

---

### P1：严格模式裁决（提高质量）

在 P0 基础上，利用 ledger 的事件记录做更精细的裁决。

#### 动机

辩论中 reviewer 可能：
- 第1轮提出一个 issue，第2轮被另一个 reviewer 反驳，第3轮又没人再提 → 应该丢弃或降级
- 第1轮提出一个 issue，第2轮自己改口撤回 → 应该丢弃

P0 只处理简单的 `retract`（reviewer 主动撤回）。P1 增加 `rebut`（被反驳）的处理。

#### 改动

在 `LedgerIssue` 上新增事件记录：

```go
type IssueEvent struct {
    Round    int    // 发生在第几轮
    Action   string // "assert" | "retract" | "rebut" | "update"
    Evidence string // 简短证据摘录
}
```

最终合并前，对每个 issue 做裁决：
- 最后一个事件是 `retract` → 丢弃
- 有 `rebut` 且之后无新证据的 `assert`/`update` → 降级为 `nitpick`

"新证据"的判定（满足任一条）：
- 给出了更精确的 file:line
- 引用了新的代码片段
- 给出了可复现的行为描述

#### 新增文件

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/orchestrator/adjudicate.go` | 新增 | 裁决逻辑 |
| `internal/orchestrator/adjudicate_test.go` | 新增 | 裁决测试 |

---

### P2：Description 扩写（优化输出质量）

#### 动机

当前 `issues.json` schema 要求 `description` 很长（包含问题描述、原始代码、修复代码、修复原因）。在增量提取阶段生成这么长的 description 会增加失败概率。

#### 做法

1. 增量提取阶段：`description` 只要求 1-2 句简短描述
2. 全局合并完成后，**批量**扩写 description（每批 5 个 issue，多批并行）

#### 批量扩写策略

**批量大小**：每次 5 个 issue。依据：
- 每个 issue 的输入很短（标题+简述约 100 token）
- 每个 issue 的输出约 300-500 token
- 5 个总共 ≈ 2500 token 输出，不会截断

**并行**：多个批次之间互相无依赖，可以并行调用 LLM。20 个 issue = 4 批 × 5 个，并行跑只需等最慢那批。

**扩写 prompt 示例**：

```
请为以下 5 个 issues 分别生成完整的 description。

Issue 1: [high] auth.go:42 - Missing token validation
  简述：未校验 token 有效期...

Issue 2: [medium] handler.go:15 - Unchecked error return
  简述：忽略了 db.Query 的 error...

...

对每个 issue 输出 markdown 格式的 description，包含：
(1) 问题是什么 (2) 为什么重要 (3) 原始代码 (4) 修复建议 (5) 修复原因

输出 JSON: [{ "index": 1, "description": "..." }, ...]
```

#### 新增文件

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/prompt/templates/structurize_expand.tmpl` | 新增 | 批量扩写 prompt |
| `internal/orchestrator/orchestrator.go` | 修改 | 合并后调用批量扩写 |

---

## 关键设计决策

### Issue ID 生成规则

由本地代码生成，不让 LLM 编造：
- 每个 ledger 内自增：`I1`, `I2`, `I3`, ...
- 新 issue 从 LLM 返回的 `add` 中提取后，由本地分配 ID
- LLM 只需在 `retract`/`update` 时引用已有的 ID

### Ledger 摘要格式

紧凑的表格，控制 token 消耗：

```
| ID | Severity | File:Line     | Title                    |
|----|----------|---------------|--------------------------|
| I1 | high     | auth.go:42    | Missing token validation |
| I2 | medium   | handler.go:15 | Unchecked error return   |
（共 2 条 active issues）
```

默认最多展示 100 条。超过则只保留 severity 更高的，并注明截断。

### 第1轮的处理

**统一走 delta 格式**，不搞两套路径。第1轮 delta 里只会有 `add`，不会有 `retract`/`update`。

模板里用条件判断省略空的 ledger 摘要：

```
{{- if .LedgerSummary}}
该 reviewer 之前已发现的 issues：
{{.LedgerSummary}}
{{- end}}
```

这样代码路径只有一条，测试和维护简单。第1轮和后续轮的唯一区别就是 prompt 里有没有 ledger 摘要。

### 并行处理

**每轮辩论结束后，多个 reviewer 的增量提取并行跑**。时序如下：

```
Round N 辩论（reviewer 并行）
    → 全部完成
    → 增量提取（reviewer 并行，用 errgroup）
    → Round N+1
```

每个 reviewer 的 ledger 互相独立，不存在竞争，无需加锁。

不建议把"辩论"和"提取"交错并行（比如 reviewer-A 辩论完就立刻提取，不等 reviewer-B）。原因是增加了复杂度但收益很小——增量提取本身很快（输入短、输出短）。

### Delta 解析失败的退化

**分两层处理**：

**第一层：单轮单 reviewer 失败 → 跳过该轮更新**

如果某个 reviewer 某轮增量提取失败（重试3次仍解析不出 JSON）：
- 跳过该轮的 ledger 更新，该 reviewer 这轮的新 issues 丢失，但旧 issues 不受影响
- 日志记录 warning
- 不影响其他 reviewer 的提取

**第二层：全部失败 → fallback 到 legacy**

如果所有 reviewer 在所有轮次都失败（最终合并时发现 0 个 issue），fallback 到原来的一次性提取作为最后手段：

```go
// runSummaryPhase 中
allIssues := mergeAllLedgers(ledgers)
if len(allIssues) == 0 && len(o.conversationHistory) > 0 {
    // 所有增量提取都失败了，fallback 到 legacy
    allIssues = o.structurizeIssuesLegacy(ctx, display)
}
```

这样既避免了混用两套逻辑的复杂度，又保证了极端情况下不会输出空结果。

---

## 测试计划

### P0 单元测试

```
TestLedger_AddIssues           - 基本新增
TestLedger_RetractIssue        - 撤回已有 issue
TestLedger_UpdateIssue         - 更新已有 issue 的字段
TestLedger_BuildSummary        - 摘要生成格式正确
TestLedger_BuildSummary_Limit  - 超过 100 条时截断
TestLedger_ToMergedIssues      - 转换为 MergedIssue（不含 retracted）
TestLedger_UnknownID           - update/retract 引用不存在的 ID → 跳过
TestParseDelta                 - 解析 delta JSON
TestParseDelta_Invalid         - 解析失败的各种情况
```

### P0 集成测试（mock provider）

```
TestIncrementalStructurize_TwoRounds    - 2轮辩论，第2轮有 retract
TestIncrementalStructurize_MultiReviewer - 多 reviewer 并行提取后合并
TestIncrementalStructurize_ParseFailure  - 某轮解析失败，不影响其他轮
```

### P1 单元测试

```
TestAdjudicate_RebutWithoutEvidence  - 被反驳且无新证据 → 降级
TestAdjudicate_RebutWithEvidence     - 被反驳但有新证据 → 保留
TestAdjudicate_RetractedIssue        - 主动撤回 → 丢弃
```
