package display

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/guwanhua/hydra/internal/orchestrator"
)

// MessageForMarkdown represents a debate message for markdown export.
type MessageForMarkdown struct {
	ReviewerID string
	Content    string
}

// SummaryForMarkdown represents a reviewer summary for markdown export.
type SummaryForMarkdown struct {
	ReviewerID string
	Summary    string
}

// MergedIssueForMarkdown mirrors MergedIssue for markdown export.
type MergedIssueForMarkdown struct {
	Severity     string
	Title        string
	File         string
	Line         int
	Description  string
	SuggestedFix string
	RaisedBy     []string
}

// DebateResultForMarkdown holds the fields needed to generate a markdown report.
// Uses local types to avoid circular imports with the orchestrator package.
type DebateResultForMarkdown struct {
	PRNumber         string
	Analysis         string
	FinalConclusion  string
	Messages         []MessageForMarkdown
	Summaries        []SummaryForMarkdown
	TokenUsage       []orchestrator.TokenUsage
	ConvergedAtRound *int
	ParsedIssues     []MergedIssueForMarkdown
}

// FormatMarkdownFromResult generates a markdown report from a DebateResultForMarkdown.
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

// FormatMarkdown generates a markdown report from an orchestrator DebateResult.
func FormatMarkdown(result *orchestrator.DebateResult) string {
	// Convert orchestrator types to local markdown types
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

// RenderTerminalMarkdown renders markdown text for terminal display using glamour.
// Falls back to raw text if glamour fails.
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
