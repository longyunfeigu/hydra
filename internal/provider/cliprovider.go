package provider

import (
	"fmt"

	"github.com/google/uuid"
)

// CliSessionHelper provides shared session management for CLI-based providers.
type CliSessionHelper struct {
	sessionID    string
	firstMessage bool
	sessionName  string
}

// Start begins a new session with the given name.
func (h *CliSessionHelper) Start(name string) {
	h.sessionID = uuid.New().String()
	h.firstMessage = true
	h.sessionName = name
}

// End clears the current session.
func (h *CliSessionHelper) End() {
	h.sessionID = ""
	h.firstMessage = true
	h.sessionName = ""
}

// SessionID returns the current session ID.
func (h *CliSessionHelper) SessionID() string {
	return h.sessionID
}

// IsFirstMessage returns whether the next message is the first in the session.
func (h *CliSessionHelper) IsFirstMessage() bool {
	return h.firstMessage
}

// ShouldSendFullHistory returns true if there is no active session or this is the first message.
func (h *CliSessionHelper) ShouldSendFullHistory() bool {
	return !(h.sessionID != "" && !h.firstMessage)
}

// MarkMessageSent marks that a message has been sent in the current session.
func (h *CliSessionHelper) MarkMessageSent() {
	h.firstMessage = false
}

// BuildPrompt builds the full prompt from messages and system prompt for first call or non-session mode.
func (h *CliSessionHelper) BuildPrompt(messages []Message, systemPrompt string) string {
	var prompt string
	if h.sessionName != "" && h.firstMessage {
		prompt += fmt.Sprintf("[%s]\n\n", h.sessionName)
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
