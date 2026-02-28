package provider

import "context"

// Message represents a chat message.
type Message struct {
	Role    string // "system", "user", "assistant"
	Content string
}

// ChatOptions controls provider behavior for a single call.
type ChatOptions struct {
	DisableTools bool
}

// AIProvider is the core interface for all AI providers.
type AIProvider interface {
	Name() string
	Chat(ctx context.Context, messages []Message, systemPrompt string, opts *ChatOptions) (string, error)
	ChatStream(ctx context.Context, messages []Message, systemPrompt string) (<-chan string, <-chan error)
}

// SessionProvider extends AIProvider with session management for CLI providers.
type SessionProvider interface {
	AIProvider
	StartSession(name string)
	EndSession()
	SessionID() string
	IsFirstMessage() bool
	MarkMessageSent()
	ShouldSendFullHistory() bool
}
