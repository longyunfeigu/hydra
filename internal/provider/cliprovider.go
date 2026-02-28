package provider

import (
	"fmt"
	"sync"
)

// CliSessionHelper provides shared session management for CLI-based providers.
// Session IDs are NOT generated upfront — each CLI provider extracts the real
// session/thread ID from its first response (stream-json for Claude, JSONL for Codex).
// All sessionID access is mutex-protected for safe concurrent use from streaming goroutines.
type CliSessionHelper struct {
	mu           sync.Mutex
	sessionID    string
	firstMessage bool
	sessionName  string
}

// Start begins a new session with the given name.
// Session ID starts empty and will be set by the provider after the first response.
func (h *CliSessionHelper) Start(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = ""
	h.firstMessage = true
	h.sessionName = name
}

// End clears the current session.
func (h *CliSessionHelper) End() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = ""
	h.firstMessage = true
	h.sessionName = ""
}

// SessionID returns the current session ID.
func (h *CliSessionHelper) SessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessionID
}

// SetSessionID sets the session ID (called by providers after parsing CLI response).
func (h *CliSessionHelper) SetSessionID(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionID = id
}

// IsFirstMessage returns whether the next message is the first in the session.
func (h *CliSessionHelper) IsFirstMessage() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.firstMessage
}

// ShouldSendFullHistory returns true if there is no active session or this is the first message.
func (h *CliSessionHelper) ShouldSendFullHistory() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !(h.sessionID != "" && !h.firstMessage)
}

// MarkMessageSent marks that a message has been sent in the current session.
func (h *CliSessionHelper) MarkMessageSent() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.firstMessage = false
}

// BuildPrompt builds the full prompt from messages and system prompt for first call or non-session mode.
func (h *CliSessionHelper) BuildPrompt(messages []Message, systemPrompt string) string {
	h.mu.Lock()
	name := h.sessionName
	first := h.firstMessage
	h.mu.Unlock()

	var prompt string
	if name != "" && first {
		prompt += fmt.Sprintf("[%s]\n\n", name)
	}
	if systemPrompt != "" {
		prompt += fmt.Sprintf("System: %s\n\n", systemPrompt)
	}
	for _, msg := range messages {
		prompt += fmt.Sprintf("%s: %s\n\n", msg.Role, msg.Content)
	}
	return prompt
}

// BuildPromptLastOnly returns only the last user message content for session continuation.
func (h *CliSessionHelper) BuildPromptLastOnly(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}
