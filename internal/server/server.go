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

	"github.com/guwanhua/hydra/internal/checkout"
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

// inFlightEntry 表示一个正在进行的 review，支持取消和等待完成。
type inFlightEntry struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Server 是 Hydra 的 Webhook HTTP Server。
type Server struct {
	cfg           ServerConfig
	sem           chan struct{} // 并发信号量
	mu            sync.Mutex
	inFlight      map[string]*inFlightEntry // "projectPath/mrIID" → entry，取消重跑
	logger        *log.Logger
	server        *http.Server
	checkoutMgr   *checkout.Manager
	cancelCleanup context.CancelFunc
}

// New 创建一个新的 Server 实例。
func New(cfg ServerConfig) *Server {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	logger := log.New(os.Stdout, "[hydra] ", log.LstdFlags)
	var checkoutMgr *checkout.Manager
	if cfg.HydraConfig != nil {
		checkoutMgr = checkout.NewManager(cfg.HydraConfig.Checkout)
	}
	return &Server{
		cfg:         cfg,
		sem:         make(chan struct{}, cfg.MaxConcurrent),
		inFlight:    make(map[string]*inFlightEntry),
		logger:      logger,
		checkoutMgr: checkoutMgr,
	}
}

// Start 启动 HTTP Server。
func (s *Server) Start() error {
	cleanupCtx, cancel := context.WithCancel(context.Background())
	s.cancelCleanup = cancel
	if s.checkoutMgr != nil {
		s.checkoutMgr.StartCleanup(cleanupCtx)
	}

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

// Shutdown 优雅关闭 Server，取消所有进行中的 review 并等待完成。
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Printf("shutting down server...")
	s.mu.Lock()
	entries := make([]*inFlightEntry, 0, len(s.inFlight))
	for _, entry := range s.inFlight {
		entry.cancel()
		entries = append(entries, entry)
	}
	s.mu.Unlock()
	for _, entry := range entries {
		<-entry.done
	}
	if s.cancelCleanup != nil {
		s.cancelCleanup()
	}
	if s.checkoutMgr != nil {
		s.checkoutMgr.Wait()
	}
	if s.server == nil {
		return nil
	}
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

	// TODO: 暂时跳过 webhook secret 验证
	// if !ValidateWebhookRequest(r, s.cfg.WebhookSecret) {
	// 	http.Error(w, "Unauthorized", http.StatusUnauthorized)
	// 	return
	// }

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

// triggerReview 异步执行审查流程，含取消重跑和并发控制。
// 同一 MR 收到新 webhook 时，取消正在进行的 review，等待其清理完成后重新执行。
func (s *Server) triggerReview(event *MergeRequestEvent) {
	key := fmt.Sprintf("%s/%d", event.Project.PathWithNamespace, event.ObjectAttributes.IID)

	// 1. 注册自己，取消已有的 review
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	done := make(chan struct{})
	entry := &inFlightEntry{cancel: cancel, done: done}

	s.mu.Lock()
	if existing, ok := s.inFlight[key]; ok {
		s.logger.Printf("[cancel] cancelling previous review for %s", key)
		existing.cancel()
		waitCh := existing.done
		s.inFlight[key] = entry // 立即替换，保证后续新 webhook 取消的是自己
		s.mu.Unlock()
		<-waitCh // 等旧 review 清理完（worktree 释放、sem 归还）
	} else {
		s.inFlight[key] = entry
		s.mu.Unlock()
	}

	// 2. 清理（无论正常结束还是被取消）
	defer func() {
		cancel()
		close(done)
		s.mu.Lock()
		if cur, ok := s.inFlight[key]; ok && cur == entry {
			delete(s.inFlight, key)
		}
		s.mu.Unlock()
	}()

	// 3. 等待期间可能被更新的 webhook 取消了
	if ctx.Err() != nil {
		s.logger.Printf("[superseded] review for %s was superseded", key)
		return
	}

	// 4. 信号量：限制并发数。超出并发上限时阻塞等待（排队），不丢弃任务。
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.logger.Printf("[cancelled] review for %s was cancelled before slot acquired", key)
		return
	}

	// 5. 执行 review
	s.logger.Printf("[trigger] starting review for %s", key)
	plat := gitlab.New(s.cfg.GitLabHost)
	if err := RunServerReview(ctx, event, plat, s.cfg.HydraConfig, s.checkoutMgr, s.logger); err != nil {
		if ctx.Err() != nil {
			s.logger.Printf("[cancelled] review for %s was cancelled", key)
		} else {
			s.logger.Printf("[error] %s: %v", key, err)
		}
	} else {
		s.logger.Printf("[done] review completed for %s", key)
	}
}
