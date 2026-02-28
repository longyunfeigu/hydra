package display

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/guwanhua/hydra/internal/orchestrator"
)

// MessageForMarkdown 表示用于 Markdown 导出的辩论消息。
// 包含审查者标识和消息内容。
type MessageForMarkdown struct {
	ReviewerID string // 审查者标识
	Content    string // 消息内容
}

// SummaryForMarkdown 表示用于 Markdown 导出的审查者摘要。
type SummaryForMarkdown struct {
	ReviewerID string // 审查者标识
	Summary    string // 摘要内容
}

// MergedIssueForMarkdown 是 MergedIssue 的 Markdown 导出版本。
// 使用本地类型避免与 orchestrator 包的循环导入。
type MergedIssueForMarkdown struct {
	Severity     string   // 严重等级
	Title        string   // 问题标题
	File         string   // 问题所在文件
	Line         int      // 问题所在行号
	Description  string   // 问题描述
	SuggestedFix string   // 建议的修复方案
	RaisedBy     []string // 提出该问题的审查者列表
}

// DebateResultForMarkdown 包含生成 Markdown 报告所需的所有字段。
// 使用本地类型（而非直接引用 orchestrator 的类型）以避免循环导入。
// 这个结构体是将审查结果导出为 Markdown 文件的数据载体。
type DebateResultForMarkdown struct {
	PRNumber         string                   // PR 编号或标签（如 "Local Changes"）
	Analysis         string                   // 分析文本
	FinalConclusion  string                   // 最终结论
	Messages         []MessageForMarkdown     // 辩论消息列表
	Summaries        []SummaryForMarkdown     // 审查者摘要列表
	TokenUsage       []orchestrator.TokenUsage // Token 使用量统计
	ConvergedAtRound *int                     // 收敛轮次（nil 表示未收敛）
	ParsedIssues     []MergedIssueForMarkdown // 结构化问题列表
}

// FormatMarkdownFromResult 从 DebateResultForMarkdown 生成完整的 Markdown 审查报告。
// 报告包含以下章节：标题、分析、辩论记录、审查者摘要、最终结论、问题列表和 Token 用量。
// 对于本地变更（非 PR）会使用不同的标题格式。
func FormatMarkdownFromResult(r *DebateResultForMarkdown) string {
	var b strings.Builder

	isLocal := r.PRNumber == "Local Changes" || strings.HasPrefix(r.PRNumber, "Last Commit")
	if isLocal {
		fmt.Fprintf(&b, "# %s Review\n\n", r.PRNumber)
	} else {
		fmt.Fprintf(&b, "# Code Review: %s\n\n", r.PRNumber)
	}

	fmt.Fprintf(&b, "## Analysis\n\n%s\n\n", r.Analysis)

	// Debate rounds
	fmt.Fprintf(&b, "## Debate\n\n")
	for _, msg := range r.Messages {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", msg.ReviewerID, msg.Content)
	}

	// Summaries
	if len(r.Summaries) > 0 {
		fmt.Fprintf(&b, "## Summaries\n\n")
		for _, s := range r.Summaries {
			fmt.Fprintf(&b, "### %s\n\n%s\n\n", s.ReviewerID, s.Summary)
		}
	}

	// Final conclusion
	fmt.Fprintf(&b, "## Final Conclusion\n\n%s\n\n", r.FinalConclusion)

	// Issues table
	if len(r.ParsedIssues) > 0 {
		fmt.Fprintf(&b, "## Issues (%d)\n\n", len(r.ParsedIssues))
		for i, issue := range r.ParsedIssues {
			location := issue.File
			if issue.Line > 0 {
				location = fmt.Sprintf("%s:%d", issue.File, issue.Line)
			}
			fmt.Fprintf(&b, "%d. **[%s]** %s\n", i+1, strings.ToUpper(issue.Severity), issue.Title)
			fmt.Fprintf(&b, "   - Location: `%s`\n", location)
			fmt.Fprintf(&b, "   - Found by: %s\n", strings.Join(issue.RaisedBy, ", "))
			if issue.SuggestedFix != "" {
				fmt.Fprintf(&b, "   - Fix: %s\n", issue.SuggestedFix)
			}
			fmt.Fprintf(&b, "\n")
		}
	}

	// Token usage
	if len(r.TokenUsage) > 0 {
		fmt.Fprintf(&b, "## Token Usage\n\n")
		fmt.Fprintf(&b, "| Reviewer | Input | Output |\n")
		fmt.Fprintf(&b, "|----------|------:|-------:|\n")
		var totalIn, totalOut int
		for _, u := range r.TokenUsage {
			totalIn += u.InputTokens
			totalOut += u.OutputTokens
			fmt.Fprintf(&b, "| %s | %s | %s |\n", u.ReviewerID, formatNumber(u.InputTokens), formatNumber(u.OutputTokens))
		}
		fmt.Fprintf(&b, "| **Total** | **%s** | **%s** |\n\n", formatNumber(totalIn), formatNumber(totalOut))

		if r.ConvergedAtRound != nil {
			fmt.Fprintf(&b, "Converged at round %d.\n", *r.ConvergedAtRound)
		}
	}

	return b.String()
}

// FormatMarkdown 从 orchestrator 的 DebateResult 生成 Markdown 报告。
// 将 orchestrator 包的类型转换为本地 Markdown 导出类型后调用 FormatMarkdownFromResult。
// 这个函数是 orchestrator 调用 Markdown 导出的主入口。
func FormatMarkdown(result *orchestrator.DebateResult) string {
	// 将 orchestrator 类型转换为本地 Markdown 导出类型
	messages := make([]MessageForMarkdown, len(result.Messages))
	for i, m := range result.Messages {
		messages[i] = MessageForMarkdown{ReviewerID: m.ReviewerID, Content: m.Content}
	}
	summaries := make([]SummaryForMarkdown, len(result.Summaries))
	for i, s := range result.Summaries {
		summaries[i] = SummaryForMarkdown{ReviewerID: s.ReviewerID, Summary: s.Summary}
	}
	issues := make([]MergedIssueForMarkdown, len(result.ParsedIssues))
	for i, iss := range result.ParsedIssues {
		line := 0
		if iss.Line != nil {
			line = *iss.Line
		}
		issues[i] = MergedIssueForMarkdown{
			Severity:     iss.Severity,
			Title:        iss.Title,
			File:         iss.File,
			Line:         line,
			Description:  iss.Description,
			SuggestedFix: iss.SuggestedFix,
			RaisedBy:     iss.RaisedBy,
		}
	}

	r := &DebateResultForMarkdown{
		PRNumber:         result.PRNumber,
		Analysis:         result.Analysis,
		FinalConclusion:  result.FinalConclusion,
		Messages:         messages,
		Summaries:        summaries,
		TokenUsage:       result.TokenUsage,
		ConvergedAtRound: result.ConvergedAtRound,
		ParsedIssues:     issues,
	}
	return FormatMarkdownFromResult(r)
}

// RenderTerminalMarkdown 使用 glamour 库将 Markdown 文本渲染为终端友好的格式。
// 自动适配终端的颜色主题（深色/浅色），设置 120 字符的自动换行宽度。
// 如果 glamour 渲染失败（例如不支持的终端），则回退返回原始文本。
func RenderTerminalMarkdown(text string) string {
	r, err := glamour.NewTermRenderer(
		glamour.WithAutoStyle(),
		glamour.WithWordWrap(120),
	)
	if err != nil {
		return text
	}

	out, err := r.Render(text)
	if err != nil {
		return text
	}
	return out
}
