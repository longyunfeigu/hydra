package orchestrator

import (
	"context"
	"testing"

	"github.com/guwanhua/hydra/internal/provider"
)

type summarySessionProvider struct {
	lastMessages []provider.Message
	response     string
}

func (p *summarySessionProvider) Name() string { return "summary-session" }

func (p *summarySessionProvider) Chat(_ context.Context, messages []provider.Message, _ string, _ *provider.ChatOptions) (string, error) {
	p.lastMessages = append([]provider.Message(nil), messages...)
	return p.response, nil
}

func (p *summarySessionProvider) ChatStream(_ context.Context, _ []provider.Message, _ string) (<-chan string, <-chan error) {
	ch := make(chan string, 1)
	errs := make(chan error, 1)
	close(ch)
	close(errs)
	return ch, errs
}

func (p *summarySessionProvider) StartSession(_ string)          {}
func (p *summarySessionProvider) EndSession()                    {}
func (p *summarySessionProvider) SessionID() string              { return "sid" }
func (p *summarySessionProvider) IsFirstMessage() bool           { return false }
func (p *summarySessionProvider) MarkMessageSent()               {}
func (p *summarySessionProvider) ShouldSendFullHistory() bool    { return false }

func TestCollectSummaries_SessionProviderEmbedsContextInSingleMessage(t *testing.T) {
	sp := &summarySessionProvider{response: "final summary"}
	other := &mockProvider{responses: []string{"other summary"}}

	o := &DebateOrchestrator{
		reviewers: []Reviewer{
			{ID: "r1", Provider: sp, SystemPrompt: "sys"},
			{ID: "r2", Provider: other, SystemPrompt: "sys"},
		},
		conversationHistory: []DebateMessage{
			{ReviewerID: "r1", Content: "R1 from r1"},
			{ReviewerID: "r2", Content: "R1 from r2"},
			{ReviewerID: "r1", Content: "R2 from r1"},
			{ReviewerID: "r2", Content: "R2 from r2"},
		},
		lastSeenIndex: map[string]int{
			"r1": 3,
			"r2": 3,
		},
		tokenUsage: make(map[string]*tokenCount),
	}

	summaries, err := o.collectSummaries(context.Background())
	if err != nil {
		t.Fatalf("collectSummaries returned error: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(summaries))
	}

	if len(sp.lastMessages) != 1 {
		t.Fatalf("session provider should receive exactly 1 merged message, got %d", len(sp.lastMessages))
	}
	content := sp.lastMessages[0].Content
	if !containsStr(content, "Latest debate context:") {
		t.Fatalf("expected merged summary prompt to include latest context, got: %s", content)
	}
	if !containsStr(content, "Please summarize your key points and conclusions") {
		t.Fatalf("expected merged summary prompt to include summary instruction, got: %s", content)
	}
	if !containsStr(content, "R2 from r2") {
		t.Fatalf("expected merged summary prompt to include latest round context, got: %s", content)
	}
}

