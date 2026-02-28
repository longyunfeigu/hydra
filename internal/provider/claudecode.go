package provider

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ClaudeCodeProvider implements AIProvider and SessionProvider using the Claude Code CLI.
type ClaudeCodeProvider struct {
	cwd     string
	timeout time.Duration
	session CliSessionHelper
}

// NewClaudeCodeProvider creates a new ClaudeCodeProvider.
func NewClaudeCodeProvider() *ClaudeCodeProvider {
	return &ClaudeCodeProvider{
		timeout: 15 * time.Minute,
	}
}

// SetCwd sets the working directory for CLI invocations.
func (p *ClaudeCodeProvider) SetCwd(cwd string) {
	p.cwd = cwd
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) StartSession(name string)  { p.session.Start(name) }
func (p *ClaudeCodeProvider) EndSession()                { p.session.End() }
func (p *ClaudeCodeProvider) SessionID() string          { return p.session.SessionID() }
func (p *ClaudeCodeProvider) IsFirstMessage() bool       { return p.session.IsFirstMessage() }
func (p *ClaudeCodeProvider) MarkMessageSent()           { p.session.MarkMessageSent() }
func (p *ClaudeCodeProvider) ShouldSendFullHistory() bool { return p.session.ShouldSendFullHistory() }

func (p *ClaudeCodeProvider) Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error) {
	return WithRetry(func() (string, error) {
		var prompt string
		if p.session.ShouldSendFullHistory() {
			prompt = p.session.BuildPrompt(messages, systemPrompt)
		} else {
			prompt = p.session.BuildPromptLastOnly(messages)
		}
		result, err := p.runClaude(ctx, prompt, systemPrompt, opts)
		if err != nil {
			return "", err
		}
		p.session.MarkMessageSent()
		return result, nil
	}, nil)
}

func (p *ClaudeCodeProvider) ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error) {
	var prompt string
	if p.session.ShouldSendFullHistory() {
		prompt = p.session.BuildPrompt(messages, systemPrompt)
	} else {
		prompt = p.session.BuildPromptLastOnly(messages)
	}

	chunks := make(chan string, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)

		err := p.runClaudeStream(ctx, prompt, systemPrompt, chunks)
		if err != nil {
			errs <- err
			return
		}
		p.session.MarkMessageSent()
	}()

	return chunks, errs
}

func (p *ClaudeCodeProvider) buildArgs(systemPrompt string, opts *ChatOptions) []string {
	args := []string{"-p", "-", "--dangerously-skip-permissions"}

	if opts != nil && opts.DisableTools {
		args = append(args, "--tools", "")
	}

	sid := p.session.SessionID()
	if sid != "" {
		if p.session.IsFirstMessage() {
			args = append(args, "--session-id", sid)
			if systemPrompt != "" {
				args = append(args, "--system-prompt", systemPrompt)
			}
		} else {
			args = append(args, "--resume", sid)
		}
	}

	return args
}

func (p *ClaudeCodeProvider) runClaude(ctx context.Context, prompt, systemPrompt string, opts *ChatOptions) (string, error) {
	args := p.buildArgs(systemPrompt, opts)
	cmd := exec.CommandContext(ctx, "claude", args...)
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

	return strings.TrimSpace(stdout.String()), nil
}

func (p *ClaudeCodeProvider) runClaudeStream(ctx context.Context, prompt, systemPrompt string, chunks chan<- string) error {
	args := p.buildArgs(systemPrompt, nil)
	cmd := exec.CommandContext(ctx, "claude", args...)
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

	// Read stdout and send chunks
	buf := make([]byte, 4096)
	for {
		n, readErr := stdoutPipe.Read(buf)
		if n > 0 {
			updateActivity()
			chunks <- string(buf[:n])
		}
		if readErr != nil {
			break
		}
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude CLI exited with error: %w", err)
	}
	return nil
}
