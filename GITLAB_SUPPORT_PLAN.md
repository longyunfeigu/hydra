# Hydra GitLab 支持改造方案

## Context

Hydra 是一个多模型对抗式代码审查 CLI 工具，当前仅支持 GitHub。用户希望扩展到 GitLab 支持。项目的 GitHub 集成分散在 4 个文件中（12 处 `gh` CLI 调用、5 个硬编码 `github.com` 正则），且没有任何平台抽象层。本方案旨在引入平台抽象接口，使 GitHub 和 GitLab 实现可以互换。

---

## Phase 1: 引入 Platform 抽象层（纯重构，不破坏现有功能）

### Step 1.1: 创建 `internal/platform/` 包 — 接口与共享类型

**新建文件:** `internal/platform/platform.go`

定义小接口（Go 惯例，避免 God Interface）：

```go
// MRProvider 获取 MR/PR 元数据和 diff
type MRProvider interface {
    GetDiff(mrID, repo string) (string, error)
    GetInfo(mrID, repo string) (*MRInfo, error)
    GetHeadCommitInfo(mrID, repo string) (*CommitInfo, error)
    GetChangedFiles(mrID, repo string) ([]DiffFile, error)
}

// MRCommenter 发布和查询评论
type MRCommenter interface {
    PostComment(mrID string, opts PostCommentOpts) CommentResult
    PostReview(mrID string, classified []ClassifiedComment, commitInfo CommitInfo, repo string) ReviewResult
    GetExistingComments(mrID, repo string) []ExistingComment
}

// RepoDetector 检测和解析仓库信息
type RepoDetector interface {
    DetectRepoFromRemote() (string, error)
    ParseMRURL(url string) (repo, mrID string, err error)
    BuildMRURL(repo, mrID string) string
}

// HistoryProvider 查询 MR 历史（用于 context gathering）
type HistoryProvider interface {
    GetMRDetails(mrNumber int, cwd string) (*MRDetail, error)
}

// Platform 组合了所有子接口
type Platform interface {
    Name() string  // "github" | "gitlab"
    MRProvider
    MRCommenter
    RepoDetector
    HistoryProvider
}
```

共享类型定义（从现有 `internal/github/` 迁移 + 调整）：

```go
type MRInfo struct {
    Title, Description, HeadSHA string
}

// CommitInfo: GitHub 只用 HeadSHA，GitLab 需要 3 个 SHA
type CommitInfo struct {
    HeadSHA  string
    BaseSHA  string  // GitLab 必需，GitHub 留空
    StartSHA string  // GitLab 必需，GitHub 留空
}

type DiffFile struct {
    Filename string  // GitHub: filename, GitLab: new_path
    Patch    string  // GitHub: patch, GitLab: diff
}

// CommentResult, ClassifiedComment, ReviewResult, ReviewCommentInput,
// PostCommentOpts, ExistingComment, IssueForComment, MRDetail
// — 均从 internal/github/commenter.go 迁移，去除 GitHub 特定字段
```

### Step 1.2: 迁移可复用代码到 `internal/platform/`

**新建文件:** `internal/platform/diffparse.go`
- 迁移 `ParseDiffLines()` — 来自 `internal/github/diff.go:26-58`（纯 unified diff 解析，零平台依赖）

**新建文件:** `internal/platform/classify.go`
- 迁移 `ClassifyComments()` 的核心算法 — 来自 `internal/github/commenter.go:203-233`
- 迁移 `isDuplicateComment()` — 来自 `commenter.go:271-289`
- 迁移 `truncStr()` — 来自 `commenter.go:496-501`
- 这些函数改为接受 `map[string]map[int]bool` 参数而非内部调用 `GetDiffInfo()`

**新建文件:** `internal/platform/format.go`
- 迁移 `formatIssueBody()` — 来自 `commenter.go:463-475`
- 迁移 `severityToBadge()` — 来自 `commenter.go:479-492`

### Step 1.3: 重构 GitHub 实现为 Platform 接口

**新建文件:** `internal/platform/github/github.go`
- 将 `internal/github/commenter.go` 和 `internal/github/diff.go` 的逻辑封装为 `GitHubPlatform struct`
- 实现 `Platform` 接口的所有方法
- 内部仍然使用 `gh` CLI（保持不变）
- 关键映射：
  - `GetHeadCommitInfo()` → 调 `gh pr view --json headRefOid`，返回只填 `HeadSHA` 的 `CommitInfo`
  - `GetChangedFiles()` → 调 `gh api repos/.../pulls/.../files`，将 `filename`/`patch` 映射到 `DiffFile`
  - `PostReview()` → 现有的 Reviews API 批量提交 + 降级逻辑

### Step 1.4: 修改 `cmd/review.go` 注入 Platform

**修改文件:** `cmd/review.go`

关键变更：
1. 移除 `import ghub "github.com/guwanhua/hydra/internal/github"`
2. 新增 `import "github.com/guwanhua/hydra/internal/platform"`
3. `resolvePRTarget()` → `resolveTarget()` 接收 `Platform` 参数
   - 用 `platform.ParseMRURL()` 替代硬编码的 `github.com` regex（review.go:356,362,373,382）
   - 用 `platform.GetDiff()` / `platform.GetInfo()` 替代 `gh pr diff` / `gh pr view`（review.go:396,402）
   - 用 `platform.BuildMRURL()` 替代硬编码 URL 模板（review.go:385）
4. 评论发布部分（review.go:211-223）改为调 `platform.PostIssuesAsComments()`
5. `--no-post` flag 描述从 "Skip GitHub comment flow" 改为 "Skip posting comments"

### Step 1.5: 修改 `internal/context/history.go` 使用 Platform

**修改文件:** `internal/context/history.go`

- `getPRDetails()` (history.go:125-167) 改为接收 `HistoryProvider` 接口参数
- `CollectHistory()` 签名变更：新增 `HistoryProvider` 参数
- `extractPRNumbers()` 和 `findPRNumbers()` 保持不变（纯 git log 解析，平台无关）

**修改文件:** `internal/context/gatherer.go`
- `ContextGatherer` 新增 `platform platform.HistoryProvider` 字段
- `Gather()` 中 `CollectHistory()` 调用传入 platform

### Step 1.6: 平台自动检测

**新建文件:** `internal/platform/detect.go`

```go
func DetectFromRemote() (Platform, error) {
    // 1. 执行 git remote get-url origin
    // 2. 匹配 github.com → NewGitHubPlatform()
    // 3. 匹配 gitlab.com 或配置的 GitLab 域名 → NewGitLabPlatform()
    // 4. 未匹配 → 返回错误
}
```

### Step 1.7: 删除旧包

- 删除 `internal/github/` 整个目录（功能已迁移到 `internal/platform/github/`）
- 更新所有 import 路径

### Step 1.8: 验证

- 运行所有现有测试确保通过：`go test ./...`
- 手动测试 `hydra review <PR>` 确认 GitHub 功能不受影响

---

## Phase 2: 实现 GitLab 支持

### Step 2.1: 新建 GitLab 实现

**新建文件:** `internal/platform/gitlab/gitlab.go`

核心实现映射（均使用 `glab` CLI）：

| 方法 | 实现 |
|------|------|
| `GetDiff()` | `glab mr diff <id>` |
| `GetInfo()` | `glab mr view <id> --output json` → 解析 `title` + `description` |
| `GetHeadCommitInfo()` | `glab mr view --output json` → 提取 `diff_refs` 中的 3 个 SHA |
| `GetChangedFiles()` | `glab api projects/:id/merge_requests/:iid/diffs` → `new_path`/`diff` 映射到 `DiffFile` |
| `PostComment()` | 三级降级：(1) Discussions API 行内 (2) Discussions API 文件级 (3) `glab mr note` 全局 |
| `PostReview()` | 尝试 Draft Notes API + `bulk_publish`；失败则降级为逐条 Discussions API |
| `GetExistingComments()` | `glab api projects/:id/merge_requests/:iid/discussions` |
| `DetectRepoFromRemote()` | 正则匹配 `gitlab.com` 或自定义域名，提取完整项目路径 |
| `ParseMRURL()` | 正则: `(.+?)/-/merge_requests/(\d+)` |
| `BuildMRURL()` | `https://{host}/{repo}/-/merge_requests/{id}` |
| `GetMRDetails()` | `glab api projects/:id/merge_requests/:iid` |

**关键差异处理：**

1. **3 个 SHA 问题**: `GetHeadCommitInfo()` 返回完整 `CommitInfo{HeadSHA, BaseSHA, StartSHA}`，`PostComment()` 中构建 `position` 对象时使用
2. **嵌套组路径**: `DetectRepoFromRemote()` 捕获 `gitlab.com` 后的完整路径（非固定 2 段），API 调用时 URL-encode
3. **Draft Notes API 可能不可用**（GitLab Premium）: `PostReview()` 先尝试 Draft Notes，失败则逐条发 Discussion
4. **`glab mr note --unique`**: 全局评论内置去重，可简化去重逻辑

### Step 2.2: 更新配置结构

**修改文件:** `internal/config/config.go`

新增：
```go
type PlatformConfig struct {
    Type string `yaml:"type,omitempty"` // "auto"(默认) | "github" | "gitlab"
    Host string `yaml:"host,omitempty"` // 自托管域名, 如 "gitlab.company.com"
}
```

在 `HydraConfig` 中添加 `Platform PlatformConfig`。

### Step 2.3: 更新默认配置

**修改文件:** `cmd/init.go`

在 `defaultConfig` 中添加：
```yaml
# Platform: auto-detected from git remote (github or gitlab)
# platform:
#   type: auto
#   host: gitlab.example.com  # for self-hosted GitLab
```

### Step 2.4: 用户可见文本泛化

**修改文件（少量字符串替换）：**

| 文件 | 位置 | 当前 | 改为 |
|------|------|------|------|
| `cmd/review.go:54` | flag 描述 | "Skip GitHub comment flow" | "Skip posting review comments" |
| `cmd/review.go:217` | spinner 文本 | "Posting comments to GitHub..." | "Posting review comments..." |
| `display/terminal.go:214` | 上下文展示 | "Related PRs:" | "Related Changes:" |
| `orchestrator/orchestrator.go:670` | AI prompt | "posted as a GitHub PR comment" | "posted as a review comment" |

**不做的事**（避免无意义的大规模重命名）：
- 内部类型名 `PRNumber`, `RelatedPR` 等保持不变 — 它们是内部实现细节，不影响用户
- `DebateResult.PRNumber` 字段保持 — 实际上它已经存储 "Local Changes" 等非 PR 值

### Step 2.5: 添加测试

- `internal/platform/gitlab/gitlab_test.go` — GitLab URL 解析、DiffFile 映射等单测
- `internal/platform/detect_test.go` — 平台检测逻辑测试（GitHub/GitLab/未知/自托管）
- 迁移 `internal/github/diff_test.go` 到 `internal/platform/diffparse_test.go`

---

## GitHub 与 GitLab API 完整对照表

| 功能 | GitHub (gh CLI) | GitLab (glab CLI) |
|------|----------------|-------------------|
| 获取 diff | `gh pr diff <url>` | `glab mr diff <id>` |
| 获取 MR 信息 | `gh pr view --json title,body` | `glab mr view --output json`（字段名: `description` 非 `body`）|
| 获取 HEAD SHA | `headRefOid` 一个字段 | `diff_refs` 含 3 个 SHA (`head_sha`, `base_sha`, `start_sha`) |
| 获取变更文件 | `gh api repos/.../pulls/.../files` | `glab api projects/:id/merge_requests/:iid/diffs`（`new_path`/`diff`）|
| 行内评论 | `POST .../pulls/.../comments` (需 `commit_id`) | `POST .../discussions` (需 `position` 含 3 个 SHA) |
| 文件级评论 | `subject_type: "file"` | `position_type: "file"` |
| 全局评论 | `gh pr comment` | `glab mr note --unique`（内置去重） |
| 批量提交 | Reviews API 一次提交 | Draft Notes API → `bulk_publish` |
| 获取已有评论 | `.../pulls/.../comments` | `.../discussions`（含行内和全局） |
| 仓库标识 | `owner/repo`（固定 2 级） | `group/subgroup/project`（支持嵌套组，最多 20 级） |
| MR URL 格式 | `/pull/123` | `/-/merge_requests/123` |
| CLI 工具 | `gh` | `glab` |
| 认证 | `gh auth login` | `glab auth login` |

## 术语映射

| GitHub | GitLab | 内部代码（保持不变） |
|--------|--------|---------------------|
| Pull Request (PR) | Merge Request (MR) | `PRNumber` (作为通用标签使用) |
| Repository | Project | `repo` |
| Owner/Organization | Namespace/Group | — |
| PR number | MR IID (internal ID) | — |
| `body` (PR description) | `description` | `MRInfo.Description` |
| `headRefOid` | `diff_refs.head_sha` | `CommitInfo.HeadSHA` |
| Review | Draft Notes + Bulk Publish | `PostReview()` |
| `commit_id` | `position{base_sha, head_sha, start_sha}` | `CommitInfo` |
| `side: RIGHT` | `new_line` | — |
| `subject_type: file` | `position_type: file` | — |

---

## 文件变更汇总

| 操作 | 文件 | 工作量 |
|------|------|--------|
| **新建** | `internal/platform/platform.go` | 中 |
| **新建** | `internal/platform/detect.go` | 小 |
| **新建** | `internal/platform/diffparse.go` | 小（迁移） |
| **新建** | `internal/platform/classify.go` | 小（迁移） |
| **新建** | `internal/platform/format.go` | 小（迁移） |
| **新建** | `internal/platform/github/github.go` | 大（重构） |
| **新建** | `internal/platform/gitlab/gitlab.go` | 大（核心新增） |
| **修改** | `internal/config/config.go` | 小 |
| **修改** | `cmd/review.go` | 中 |
| **修改** | `cmd/init.go` | 小 |
| **修改** | `internal/context/history.go` | 中 |
| **修改** | `internal/context/gatherer.go` | 小 |
| **修改** | `internal/orchestrator/orchestrator.go` | 小（仅 prompt 文本） |
| **修改** | `internal/display/terminal.go` | 小（仅 1 处文本） |
| **删除** | `internal/github/` (整个目录) | — |
| **新建** | 多个 `_test.go` 文件 | 中 |

---

## 风险与注意事项

1. **GitLab Draft Notes API 是 Premium 功能** — 免费版用户无法使用批量提交，必须实现逐条发布的降级路径
2. **GitLab 嵌套组** — 项目路径可能是 `group/sub1/sub2/project`（最多 20 级），API 调用时需 URL-encode
3. **`glab` CLI 不支持行内评论** — 必须通过 `glab api` 直接调用 Discussions API
4. **GitLab 行内评论需要 3 个 SHA** — 这是与 GitHub 最大的 API 差异，`CommitInfo` 结构体是解决方案
5. **自托管 GitLab** — 域名不是 `gitlab.com`，需要通过配置 `platform.host` 指定，或从 `glab` 的 auth 配置读取
6. **向后兼容** — 现有 GitHub 用户的配置文件无需修改，`platform.type` 默认为 `auto`

## 验证方式

1. **单元测试**: `go test ./internal/platform/...` — 覆盖 URL 解析、diff 解析、平台检测
2. **GitHub 回归**: `hydra review <github-pr>` — 确认现有功能不受影响
3. **GitLab 功能**: `hydra review <gitlab-mr-url>` — 验证完整流程
4. **自动检测**: 在 GitHub/GitLab 仓库中分别运行 `hydra review --local` 验证平台识别
5. **配置兼容**: 使用旧配置文件（无 `platform` 段）运行确认默认行为正确
