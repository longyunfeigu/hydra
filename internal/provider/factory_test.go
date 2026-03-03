package provider

import (
	"context"
	"testing"

	"github.com/guwanhua/hydra/internal/config"
)

func TestCreateProvider_ClaudeCode(t *testing.T) {
	cfg := &config.HydraConfig{}
	p, err := CreateProvider("claude-code", "", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*ClaudeCodeProvider); !ok {
		t.Errorf("expected *ClaudeCodeProvider, got %T", p)
	}
	if p.Name() != "claude-code" {
		t.Errorf("Name() = %q, want %q", p.Name(), "claude-code")
	}
}

func TestCreateProvider_CodexCli(t *testing.T) {
	cfg := &config.HydraConfig{}
	p, err := CreateProvider("codex-cli", "", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*CodexCliProvider); !ok {
		t.Errorf("expected *CodexCliProvider, got %T", p)
	}
	if p.Name() != "codex-cli" {
		t.Errorf("Name() = %q, want %q", p.Name(), "codex-cli")
	}
}

func TestCreateProvider_Mock(t *testing.T) {
	cfg := &config.HydraConfig{}
	p, err := CreateProvider("mock", "", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*MockProvider); !ok {
		t.Errorf("expected *MockProvider, got %T", p)
	}
	if p.Name() != "mock" {
		t.Errorf("Name() = %q, want %q", p.Name(), "mock")
	}
}

func TestCreateProvider_MockPrefix(t *testing.T) {
	cfg := &config.HydraConfig{}
	p, err := CreateProvider("mock-test", "", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*MockProvider); !ok {
		t.Errorf("expected *MockProvider, got %T", p)
	}
}

func TestCreateProvider_GlobalMock(t *testing.T) {
	cfg := &config.HydraConfig{Mock: true}
	p, err := CreateProvider("claude-code", "", "", cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := p.(*MockProvider); !ok {
		t.Errorf("expected *MockProvider when Mock=true, got %T", p)
	}
}

func TestCreateProvider_Unknown(t *testing.T) {
	cfg := &config.HydraConfig{}
	_, err := CreateProvider("unknown-model", "", "", cfg)
	if err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
}

func TestMockProvider_Chat(t *testing.T) {
	p := NewMockProvider()
	ctx := context.Background()
	msgs := []Message{{Role: "user", Content: "hello"}}

	expected := []string{
		"This is a mock response for testing.",
		"Mock review: The code looks good overall.",
		"Mock summary: No critical issues found.",
		"This is a mock response for testing.", // wraps around
	}

	for i, want := range expected {
		got, err := p.Chat(ctx, msgs, "", nil)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if got != want {
			t.Errorf("call %d: got %q, want %q", i, got, want)
		}
	}
}

func TestMockProvider_ChatStream(t *testing.T) {
	p := NewMockProvider()
	ctx := context.Background()
	msgs := []Message{{Role: "user", Content: "hello"}}

	chunks, errs := p.ChatStream(ctx, msgs, "")

	var result string
	for chunk := range chunks {
		result += chunk
	}

	if err, ok := <-errs; ok && err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	want := "This is a mock response for testing."
	if result != want {
		t.Errorf("stream result = %q, want %q", result, want)
	}
}
