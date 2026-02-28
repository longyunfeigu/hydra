package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/guwanhua/hydra/internal/util"
)

// ClaudeCodeProvider implements AIProvider and SessionProvider using the Claude Code CLI.
// Uses --output-format json/stream-json for structured output and session ID extraction.
type ClaudeCodeProvider struct {
	cwd             string
	timeout         time.Duration
	session         CliSessionHelper
	skipPermissions bool
}

// NewClaudeCodeProvider creates a new ClaudeCodeProvider.
func NewClaudeCodeProvider() *ClaudeCodeProvider {
	return &ClaudeCodeProvider{
		timeout:         15 * time.Minute,
		skipPermissions: true,
	}
}

// SetCwd sets the working directory for CLI invocations.
func (p *ClaudeCodeProvider) SetCwd(cwd string) {
	p.cwd = cwd
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) StartSession(name string)    { p.session.Start(name) }
func (p *ClaudeCodeProvider) EndSession()                  { p.session.End() }
func (p *ClaudeCodeProvider) SessionID() string            { return p.session.SessionID() }
func (p *ClaudeCodeProvider) IsFirstMessage() bool         { return p.session.IsFirstMessage() }
func (p *ClaudeCodeProvider) MarkMessageSent()             { p.session.MarkMessageSent() }
func (p *ClaudeCodeProvider) ShouldSendFullHistory() bool  { return p.session.ShouldSendFullHistory() }

func (p *ClaudeCodeProvider) Chat(ctx context.Context, messages []Message, systemPrompt string, _ *ChatOptions) (string, error) {
	return WithRetry(func() (string, error) {
		snap := p.session.Snapshot()
		var prompt string
		if snap.ShouldSendFull() {
			prompt = p.session.BuildPrompt(messages, systemPrompt)
		} else {
			prompt = p.session.BuildPromptLastOnly(messages)
		}
		result, err := p.runClaude(ctx, prompt, systemPrompt, snap)
		if err != nil {
			return "", err
		}
		p.session.MarkMessageSent()
		return result, nil
	}, nil)
}

func (p *ClaudeCodeProvider) ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error) {
	snap := p.session.Snapshot()
	var prompt string
	if snap.ShouldSendFull() {
		prompt = p.session.BuildPrompt(messages, systemPrompt)
	} else {
		prompt = p.session.BuildPromptLastOnly(messages)
	}

	chunks := make(chan string, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)

		err := p.runClaudeStream(ctx, prompt, systemPrompt, snap, chunks)
		if err != nil {
			errs <- err
			return
		}
		p.session.MarkMessageSent()
	}()

	return chunks, errs
}

// buildArgs constructs CLI arguments for the claude command.
// Uses a SessionSnapshot for atomic reads of correlated session state.
func (p *ClaudeCodeProvider) buildArgs(systemPrompt string, streaming bool, snap SessionSnapshot) []string {
	args := []string{"-p", "-"}

	if p.skipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}

	if streaming {
		args = append(args, "--output-format", "stream-json")
	} else {
		args = append(args, "--output-format", "json")
	}

	if snap.ID != "" && !snap.FirstMessage {
		// Resume existing session (system prompt remembered from first call)
		args = append(args, "--resume", snap.ID)
	} else if systemPrompt != "" {
		// First message or no session: pass system prompt
		args = append(args, "--system-prompt", systemPrompt)
	}

	return args
}

// filterClaudeEnv returns os.Environ() with CLAUDECODE removed, so nested
// claude processes don't fail with "cannot launch inside another session".
func filterClaudeEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			env = append(env, e)
		}
	}
	return env
}

// Claude CLI JSON event types.
//
// --output-format json: JSON array  [system, assistant, result]
// --output-format stream-json: JSONL (one event per line)
//
// assistant event: {"type":"assistant","message":{"content":[{"text":"..."}]},"session_id":"..."}
// result event:    {"type":"result","subtype":"success","result":"...","session_id":"..."}
type claudeEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	SessionID string         `json:"session_id,omitempty"`
	Result    string         `json:"result,omitempty"`
	IsError   bool           `json:"is_error,omitempty"`
	Message   *claudeMessage `json:"message,omitempty"`
}

type claudeMessage struct {
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// runClaude executes claude CLI with --output-format json (non-streaming).
// The output is a JSON array: [system_event, assistant_event, result_event].
func (p *ClaudeCodeProvider) runClaude(ctx context.Context, prompt, systemPrompt string, snap SessionSnapshot) (string, error) {
	args := p.buildArgs(systemPrompt, false, snap)
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = filterClaudeEnv()
	if p.cwd != "" {
		cmd.Dir = p.cwd
	}

	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude CLI failed: %w: %s", err, stderr.String())
	}

	// Parse JSON array output
	var events []claudeEvent
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		util.Warnf("claude CLI output is not valid JSON array, falling back to plain text: %v", err)
		return strings.TrimSpace(stdout.String()), nil
	}

	// Extract session ID and result from events
	for _, ev := range events {
		if ev.SessionID != "" {
			p.session.SetSessionID(ev.SessionID)
		}
		if ev.Type == "result" {
			if ev.IsError {
				return "", fmt.Errorf("claude returned error: %s", ev.Result)
			}
			return ev.Result, nil
		}
	}

	// Fallback: extract text from assistant event
	for _, ev := range events {
		if ev.Type == "assistant" && ev.Message != nil {
			return extractTextFromContent(ev.Message.Content), nil
		}
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runClaudeStream executes claude CLI with --output-format stream-json.
// Output is JSONL: one event per line, parsed as they arrive.
func (p *ClaudeCodeProvider) runClaudeStream(ctx context.Context, prompt, systemPrompt string, snap SessionSnapshot, chunks chan<- string) error {
	args := p.buildArgs(systemPrompt, true, snap)
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = filterClaudeEnv()
	if p.cwd != "" {
		cmd.Dir = p.cwd
	}

	cmd.Stdin = strings.NewReader(prompt)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start claude CLI: %w", err)
	}

	var mu sync.Mutex
	lastActivity := time.Now()

	updateActivity := func() {
		mu.Lock()
		lastActivity = time.Now()
		mu.Unlock()
	}

	// Inactivity timeout goroutine
	done := make(chan struct{})
	defer close(done)

	if p.timeout > 0 {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					mu.Lock()
					elapsed := time.Since(lastActivity)
					mu.Unlock()
					if elapsed > p.timeout {
						cmd.Process.Kill()
						return
					}
				}
			}
		}()
	}

	// Read stderr in background to track activity
	go func() {
		buf := make([]byte, 4096)
		for {
			_, readErr := stderrPipe.Read(buf)
			if readErr != nil {
				return
			}
			updateActivity()
		}
	}()

	// Read stdout with JSONL line buffering
	var lineBuf string
	buf := make([]byte, 4096)
	for {
		n, readErr := stdoutPipe.Read(buf)
		if n > 0 {
			updateActivity()
			lineBuf += string(buf[:n])

			for {
				idx := strings.Index(lineBuf, "\n")
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(lineBuf[:idx])
				lineBuf = lineBuf[idx+1:]

				if line == "" {
					continue
				}

				p.handleStreamEvent(line, chunks)
			}
		}
		if readErr != nil {
			break
		}
	}

	// Process remaining buffer
	if trimmed := strings.TrimSpace(lineBuf); trimmed != "" {
		p.handleStreamEvent(trimmed, chunks)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude CLI exited with error: %w", err)
	}
	return nil
}

// handleStreamEvent parses a single stream-json line and dispatches accordingly.
func (p *ClaudeCodeProvider) handleStreamEvent(line string, chunks chan<- string) {
	var event claudeEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		util.Warnf("claude stream: failed to parse JSONL line: %v", err)
		return
	}

	// Always capture session ID from any event that has one
	if event.SessionID != "" {
		p.session.SetSessionID(event.SessionID)
	}

	switch event.Type {
	case "assistant":
		// Complete assistant message — extract text and send as chunk
		if event.Message != nil {
			if text := extractTextFromContent(event.Message.Content); text != "" {
				chunks <- text
			}
		}
	case "result":
		// Final result — session_id already captured above.
		// Don't re-send text since it was already sent via assistant event.
		if event.IsError {
			util.Warnf("claude stream returned error result: %s", event.Result)
		}
	}
}

// extractTextFromContent joins all text blocks from a Claude message content array.
func extractTextFromContent(content []claudeContent) string {
	var parts []string
	for _, c := range content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}
