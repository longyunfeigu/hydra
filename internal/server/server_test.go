package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guwanhua/hydra/internal/config"
)

func newTestServer() *Server {
	cfg := &config.HydraConfig{
		Defaults: config.DefaultsConfig{MaxRounds: 3},
		Reviewers: map[string]config.ReviewerConfig{
			"test": {Model: "mock", Prompt: "Test reviewer"},
		},
		Analyzer:   config.ReviewerConfig{Model: "mock", Prompt: "Test analyzer"},
		Summarizer: config.ReviewerConfig{Model: "mock", Prompt: "Test summarizer"},
	}

	return New(ServerConfig{
		HydraConfig:   cfg,
		Addr:          ":0",
		WebhookSecret: "test-secret",
		MaxConcurrent: 2,
		GitLabHost:    "gitlab.com",
	})
}

func TestHealthEndpoint(t *testing.T) {
	srv := newTestServer()
	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health check status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Errorf("health status = %q, want %q", resp["status"], "ok")
	}
}

func TestWebhookUnauthorized(t *testing.T) {
	srv := newTestServer()

	body := `{"object_kind":"merge_request","object_attributes":{"action":"open","state":"opened","title":"Test"}}`
	req := httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "wrong-secret")
	w := httptest.NewRecorder()

	srv.handleWebhook(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestWebhookBadRequest(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader("not json"))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	w := httptest.NewRecorder()

	srv.handleWebhook(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestWebhookMethodNotAllowed(t *testing.T) {
	srv := newTestServer()

	req := httptest.NewRequest("GET", "/webhook/gitlab", nil)
	w := httptest.NewRecorder()

	srv.handleWebhook(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
}

func TestWebhookAccepted(t *testing.T) {
	srv := newTestServer()

	body := `{
		"object_kind": "merge_request",
		"project": {"id": 1, "path_with_namespace": "group/project", "web_url": "https://gitlab.com/group/project"},
		"object_attributes": {"iid": 1, "action": "open", "state": "opened", "title": "Test MR", "url": "https://gitlab.com/group/project/-/merge_requests/1"}
	}`
	req := httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	w := httptest.NewRecorder()

	srv.handleWebhook(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "accepted" {
		t.Errorf("response status = %q, want %q", resp["status"], "accepted")
	}
}

func TestWebhookSkippedEvent(t *testing.T) {
	srv := newTestServer()

	body := `{
		"object_kind": "merge_request",
		"project": {"id": 1, "path_with_namespace": "group/project"},
		"object_attributes": {"iid": 1, "action": "close", "state": "closed", "title": "Test MR"}
	}`
	req := httptest.NewRequest("POST", "/webhook/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	w := httptest.NewRecorder()

	srv.handleWebhook(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "skipped" {
		t.Errorf("response status = %q, want %q", resp["status"], "skipped")
	}
}

func TestDeduplication(t *testing.T) {
	srv := newTestServer()

	// 模拟一个 MR 已在 review 中
	srv.inFlight.Store("group/project/1", true)

	event := &MergeRequestEvent{
		Project:          ProjectInfo{PathWithNamespace: "group/project"},
		ObjectAttributes: MRAttributes{IID: 1},
	}

	// 触发 review — 应该被跳过（因为已有同一 MR 在 review）
	srv.triggerReview(event)

	// 验证 inFlight 仍然存在（说明 triggerReview 提前返回了）
	if _, ok := srv.inFlight.Load("group/project/1"); !ok {
		t.Error("expected inFlight entry to still exist")
	}
}
