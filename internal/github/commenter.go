// Package github 提供与 GitHub API 交互的功能，包括发布 PR 评审评论、解析 diff 等。
package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// CommentResult 表示发布单条评论的结果。
// Success 表示是否成功发布，Inline 表示是否为行内评论（而非全局评论），
// Error 记录失败时的错误信息。
type CommentResult struct {
	Success bool   // 评论是否成功发布
	Inline  bool   // 是否为行内评论（true）还是全局评论（false）
	Error   string // 失败时的错误信息
}

// ReviewCommentInput 是单条评审评论的输入参数。
// 包含文件路径、行号（可选）和评论内容。
type ReviewCommentInput struct {
	Path string // 评论对应的文件路径
	Line *int   // 评论对应的行号，nil 表示文件级评论
	Body string // 评论正文内容
}

// ClassifiedComment 是经过分类后的评论，包含放置模式信息。
// Mode 决定了评论在 PR 上的展示方式：行内、文件级或全局。
type ClassifiedComment struct {
	Input ReviewCommentInput
	Mode  string // "inline"（行内）、"file"（文件级）、"global"（全局）
}

// ReviewResult 汇总批量发布评审评论的结果统计。
// 记录各种类型评论的发布数量及失败、跳过的数量。
type ReviewResult struct {
	Posted    int // 成功发布的评论总数
	Inline    int // 行内评论数
	FileLevel int // 文件级评论数
	Global    int // 全局评论数
	Failed    int // 发布失败的评论数
	Skipped   int // 因重复而跳过的评论数
}

// IssueForComment 是将代码审查问题转换为评审评论的最小结构体。
// 包含问题的位置、描述、严重性等信息，用于生成格式化的评论内容。
type IssueForComment struct {
	File         string // 问题所在文件路径
	Line         *int   // 问题所在行号，nil 表示文件级问题
	Title        string // 问题标题
	Description  string // 问题详细描述
	Severity     string // 严重等级：critical/high/medium/low
	SuggestedFix string // 建议的修复方案
	RaisedBy     string // 提出该问题的审查者标识
}

// prNumberRegex 用于验证 PR 编号格式（纯数字）
var prNumberRegex = regexp.MustCompile(`^\d+$`)

// repoRegex 用于从 GitHub 远程 URL 中提取 owner/repo 格式的仓库标识
var repoRegex = regexp.MustCompile(`github\.com[:/]([^/]+/[^/.]+)`)

// validatePRNumber 验证 PR 编号是否为有效的纯数字格式。
// 无效时返回错误，防止在 API 调用中使用非法的 PR 编号。
func validatePRNumber(prNumber string) error {
	if !prNumberRegex.MatchString(prNumber) {
		return fmt.Errorf("invalid PR number: %s", prNumber)
	}
	return nil
}

// getRepo 获取 GitHub 仓库标识（owner/repo 格式）。
// 如果调用方已提供仓库名则直接返回；否则从 git remote origin URL 自动检测。
// 支持 SSH (git@github.com:) 和 HTTPS (github.com/) 两种远程 URL 格式。
func getRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	// 执行 git 命令获取 origin 远程仓库 URL
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("could not detect GitHub repo from git remote: %w", err)
	}
	remoteURL := strings.TrimSpace(string(out))
	// 使用正则表达式从 URL 中提取 owner/repo 部分
	m := repoRegex.FindStringSubmatch(remoteURL)
	if m == nil {
		return "", fmt.Errorf("could not detect GitHub repo from git remote URL: %s", remoteURL)
	}
	return m[1], nil
}

// GetPRHeadSha 获取指定 PR 的 HEAD 提交 SHA 值。
// 通过 gh CLI 工具查询 PR 的 headRefOid 字段来获取最新提交的 SHA。
// 这个 SHA 在发布行内评论时需要，用于将评论关联到正确的提交。
func GetPRHeadSha(prNumber, repo string) (string, error) {
	if err := validatePRNumber(prNumber); err != nil {
		return "", err
	}
	args := []string{"pr", "view", prNumber, "--json", "headRefOid", "--jq", ".headRefOid"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get PR head SHA: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// PostCommentOpts 包含 PostComment 函数所需的所有选项参数。
type PostCommentOpts struct {
	Path      string // 评论对应的文件路径
	Line      *int   // 评论对应的行号
	Body      string // 评论正文
	CommitSha string // 提交 SHA，用于关联评论到特定提交
	Repo      string // 仓库标识（owner/repo），为空时自动检测
}

// PostComment 在 PR 上发布单条评论，采用三级降级策略：
// 1. 行内评论（inline）：精确定位到代码行，需要行号在 diff 范围内
// 2. 文件级评论（file-level）：关联到文件但不定位到具体行
// 3. 全局 PR 评论（global）：作为普通 PR 评论发布，作为最终兜底方案
// 这种降级策略确保评论始终能成功发布，即使 GitHub API 拒绝精确定位的评论。
func PostComment(prNumber string, opts PostCommentOpts) CommentResult {
	if err := validatePRNumber(prNumber); err != nil {
		return CommentResult{Success: false, Error: err.Error()}
	}
	repo, err := getRepo(opts.Repo)
	if err != nil {
		return CommentResult{Success: false, Error: err.Error()}
	}

	// 第一级：尝试发布行内评论（需要有效的行号）
	if opts.Line != nil {
		payload, _ := json.Marshal(map[string]interface{}{
			"body":      opts.Body,
			"commit_id": opts.CommitSha,
			"path":      opts.Path,
			"line":      *opts.Line,
			"side":      "RIGHT",
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/comments", repo, prNumber),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			return CommentResult{Success: true, Inline: true}
		}
	}

	// 第二级：尝试发布文件级评审评论（不需要精确行号在 diff 范围内）
	{
		lineRef := ""
		if opts.Line != nil {
			lineRef = fmt.Sprintf("**Line %d:**\n\n", *opts.Line)
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"body":         lineRef + opts.Body,
			"commit_id":    opts.CommitSha,
			"path":         opts.Path,
			"subject_type": "file",
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/comments", repo, prNumber),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			return CommentResult{Success: true, Inline: true}
		}
	}

	// 第三级（兜底）：发布普通 PR 评论，在评论正文中标注文件位置信息
	{
		location := fmt.Sprintf("**%s**\n\n", opts.Path)
		if opts.Line != nil {
			location = fmt.Sprintf("**%s:%d**\n\n", opts.Path, *opts.Line)
		}
		body := location + opts.Body

		args := []string{"pr", "comment", prNumber, "--body-file", "-"}
		if opts.Repo != "" {
			args = append(args, "--repo", opts.Repo)
		}
		cmd := exec.Command("gh", args...)
		cmd.Stdin = strings.NewReader(body)
		if err := cmd.Run(); err != nil {
			return CommentResult{Success: false, Error: truncStr(err.Error(), 200)}
		}
		return CommentResult{Success: true, Inline: false}
	}
}

// ClassifyComments 根据 diff 信息对评论进行分类，决定每条评论的放置方式。
// 分类逻辑：
//   - 如果文件在 diff 中且行号在 diff 有效行内 -> "inline"（行内评论）
//   - 如果文件在 diff 中但行号不在有效行内   -> "file"（文件级评论）
//   - 如果文件不在 diff 中                     -> "global"（全局评论）
func ClassifyComments(prNumber string, comments []ReviewCommentInput, repo string) ([]ClassifiedComment, error) {
	if err := validatePRNumber(prNumber); err != nil {
		return nil, err
	}
	resolvedRepo, err := getRepo(repo)
	if err != nil {
		return nil, err
	}
	// 获取 PR 的 diff 信息，包含每个文件的有效行号映射
	diffInfo, err := GetDiffInfo(prNumber, resolvedRepo)
	if err != nil {
		return nil, err
	}

	classified := make([]ClassifiedComment, 0, len(comments))
	for _, c := range comments {
		fileLines, fileInDiff := diffInfo[c.Path]
		if fileInDiff && c.Line != nil && fileLines[*c.Line] {
			// 文件在 diff 中且行号有效，可以发布行内评论
			classified = append(classified, ClassifiedComment{Input: c, Mode: "inline"})
		} else if fileInDiff {
			// 文件在 diff 中但行号不在有效范围，降级为文件级评论
			classified = append(classified, ClassifiedComment{Input: c, Mode: "file"})
		} else {
			// 文件不在 diff 中，只能发布全局评论
			classified = append(classified, ClassifiedComment{Input: c, Mode: "global"})
		}
	}

	return classified, nil
}

// existingComment 表示 PR 上已存在的评论，用于去重检查。
type existingComment struct {
	Path string `json:"path"` // 评论对应的文件路径
	Line *int   `json:"line"` // 评论对应的行号
	Body string `json:"body"` // 评论正文内容
}

// getExistingComments 获取 PR 上所有已存在的评审评论。
// 通过 GitHub API 分页获取，用于在发布新评论前进行去重检查，
// 避免重复运行时产生重复评论。
func getExistingComments(prNumber, resolvedRepo string) []existingComment {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%s/comments", resolvedRepo, prNumber),
		"--paginate", "--jq", ".[]",
	).Output()
	if err != nil {
		return nil
	}
	// 逐行解析 JSON 格式的评论数据
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var comments []existingComment
	for _, line := range lines {
		if line == "" {
			continue
		}
		var c existingComment
		if json.Unmarshal([]byte(line), &c) == nil {
			comments = append(comments, c)
		}
	}
	return comments
}

// isDuplicateComment 检查待发布的评论是否与已存在的评论重复。
// 比较逻辑：文件路径相同、行号相同、且评论正文前100个字符相同则视为重复。
// 使用前缀比较而非全文比较是为了容忍格式上的微小差异。
func isDuplicateComment(comment ReviewCommentInput, existing []existingComment) bool {
	prefix := truncStr(comment.Body, 100)
	for _, e := range existing {
		if e.Path != comment.Path {
			continue
		}
		// 比较行号指针（nil 表示文件级评论）
		if (comment.Line == nil) != (e.Line == nil) {
			continue
		}
		if comment.Line != nil && e.Line != nil && *comment.Line != *e.Line {
			continue
		}
		if truncStr(e.Body, 100) == prefix {
			return true
		}
	}
	return false
}

// PostReview 批量发布已分类的评论到 PR 上。
// 使用 GitHub Reviews API 批量提交行内和文件级评论以减少 API 调用次数，
// 如果批量提交失败则降级为逐条发布（通过 PostComment 的三级降级策略）。
// 全局评论则始终逐条发布。发布前会自动去重，跳过已存在的评论。
func PostReview(prNumber string, classified []ClassifiedComment, commitSha, repo string) ReviewResult {
	if err := validatePRNumber(prNumber); err != nil {
		return ReviewResult{Failed: len(classified)}
	}
	resolvedRepo, err := getRepo(repo)
	if err != nil {
		return ReviewResult{Failed: len(classified)}
	}

	// 获取已存在的评论用于去重
	existing := getExistingComments(prNumber, resolvedRepo)

	var result ReviewResult

	// reviewComment 是 GitHub Reviews API 接受的评论格式
	type reviewComment struct {
		Path        string `json:"path"`
		Line        *int   `json:"line,omitempty"`
		Side        string `json:"side,omitempty"`
		Body        string `json:"body"`
		SubjectType string `json:"subject_type,omitempty"`
	}

	var reviewComments []reviewComment    // 待批量提交的行内/文件级评论
	var reviewOriginals []ClassifiedComment // 对应的原始分类评论（用于统计）
	var globalEntries []ClassifiedComment   // 需要单独发布的全局评论

	// 遍历分类后的评论，去重后按类型分组
	for _, cc := range classified {
		if isDuplicateComment(cc.Input, existing) {
			result.Skipped++
			continue
		}

		switch cc.Mode {
		case "inline":
			// 行内评论：关联到代码的具体行
			reviewComments = append(reviewComments, reviewComment{
				Path: cc.Input.Path,
				Line: cc.Input.Line,
				Side: "RIGHT",
				Body: cc.Input.Body,
			})
			reviewOriginals = append(reviewOriginals, cc)
		case "file":
			// 文件级评论：关联到文件，在正文中标注行号
			lineRef := ""
			if cc.Input.Line != nil {
				lineRef = fmt.Sprintf("**Line %d:**\n\n", *cc.Input.Line)
			}
			reviewComments = append(reviewComments, reviewComment{
				Path:        cc.Input.Path,
				Body:        lineRef + cc.Input.Body,
				SubjectType: "file",
			})
			reviewOriginals = append(reviewOriginals, cc)
		default:
			// 全局评论：无法关联到 diff 中的文件，单独处理
			globalEntries = append(globalEntries, cc)
		}
	}

	// 通过 Reviews API 批量提交行内和文件级评论
	if len(reviewComments) > 0 {
		payload, _ := json.Marshal(map[string]interface{}{
			"commit_id": commitSha,
			"event":     "COMMENT",
			"comments":  reviewComments,
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/reviews", resolvedRepo, prNumber),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			// 批量提交成功，统计各类型计数
			for _, orig := range reviewOriginals {
				result.Posted++
				if orig.Mode == "inline" {
					result.Inline++
				} else {
					result.FileLevel++
				}
			}
		} else {
			// 批量提交失败，降级为逐条发布（每条评论使用三级降级策略）
			for _, orig := range reviewOriginals {
				cr := PostComment(prNumber, PostCommentOpts{
					Path:      orig.Input.Path,
					Line:      orig.Input.Line,
					Body:      orig.Input.Body,
					CommitSha: commitSha,
					Repo:      repo,
				})
				if cr.Success {
					result.Posted++
					if cr.Inline {
						result.Inline++
					} else {
						result.Global++
					}
				} else {
					result.Failed++
				}
			}
		}
	}

	// 逐条发布全局评论（使用 gh pr comment 命令）
	for _, cc := range globalEntries {
		location := fmt.Sprintf("**%s**\n\n", cc.Input.Path)
		if cc.Input.Line != nil {
			location = fmt.Sprintf("**%s:%d**\n\n", cc.Input.Path, *cc.Input.Line)
		}
		body := location + cc.Input.Body

		args := []string{"pr", "comment", prNumber, "--body-file", "-"}
		if repo != "" {
			args = append(args, "--repo", repo)
		}
		cmd := exec.Command("gh", args...)
		cmd.Stdin = strings.NewReader(body)
		if err := cmd.Run(); err == nil {
			result.Posted++
			result.Global++
		} else {
			result.Failed++
		}
	}

	return result
}

// PostIssuesAsComments 是将代码审查问题发布为 PR 评论的主入口函数。
// 完整流程：
//  1. 获取 PR 的 HEAD 提交 SHA
//  2. 将每个问题格式化为评论正文（包含严重性徽章、描述、修复建议等）
//  3. 根据 diff 信息分类评论的放置方式
//  4. 批量发布评论到 PR
func PostIssuesAsComments(prNumber string, issues []IssueForComment, repo string) ReviewResult {
	commitSha, err := GetPRHeadSha(prNumber, repo)
	if err != nil {
		return ReviewResult{Failed: len(issues)}
	}

	// 将问题列表转换为评论输入格式
	comments := make([]ReviewCommentInput, 0, len(issues))
	for _, issue := range issues {
		body := formatIssueBody(issue)
		comments = append(comments, ReviewCommentInput{
			Path: issue.File,
			Line: issue.Line,
			Body: body,
		})
	}

	// 根据 diff 信息分类每条评论的放置方式
	classified, err := ClassifyComments(prNumber, comments, repo)
	if err != nil {
		return ReviewResult{Failed: len(issues)}
	}

	// 批量发布分类后的评论
	return PostReview(prNumber, classified, commitSha, repo)
}

// formatIssueBody 将单个问题格式化为 Markdown 格式的评论正文。
// 包含严重性徽章、标题、描述，以及可选的修复建议和提出者信息。
func formatIssueBody(issue IssueForComment) string {
	severityBadge := severityToBadge(issue.Severity)
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%s **%s**\n\n", severityBadge, issue.Title))
	sb.WriteString(issue.Description)
	if issue.SuggestedFix != "" {
		sb.WriteString(fmt.Sprintf("\n\n**Suggested fix:** %s", issue.SuggestedFix))
	}
	if issue.RaisedBy != "" {
		sb.WriteString(fmt.Sprintf("\n\n_Raised by: %s_", issue.RaisedBy))
	}
	return sb.String()
}

// severityToBadge 将严重等级字符串转换为对应的彩色圆点 emoji 徽章。
// critical -> 红色, high -> 橙色, medium -> 黄色, low -> 绿色, 其他 -> 白色
func severityToBadge(severity string) string {
	switch strings.ToLower(severity) {
	case "critical":
		return "🔴"
	case "high":
		return "🟠"
	case "medium":
		return "🟡"
	case "low":
		return "🟢"
	default:
		return "⚪"
	}
}

// truncStr 将字符串截断到指定的最大长度。
// 用于限制评论内容在去重比较和错误信息中的长度。
func truncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
