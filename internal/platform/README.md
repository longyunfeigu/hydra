# platform - 多平台抽象层

定义 GitHub/GitLab 的统一接口，以及 diff 解析、评论格式化等共享工具。

## 目录结构

```
platform/
├── platform.go      # 统一接口 + 共享类型
├── diffparse.go     # Diff 解析、评论分类、去重
├── format.go        # 问题格式化（Markdown + 严重度徽章）
├── detect/          # 平台自动检测（从 git remote URL）
├── github/          # GitHub 实现（gh CLI）
└── gitlab/          # GitLab 实现（glab CLI）
```

## 完整接口定义

```go
// MRProvider 获取 MR/PR 的元数据和 diff 信息。
type MRProvider interface {
    GetDiff(mrID, repo string) (string, error)
    GetInfo(mrID, repo string) (*MRInfo, error)
    GetHeadCommitInfo(mrID, repo string) (*CommitInfo, error)
    GetChangedFiles(mrID, repo string) ([]DiffFile, error)
}

// MRCommenter 发布和查询 MR/PR 评论。
type MRCommenter interface {
    PostComment(mrID string, opts PostCommentOpts) CommentResult
    PostReview(mrID string, classified []ClassifiedComment, commitInfo CommitInfo, repo string) ReviewResult
    GetExistingComments(mrID, repo string) []ExistingComment
}

// IssueCommenter 提供将结构化问题发布为评论的高级入口。
type IssueCommenter interface {
    PostIssuesAsComments(mrID string, issues []IssueForComment, repo string) ReviewResult
}

// RepoDetector 检测和解析仓库信息。
type RepoDetector interface {
    DetectRepoFromRemote() (string, error)
    ParseMRURL(url string) (repo, mrID string, err error)
    BuildMRURL(repo, mrID string) string
}

// HistoryProvider 查询 MR/PR 历史信息（用于 context gathering）。
type HistoryProvider interface {
    GetMRDetails(mrNumber int, cwd string) (*MRDetail, error)
}

// Platform 组合了所有子接口，代表一个完整的代码托管平台。
type Platform interface {
    Name() string // "github" | "gitlab"
    MRProvider
    MRCommenter
    IssueCommenter
    RepoDetector
    HistoryProvider
}
```

## 核心数据结构示例

### MRInfo

包含 MR/PR 的基本元数据（标题、描述、HEAD SHA）：

```go
&MRInfo{
    Title:       "feat: add user authentication",
    Description: "This PR implements JWT-based auth...",
    HeadSHA:     "abc123def456",
}
```

### CommitInfo

包含评论发布所需的提交 SHA 信息。GitHub 和 GitLab 有本质区别：

```go
// GitHub: 只需 HeadSHA
CommitInfo{HeadSHA: "abc123"}

// GitLab: 需要 3 个 SHA（用于 Discussions API 定位评论位置）
CommitInfo{
    HeadSHA:  "abc123",  // MR 的最新提交
    BaseSHA:  "def456",  // 目标分支的基准提交
    StartSHA: "789ghi",  // MR 创建时的起始提交
}
```

### DiffFile

表示 MR/PR 中的单个变更文件及其补丁内容：

```go
[]DiffFile{
    {Filename: "src/auth.go", Patch: "@@ -10,3 +10,5 @@\n func Login()..."},
    {Filename: "src/handler.go", Patch: "@@ -1,5 +1,8 @@\n+import \"jwt\"..."},
}
```

### IssueForComment

将代码审查问题转换为评审评论的输入结构：

```go
IssueForComment{
    File:         "src/auth.go",
    Line:         intPtr(42),
    Title:        "SQL injection vulnerability",
    Description:  "The query uses string concatenation...",
    Severity:     "high",
    SuggestedFix: "Use parameterized queries",
    RaisedBy:     "security-reviewer, perf-reviewer",
}
```

### ClassifiedComment

经过分类后的评论，根据 diff 信息决定发布模式。三种模式：

```go
// inline: 文件在 diff 中且行号匹配 — 显示为代码行内评论
ClassifiedComment{Input: comment, Mode: "inline"}

// file: 文件在 diff 中但行号不在 diff 范围 — 显示为文件级评论
ClassifiedComment{Input: comment, Mode: "file"}

// global: 文件不在 diff 中 — 显示为 MR/PR 全局评论
ClassifiedComment{Input: comment, Mode: "global"}
```

### ReviewResult

汇总批量发布评审评论的结果统计：

```go
ReviewResult{
    Posted:    8,
    Inline:    5,   // 行内评论
    FileLevel: 2,   // 文件级评论
    Global:    1,   // 全局评论
    Failed:    0,
    Skipped:   1,   // 去重跳过
}
```

## 评论分类与发布流程

```
PostIssuesAsComments(mrID, issues, repo)
  │
  ├─ 1. GetHeadCommitInfo(mrID, repo) → CommitInfo
  │     获取提交 SHA（GitHub 1 个，GitLab 3 个）
  │
  ├─ 2. FormatIssueBody(issue) → Markdown 正文
  │     将每个 IssueForComment 格式化为带徽章的 Markdown
  │
  ├─ 3. GetChangedFiles(mrID, repo) → []DiffFile
  │     获取变更文件列表及 patch 内容
  │
  ├─ 4. ParseDiffLines(patch) → map[int]bool
  │     从 patch 解析出右侧（新文件）的有效行号集合
  │
  ├─ 5. ClassifyCommentsByDiff(comments, diffInfo)
  │     ├─ 文件在 diff 且行号匹配  → inline
  │     ├─ 文件在 diff 但行号不在   → file
  │     └─ 文件不在 diff            → global
  │
  ├─ 6. IsDuplicateComment(comment, existing)
  │     对比前 100 字符 + path + line 去重
  │
  └─ 7. PostReview(mrID, classified, commitInfo, repo)
        ├─ 批量提交 inline + file 评论
        ├─ 批量失败 → 逐条 PostComment 降级
        └─ 逐条发布 global 评论
```

## FormatIssueBody 输出示例

给定上方的 `IssueForComment` 示例，`FormatIssueBody` 生成的 Markdown：

```markdown
🟠 **SQL injection vulnerability**

The query uses string concatenation...

**Suggested fix:** Use parameterized queries

_Raised by: security-reviewer, perf-reviewer_
```

## 严重度徽章映射

`SeverityToBadge` 将严重等级字符串转换为对应的 emoji 徽章：

```
"critical" → 🔴
"high"     → 🟠
"medium"   → 🟡
"low"      → 🟢
其他       → ⚪
```
