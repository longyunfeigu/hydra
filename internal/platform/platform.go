// Package platform 定义了代码托管平台的抽象接口和共享类型。
// 支持 GitHub 和 GitLab 等平台的统一操作，包括 MR/PR 管理、评论发布和仓库检测。
package platform

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

// RepoDetector 检测和解析仓库信息。
type RepoDetector interface {
	DetectRepoFromRemote() (string, error)
	ParseMRURL(url string) (repo, mrID string, err error)
	BuildMRURL(repo, mrID string) string
}

// HistoryProvider 查询 MR/PR 历史信息（用于 context gathering）。
type HistoryProvider interface {
	GetMRDetails(mrNumber int, cwd string) (*MRDetail, error)
	GetMRsForCommit(commitSHA string, cwd string) ([]int, error)
}

// IssueCommenter 提供将结构化问题发布为评论的高级入口。
type IssueCommenter interface {
	PostIssuesAsComments(mrID string, issues []IssueForComment, repo string) ReviewResult
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

// MRInfo 包含 MR/PR 的基本元数据。
type MRInfo struct {
	Title       string
	Description string
	HeadSHA     string
}

// CommitInfo 包含评论发布所需的提交 SHA 信息。
// GitHub 仅需 HeadSHA，GitLab 需要 3 个 SHA（HeadSHA, BaseSHA, StartSHA）。
type CommitInfo struct {
	HeadSHA  string
	BaseSHA  string // GitLab 必需，GitHub 留空
	StartSHA string // GitLab 必需，GitHub 留空
}

// DiffFile 表示 MR/PR 中的单个变更文件。
type DiffFile struct {
	Filename string // GitHub: filename, GitLab: new_path
	Patch    string // GitHub: patch, GitLab: diff
}

// CommentResult 表示发布单条评论的结果。
type CommentResult struct {
	Success bool
	Inline  bool
	Mode    string // "inline" | "file" | "global"
	Error   string
}

// ReviewCommentInput 是单条评审评论的输入参数。
type ReviewCommentInput struct {
	Path string
	Line *int
	Body string
}

// ClassifiedComment 是经过分类后的评论，包含放置模式信息。
type ClassifiedComment struct {
	Input ReviewCommentInput
	Mode  string // "inline"、"file"、"global"
}

// ReviewResult 汇总批量发布评审评论的结果统计。
type ReviewResult struct {
	Posted    int
	Inline    int
	FileLevel int
	Global    int
	Failed    int
	Skipped   int
}

// PostCommentOpts 包含 PostComment 所需的所有选项参数。
type PostCommentOpts struct {
	Path       string
	Line       *int
	Body       string
	CommitInfo CommitInfo
	Repo       string
}

// ExistingComment 表示 MR/PR 上已存在的评论，用于去重检查。
type ExistingComment struct {
	Path string `json:"path"`
	Line *int   `json:"line"`
	Body string `json:"body"`
}

// IssueForComment 是将代码审查问题转换为评审评论的结构体。
type IssueForComment struct {
	File         string
	Line         *int
	Title        string
	Description  string
	Severity     string
	SuggestedFix string
	RaisedBy     string
}

// MRDetail 包含历史 MR/PR 的详细信息，用于 context gathering。
type MRDetail struct {
	Number   int
	Title    string
	Author   string
	MergedAt string
	Files    []string
}
