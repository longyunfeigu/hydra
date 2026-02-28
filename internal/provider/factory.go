package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/guwanhua/hydra/internal/config"
)

// CreateProvider creates an AIProvider based on the model name and config.
func CreateProvider(model string, cfg *config.HydraConfig) (AIProvider, error) {
	// Global mock mode: override all models to MockProvider
	if cfg.Mock {
		return NewMockProvider(), nil
	}

	switch {
	case model == "claude-code":
		return NewClaudeCodeProvider(), nil
	case model == "codex-cli":
		return NewCodexCliProvider(), nil
	case strings.HasPrefix(model, "mock"):
		return NewMockProvider(), nil
	default:
		return nil, fmt.Errorf("unknown provider model: %s", model)
	}
}

// MockProvider is a test provider that returns canned responses.
type MockProvider struct {
	mu        sync.Mutex
	responses []string
	callCount int
}

// NewMockProvider creates a MockProvider with default test responses.
func NewMockProvider() *MockProvider {
	return &MockProvider{
		responses: []string{
			"This is a mock response for testing.",
			"Mock review: The code looks good overall.",
			"Mock summary: No critical issues found.",
		},
	}
}

func (p *MockProvider) Name() string { return "mock" }

func (p *MockProvider) Chat(_ context.Context, _ []Message, _ string, _ *ChatOptions) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	idx := p.callCount % len(p.responses)
	p.callCount++
	return p.responses[idx], nil
}

func (p *MockProvider) ChatStream(_ context.Context, _ []Message, _ string) (<-chan string, <-chan error) {
	chunks := make(chan string, 1)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)

		p.mu.Lock()
		idx := p.callCount % len(p.responses)
		p.callCount++
		resp := p.responses[idx]
		p.mu.Unlock()

		chunks <- resp
	}()

	return chunks, errs
}

// SessionProvider interface methods for MockProvider.
func (p *MockProvider) StartSession(_ string)        {}
func (p *MockProvider) EndSession()                   {}
func (p *MockProvider) SessionID() string             { return "mock-session" }
func (p *MockProvider) IsFirstMessage() bool          { return true }
func (p *MockProvider) MarkMessageSent()              {}
func (p *MockProvider) ShouldSendFullHistory() bool   { return true }
