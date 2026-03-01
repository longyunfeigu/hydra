// Package github 实现了 GitHub 平台的 Platform 接口。
// 通过 gh CLI 工具与 GitHub API 交互。
package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/guwanhua/hydra/internal/platform"
)

// repoRegex 用于从 GitHub 远程 URL 中提取 owner/repo 格式的仓库标识
var repoRegex = regexp.MustCompile(`github\.com[:/]([^/]+/[^/.]+)`)

// prNumberRegex 用于验证 PR 编号格式（纯数字）
var prNumberRegex = regexp.MustCompile(`^\d+$`)

// urlPRNumberRegex 从 GitHub PR URL 中提取 PR 编号
var urlPRNumberRegex = regexp.MustCompile(`/pull/(\d+)`)

// urlRepoRegex 从 GitHub PR URL 中提取仓库标识
var urlRepoRegex = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/`)

// GitHubPlatform 实现了 platform.Platform 接口的 GitHub 版本。
type GitHubPlatform struct{}

// New 创建一个新的 GitHubPlatform 实例。
func New() *GitHubPlatform {
	return &GitHubPlatform{}
}

func (g *GitHubPlatform) Name() string {
	return "github"
}

// DetectRepoFromRemote 从 git remote URL 中检测 GitHub 仓库标识。
func (g *GitHubPlatform) DetectRepoFromRemote() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("could not detect GitHub repo from git remote: %w", err)
	}
	remoteURL := strings.TrimSpace(string(out))
	m := repoRegex.FindStringSubmatch(remoteURL)
	if m == nil {
		return "", fmt.Errorf("could not detect GitHub repo from git remote URL: %s", remoteURL)
	}
	return m[1], nil
}

// ParseMRURL 解析 GitHub PR URL，提取仓库和 PR 编号。
func (g *GitHubPlatform) ParseMRURL(url string) (repo, mrID string, err error) {
	mNum := urlPRNumberRegex.FindStringSubmatch(url)
	if len(mNum) < 2 {
		return "", "", fmt.Errorf("could not parse PR number from URL: %s", url)
	}
	mrID = mNum[1]

	mRepo := urlRepoRegex.FindStringSubmatch(url)
	if len(mRepo) > 1 {
		repo = mRepo[1]
	}
	return repo, mrID, nil
}

// BuildMRURL 构建 GitHub PR URL。
func (g *GitHubPlatform) BuildMRURL(repo, mrID string) string {
	return fmt.Sprintf("https://github.com/%s/pull/%s", repo, mrID)
}

// resolveRepo 解析仓库标识，为空时自动检测。
func (g *GitHubPlatform) resolveRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	return g.DetectRepoFromRemote()
}

// validatePRNumber 验证 PR 编号格式。
func validatePRNumber(prNumber string) error {
	if !prNumberRegex.MatchString(prNumber) {
		return fmt.Errorf("invalid PR number: %s", prNumber)
	}
	return nil
}

// GetDiff 获取 PR 的 diff 内容。
func (g *GitHubPlatform) GetDiff(mrID, repo string) (string, error) {
	args := []string{"pr", "diff", mrID}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get PR diff: %w", err)
	}
	return string(out), nil
}

// GetInfo 获取 PR 的标题和描述。
func (g *GitHubPlatform) GetInfo(mrID, repo string) (*platform.MRInfo, error) {
	args := []string{"pr", "view", mrID, "--json", "title,body"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PR info: %w", err)
	}
	var info struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("failed to parse PR info: %w", err)
	}
	return &platform.MRInfo{
		Title:       info.Title,
		Description: info.Body,
	}, nil
}

// GetHeadCommitInfo 获取 PR 的 HEAD 提交信息。
// GitHub 仅需 HeadSHA。
func (g *GitHubPlatform) GetHeadCommitInfo(mrID, repo string) (*platform.CommitInfo, error) {
	if err := validatePRNumber(mrID); err != nil {
		return nil, err
	}
	args := []string{"pr", "view", mrID, "--json", "headRefOid", "--jq", ".headRefOid"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	out, err := exec.Command("gh", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PR head SHA: %w", err)
	}
	return &platform.CommitInfo{
		HeadSHA: strings.TrimSpace(string(out)),
	}, nil
}

// diffFile 表示 GitHub API 返回的 PR 文件信息。
type diffFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

// GetChangedFiles 获取 PR 中的变更文件列表。
func (g *GitHubPlatform) GetChangedFiles(mrID, repo string) ([]platform.DiffFile, error) {
	if err := validatePRNumber(mrID); err != nil {
		return nil, err
	}
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return nil, err
	}
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%s/files", resolvedRepo, mrID),
		"--paginate",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PR files: %w", err)
	}

	var files []diffFile
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("failed to parse PR files: %w", err)
	}

	result := make([]platform.DiffFile, len(files))
	for i, f := range files {
		result[i] = platform.DiffFile{
			Filename: f.Filename,
			Patch:    f.Patch,
		}
	}
	return result, nil
}

// GetExistingComments 获取 PR 上已存在的评审评论。
func (g *GitHubPlatform) GetExistingComments(mrID, repo string) []platform.ExistingComment {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return nil
	}
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%s/comments", resolvedRepo, mrID),
		"--paginate", "--jq", ".[]",
	).Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var comments []platform.ExistingComment
	for _, line := range lines {
		if line == "" {
			continue
		}
		var c platform.ExistingComment
		if json.Unmarshal([]byte(line), &c) == nil {
			comments = append(comments, c)
		}
	}
	return comments
}

// PostComment 在 PR 上发布单条评论，采用三级降级策略。
func (g *GitHubPlatform) PostComment(mrID string, opts platform.PostCommentOpts) platform.CommentResult {
	if err := validatePRNumber(mrID); err != nil {
		return platform.CommentResult{Success: false, Error: err.Error()}
	}
	resolvedRepo, err := g.resolveRepo(opts.Repo)
	if err != nil {
		return platform.CommentResult{Success: false, Error: err.Error()}
	}

	// 第一级：行内评论
	if opts.Line != nil {
		payload, _ := json.Marshal(map[string]interface{}{
			"body":      opts.Body,
			"commit_id": opts.CommitInfo.HeadSHA,
			"path":      opts.Path,
			"line":      *opts.Line,
			"side":      "RIGHT",
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/comments", resolvedRepo, mrID),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			return platform.CommentResult{Success: true, Inline: true}
		}
	}

	// 第二级：文件级评论
	{
		lineRef := ""
		if opts.Line != nil {
			lineRef = fmt.Sprintf("**Line %d:**\n\n", *opts.Line)
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"body":         lineRef + opts.Body,
			"commit_id":    opts.CommitInfo.HeadSHA,
			"path":         opts.Path,
			"subject_type": "file",
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/comments", resolvedRepo, mrID),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			return platform.CommentResult{Success: true, Inline: true}
		}
	}

	// 第三级：全局 PR 评论
	{
		location := fmt.Sprintf("**%s**\n\n", opts.Path)
		if opts.Line != nil {
			location = fmt.Sprintf("**%s:%d**\n\n", opts.Path, *opts.Line)
		}
		body := location + opts.Body

		args := []string{"pr", "comment", mrID, "--body-file", "-"}
		if opts.Repo != "" {
			args = append(args, "--repo", opts.Repo)
		}
		cmd := exec.Command("gh", args...)
		cmd.Stdin = strings.NewReader(body)
		if err := cmd.Run(); err != nil {
			return platform.CommentResult{Success: false, Error: platform.TruncStr(err.Error(), 200)}
		}
		return platform.CommentResult{Success: true, Inline: false}
	}
}

// PostReview 批量发布已分类的评论到 PR。
func (g *GitHubPlatform) PostReview(mrID string, classified []platform.ClassifiedComment, commitInfo platform.CommitInfo, repo string) platform.ReviewResult {
	if err := validatePRNumber(mrID); err != nil {
		return platform.ReviewResult{Failed: len(classified)}
	}
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return platform.ReviewResult{Failed: len(classified)}
	}

	existing := g.GetExistingComments(mrID, repo)
	var result platform.ReviewResult

	type reviewComment struct {
		Path        string `json:"path"`
		Line        *int   `json:"line,omitempty"`
		Side        string `json:"side,omitempty"`
		Body        string `json:"body"`
		SubjectType string `json:"subject_type,omitempty"`
	}

	var reviewComments []reviewComment
	var reviewOriginals []platform.ClassifiedComment
	var globalEntries []platform.ClassifiedComment

	for _, cc := range classified {
		if platform.IsDuplicateComment(cc.Input, existing) {
			result.Skipped++
			continue
		}

		switch cc.Mode {
		case "inline":
			reviewComments = append(reviewComments, reviewComment{
				Path: cc.Input.Path,
				Line: cc.Input.Line,
				Side: "RIGHT",
				Body: cc.Input.Body,
			})
			reviewOriginals = append(reviewOriginals, cc)
		case "file":
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
			globalEntries = append(globalEntries, cc)
		}
	}

	// 批量提交行内和文件级评论
	if len(reviewComments) > 0 {
		payload, _ := json.Marshal(map[string]interface{}{
			"commit_id": commitInfo.HeadSHA,
			"event":     "COMMENT",
			"comments":  reviewComments,
		})
		cmd := exec.Command("gh", "api",
			fmt.Sprintf("repos/%s/pulls/%s/reviews", resolvedRepo, mrID),
			"--input", "-",
		)
		cmd.Stdin = strings.NewReader(string(payload))
		if err := cmd.Run(); err == nil {
			for _, orig := range reviewOriginals {
				result.Posted++
				if orig.Mode == "inline" {
					result.Inline++
				} else {
					result.FileLevel++
				}
			}
		} else {
			// 降级为逐条发布
			for _, orig := range reviewOriginals {
				cr := g.PostComment(mrID, platform.PostCommentOpts{
					Path:       orig.Input.Path,
					Line:       orig.Input.Line,
					Body:       orig.Input.Body,
					CommitInfo: commitInfo,
					Repo:       repo,
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

	// 逐条发布全局评论
	for _, cc := range globalEntries {
		location := fmt.Sprintf("**%s**\n\n", cc.Input.Path)
		if cc.Input.Line != nil {
			location = fmt.Sprintf("**%s:%d**\n\n", cc.Input.Path, *cc.Input.Line)
		}
		body := location + cc.Input.Body

		args := []string{"pr", "comment", mrID, "--body-file", "-"}
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

// PostIssuesAsComments 将代码审查问题发布为 PR 评论的主入口。
func (g *GitHubPlatform) PostIssuesAsComments(mrID string, issues []platform.IssueForComment, repo string) platform.ReviewResult {
	commitInfo, err := g.GetHeadCommitInfo(mrID, repo)
	if err != nil {
		return platform.ReviewResult{Failed: len(issues)}
	}

	comments := make([]platform.ReviewCommentInput, 0, len(issues))
	for _, issue := range issues {
		body := platform.FormatIssueBody(issue)
		comments = append(comments, platform.ReviewCommentInput{
			Path: issue.File,
			Line: issue.Line,
			Body: body,
		})
	}

	// 获取 diff 信息并分类评论
	changedFiles, err := g.GetChangedFiles(mrID, repo)
	if err != nil {
		return platform.ReviewResult{Failed: len(issues)}
	}

	diffInfo := make(map[string]map[int]bool)
	for _, f := range changedFiles {
		if f.Patch != "" {
			diffInfo[f.Filename] = platform.ParseDiffLines(f.Patch)
		} else {
			diffInfo[f.Filename] = make(map[int]bool)
		}
	}

	classified := platform.ClassifyCommentsByDiff(comments, diffInfo)
	return g.PostReview(mrID, classified, *commitInfo, repo)
}

// GetMRDetails 获取单个 PR 的详细信息，用于历史 PR 关联。
func (g *GitHubPlatform) GetMRDetails(mrNumber int, cwd string) (*platform.MRDetail, error) {
	cmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", mrNumber),
		"--json", "number,title,author,mergedAt,files",
	)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PR details: %w", err)
	}

	var data struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		MergedAt string `json:"mergedAt"`
		Author   struct {
			Login string `json:"login"`
		} `json:"author"`
		Files []struct {
			Path string `json:"path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("failed to parse PR details: %w", err)
	}

	files := make([]string, len(data.Files))
	for i, f := range data.Files {
		files[i] = f.Path
	}

	return &platform.MRDetail{
		Number:   data.Number,
		Title:    data.Title,
		Author:   data.Author.Login,
		MergedAt: data.MergedAt,
		Files:    files,
	}, nil
}

// GetMRsForCommit 通过 GitHub API 查找与指定 commit SHA 关联的 PR 编号列表。
func (g *GitHubPlatform) GetMRsForCommit(commitSHA string, cwd string) ([]int, error) {
	resolvedRepo, err := g.resolveRepo("")
	if err != nil {
		return nil, err
	}

	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/commits/%s/pulls", resolvedRepo, commitSHA),
		"--jq", ".[].number",
	)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PRs for commit %s: %w", commitSHA, err)
	}

	var numbers []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err == nil && n > 0 {
			numbers = append(numbers, n)
		}
	}
	return numbers, nil
}
