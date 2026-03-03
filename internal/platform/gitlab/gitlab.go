// Package gitlab 实现了 GitLab 平台的 Platform 接口。
// 通过 glab CLI 工具与 GitLab API 交互。
package gitlab

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strings"

	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/util"
)

// mrURLRegex 从 GitLab MR URL 中提取项目路径和 MR IID
var mrURLRegex = regexp.MustCompile(`(?:https?://[^/]+/)?(.+?)/-/merge_requests/(\d+)`)

// remoteRegex 从 git remote URL 中提取 GitLab 项目路径
var remoteRegex = regexp.MustCompile(`[:/](.+?)(?:\.git)?$`)

// GitLabPlatform 实现了 platform.Platform 接口的 GitLab 版本。
type GitLabPlatform struct {
	host string // 自托管 GitLab 域名，为空表示 gitlab.com
}

// New 创建一个新的 GitLabPlatform 实例。
func New(host string) *GitLabPlatform {
	return &GitLabPlatform{host: host}
}

func (g *GitLabPlatform) Name() string {
	return "gitlab"
}

// getHost 返回 GitLab 主机地址。
func (g *GitLabPlatform) getHost() string {
	if g.host != "" {
		return g.host
	}
	return "gitlab.com"
}

// DetectRepoFromRemote 从 git remote URL 中检测 GitLab 项目路径。
// 支持嵌套组路径（如 group/subgroup/project）。
func (g *GitLabPlatform) DetectRepoFromRemote() (string, error) {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return "", fmt.Errorf("could not detect GitLab repo from git remote: %w", err)
	}
	remoteURL := strings.TrimSpace(string(out))

	// 匹配 SSH (git@host:path) 或 HTTPS (host/path) 格式
	m := remoteRegex.FindStringSubmatch(remoteURL)
	if m == nil {
		return "", fmt.Errorf("could not detect GitLab repo from git remote URL: %s", remoteURL)
	}
	return m[1], nil
}

// ParseMRURL 解析 GitLab MR URL，提取项目路径和 MR IID。
func (g *GitLabPlatform) ParseMRURL(rawURL string) (repo, mrID string, err error) {
	m := mrURLRegex.FindStringSubmatch(rawURL)
	if len(m) < 3 {
		return "", "", fmt.Errorf("could not parse MR URL: %s", rawURL)
	}
	return m[1], m[2], nil
}

// BuildMRURL 构建 GitLab MR URL。
func (g *GitLabPlatform) BuildMRURL(repo, mrID string) string {
	return fmt.Sprintf("https://%s/%s/-/merge_requests/%s", g.getHost(), repo, mrID)
}

// encodeProject 对 GitLab 项目路径进行 URL 编码（用于 API 调用）。
func encodeProject(repo string) string {
	return url.PathEscape(repo)
}

// glabPostJSON 通过 glab api 发送 JSON POST 请求。
// 必须设置 Content-Type: application/json，否则 glab 默认不设 Content-Type，
// 导致 GitLab 返回 415 或将 position 中的整数字段当作字符串处理（inline comment 失效）。
func glabPostJSON(endpoint string, payload []byte) ([]byte, error) {
	cmd := exec.Command("glab", "api", endpoint,
		"--method", "POST",
		"--input", "-",
		"-H", "Content-Type: application/json",
	)
	cmd.Stdin = strings.NewReader(string(payload))
	return cmd.CombinedOutput()
}

// glabPutJSON 通过 glab api 发送 JSON PUT 请求。
func glabPutJSON(endpoint string, payload []byte) ([]byte, error) {
	cmd := exec.Command("glab", "api", endpoint,
		"--method", "PUT",
		"--input", "-",
		"-H", "Content-Type: application/json",
	)
	cmd.Stdin = strings.NewReader(string(payload))
	return cmd.CombinedOutput()
}

// GetDiff 获取 MR 的 diff 内容。
// 有 repo 参数时用 glab api（server 模式），否则用 glab mr（CLI 模式，向后兼容）。
func (g *GitLabPlatform) GetDiff(mrID, repo string) (string, error) {
	if repo != "" {
		return g.getDiffViaAPI(mrID, repo)
	}
	out, err := exec.Command("glab", "mr", "diff", mrID).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get MR diff: %w", err)
	}
	return string(out), nil
}

// getDiffViaAPI 通过 glab api 获取 MR diff（不需要 git 上下文）。
func (g *GitLabPlatform) getDiffViaAPI(mrID, repo string) (string, error) {
	encoded := encodeProject(repo)
	out, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s/diffs", encoded, mrID),
	).Output()
	if err != nil {
		return "", fmt.Errorf("failed to get MR diff via API: %w", err)
	}

	var diffs []struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
		Diff    string `json:"diff"`
	}
	if err := json.Unmarshal(out, &diffs); err != nil {
		return "", fmt.Errorf("failed to parse MR diff: %w", err)
	}

	var sb strings.Builder
	for _, d := range diffs {
		sb.WriteString(fmt.Sprintf("--- a/%s\n+++ b/%s\n%s\n", d.OldPath, d.NewPath, d.Diff))
	}
	return sb.String(), nil
}

// GetInfo 获取 MR 的标题和描述。
// 有 repo 参数时用 glab api（server 模式），否则用 glab mr（CLI 模式，向后兼容）。
func (g *GitLabPlatform) GetInfo(mrID, repo string) (*platform.MRInfo, error) {
	if repo != "" {
		return g.getInfoViaAPI(mrID, repo)
	}
	out, err := exec.Command("glab", "mr", "view", mrID, "--output", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR info: %w", err)
	}
	var info struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("failed to parse MR info: %w", err)
	}
	return &platform.MRInfo{
		Title:       info.Title,
		Description: info.Description,
	}, nil
}

// getInfoViaAPI 通过 glab api 获取 MR 信息（不需要 git 上下文）。
func (g *GitLabPlatform) getInfoViaAPI(mrID, repo string) (*platform.MRInfo, error) {
	encoded := encodeProject(repo)
	out, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s", encoded, mrID),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR info via API: %w", err)
	}
	var info struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("failed to parse MR info: %w", err)
	}
	return &platform.MRInfo{
		Title:       info.Title,
		Description: info.Description,
	}, nil
}

// GetHeadCommitInfo 获取 MR 的 HEAD 提交信息。
// GitLab 需要 3 个 SHA（head_sha, base_sha, start_sha）用于 Discussions API。
// 有 repo 参数时用 glab api（server 模式），否则用 glab mr（CLI 模式，向后兼容）。
func (g *GitLabPlatform) GetHeadCommitInfo(mrID, repo string) (*platform.CommitInfo, error) {
	if repo != "" {
		return g.getHeadCommitInfoViaAPI(mrID, repo)
	}
	out, err := exec.Command("glab", "mr", "view", mrID, "--output", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR commit info: %w", err)
	}
	return parseCommitInfo(out)
}

// getHeadCommitInfoViaAPI 通过 glab api 获取 MR 提交信息（不需要 git 上下文）。
func (g *GitLabPlatform) getHeadCommitInfoViaAPI(mrID, repo string) (*platform.CommitInfo, error) {
	encoded := encodeProject(repo)
	out, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s", encoded, mrID),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR commit info via API: %w", err)
	}
	return parseCommitInfo(out)
}

// parseCommitInfo 从 MR JSON 响应中解析 diff_refs 提交信息。
func parseCommitInfo(data []byte) (*platform.CommitInfo, error) {
	var result struct {
		DiffRefs struct {
			HeadSha  string `json:"head_sha"`
			BaseSha  string `json:"base_sha"`
			StartSha string `json:"start_sha"`
		} `json:"diff_refs"`
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("failed to parse MR commit info: %w", err)
	}

	headSHA := result.DiffRefs.HeadSha
	if headSHA == "" {
		headSHA = result.SHA
	}

	return &platform.CommitInfo{
		HeadSHA:  headSHA,
		BaseSHA:  result.DiffRefs.BaseSha,
		StartSHA: result.DiffRefs.StartSha,
	}, nil
}

// GetChangedFiles 获取 MR 中的变更文件列表。
func (g *GitLabPlatform) GetChangedFiles(mrID, repo string) ([]platform.DiffFile, error) {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return nil, err
	}
	encodedProject := encodeProject(resolvedRepo)

	out, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s/diffs", encodedProject, mrID),
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR diffs: %w", err)
	}

	var diffs []struct {
		NewPath string `json:"new_path"`
		Diff    string `json:"diff"`
	}
	if err := json.Unmarshal(out, &diffs); err != nil {
		return nil, fmt.Errorf("failed to parse MR diffs: %w", err)
	}

	result := make([]platform.DiffFile, len(diffs))
	for i, d := range diffs {
		result[i] = platform.DiffFile{
			Filename: d.NewPath,
			Patch:    d.Diff,
		}
	}
	return result, nil
}

// resolveRepo 解析项目路径，为空时自动检测。
func (g *GitLabPlatform) resolveRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	return g.DetectRepoFromRemote()
}

// GetExistingComments 获取 MR 上已存在的评论（包括 discussions）。
func (g *GitLabPlatform) GetExistingComments(mrID, repo string) []platform.ExistingComment {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return nil
	}
	encodedProject := encodeProject(resolvedRepo)

	out, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s/discussions", encodedProject, mrID),
	).Output()
	if err != nil {
		return nil
	}

	var discussions []struct {
		Notes []struct {
			Body     string `json:"body"`
			Position *struct {
				NewPath string `json:"new_path"`
				NewLine *int   `json:"new_line"`
			} `json:"position"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(out, &discussions); err != nil {
		return nil
	}

	var comments []platform.ExistingComment
	for _, d := range discussions {
		for _, note := range d.Notes {
			c := platform.ExistingComment{Body: note.Body}
			if note.Position != nil {
				c.Path = note.Position.NewPath
				c.Line = note.Position.NewLine
			}
			comments = append(comments, c)
		}
	}
	return comments
}

// PostComment 在 MR 上发布单条评论，采用三级降级策略：
// 1. Discussions API 行内评论
// 2. Discussions API 文件级评论
// 3. glab mr note 全局评论
func (g *GitLabPlatform) PostComment(mrID string, opts platform.PostCommentOpts) platform.CommentResult {
	resolvedRepo, err := g.resolveRepo(opts.Repo)
	if err != nil {
		return platform.CommentResult{Success: false, Error: err.Error()}
	}
	encodedProject := encodeProject(resolvedRepo)

	// 第一级：行内评论（通过 Discussions API，使用 JSON body 确保 new_line 为整数类型）
	if opts.Line != nil && opts.CommitInfo.HeadSHA != "" {
		endpoint := fmt.Sprintf("projects/%s/merge_requests/%s/discussions", encodedProject, mrID)
		payload, _ := json.Marshal(map[string]interface{}{
			"body": opts.Body,
			"position": buildTextPosition(opts.Path, *opts.Line, opts.CommitInfo),
		})
		out, err := glabPostJSON(endpoint, payload)
		if err == nil {
			util.Debugf("PostComment success mode=inline path=%s line=%d", opts.Path, *opts.Line)
			return platform.CommentResult{Success: true, Inline: true, Mode: "inline"}
		}
		util.Debugf("PostComment inline failed for %s:%d: %v | response: %s", opts.Path, *opts.Line, err, string(out))
	}

	// 第二级：文件级评论（通过 Discussions API，position_type: file，使用 JSON body）
	if opts.CommitInfo.HeadSHA != "" {
		lineRef := ""
		if opts.Line != nil {
			lineRef = fmt.Sprintf("**Line %d:**\n\n", *opts.Line)
		}
		endpoint := fmt.Sprintf("projects/%s/merge_requests/%s/discussions", encodedProject, mrID)
		payload, _ := json.Marshal(map[string]interface{}{
			"body": lineRef + opts.Body,
			"position": buildFilePosition(opts.Path, opts.CommitInfo),
		})
		out, err := glabPostJSON(endpoint, payload)
		if err == nil {
			util.Debugf("PostComment success mode=file path=%s", opts.Path)
			return platform.CommentResult{Success: true, Inline: false, Mode: "file"}
		}
		util.Debugf("PostComment file-level failed for %s: %v | response: %s", opts.Path, err, string(out))
	}

	// 第三级：全局评论（通过 glab mr note）
	{
		location := fmt.Sprintf("**%s**\n\n", opts.Path)
		if opts.Line != nil {
			location = fmt.Sprintf("**%s:%d**\n\n", opts.Path, *opts.Line)
		}
		body := location + opts.Body

		cmd := exec.Command("glab", "mr", "note", mrID, "--unique", "--message", body)
		if err := cmd.Run(); err != nil {
			return platform.CommentResult{Success: false, Error: platform.TruncStr(err.Error(), 200)}
		}
		util.Debugf("PostComment success mode=global path=%s", opts.Path)
		return platform.CommentResult{Success: true, Inline: false, Mode: "global"}
	}
}

func buildTextPosition(path string, line int, commitInfo platform.CommitInfo) map[string]interface{} {
	return map[string]interface{}{
		"position_type": "text",
		"new_path":      path,
		"old_path":      path,
		"new_line":      line,
		"head_sha":      commitInfo.HeadSHA,
		"base_sha":      commitInfo.BaseSHA,
		"start_sha":     commitInfo.StartSHA,
	}
}

func buildFilePosition(path string, commitInfo platform.CommitInfo) map[string]interface{} {
	return map[string]interface{}{
		"position_type": "file",
		"new_path":      path,
		"old_path":      path,
		"head_sha":      commitInfo.HeadSHA,
		"base_sha":      commitInfo.BaseSHA,
		"start_sha":     commitInfo.StartSHA,
	}
}

// PostReview 批量发布已分类的评论到 MR。
// 先尝试 Draft Notes API + bulk_publish，失败则降级为逐条 Discussions API。
func (g *GitLabPlatform) PostReview(mrID string, classified []platform.ClassifiedComment, commitInfo platform.CommitInfo, repo string) platform.ReviewResult {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return platform.ReviewResult{Failed: len(classified)}
	}
	encodedProject := encodeProject(resolvedRepo)

	existing := g.GetExistingComments(mrID, repo)
	var result platform.ReviewResult

	var inlineEntries []platform.ClassifiedComment
	var fileEntries []platform.ClassifiedComment
	var globalEntries []platform.ClassifiedComment

	for _, cc := range classified {
		if platform.IsDuplicateComment(cc.Input, existing) {
			result.Skipped++
			continue
		}
		switch cc.Mode {
		case "inline":
			inlineEntries = append(inlineEntries, cc)
		case "file":
			fileEntries = append(fileEntries, cc)
		default:
			globalEntries = append(globalEntries, cc)
		}
	}

	// 尝试使用 Draft Notes API 批量提交（需要 GitLab Premium）
	draftNotesSucceeded := false
	if len(inlineEntries)+len(fileEntries) > 0 {
		draftNotesSucceeded = g.tryDraftNotes(encodedProject, mrID, commitInfo, inlineEntries, fileEntries, &result)
	}

	// 如果 Draft Notes API 不可用，降级为逐条 Discussions API
	if !draftNotesSucceeded {
		for _, cc := range inlineEntries {
			cr := g.PostComment(mrID, platform.PostCommentOpts{
				Path:       cc.Input.Path,
				Line:       cc.Input.Line,
				Body:       cc.Input.Body,
				CommitInfo: commitInfo,
				Repo:       repo,
			})
			if cr.Success {
				result.Posted++
				switch cr.Mode {
				case "inline":
					result.Inline++
				case "file":
					result.FileLevel++
				default:
					result.Global++
				}
			} else {
				result.Failed++
			}
		}
		for _, cc := range fileEntries {
			lineRef := ""
			if cc.Input.Line != nil {
				lineRef = fmt.Sprintf("**Line %d:**\n\n", *cc.Input.Line)
			}
			cr := g.PostComment(mrID, platform.PostCommentOpts{
				Path:       cc.Input.Path,
				Body:       lineRef + cc.Input.Body,
				CommitInfo: commitInfo,
				Repo:       repo,
			})
			if cr.Success {
				result.Posted++
				switch cr.Mode {
				case "inline":
					result.Inline++
				case "file":
					result.FileLevel++
				default:
					result.Global++
				}
			} else {
				result.Failed++
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

		cmd := exec.Command("glab", "mr", "note", mrID, "--unique", "--message", body)
		if err := cmd.Run(); err == nil {
			result.Posted++
			result.Global++
		} else {
			result.Failed++
		}
	}

	util.Debugf("PostReview final result: posted=%d inline=%d file=%d global=%d failed=%d skipped=%d",
		result.Posted, result.Inline, result.FileLevel, result.Global, result.Failed, result.Skipped)
	return result
}

// tryDraftNotes 尝试使用 Draft Notes API 批量提交评论。
// 成功返回 true，失败（如 GitLab 版本不支持）返回 false。
func (g *GitLabPlatform) tryDraftNotes(encodedProject, mrID string, commitInfo platform.CommitInfo, inlineEntries, fileEntries []platform.ClassifiedComment, result *platform.ReviewResult) bool {
	var draftIDs []string

	// 创建 draft notes
	for _, cc := range inlineEntries {
		if cc.Input.Line == nil {
			return false
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"note": cc.Input.Body,
			"position": buildTextPosition(cc.Input.Path, *cc.Input.Line, commitInfo),
		})
		endpoint := fmt.Sprintf("projects/%s/merge_requests/%s/draft_notes", encodedProject, mrID)
		out, err := glabPostJSON(endpoint, payload)
		if err != nil {
			return false // Draft Notes API 不可用
		}
		var resp struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(out, &resp) == nil {
			draftIDs = append(draftIDs, fmt.Sprintf("%d", resp.ID))
		}
	}

	for _, cc := range fileEntries {
		lineRef := ""
		if cc.Input.Line != nil {
			lineRef = fmt.Sprintf("**Line %d:**\n\n", *cc.Input.Line)
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"note": lineRef + cc.Input.Body,
			"position": buildFilePosition(cc.Input.Path, commitInfo),
		})
		endpoint := fmt.Sprintf("projects/%s/merge_requests/%s/draft_notes", encodedProject, mrID)
		out, err := glabPostJSON(endpoint, payload)
		if err != nil {
			return false
		}
		var resp struct {
			ID int `json:"id"`
		}
		if json.Unmarshal(out, &resp) == nil {
			draftIDs = append(draftIDs, fmt.Sprintf("%d", resp.ID))
		}
	}

	// 批量发布所有 draft notes
	if len(draftIDs) == 0 {
		util.Warnf("tryDraftNotes: no draft note IDs returned, fallback to Discussions API")
		return false
	}

	cmd := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%s/draft_notes/bulk_publish", encodedProject, mrID),
		"--method", "POST",
	)
	if err := cmd.Run(); err != nil {
		util.Warnf("tryDraftNotes: bulk_publish failed, fallback to Discussions API: %v", err)
		return false
	}

	for range inlineEntries {
		result.Posted++
		result.Inline++
	}
	for range fileEntries {
		result.Posted++
		result.FileLevel++
	}

	return true
}

// PostIssuesAsComments 将代码审查问题发布为 MR 评论的主入口。
func (g *GitLabPlatform) PostIssuesAsComments(mrID string, issues []platform.IssueForComment, repo string) platform.ReviewResult {
	commitInfo, err := g.GetHeadCommitInfo(mrID, repo)
	if err != nil {
		return platform.ReviewResult{Failed: len(issues)}
	}
	util.Debugf("PostIssuesAsComments: commitInfo head=%s base=%s start=%s", commitInfo.HeadSHA, commitInfo.BaseSHA, commitInfo.StartSHA)

	comments := make([]platform.ReviewCommentInput, 0, len(issues))
	for _, issue := range issues {
		body := platform.FormatIssueBody(issue)
		comments = append(comments, platform.ReviewCommentInput{
			Path: issue.File,
			Line: issue.Line,
			Body: body,
		})
	}

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

	// 调试：打印 diff 中的文件路径
	util.Debugf("PostIssuesAsComments: diff contains %d files:", len(diffInfo))
	for path, lines := range diffInfo {
		util.Debugf("  diff file: %s (lines in diff: %d)", path, len(lines))
	}
	for _, c := range comments {
		lineStr := "nil"
		if c.Line != nil {
			lineStr = fmt.Sprintf("%d", *c.Line)
		}
		util.Debugf("  comment path: %q line: %s", c.Path, lineStr)
	}

	classified := platform.ClassifyCommentsByDiff(comments, diffInfo)

	// 调试：打印分类结果
	for _, cc := range classified {
		lineStr := "nil"
		if cc.Input.Line != nil {
			lineStr = fmt.Sprintf("%d", *cc.Input.Line)
		}
		util.Debugf("  classified: path=%q line=%s mode=%s", cc.Input.Path, lineStr, cc.Mode)
	}
	return g.PostReview(mrID, classified, *commitInfo, repo)
}

// PostNote 在 MR 上发布一条全局 note（评论）。
func (g *GitLabPlatform) PostNote(mrID, repo, body string) error {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return err
	}
	encoded := encodeProject(resolvedRepo)

	payload, _ := json.Marshal(map[string]string{"body": body})
	endpoint := fmt.Sprintf("projects/%s/merge_requests/%s/notes", encoded, mrID)
	if _, err := glabPostJSON(endpoint, payload); err != nil {
		return fmt.Errorf("failed to post MR note: %w", err)
	}
	return nil
}

// UpsertSummaryNote upsert Hydra 的总结 note（通过 marker 匹配已有 note）。
// 找到 marker 则更新该 note，否则创建新 note。
func (g *GitLabPlatform) UpsertSummaryNote(mrID, repo, marker, body string) error {
	resolvedRepo, err := g.resolveRepo(repo)
	if err != nil {
		return err
	}
	encoded := encodeProject(resolvedRepo)

	listEndpoint := fmt.Sprintf("projects/%s/merge_requests/%s/notes", encoded, mrID)
	out, err := exec.Command("glab", "api", listEndpoint).Output()
	if err != nil {
		return fmt.Errorf("failed to list MR notes: %w", err)
	}

	var notes []struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &notes); err != nil {
		return fmt.Errorf("failed to parse MR notes: %w", err)
	}

	for _, note := range notes {
		if marker != "" && strings.Contains(note.Body, marker) {
			payload, _ := json.Marshal(map[string]string{"body": body})
			updateEndpoint := fmt.Sprintf("projects/%s/merge_requests/%s/notes/%d", encoded, mrID, note.ID)
			if _, err := glabPutJSON(updateEndpoint, payload); err != nil {
				return fmt.Errorf("failed to update summary note: %w", err)
			}
			return nil
		}
	}

	return g.PostNote(mrID, repo, body)
}

// GetMRDetails 获取单个 MR 的详细信息，用于历史关联。
func (g *GitLabPlatform) GetMRDetails(mrNumber int, cwd string) (*platform.MRDetail, error) {
	resolvedRepo, err := g.resolveRepo("")
	if err != nil {
		return nil, err
	}
	encodedProject := encodeProject(resolvedRepo)

	cmd := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%d", encodedProject, mrNumber),
	)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MR details: %w", err)
	}

	var data struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
		MergedAt string `json:"merged_at"`
	}
	if err := json.Unmarshal(out, &data); err != nil {
		return nil, fmt.Errorf("failed to parse MR details: %w", err)
	}

	// 获取 MR 变更的文件列表
	changesOut, err := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/merge_requests/%d/diffs", encodedProject, mrNumber),
	).Output()

	var files []string
	if err == nil {
		var diffs []struct {
			NewPath string `json:"new_path"`
		}
		if json.Unmarshal(changesOut, &diffs) == nil {
			for _, d := range diffs {
				files = append(files, d.NewPath)
			}
		}
	}

	return &platform.MRDetail{
		Number:   data.IID,
		Title:    data.Title,
		Author:   data.Author.Username,
		MergedAt: data.MergedAt,
		Files:    files,
	}, nil
}

// GetMRsForCommit 通过 GitLab API 查找与指定 commit SHA 关联的 MR 编号列表。
func (g *GitLabPlatform) GetMRsForCommit(commitSHA string, cwd string) ([]int, error) {
	resolvedRepo, err := g.resolveRepo("")
	if err != nil {
		return nil, err
	}
	encodedProject := encodeProject(resolvedRepo)

	cmd := exec.Command("glab", "api",
		fmt.Sprintf("projects/%s/repository/commits/%s/merge_requests", encodedProject, commitSHA),
	)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get MRs for commit %s: %w", commitSHA, err)
	}

	var mrs []struct {
		IID int `json:"iid"`
	}
	if err := json.Unmarshal(out, &mrs); err != nil {
		return nil, fmt.Errorf("failed to parse MRs for commit %s: %w", commitSHA, err)
	}

	numbers := make([]int, 0, len(mrs))
	for _, mr := range mrs {
		if mr.IID > 0 {
			numbers = append(numbers, mr.IID)
		}
	}
	return numbers, nil
}
