package platform

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// IssueMarkerPrefix 是 Hydra inline 评论的幂等标记前缀。
const IssueMarkerPrefix = "<!-- hydra:issue:"

// FormatIssueBody 将单个问题格式化为 Markdown 格式的评论正文。
// 在开头插入隐藏的幂等标记 <!-- hydra:issue:<hash> -->，
// hash 由 (file, line, severity, title) 生成，确保同一 issue 重复审查时可识别。
func FormatIssueBody(issue IssueForComment) string {
	marker := BuildIssueMarker(issue.File, issue.Line, issue.Severity, issue.Title)

	severityBadge := SeverityToBadge(issue.Severity)
	var sb strings.Builder
	sb.WriteString(marker)
	sb.WriteByte('\n')
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

// BuildIssueMarker 生成用于幂等去重的隐藏 HTML 标记。
// 格式: <!-- hydra:issue:<sha256_prefix_8> -->
// 输入: file + line + severity + title，对大小写和空格做归一化处理。
func BuildIssueMarker(file string, line *int, severity, title string) string {
	lineStr := "0"
	if line != nil {
		lineStr = fmt.Sprintf("%d", *line)
	}
	key := fmt.Sprintf("%s:%s:%s:%s",
		strings.ToLower(strings.TrimSpace(file)),
		lineStr,
		strings.ToLower(strings.TrimSpace(severity)),
		strings.ToLower(strings.TrimSpace(title)),
	)
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%s%x -->", IssueMarkerPrefix, h[:4])
}

// SeverityToBadge 将严重等级字符串转换为对应的 emoji 徽章。
func SeverityToBadge(severity string) string {
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

// TruncStr 将字符串截断到指定的最大长度。
func TruncStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
