# Hydra: 借鉴 Magpie 实现 4 个特性

## Context

Hydra (Go) 和 Magpie (TypeScript) 是同一作者的两个多 AI 对抗式代码审查工具。Magpie 作为先行版本，已实现了一些 Hydra 缺少的特性。本次需要将以下 4 个特性从 Magpie 移植到 Hydra：

1. **Near-Line Matching (±20)** — inline 评论行号不在 diff 时，找最近有效行
2. **Round-1 Convergence** — 独立评审达成一致即可提前终止
3. **Language Config** — 支持输出语言配置
4. **大 PR 审核** — diff 过滤 + 大 prompt 临时文件处理

---

## 特性 1: Near-Line Matching

### 现状
- `internal/platform/diffparse.go:ClassifyCommentsByDiff()` 当行号不在 diff 中时，回退到 `firstDiffLine()` 取文件第一个变更行，或 file-level
- 缺少"就近匹配"逻辑

### 方案
在 `internal/platform/diffparse.go` 中：

1. **新增 `FindNearestLine()` 函数**
```go
// FindNearestLine 在 diff 行号集合中找到距离 targetLine 最近的行，阈值 20 行。
// 返回最近行号和是否找到。
func FindNearestLine(diffLines map[int]bool, targetLine int) (int, bool) {
    nearest := 0
    minDist := math.MaxInt
    for line := range diffLines {
        dist := abs(line - targetLine)
        if dist < minDist {
            minDist = dist
            nearest = line
        }
    }
    if nearest != 0 && minDist <= 20 {
        return nearest, true
    }
    return 0, false
}
```

2. **修改 `ClassifyCommentsByDiff()`** — 4 级降级策略：

```
评论: {file: "foo.go", line: 42, body: "这里有 bug"}
    │
    ├─ Tier 1: line 42 精确命中 diff → inline（原样）
    │
    ├─ Tier 2: line 42 不在 diff，但 line 38 在 diff 且距离 ≤20
    │          → inline，行号改为 38，body 前缀加 "**Line 42:**\n\n"
    │
    ├─ Tier 3: 文件在 diff 中但没有行号在 ±20 范围内
    │          → file-level 评论
    │
    └─ Tier 4: 文件不在 diff 中 → global PR 评论
```

当前 Hydra 的 Tier 2 是取 firstDiffLine（文件第一个变更行），改为先尝试 FindNearestLine，找不到再 fallback。

### 修改文件
- `internal/platform/diffparse.go` — 新增 FindNearestLine, 修改 ClassifyCommentsByDiff
- `internal/platform/diffparse_test.go` — 新增测试

---

## 特性 2: Round-1 Convergence

### 现状与问题

当前有**两层硬编码限制**阻止 Round 1 收敛：

```
Round 1: 3 个 reviewer 各自独立审查 → 完成
                                        ↓
                        ❌ 跳过（line 188: round >= 2）
                                        ↓
Round 2: 看到彼此意见后辩论 → 完成
                                        ↓
                        ✅ 进入 checkConvergence()
                        ❌ 再次拦截（line 570: roundsCompleted < 2）
                                        ↓
Round 3: 又辩论一轮 → 完成
                                        ↓
                        ✅ 终于允许收敛检查
```

**问题**：一个简单 typo 修复，3 个 reviewer 在 Round 1 全说 "LGTM"，系统还要白跑 2 轮辩论才能结束，浪费 token。

### 方案：改 3 个地方

**1. `runDebatePhase()` (orchestrator.go line 188)** — 去掉 `round >= 2`：
```go
// 改前
if o.options.CheckConvergence && round >= 2 && round < o.options.MaxRounds {

// 改后
if o.options.CheckConvergence && round < o.options.MaxRounds {
```

**2. `checkConvergence()` (orchestrator.go line 570)** — 去掉内部 guard：
```go
// 删除这 3 行
if roundsCompleted < 2 {
    return false, nil
}
```

**3. `convergence_check.tmpl`** — 让 AI Judge 知道 Round 1 的特殊性：

```
// 改前（line 3）
IMPORTANT: This is Round {{.RoundsCompleted}}. Reviewers have now seen each other's opinions.

// 改后
{{if .IsFirstRound}}
IMPORTANT: This is Round 1. Reviewers have NOT seen each other's opinions —
they reviewed independently. If they independently arrived at the same
conclusions, that IS valid convergence.
{{else}}
IMPORTANT: This is Round {{.RoundsCompleted}}. Reviewers have now seen each other's opinions.
{{end}}
```

需要在 `checkConvergence()` 构建模板数据时传入 `IsFirstRound`：
```go
convergencePrompt := prompt.MustRender("convergence_check.tmpl", map[string]any{
    "ReviewerCount":   len(o.reviewers),
    "RoundsCompleted": roundsCompleted,
    "IsFirstRound":    roundsCompleted <= 1,  // 新增
    "MessagesText":    messagesText,
})
```

### 改完后的流程

```
场景：简单 PR，3 个 reviewer 都说 "LGTM"

Round 1: 独立审查 → 完成
    ↓
收敛检查 → AI Judge 看到 "独立评审，一致同意"
    ↓
CONVERGED → 结束，省掉 2 轮辩论的 token

场景：复杂 PR，reviewer 意见不一

Round 1: 独立审查 → A 发现安全问题, B 说 LGTM, C 发现性能问题
    ↓
收敛检查 → AI Judge: "意见不一致" → NOT_CONVERGED
    ↓
Round 2: 辩论 → 正常继续...
```

### 修改文件
- `internal/orchestrator/orchestrator.go` — runDebatePhase (line 188), checkConvergence (line 570, 586-589)
- `internal/prompt/templates/convergence_check.tmpl` — 添加 IsFirstRound 条件分支
- `internal/orchestrator/convergence_test.go` — 补充 round-1 收敛测试

---

## 特性 3: Language Config

### 现状
- `DefaultsConfig` 没有 language 字段
- 所有 prompt 模板没有语言指令

### 方案

1. **Config 层**: `internal/config/config.go` — `DefaultsConfig` 新增 `Language string`
2. **Orchestrator 层**: `internal/orchestrator/orchestrator.go`
   - `OrchestratorOptions` 新增 `Language string`
   - 新增 `langSuffix()` 方法返回 `\n\nIMPORTANT: You MUST respond in {language}.`
3. **Prompt 注入**: 在以下模板尾部追加 `{{if .Language}}...{{end}}`：
   - `reviewer_first_round.tmpl`
   - `reviewer_debate_session.tmpl`
   - `reviewer_debate_full.tmpl`
   - `reviewer_summary.tmpl`
   - `final_conclusion.tmpl`
   - `structurize_issues.tmpl` — 特殊处理：title/description/suggestedFix 用指定语言，JSON key 和 severity/category 保持英文
4. **CLI 层**: `cmd/review.go` — 从 config 传递 Language 到 OrchestratorOptions
5. **Config 模板**: `cmd/init.go` — 默认配置加 `language: ""` 注释示例

### 修改文件
- `internal/config/config.go` — DefaultsConfig 加字段
- `internal/orchestrator/orchestrator.go` — Options 加字段, 构建 prompt 时传入
- `internal/orchestrator/types.go` — OrchestratorOptions 加字段
- `internal/prompt/templates/*.tmpl` — 6 个模板加语言后缀
- `cmd/review.go` — 传递 Language

---

## 特性 4: 大 PR 审核

### 现状与问题
- 没有 diff 过滤（protobuf 生成、lock 文件、vendor 等全量发给 AI）
- 大 diff 直接拼进 prompt 通过 stdin 发给 CLI，可能因 stdin 过大失败（E2BIG）
- 没有 `diff_exclude` 配置项

### 解决方案：两道防线

#### 完整流程图

```
用户执行: hydra review 12345
    │
    ├── 1. 获取 PR diff（gh pr diff / glab mr diff）
    │
    ├── 2. 【第一道防线】FilterDiff()
    │       按文件类型过滤掉不需要 review 的垃圾文件
    │       ┌─────────────────────────────────────────────┐
    │       │ 原始 diff: 50 个文件, 8000 行               │
    │       │   过滤: *.pb.go (3 files, 2000 lines)       │
    │       │   过滤: go.sum (1 file, 500 lines)          │
    │       │   过滤: package-lock.json (1 file, 3000 lines)│
    │       │   过滤: vendor/** (5 files, 1000 lines)      │
    │       │   过滤: *generated* (2 files, 800 lines)     │
    │       │ 过滤后: 38 个文件, 700 行 ← 真正需要 review 的│
    │       └─────────────────────────────────────────────┘
    │       日志: "Diff filter: excluded 12 file(s) (~7300 lines)"
    │
    ├── 3. AnnotateDiffWithLineNumbers() + 组装 prompt
    │
    ├── 4. 发给每个 reviewer
    │       │
    │       └── 【第二道防线】PreparePromptForCli(prompt)
    │               │
    │               ├─ ≤ 100KB → 直接写入 stdin，正常执行
    │               │
    │               └─ > 100KB →
    │                   1. 写入临时文件 /tmp/hydra_prompt_xxx.txt
    │                   2. 给 CLI 发短指令:
    │                      "完整 prompt 在 /tmp/hydra_prompt_xxx.txt，
    │                       请读取并执行其中的所有指令"
    │                   3. CLI 工具用自带文件读取能力读文件
    │                   4. 执行完毕后 defer 删除临时文件
    │
    └── 5. 正常 review 流程继续...
```

### 4a. Diff 过滤

**判断标准**：不按大小判断，而是**按文件类型过滤掉不需要 review 的文件**。

1. **新建 `internal/platform/difffilter.go`**：

```go
// 内置排除模式：protobuf 生成、代码生成、vendor、lock 文件
var BuiltinExcludePatterns = []string{
    "*.pb.go", "*.pb.cc", "*.pb.h",      // protobuf
    "*generated*", "**/generated/**",      // 代码生成
    "*.gen.go", "*.gen.ts",               // go generate / codegen
    "vendor/**", "**/vendor/**",           // vendored deps
    "go.sum",                              // go 依赖校验和
    "package-lock.json", "yarn.lock", "pnpm-lock.yaml", // JS lock
}

// FilterDiff 从 unified diff 中移除匹配排除模式的文件段落。
// patterns 合并内置模式 + 用户自定义模式。
func FilterDiff(diff string, userPatterns []string) (filtered string, excludedCount int, excludedLines int)

// SplitDiffByFile 按 "diff --git" 分割成每个文件的段落
func SplitDiffByFile(diff string) []string

// ExtractFilePath 从 "diff --git a/path b/path" 提取文件路径
func ExtractFilePath(section string) string

// ShouldExclude 检查文件路径是否匹配任意排除模式
func ShouldExclude(path string, patterns []string) bool

// GlobMatch 简易 glob 匹配：* 不跨 /，** 跨 /，无 / 的模式匹配 basename
func GlobMatch(filePath string, pattern string) bool
```

**过滤流程**：
1. 合并内置模式 + 用户 `diff_exclude` 模式
2. 按 `diff --git` 分割 diff 为每个文件的段落
3. 每段提取文件路径 (`diff --git a/xxx b/xxx`)
4. glob 匹配：命中则丢弃整段
5. 拼回保留的段落

**Glob 匹配规则** (参考 Magpie 的 `diff-filter.ts:84-118`)：
- `*` 匹配不含 `/` 的任意字符
- `**` 匹配含 `/` 的任意字符（跨目录）
- 模式中没有 `/` 时，只匹配文件名 basename（如 `*.pb.go` 匹配 `pkg/api/types.pb.go`）
- 模式中有 `/` 时，匹配完整路径（如 `vendor/**` 匹配 `vendor/github.com/xxx`）

2. **Config 层**: `DefaultsConfig` 新增 `DiffExclude []string`

用户可在 config 中自定义额外排除：
```yaml
defaults:
  diff_exclude:
    - "*.min.js"
    - "docs/api/**"
    - "*.snapshot"
```

3. **调用点**: `cmd/review.go` 的 3 个 resolve 方法中获取 diff 后调用 FilterDiff：
   - `resolveLocalTarget()` — git diff 后过滤
   - `resolveBranchTarget()` — branch diff 后过滤
   - `resolveMRTarget()` — PR/MR diff 后过滤

### 4b. 大 Prompt 临时文件

**判断标准**：prompt 的 UTF-8 字节数 > **100KB** (100 * 1024 bytes)。

**为什么需要？** CLI 工具（claude/codex）通过 stdin 接收 prompt，但：
- Linux 内核对 `execve` 参数有 ~128KB 限制（E2BIG 错误）
- 某些 CLI 实现有 stdin buffer 限制
- 100KB 阈值留安全余量

1. **新建 `internal/provider/promptfile.go`**：

```go
const PromptSizeThreshold = 100 * 1024 // 100KB

type PreparedPrompt struct {
    Prompt  string   // 发给 stdin 的内容（可能是原始 prompt 或短指令）
    Cleanup func()   // 清理临时文件的回调
}

// PreparePromptForCli 检查 prompt 大小，超过阈值时写入临时文件。
func PreparePromptForCli(prompt string) PreparedPrompt {
    if len(prompt) <= PromptSizeThreshold {
        return PreparedPrompt{Prompt: prompt, Cleanup: func() {}}
    }

    // 写入临时文件
    tmpFile = filepath.Join(os.TempDir(), fmt.Sprintf("hydra_prompt_%d.txt", time.Now().UnixNano()))
    os.WriteFile(tmpFile, []byte(prompt), 0600)

    // 返回短指令，让 CLI 工具自己读文件
    shortPrompt := fmt.Sprintf(
        "The full review prompt is too large for stdin. It has been written to a file.\n\n"+
        "Please read the file at: %s\n"+
        "Then follow all the instructions contained within it exactly.\n"+
        "The file contains the complete code review prompt including the diff and all context.",
        tmpFile,
    )

    return PreparedPrompt{
        Prompt:  shortPrompt,
        Cleanup: func() { os.Remove(tmpFile) },
    }
}
```

2. **修改 CLI providers** — 在构建 stdin prompt 前调用：

`claudecode.go` 和 `codexcli.go` 的 ChatStream/Chat 方法中：
```go
prepared := PreparePromptForCli(stdinPrompt)
defer prepared.Cleanup()
// 将 prepared.Prompt 写入 stdin（而非原始 stdinPrompt）
```

### 修改文件
- `internal/platform/difffilter.go` — **新建**，diff 过滤 + glob 匹配
- `internal/platform/difffilter_test.go` — **新建**，测试过滤和 glob
- `internal/config/config.go` — DefaultsConfig 加 DiffExclude 字段
- `cmd/review.go` — 3 个 resolveTarget 方法中调用 FilterDiff，传入 cfg.Defaults.DiffExclude
- `internal/provider/promptfile.go` — **新建**，大 prompt 临时文件处理
- `internal/provider/promptfile_test.go` — **新建**，测试阈值判断
- `internal/provider/claudecode.go` — 调用 PreparePromptForCli
- `internal/provider/codexcli.go` — 调用 PreparePromptForCli

---

## 实施顺序

1. **Near-Line Matching** — 最简单，独立改动，1 个文件 + 测试
2. **Round-1 Convergence** — 简单，改 orchestrator + 模板，3 处改动
3. **Language Config** — 中等，跨多文件但每处改动小
4. **大 PR 审核** — 最大，新建 3 个文件 + 改多处调用点

## 验证方式

1. **Near-Line**: `go test ./internal/platform/ -run TestFindNearestLine` + `TestClassifyNearLine`
2. **Round-1 Convergence**: `go test ./internal/orchestrator/ -run TestConvergence` — 验证 round=1 时能检测收敛
3. **Language Config**: 用 `language: zh` 配置运行 `hydra review --local`，确认输出为中文
4. **大 PR 审核**:
   - `go test ./internal/platform/ -run TestFilterDiff` — 验证 *.pb.go 等被过滤
   - `go test ./internal/platform/ -run TestGlobMatch` — 验证 glob 匹配逻辑
   - `go test ./internal/provider/ -run TestPreparePrompt` — 验证阈值判断和临时文件
   - 用大仓库 PR 测试，确认 diff 过滤生效且不报 E2BIG
