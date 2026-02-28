package orchestrator

import (
	"time"

	"github.com/guwanhua/hydra/internal/provider"
)

// Reviewer wraps an AI provider with its identity and system prompt.
type Reviewer struct {
	ID           string
	Provider     provider.AIProvider
	SystemPrompt string
}

// DebateMessage is a single message in the debate conversation.
type DebateMessage struct {
	ReviewerID string    `json:"reviewerId"`
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
}

// DebateSummary holds a reviewer's final summary.
type DebateSummary struct {
	ReviewerID string `json:"reviewerId"`
	Summary    string `json:"summary"`
}

// TokenUsage tracks estimated token consumption per reviewer.
type TokenUsage struct {
	ReviewerID    string  `json:"reviewerId"`
	InputTokens   int     `json:"inputTokens"`
	OutputTokens  int     `json:"outputTokens"`
	EstimatedCost float64 `json:"estimatedCost"`
}

// ReviewerStatus tracks a reviewer's progress during parallel execution.
type ReviewerStatus struct {
	ReviewerID string
	Status     string  // "pending", "thinking", "done"
	StartTime  int64   // unix ms
	EndTime    int64   // unix ms
	Duration   float64 // seconds
}

// OrchestratorOptions controls debate behavior.
type OrchestratorOptions struct {
	MaxRounds        int
	CheckConvergence bool
}

// OrchestratorConfig holds all configuration for creating a DebateOrchestrator.
type OrchestratorConfig struct {
	Reviewers       []Reviewer
	Analyzer        Reviewer
	Summarizer      Reviewer
	ContextGatherer ContextGathererInterface // nil if disabled
	Options         OrchestratorOptions
}

// ContextGathererInterface abstracts context gathering to avoid circular imports.
type ContextGathererInterface interface {
	Gather(diff, prNumber, baseBranch string) (*GatheredContext, error)
}

// GatheredContext holds gathered context data.
type GatheredContext struct {
	Summary         string
	RawReferences   []RawReference
	AffectedModules []AffectedModule
	RelatedPRs      []RelatedPR
}

// AffectedModule represents a code module impacted by the changes.
type AffectedModule struct {
	Name          string
	Path          string
	AffectedFiles []string
	ImpactLevel   string // "core", "moderate", "peripheral"
}

// RelatedPR represents a previously merged PR relevant to the current changes.
type RelatedPR struct {
	Number int
	Title  string
}

// RawReference describes where a symbol is referenced in the codebase.
type RawReference struct {
	Symbol       string
	FoundInFiles []ReferenceLocation
}

// ReferenceLocation is a single reference location.
type ReferenceLocation struct {
	File    string
	Line    int
	Content string
}

// DebateResult is the final output of the debate orchestration.
type DebateResult struct {
	PRNumber         string           `json:"prNumber"`
	Analysis         string           `json:"analysis"`
	Context          *GatheredContext  `json:"context,omitempty"`
	Messages         []DebateMessage  `json:"messages"`
	Summaries        []DebateSummary  `json:"summaries"`
	FinalConclusion  string           `json:"finalConclusion"`
	TokenUsage       []TokenUsage     `json:"tokenUsage"`
	ConvergedAtRound *int             `json:"convergedAtRound,omitempty"`
	ParsedIssues     []MergedIssue    `json:"parsedIssues,omitempty"`
}

// ReviewIssue is a single structured issue from a reviewer.
type ReviewIssue struct {
	Severity     string   `json:"severity"`
	Category     string   `json:"category"`
	File         string   `json:"file"`
	Line         *int     `json:"line,omitempty"`
	EndLine      *int     `json:"endLine,omitempty"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	SuggestedFix string   `json:"suggestedFix,omitempty"`
	CodeSnippet  string   `json:"codeSnippet,omitempty"`
	RaisedBy     []string `json:"raisedBy,omitempty"`
}

// MergedIssue is a deduplicated issue with attribution from multiple reviewers.
type MergedIssue struct {
	ReviewIssue
	RaisedBy     []string `json:"raisedBy"`
	Descriptions []string `json:"descriptions"`
}

// ReviewerOutput is the parsed structured output from a reviewer's response.
type ReviewerOutput struct {
	Issues  []ReviewIssue `json:"issues"`
	Verdict string        `json:"verdict"`
	Summary string        `json:"summary"`
}

// DisplayCallbacks is the interface for terminal display integration.
// The orchestrator calls these during execution to update the UI.
type DisplayCallbacks interface {
	OnWaiting(reviewerID string)
	OnMessage(reviewerID string, content string)
	OnParallelStatus(round int, statuses []ReviewerStatus)
	OnRoundComplete(round int, converged bool)
	OnConvergenceJudgment(verdict string, reasoning string)
	OnContextGathered(ctx *GatheredContext)
}
