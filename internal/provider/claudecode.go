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

// ClaudeCodeProvider 通过调用 Claude Code CLI 实现 AIProvider 和 SessionProvider。
//
// 核心机制：
//   - 通过 os/exec 调用 `claude` 命令，prompt 通过 stdin 传入（避免参数过长导致 E2BIG）
//   - 非流式模式使用 --output-format json，输出为 JSON 数组 [system, assistant, result]
//   - 流式模式使用 --output-format stream-json，输出为 JSONL（每行一个事件）
//   - 会话复用通过 --resume <session_id> 实现，session_id 从首次响应中提取
type ClaudeCodeProvider struct {
	cwd             string           // CLI 的工作目录
	timeout         time.Duration    // 无活动超时时间（默认 15 分钟）
	session         CliSessionHelper // 会话状态管理
	skipPermissions bool             // 是否跳过 CLI 的权限确认（非交互模式必须为 true）
}

func NewClaudeCodeProvider() *ClaudeCodeProvider {
	return &ClaudeCodeProvider{
		timeout:         15 * time.Minute,
		skipPermissions: true,
	}
}

func (p *ClaudeCodeProvider) SetCwd(cwd string) {
	p.cwd = cwd
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

// --- SessionProvider 接口实现，委托给 CliSessionHelper ---
func (p *ClaudeCodeProvider) StartSession(name string)    { p.session.Start(name) }
func (p *ClaudeCodeProvider) EndSession()                  { p.session.End() }
func (p *ClaudeCodeProvider) SessionID() string            { return p.session.SessionID() }
func (p *ClaudeCodeProvider) IsFirstMessage() bool         { return p.session.IsFirstMessage() }
func (p *ClaudeCodeProvider) MarkMessageSent()             { p.session.MarkMessageSent() }
func (p *ClaudeCodeProvider) ShouldSendFullHistory() bool  { return p.session.ShouldSendFullHistory() }

// Chat 同步调用 Claude CLI 并返回完整响应。带指数退避重试。
func (p *ClaudeCodeProvider) Chat(ctx context.Context, messages []Message, systemPrompt string, _ *ChatOptions) (string, error) {
	return WithRetry(func() (string, error) {
		// 原子读取会话状态，避免 TOCTOU 竞态
		snap := p.session.Snapshot()
		var prompt string
		if snap.ShouldSendFull() {
			prompt = p.session.BuildPrompt(messages, systemPrompt)
		} else {
			// 会话续传：只发最后一条 user 消息
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

// ChatStream 流式调用 Claude CLI，通过 channel 逐块返回响应。
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

// buildArgs 构建 claude CLI 的命令行参数。
//
// 参数说明：
//   -p -                      : 从 stdin 读取 prompt（pipe 模式）
//   --dangerously-skip-permissions : 跳过交互式权限确认（可通过配置关闭）
//   --output-format json/stream-json : 结构化输出格式
//   --resume <id>             : 复用已有会话（非首次消息时）
//   --system-prompt <prompt>  : 系统提示（仅首次消息时传入）
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
		// 续传已有会话（系统提示从首次调用中记住）
		args = append(args, "--resume", snap.ID)
	} else if systemPrompt != "" {
		// 首次消息或无会话：传入系统提示
		args = append(args, "--system-prompt", systemPrompt)
	}

	return args
}

// filterClaudeEnv 从环境变量中移除 CLAUDECODE，
// 避免子进程因 "cannot launch inside another session" 而失败。
func filterClaudeEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "CLAUDECODE=") {
			env = append(env, e)
		}
	}
	return env
}

// --- Claude CLI 的 JSON 事件类型 ---
//
// --output-format json 输出格式：JSON 数组 [system_event, assistant_event, result_event]
// --output-format stream-json 输出格式：JSONL（每行一个事件）
//
// assistant 事件: {"type":"assistant","message":{"content":[{"text":"..."}]},"session_id":"..."}
// result 事件:    {"type":"result","subtype":"success","result":"...","session_id":"..."}

type claudeEvent struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype,omitempty"`
	SessionID string         `json:"session_id,omitempty"` // 会话 ID，从响应中提取
	Result    string         `json:"result,omitempty"`     // result 事件的最终文本
	IsError   bool           `json:"is_error,omitempty"`
	Message   *claudeMessage `json:"message,omitempty"`    // assistant 事件的消息体
}

type claudeMessage struct {
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// runClaude 以非流式模式执行 claude CLI（--output-format json）。
// 输出为 JSON 数组，依次解析提取 session_id 和 result。
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

	// 解析 JSON 数组输出
	var events []claudeEvent
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		util.Warnf("claude CLI output is not valid JSON array, falling back to plain text: %v", err)
		return strings.TrimSpace(stdout.String()), nil
	}

	// 从事件中提取 session_id 和 result
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

	// 兜底：从 assistant 事件中提取文本
	for _, ev := range events {
		if ev.Type == "assistant" && ev.Message != nil {
			return extractTextFromContent(ev.Message.Content), nil
		}
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runClaudeStream 以流式模式执行 claude CLI（--output-format stream-json）。
// 输出为 JSONL（每行一个 JSON 事件），边读取边解析并发送到 chunks channel。
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

	// --- 无活动超时机制 ---
	// 如果 CLI 长时间无 stdout/stderr 输出，主动杀掉进程
	var mu sync.Mutex
	lastActivity := time.Now()

	updateActivity := func() {
		mu.Lock()
		lastActivity = time.Now()
		mu.Unlock()
	}

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

	// 后台读取 stderr（仅用于更新活动时间戳）
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

	// 逐行读取 stdout 并解析 JSONL 事件
	var lineBuf string
	buf := make([]byte, 4096)
	for {
		n, readErr := stdoutPipe.Read(buf)
		if n > 0 {
			updateActivity()
			lineBuf += string(buf[:n])

			// 处理所有完整的行
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

	// 处理缓冲区中剩余的不完整行
	if trimmed := strings.TrimSpace(lineBuf); trimmed != "" {
		p.handleStreamEvent(trimmed, chunks)
	}

	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude CLI exited with error: %w", err)
	}
	return nil
}

// handleStreamEvent 解析单行 JSONL 事件并分发处理。
func (p *ClaudeCodeProvider) handleStreamEvent(line string, chunks chan<- string) {
	var event claudeEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		util.Warnf("claude stream: failed to parse JSONL line: %v", err)
		return
	}

	// 从任何携带 session_id 的事件中捕获会话 ID
	if event.SessionID != "" {
		p.session.SetSessionID(event.SessionID)
	}

	switch event.Type {
	case "assistant":
		// 完整的 assistant 消息 — 提取文本并发送
		if event.Message != nil {
			if text := extractTextFromContent(event.Message.Content); text != "" {
				chunks <- text
			}
		}
	case "result":
		// 最终结果 — session_id 已在上面捕获
		// 不重复发送文本（已通过 assistant 事件发送）
		if event.IsError {
			util.Warnf("claude stream returned error result: %s", event.Result)
		}
	}
}

// extractTextFromContent 从 Claude 消息的 content 数组中拼接所有文本块。
func extractTextFromContent(content []claudeContent) string {
	var parts []string
	for _, c := range content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "")
}
