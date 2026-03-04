package orchestrator

import (
	"fmt"
	"sort"
	"strings"
)

const maxLedgerSummaryIssues = 100

// IssueLedger tracks one reviewer's issues across rounds.
type IssueLedger struct {
	ReviewerID string
	Issues     map[string]*LedgerIssue
	nextID     int
}

// LedgerIssue is a single ledger record.
type LedgerIssue struct {
	ID           string
	Status       string // active | retracted
	Severity     string
	Category     string
	File         string
	Line         *int
	Title        string
	Description  string
	SuggestedFix string
	Round        int
}

func NewIssueLedger(reviewerID string) *IssueLedger {
	return &IssueLedger{
		ReviewerID: reviewerID,
		Issues:     make(map[string]*LedgerIssue),
		nextID:     1,
	}
}

func (l *IssueLedger) nextIssueID() string {
	id := fmt.Sprintf("I%d", l.nextID)
	l.nextID++
	return id
}

// ApplyDelta applies model output delta into ledger state.
func (l *IssueLedger) ApplyDelta(delta *StructurizeDelta, round int) {
	if l == nil || delta == nil {
		return
	}

	for _, add := range delta.Add {
		id := l.nextIssueID()
		category := strings.TrimSpace(add.Category)
		if category == "" {
			category = "general"
		}
		l.Issues[id] = &LedgerIssue{
			ID:           id,
			Status:       "active",
			Severity:     add.Severity,
			Category:     category,
			File:         add.File,
			Line:         add.Line,
			Title:        add.Title,
			Description:  add.Description,
			SuggestedFix: add.SuggestedFix,
			Round:        round,
		}
	}

	for _, id := range delta.Retract {
		issue, ok := l.Issues[id]
		if !ok {
			continue
		}
		issue.Status = "retracted"
	}

	for _, update := range delta.Update {
		issue, ok := l.Issues[update.ID]
		if !ok {
			continue
		}
		if update.Severity != nil {
			issue.Severity = *update.Severity
		}
		if update.Category != nil {
			issue.Category = *update.Category
		}
		if update.File != nil {
			issue.File = *update.File
		}
		if update.Line != nil {
			issue.Line = update.Line
		}
		if update.Title != nil {
			issue.Title = *update.Title
		}
		if update.Description != nil {
			issue.Description = *update.Description
		}
		if update.SuggestedFix != nil {
			issue.SuggestedFix = *update.SuggestedFix
		}
	}
}

// BuildSummary returns a compact markdown table of active issues.
func (l *IssueLedger) BuildSummary() string {
	if l == nil || len(l.Issues) == 0 {
		return ""
	}

	active := l.activeIssues()
	if len(active) == 0 {
		return ""
	}

	truncated := false
	total := len(active)
	if len(active) > maxLedgerSummaryIssues {
		active = active[:maxLedgerSummaryIssues]
		truncated = true
	}

	var b strings.Builder
	b.WriteString("| ID | Severity | File:Line | Title |\n")
	b.WriteString("|----|----------|-----------|-------|\n")
	for _, issue := range active {
		fileLine := issue.File
		if issue.Line != nil {
			fileLine = fmt.Sprintf("%s:%d", issue.File, *issue.Line)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
			issue.ID,
			issue.Severity,
			sanitizePipe(fileLine),
			sanitizePipe(issue.Title),
		))
	}

	if truncated {
		b.WriteString(fmt.Sprintf("(showing %d of %d active issues; truncated by severity)\n", len(active), total))
	} else {
		b.WriteString(fmt.Sprintf("(%d active issues)\n", total))
	}

	return b.String()
}

// ToMergedIssues converts active ledger issues to merged-issue format.
func (l *IssueLedger) ToMergedIssues() []MergedIssue {
	if l == nil || len(l.Issues) == 0 {
		return nil
	}

	active := l.activeIssues()
	result := make([]MergedIssue, 0, len(active))
	for _, issue := range active {
		category := issue.Category
		if strings.TrimSpace(category) == "" {
			category = "general"
		}
		result = append(result, MergedIssue{
			ReviewIssue: ReviewIssue{
				Severity:     issue.Severity,
				Category:     category,
				File:         issue.File,
				Line:         issue.Line,
				Title:        issue.Title,
				Description:  issue.Description,
				SuggestedFix: issue.SuggestedFix,
			},
			RaisedBy:     []string{l.ReviewerID},
			Descriptions: []string{issue.Description},
		})
	}
	return result
}

func (l *IssueLedger) activeIssues() []*LedgerIssue {
	active := make([]*LedgerIssue, 0, len(l.Issues))
	for _, issue := range l.Issues {
		if issue.Status == "retracted" {
			continue
		}
		active = append(active, issue)
	}

	sort.Slice(active, func(i, j int) bool {
		si := severityOrder[active[i].Severity]
		sj := severityOrder[active[j].Severity]
		if si != sj {
			return si < sj
		}
		if active[i].Round != active[j].Round {
			return active[i].Round < active[j].Round
		}
		return active[i].ID < active[j].ID
	})

	return active
}

func sanitizePipe(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
