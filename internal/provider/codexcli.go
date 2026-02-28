package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CodexCliProvider implements AIProvider and SessionProvider using the Codex CLI.
type CodexCliProvider struct {
	cwd            string
	timeout        time.Duration
	session        CliSessionHelper
	sessionEnabled bool
}

// NewCodexCliProvider creates a new CodexCliProvider.
func NewCodexCliProvider() *CodexCliProvider {
	return &CodexCliProvider{
		timeout: 15 * time.Minute,
	}
}

// SetCwd sets the working directory for CLI invocations.
func (p *CodexCliProvider) SetCwd(cwd string) {
	p.cwd = cwd
}

func (p *CodexCliProvider) Name() string { return "codex-cli" }

func (p *CodexCliProvider) StartSession(name string) {
	p.sessionEnabled = true
	p.session.Start(name)
	p.session.sessionID = "" // Will be set from first response's JSONL
}

func (p *CodexCliProvider) EndSession() {
	p.sessionEnabled = false
	p.session.End()
}

func (p *CodexCliProvider) SessionID() string          { return p.session.SessionID() }
func (p *CodexCliProvider) IsFirstMessage() bool       { return p.session.IsFirstMessage() }
func (p *CodexCliProvider) MarkMessageSent()           { p.session.MarkMessageSent() }
func (p *CodexCliProvider) ShouldSendFullHistory() bool { return p.session.ShouldSendFullHistory() }

func (p *CodexCliProvider) Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error) {
	return WithRetry(func() (string, error) {
		var prompt string
		if p.sessionEnabled && !p.session.ShouldSendFullHistory() {
			prompt = p.session.BuildPromptLastOnly(messages)
		} else {
			prompt = p.session.BuildPrompt(messages, systemPrompt)
		}
		result, err := p.runCodex(ctx, prompt)
		if err != nil {
			return "", err
		}
		p.session.MarkMessageSent()
		return result, nil
	}, nil)
}

func (p *CodexCliProvider) ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error) {
	var prompt string
	if p.sessionEnabled && !p.session.ShouldSendFullHistory() {
		prompt = p.session.BuildPromptLastOnly(messages)
	} else {
		prompt = p.session.BuildPrompt(messages, systemPrompt)
	}

	chunks := make(chan string, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)

		err := p.runCodexStream(ctx, prompt, chunks)
		if err != nil {
			errs <- err
			return
		}
		p.session.MarkMessageSent()
	}()

	return chunks, errs
}

func (p *CodexCliProvider) buildArgs() []string {
	baseArgs := []string{"--json", "--dangerously-bypass-approvals-and-sandbox"}
	sid := p.session.SessionID()
	if p.sessionEnabled && sid != "" {
		return append([]string{"exec", "resume", sid}, append(baseArgs, "-")...)
	}
	return append([]string{"exec"}, append(baseArgs, "-")...)
}

// codexEvent represents a JSONL event from Codex CLI output.
type codexEvent struct {
	Type     string         `json:"type"`
	ThreadID string         `json:"thread_id,omitempty"`
	Item     *codexItemData `json:"item,omitempty"`
}

type codexItemData struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// parseJsonlOutput parses JSONL output, extracting thread_id and agent_message text.
func (p *CodexCliProvider) parseJsonlOutput(output string) string {
	var text string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var event codexEvent
		if err := json.Unmarshal([]byte(trimmed), &event); err != nil {
			continue
		}
		if event.Type == "thread.started" && event.ThreadID != "" && p.sessionEnabled {
			p.session.sessionID = event.ThreadID
		} else if event.Type == "item.completed" && event.Item != nil && event.Item.Type == "agent_message" && event.Item.Text != "" {
			text += event.Item.Text
		}
	}
	return text
}

func (p *CodexCliProvider) runCodex(ctx context.Context, prompt string) (string, error) {
	args := p.buildArgs()
	cmd := exec.CommandContext(ctx, "codex", args...)
	if p.cwd != "" {
		cmd.Dir = p.cwd
	}

	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex CLI failed: %w: %s", err, stderr.String())
	}

	return p.parseJsonlOutput(stdout.String()), nil
}

func (p *CodexCliProvider) runCodexStream(ctx context.Context, prompt string, chunks chan<- string) error {
	args := p.buildArgs()
	cmd := exec.CommandContext(ctx, "codex", args...)
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
		return fmt.Errorf("failed to start codex CLI: %w", err)
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

			// Process complete lines
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

				var event codexEvent
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					continue
				}
				if event.Type == "thread.started" && event.ThreadID != "" && p.sessionEnabled {
					p.session.sessionID = event.ThreadID
				} else if event.Type == "item.completed" && event.Item != nil && event.Item.Type == "agent_message" && event.Item.Text != "" {
					chunks <- event.Item.Text
				}
			}
		}
		if readErr != nil {
			break
		}
	}

	// Process any remaining data in line buffer
	if trimmed := strings.TrimSpace(lineBuf); trimmed != "" {
		var event codexEvent
		if err := json.Unmarshal([]byte(trimmed), &event); err == nil {
			if event.Type == "thread.started" && event.ThreadID != "" && p.sessionEnabled {
				p.session.sessionID = event.ThreadID
			} else if event.Type == "item.completed" && event.Item != nil && event.Item.Type == "agent_message" && event.Item.Text != "" {
				chunks <- event.Item.Text
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("codex CLI exited with error: %w", err)
	}
	return nil
}
