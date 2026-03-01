package platform

import (
	"fmt"
	"strings"
)

// FormatIssueBody 将单个问题格式化为 Markdown 格式的评论正文。
func FormatIssueBody(issue IssueForComment) string {
	severityBadge := SeverityToBadge(issue.Severity)
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
