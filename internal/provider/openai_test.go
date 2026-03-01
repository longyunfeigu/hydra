package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildMessages(t *testing.T) {
	messages := []Message{
		{Role: "user", Content: "Hello"},
		{Role: "assistant", Content: "Hi there"},
		{Role: "user", Content: "How are you?"},
	}

	t.Run("with system prompt", func(t *testing.T) {
		msgs := buildMessages(messages, "You are helpful.")
		if len(msgs) != 4 {
			t.Fatalf("expected 4 messages, got %d", len(msgs))
		}
		if msgs[0].Role != "system" || msgs[0].Content != "You are helpful." {
			t.Errorf("first message should be system prompt, got %+v", msgs[0])
		}
		if msgs[1].Role != "user" || msgs[1].Content != "Hello" {
			t.Errorf("second message mismatch: %+v", msgs[1])
		}
	})

	t.Run("without system prompt", func(t *testing.T) {
		msgs := buildMessages(messages, "")
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(msgs))
		}
		if msgs[0].Role != "user" {
			t.Errorf("first message should be user, got %s", msgs[0].Role)
		}
	})
}

func TestOpenAIProviderName(t *testing.T) {
	p := NewOpenAIProvider("key", "gpt-4o", "")
	if p.Name() != "openai" {
		t.Errorf("expected name 'openai', got %q", p.Name())
	}
}

func TestDefaultBaseURL(t *testing.T) {
	p := NewOpenAIProvider("key", "gpt-4o", "")
	if p.baseURL != "https://api.openai.com/v1" {
		t.Errorf("expected default base URL 'https://api.openai.com/v1', got %q", p.baseURL)
	}
}

func TestCustomBaseURL(t *testing.T) {
	p := NewOpenAIProvider("key", "gpt-4o", "https://custom.api.com/")
	if p.baseURL != "https://custom.api.com" {
		t.Errorf("expected trailing slash trimmed, got %q", p.baseURL)
	}
}

func TestChatResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		// 验证请求体
		var req openaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}
		if req.Model != "gpt-4o" {
			t.Errorf("expected model gpt-4o, got %s", req.Model)
		}
		if req.Stream {
			t.Error("expected stream=false for Chat")
		}
		if len(req.Messages) != 2 {
			t.Errorf("expected 2 messages, got %d", len(req.Messages))
		}

		resp := openaiResponse{
			Choices: []openaiChoice{
				{Message: openaiMessage{Role: "assistant", Content: "Hello from GPT!"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("test-key", "gpt-4o", server.URL)
	result, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, "Be helpful.", nil)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Hello from GPT!" {
		t.Errorf("expected 'Hello from GPT!', got %q", result)
	}
}

func TestChatAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("bad-key", "gpt-4o", server.URL)
	_, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, "", nil)

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected HTTP 401 in error, got: %v", err)
	}
}

func TestChatNoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := openaiResponse{Choices: []openaiChoice{}}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", "gpt-4o", server.URL)
	_, err := p.Chat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, "", nil)

	if err == nil {
		t.Fatal("expected error for no choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' error, got: %v", err)
	}
}

func TestChatStreamResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openaiRequest
		json.NewDecoder(r.Body).Decode(&req)
		if !req.Stream {
			t.Error("expected stream=true for ChatStream")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected ResponseWriter to be a Flusher")
		}

		chunks := []string{"Hello", " from", " streaming", "!"}
		for _, c := range chunks {
			chunk := openaiStreamChunk{
				Choices: []openaiStreamChoice{
					{Delta: openaiDelta{Content: c}},
				},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	p := NewOpenAIProvider("test-key", "gpt-4o", server.URL)
	chunks, errs := p.ChatStream(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, "Be helpful.")

	var result strings.Builder
	for chunk := range chunks {
		result.WriteString(chunk)
	}

	// 检查是否有错误
	for err := range errs {
		t.Fatalf("unexpected stream error: %v", err)
	}

	if result.String() != "Hello from streaming!" {
		t.Errorf("expected 'Hello from streaming!', got %q", result.String())
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"Rate limit exceeded"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("key", "gpt-4o", server.URL)
	chunks, errs := p.ChatStream(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	}, "")

	// Drain chunks
	for range chunks {
	}

	var gotErr error
	for err := range errs {
		gotErr = err
	}

	if gotErr == nil {
		t.Fatal("expected error for HTTP 429")
	}
	if !strings.Contains(gotErr.Error(), "429") {
		t.Errorf("expected 429 in error, got: %v", gotErr)
	}
}
