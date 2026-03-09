# 修复审核流程：收敛困难 & 重复发现新问题

## Context

用户反馈两个核心问题：
1. **单次 run 内**：经常跑 5 轮还达不到共识
2. **跨 run**：即使之前的 comment 已改正，每次 rerun 都能找出新问题，没完没了

**根因**（prompt 设计 + 流程缺陷）：
- 辩论 prompt 每轮都要求"继续穷尽式 review" + "指出别人漏了什么"，导致范围持续膨胀
- 收敛判定 "Be VERY conservative - if there is ANY doubt, respond NOT_CONVERGED"，加上 "silence is NOT agreement" 等条件，几乎不可能判定收敛
- 跨 run 没有"问题记忆"，每次 rerun 从零审，自然每次都找出新东西

---

## P0: 单次 run 内收敛修复

### 改动 1: Go 代码 — 传递轮次信息到模板

**文件**: `internal/orchestrator/orchestrator.go`

- `debateRun` struct (line 74) 新增 `currentRound int` 字段
- `runDebateRound` (line 483) 开头设置 `o.currentRound = round`
- 3 处模板渲染加入 `Round` 和 `MaxRounds`：
  - line 1185 `reviewer_debate_session.tmpl`：加 `"Round": o.currentRound, "MaxRounds": o.options.MaxRounds`
  - line 1221 `reviewer_debate_full.tmpl`：加 `"Round": o.currentRound, "MaxRounds": o.options.MaxRounds`
  - line 1301 `convergence_check.tmpl`：加 `"MaxRounds": o.options.MaxRounds`

### 改动 2: 辩论 prompt — 后期轮次转向收敛

**文件**: `internal/prompt/templates/reviewer_debate_session.tmpl`

- 轮次 ≤2：保留响应他人观点 + 补充遗漏的关键内容
- 轮次 ≥3：切换到收敛模式 — 明确列出已达成一致的问题、不再提出新 issue（除非 critical）、对争议点给出最终立场

**文件**: `internal/prompt/templates/reviewer_debate_full.tmpl`

- 同样的轮次感知逻辑
- 移除 "find ALL real issues"、"Point out what others MISSED" 在后期轮次的影响

### 改动 3: 收敛判定 — 放松标准

**文件**: `internal/prompt/templates/convergence_system.tmpl`

- 将 "Be VERY conservative - if there is ANY doubt" 改为中性的判定指引

**文件**: `internal/prompt/templates/convergence_check.tmpl`

- 移除过严条件："silence is NOT agreement"、"DIFFERENT sets of issues without cross-validating"
- Round ≥3 使用务实标准：verdict 一致 + critical 问题一致 + 没有强烈未回应反对即可
- NOT_CONSENSUS 条件收窄为：verdict 不同、critical 被忽视、blocking 上有明确反对

---

## P1: 跨 run 增量 review

### 设计思路

基础设施已经齐全：
- `GetExistingComments(mrID, repo)` 已能从 GitHub/GitLab 获取所有评论
- `IsHydra` + `Meta` 字段能区分 Hydra 评论 vs 人工评论
- `HydraCommentMeta` 包含 `Status`（active/resolved/superseded）、`IssueKey`、`RunID`
- `StripHydraMeta()` 可以去掉 marker 只留正文

**核心实现**：在 review 开始前拉取上次 Hydra 留下的 active comments，格式化后注入 reviewer 的首轮 prompt。

### 改动 4: 新增 PreviousComments 传递链

**文件**: `internal/review/runner.go`

- `RunOptions` 新增 `Commenter platform.MRCommenter` 字段（可选）
- `Prepare()` 中：如果 `opts.Commenter != nil && job.Type == "pr"`，调用 `opts.Commenter.GetExistingComments(job.MRNumber, job.Repo)` 获取现有评论
- 过滤出 `IsHydra && Meta.Status == "active"` 的评论
- 格式化为文本，传给 `OrchestratorOptions`

**文件**: `internal/orchestrator/types.go`

- `OrchestratorOptions` 新增 `PreviousComments string` 字段

**文件**: `internal/orchestrator/orchestrator.go`

- `buildFirstRoundMessages()` (line 1067)：如果 `o.options.PreviousComments != ""`，构建 `previousCommentsSection` 并传给模板

### 改动 5: 首轮 prompt 注入上次评论

**文件**: `internal/prompt/templates/reviewer_first_round.tmpl`

在 context sections 之后加入：

```
{{.PreviousCommentsSection}}
```

当有上次评论时，section 内容类似：

```
## Previous Review Findings (from last Hydra run)
The following issues were flagged in the previous review and are still active.
Your PRIMARY task: check if each issue has been fixed in the current diff.
- Mark resolved issues as FIXED
- Mark unresolved issues as STILL OPEN
- Only raise NEW issues if they are critical/blocking

1. [high] `src/auth.go:45` - Missing input validation on user token
2. [medium] `src/handler.go:123` - Error not propagated to caller
...
```

### 改动 6: 调用方传入 Commenter

**文件**: `cmd/review.go`

- 在 `runner.Prepare()` 调用时，把 `plat`（已经实现了 `MRCommenter`）传入 `RunOptions.Commenter`

**文件**: `internal/server/reviewer.go`

- 同样，`plat` 实现了 `IssueCommenter` 但 server 模式的 `reviewPlatform` 接口需要额外组合 `MRCommenter`（或者直接传 `GetExistingComments` 的结果）

---

## 修改文件清单

| 文件 | 改动摘要 |
|------|----------|
| `internal/orchestrator/orchestrator.go` | P0: 加 `currentRound`，3 处模板渲染加 Round/MaxRounds；P1: `buildFirstRoundMessages` 加 PreviousCommentsSection |
| `internal/orchestrator/types.go` | P1: `OrchestratorOptions` 加 `PreviousComments string` |
| `internal/prompt/templates/reviewer_debate_session.tmpl` | P0: 轮次感知收敛 |
| `internal/prompt/templates/reviewer_debate_full.tmpl` | P0: 轮次感知收敛 |
| `internal/prompt/templates/convergence_system.tmpl` | P0: 移除过度保守偏向 |
| `internal/prompt/templates/convergence_check.tmpl` | P0: 渐进式放松标准 |
| `internal/prompt/templates/reviewer_first_round.tmpl` | P1: 加 PreviousCommentsSection |
| `internal/review/runner.go` | P1: RunOptions 加 Commenter，Prepare 中拉取并格式化评论 |
| `cmd/review.go` | P1: 传入 plat 作为 Commenter |
| `internal/server/reviewer.go` | P1: 传入 commenter 或调整接口 |

## 验证方式

1. `go build ./...` 编译通过
2. `go test ./internal/orchestrator/...` — 现有收敛测试 + prompt 渲染测试
3. `go test ./internal/prompt/...` — 模板渲染正常
4. `go test ./internal/review/...` — runner 测试
5. 用实际 PR 跑 `hydra review`：
   - P0 验证：观察是否在 2-3 轮内收敛，后期轮次不再大量产出新 issue
   - P1 验证：先跑一次 review 产出 comments → 修复部分 → 再跑一次，观察 reviewer 是否先检查旧 issue 状态而非从零审
