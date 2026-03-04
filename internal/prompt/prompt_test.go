package prompt

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	t.Run("renders template with data", func(t *testing.T) {
		result, err := Render("reviewer_summary.tmpl", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "summarize your key points") {
			t.Errorf("unexpected result: %s", result)
		}
	})

	t.Run("renders template with map data", func(t *testing.T) {
		result, err := Render("convergence_system.tmpl", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "strict consensus judge") {
			t.Errorf("unexpected result: %s", result)
		}
	})

	t.Run("renders structurize_issues with variables", func(t *testing.T) {
		result, err := Render("structurize_issues.tmpl", map[string]any{
			"ReviewText":  "test review",
			"ReviewerIDs": "r1, r2",
			"Schema":      `{"type": "object"}`,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "test review") {
			t.Error("missing ReviewText in output")
		}
		if !strings.Contains(result, "r1, r2") {
			t.Error("missing ReviewerIDs in output")
		}
		if !strings.Contains(result, "```json") {
			t.Error("missing JSON code fence in output")
		}
		if !strings.Contains(result, `"type": "object"`) {
			t.Error("missing Schema in output")
		}
	})

	t.Run("renders structurize_retry with validation errors", func(t *testing.T) {
		result, err := Render("structurize_retry.tmpl", map[string]any{
			"ValidationErrors": "- issues.0.severity: must be one of: critical, high",
			"ReviewText":       "test review text",
			"ReviewerIDs":      "r1, r2",
			"Schema":           `{"type": "object"}`,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "validation errors") {
			t.Error("missing validation errors header")
		}
		if !strings.Contains(result, "issues.0.severity") {
			t.Error("missing validation error details")
		}
		if !strings.Contains(result, "test review text") {
			t.Error("missing ReviewText")
		}
	})

	t.Run("renders structurize_delta with optional ledger summary", func(t *testing.T) {
		result, err := Render("structurize_delta.tmpl", map[string]any{
			"ReviewerID":    "r1",
			"Round":         2,
			"RoundContent":  "new round findings",
			"LedgerSummary": "| ID | Severity | File:Line | Title |",
			"Schema":        `{"type":"object"}`,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "reviewer \"r1\"") {
			t.Error("missing ReviewerID")
		}
		if !strings.Contains(result, "round 2") {
			t.Error("missing round")
		}
		if !strings.Contains(result, "new round findings") {
			t.Error("missing round content")
		}
		if !strings.Contains(result, "Previously tracked active issues") {
			t.Error("missing ledger summary section")
		}
	})

	t.Run("renders reviewer_first_round with all sections", func(t *testing.T) {
		result, err := Render("reviewer_first_round.tmpl", map[string]any{
			"TaskPrompt":       "Review this PR",
			"ContextSection":   "\n## System Context\nSome context\n\n",
			"FocusSection":     "\nFocus on security\n",
			"CallChainSection": "\n## Call Chain\nSome calls\n",
			"Analysis":         "Analysis result",
			"ReviewerID":       "claude",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "Review this PR") {
			t.Error("missing TaskPrompt")
		}
		if !strings.Contains(result, "System Context") {
			t.Error("missing ContextSection")
		}
		if !strings.Contains(result, "You are [claude]") {
			t.Error("missing ReviewerID")
		}
	})

	t.Run("renders convergence_check with variables", func(t *testing.T) {
		result, err := Render("convergence_check.tmpl", map[string]any{
			"ReviewerCount":   2,
			"RoundsCompleted": 3,
			"MessagesText":    "reviewer messages here",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "2 reviewers") {
			t.Error("missing ReviewerCount")
		}
		if !strings.Contains(result, "Round 3") {
			t.Error("missing RoundsCompleted")
		}
	})

	t.Run("renders final_conclusion", func(t *testing.T) {
		result, err := Render("final_conclusion.tmpl", map[string]any{
			"ReviewerCount": 3,
			"SummaryText":   "reviewer summaries here",
			"DebateText":    "debate transcript here",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "3 reviewers") {
			t.Error("missing ReviewerCount")
		}
		if !strings.Contains(result, "reviewer summaries here") {
			t.Error("missing SummaryText")
		}
		if !strings.Contains(result, "debate transcript here") {
			t.Error("missing DebateText")
		}
	})

	t.Run("renders server_review", func(t *testing.T) {
		result, err := Render("server_review.tmpl", map[string]any{
			"MRURL":        "https://gitlab.com/mr/1",
			"Title":        "Fix bug",
			"Description":  "Bug fix description",
			"Diff":         "+added line",
			"HasLocalRepo": true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(result, "https://gitlab.com/mr/1") {
			t.Error("missing MRURL")
		}
		if !strings.Contains(result, "Fix bug") {
			t.Error("missing Title")
		}
		if !strings.Contains(result, "full repository source code is available") {
			t.Error("missing local repo note")
		}
	})

	t.Run("unknown template returns error", func(t *testing.T) {
		_, err := Render("nonexistent.tmpl", nil)
		if err == nil {
			t.Error("expected error for unknown template")
		}
	})
}

func TestMustRender(t *testing.T) {
	t.Run("succeeds for valid template", func(t *testing.T) {
		result := MustRender("reviewer_summary.tmpl", nil)
		if result == "" {
			t.Error("expected non-empty result")
		}
	})

	t.Run("panics for unknown template", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic for unknown template")
			}
		}()
		MustRender("nonexistent.tmpl", nil)
	})
}
