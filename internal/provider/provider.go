package provider

import "context"

// Message 表示一条对话消息。
type Message struct {
	Role    string // "system" | "user" | "assistant"
	Content string
}

// ChatOptions 控制单次调用的行为选项。
type ChatOptions struct {
	DisableTools bool // 禁用 CLI 工具调用（用于纯 JSON 输出场景）
}

// AIProvider 是所有 AI 提供者的核心接口。
// Hydra 通过此接口与不同的 AI CLI（claude-code、codex-cli）交互。
type AIProvider interface {
	Name() string
	// Chat 发送消息并等待完整响应（同步）。
	Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error)
	// ChatStream 发送消息并以流式方式返回响应片段（异步）。
	// 返回两个 channel：chunks 接收文本片段，errs 接收错误。
	ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error)
}

// SessionProvider 在 AIProvider 基础上扩展了会话管理能力。
// CLI 提供者通过会话复用来避免每次调用都发送完整历史，节省 token。
type SessionProvider interface {
	AIProvider
	StartSession(name string)        // 开始新会话
	EndSession()                      // 结束当前会话
	SessionID() string                // 获取当前会话 ID（由 CLI 响应返回）
	IsFirstMessage() bool             // 是否为会话中的第一条消息
	MarkMessageSent()                 // 标记已发送一条消息
	ShouldSendFullHistory() bool      // 是否需要发送完整历史（首条消息或无会话时）
}
