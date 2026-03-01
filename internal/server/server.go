package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/platform/gitlab"
)

// ServerConfig 是 Webhook Server 的配置。
type ServerConfig struct {
	HydraConfig   *config.HydraConfig
	Addr          string // 监听地址，如 ":8080"
	WebhookSecret string // GitLab webhook 验证密钥
	MaxConcurrent int    // 最大并发 review 数
	GitLabHost    string // GitLab 域名
}

// Server 是 Hydra 的 Webhook HTTP Server。
type Server struct {
	cfg      ServerConfig
	sem      chan struct{} // 并发信号量
	inFlight sync.Map     // "projectPath/mrIID" → bool，去重
	logger   *log.Logger
	server   *http.Server
}

// New 创建一个新的 Server 实例。
func New(cfg ServerConfig) *Server {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	logger := log.New(os.Stdout, "[hydra] ", log.LstdFlags)
	return &Server{
		cfg:    cfg,
		sem:    make(chan struct{}, cfg.MaxConcurrent),
		logger: logger,
	}
}

// Start 启动 HTTP Server。
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook/gitlab", s.handleWebhook)
	mux.HandleFunc("/health", s.handleHealth)

	s.server = &http.Server{
		Addr:    s.cfg.Addr,
		Handler: mux,
	}

	s.logger.Printf("starting server on %s", s.cfg.Addr)
	return s.server.ListenAndServe()
}

// Shutdown 优雅关闭 Server，等待 in-flight review 完成。
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Printf("shutting down server...")
	return s.server.Shutdown(ctx)
}

// handleHealth 处理健康检查请求。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleWebhook 处理 GitLab webhook 请求。
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 验证 webhook secret
	if !ValidateWebhookRequest(r, s.cfg.WebhookSecret) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 解析事件
	event, err := ParseWebhookEvent(r)
	if err != nil {
		s.logger.Printf("[error] failed to parse webhook: %v", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// 检查是否应触发审查
	if !ShouldTriggerReview(event) {
		s.logger.Printf("[skip] event not eligible: kind=%s action=%s state=%s title=%q",
			event.ObjectKind, event.ObjectAttributes.Action, event.ObjectAttributes.State, event.ObjectAttributes.Title)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "skipped"})
		return
	}

	// 立即返回 202 Accepted，异步执行 review
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})

	go s.triggerReview(event)
}

// triggerReview 异步执行审查流程，含去重和并发控制。
func (s *Server) triggerReview(event *MergeRequestEvent) {
	key := fmt.Sprintf("%s/%d", event.Project.PathWithNamespace, event.ObjectAttributes.IID)

	// 去重：同一 MR 正在 review 时跳过
	if _, loaded := s.inFlight.LoadOrStore(key, true); loaded {
		s.logger.Printf("[skip] MR %s already in review", key)
		return
	}
	defer s.inFlight.Delete(key)

	// 信号量：限制并发数
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		s.logger.Printf("[skip] max concurrent reviews reached, dropping %s", key)
		return
	}

	s.logger.Printf("[trigger] starting review for %s", key)

	plat := gitlab.New(s.cfg.GitLabHost)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := RunServerReview(ctx, event, plat, s.cfg.HydraConfig, s.logger); err != nil {
		s.logger.Printf("[error] %s: %v", key, err)
	} else {
		s.logger.Printf("[done] review completed for %s", key)
	}
}
