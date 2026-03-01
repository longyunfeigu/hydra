package provider

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestClaudeCodeProvider_Smoke verifies that the claude CLI binary is available
// and can respond to a simple prompt via the ClaudeCodeProvider.
func TestClaudeCodeProvider_Smoke(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not found in PATH, skipping smoke test")
	}

	p := NewClaudeCodeProvider()
	p.skipPermissions = true

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	msgs := []Message{{Role: "user", Content: "Reply with exactly: HYDRA_SMOKE_OK"}}
	resp, err := p.Chat(ctx, msgs, "You are a test assistant. Follow instructions exactly.", nil)
	if err != nil {
		t.Fatalf("claude-code Chat failed: %v", err)
	}
	if resp == "" {
		t.Fatal("claude-code returned empty response")
	}
	t.Logf("claude-code response: %.200s", resp)
}

// TestCodexCliProvider_Smoke verifies that the codex CLI binary is available
// and can respond to a simple prompt via the CodexCliProvider.
func TestCodexCliProvider_Smoke(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex CLI not found in PATH, skipping smoke test")
	}

	p := NewCodexCliProvider()
	p.skipPermissions = true

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	msgs := []Message{{Role: "user", Content: "Reply with exactly: HYDRA_SMOKE_OK"}}
	resp, err := p.Chat(ctx, msgs, "You are a test assistant. Follow instructions exactly.", nil)
	if err != nil {
		t.Fatalf("codex-cli Chat failed: %v", err)
	}
	if resp == "" {
		t.Fatal("codex-cli returned empty response")
	}
	t.Logf("codex-cli response: %.200s", resp)
}
