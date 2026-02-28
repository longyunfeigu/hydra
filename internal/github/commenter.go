package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// CommentResult represents the result of posting a single comment.
type CommentResult struct {
	Success bool
	Inline  bool
	Error   string
}

// ReviewCommentInput is the input for a single review comment.
type ReviewCommentInput struct {
	Path string
	Line *int
	Body string
}

// ClassifiedComment is a comment with its placement mode.
type ClassifiedComment struct {
	Input ReviewCommentInput
	Mode  string // "inline", "file", "global"
}

// ReviewResult summarizes the outcome of posting a batch of review comments.
type ReviewResult struct {
	Posted    int
	Inline    int
	FileLevel int
	Global    int
	Failed    int
	Skipped   int
}

// IssueForComment is a minimal struct for converting issues to review comments.
type IssueForComment struct {
	File         string
	Line         *int
	Title        string
	Description  string
	Severity     string
	SuggestedFix string
	RaisedBy     string
}

var prNumberRegex = regexp.MustCompile(`^\d+$`)
var repoRegex = regexp.MustCompile(`github\.com[:/]([^/]+/[^/.]+)`)

func validatePRNumber(prNumber string) error {
	if !prNumberRegex.MatchString(prNumber) {
		return fmt.Errorf("invalid PR number: %s", prNumber)
	}
	return nil
}

func getRepo(repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
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

// GetPRHeadSha returns the HEAD commit SHA for a PR.
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

// PostCommentOpts holds options for PostComment.
type PostCommentOpts struct {
	Path      string
	Line      *int
	Body      string
	CommitSha string
	Repo      string
}

// PostComment posts a single comment on a PR with 3-level fallback:
// inline -> file-level -> global PR comment.
func PostComment(prNumber string, opts PostCommentOpts) CommentResult {
	if err := validatePRNumber(prNumber); err != nil {
		return CommentResult{Success: false, Error: err.Error()}
	}
	repo, err := getRepo(opts.Repo)
	if err != nil {
		return CommentResult{Success: false, Error: err.Error()}
	}

	// Try inline comment first if we have a line number
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

	// Try file-level review comment
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

	// Last resort: regular PR comment
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

// ClassifyComments classifies comments by how they can be placed on a PR
// based on the diff info.
func ClassifyComments(prNumber string, comments []ReviewCommentInput, repo string) ([]ClassifiedComment, error) {
	if err := validatePRNumber(prNumber); err != nil {
		return nil, err
	}
	resolvedRepo, err := getRepo(repo)
	if err != nil {
		return nil, err
	}
	diffInfo, err := GetDiffInfo(prNumber, resolvedRepo)
	if err != nil {
		return nil, err
	}

	classified := make([]ClassifiedComment, 0, len(comments))
	for _, c := range comments {
		fileLines, fileInDiff := diffInfo[c.Path]
		if fileInDiff && c.Line != nil && fileLines[*c.Line] {
			classified = append(classified, ClassifiedComment{Input: c, Mode: "inline"})
		} else if fileInDiff {
			classified = append(classified, ClassifiedComment{Input: c, Mode: "file"})
		} else {
			classified = append(classified, ClassifiedComment{Input: c, Mode: "global"})
		}
	}

	return classified, nil
}

type existingComment struct {
	Path string `json:"path"`
	Line *int   `json:"line"`
	Body string `json:"body"`
}

func getExistingComments(prNumber, resolvedRepo string) []existingComment {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%s/comments", resolvedRepo, prNumber),
		"--paginate", "--jq", ".[]",
	).Output()
	if err != nil {
		return nil
	}
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

func isDuplicateComment(comment ReviewCommentInput, existing []existingComment) bool {
	prefix := truncStr(comment.Body, 100)
	for _, e := range existing {
		if e.Path != comment.Path {
			continue
		}
		// Compare line pointers
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

// PostReview posts pre-classified comments to a PR using the Reviews API
// for inline/file-level comments, with fallback to individual PostComment.
func PostReview(prNumber string, classified []ClassifiedComment, commitSha, repo string) ReviewResult {
	if err := validatePRNumber(prNumber); err != nil {
		return ReviewResult{Failed: len(classified)}
	}
	resolvedRepo, err := getRepo(repo)
	if err != nil {
		return ReviewResult{Failed: len(classified)}
	}

	existing := getExistingComments(prNumber, resolvedRepo)

	var result ReviewResult

	type reviewComment struct {
		Path        string `json:"path"`
		Line        *int   `json:"line,omitempty"`
		Side        string `json:"side,omitempty"`
		Body        string `json:"body"`
		SubjectType string `json:"subject_type,omitempty"`
	}

	var reviewComments []reviewComment
	var reviewOriginals []ClassifiedComment
	var globalEntries []ClassifiedComment

	for _, cc := range classified {
		if isDuplicateComment(cc.Input, existing) {
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

	// Post the batch review (inline + file-level)
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
			// Batch succeeded
			for _, orig := range reviewOriginals {
				result.Posted++
				if orig.Mode == "inline" {
					result.Inline++
				} else {
					result.FileLevel++
				}
			}
		} else {
			// Batch failed - try posting individually
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

	// Post global comments
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

// PostIssuesAsComments converts issues to review comments and posts them.
func PostIssuesAsComments(prNumber string, issues []IssueForComment, repo string) ReviewResult {
	commitSha, err := GetPRHeadSha(prNumber, repo)
	if err != nil {
		return ReviewResult{Failed: len(issues)}
	}

	comments := make([]ReviewCommentInput, 0, len(issues))
	for _, issue := range issues {
		body := formatIssueBody(issue)
		comments = append(comments, ReviewCommentInput{
			Path: issue.File,
			Line: issue.Line,
			Body: body,
		})
	}

	classified, err := ClassifyComments(prNumber, comments, repo)
	if err != nil {
		return ReviewResult{Failed: len(issues)}
	}

	return PostReview(prNumber, classified, commitSha, repo)
}

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

func truncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
