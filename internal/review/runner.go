package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/guwanhua/hydra/internal/checkout"
	"github.com/guwanhua/hydra/internal/config"
	appctx "github.com/guwanhua/hydra/internal/context"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/provider"
	"github.com/guwanhua/hydra/internal/util"
)

// RunOptions 控制一次 review 执行的装配细节。
type RunOptions struct {
	ReviewerIDs        []string
	SkipContext        bool
	MaxRoundsOverride  int
	DisableConvergence bool
	HistoryProvider    platform.HistoryProvider
	CheckoutPlatform   string
	CheckoutHost       string
}

// PreparedRun 是完成依赖装配、可直接执行的 review 任务。
type PreparedRun struct {
	job              Job
	reviewerIDs      []string
	maxRounds        int
	checkConvergence bool
	hasContext       bool
	orchestrator     *orchestrator.DebateOrchestrator
	release          func()
}

// Job 返回本次执行的 review 任务。
func (p *PreparedRun) Job() Job {
	return p.job
}

// ReviewerIDs 返回实际参与执行的 reviewer 列表。
func (p *PreparedRun) ReviewerIDs() []string {
	return append([]string(nil), p.reviewerIDs...)
}

// MaxRounds 返回实际生效的最大轮数。
func (p *PreparedRun) MaxRounds() int {
	return p.maxRounds
}

// CheckConvergence 返回是否启用共识检测。
func (p *PreparedRun) CheckConvergence() bool {
	return p.checkConvergence
}

// HasContext 返回本次执行是否启用了上下文收集。
func (p *PreparedRun) HasContext() bool {
	return p.hasContext
}

// Run 执行完整 review。
func (p *PreparedRun) Run(ctx context.Context, display orchestrator.DisplayCallbacks) (*orchestrator.DebateResult, error) {
	return p.orchestrator.RunStreaming(ctx, p.job.Label, p.job.Prompt, display)
}

// Close 释放本次执行持有的临时资源，例如 checkout worktree。
func (p *PreparedRun) Close() {
	if p.release != nil {
		p.release()
	}
}

// Runner 负责把配置与任务装配为可执行的 review。
type Runner struct {
	cfg         *config.HydraConfig
	checkoutMgr *checkout.Manager
}

// NewRunner 创建共享 review runner。
func NewRunner(cfg *config.HydraConfig, checkoutMgr *checkout.Manager) *Runner {
	if checkoutMgr == nil && cfg != nil {
		checkoutMgr = checkout.NewManager(cfg.Checkout)
	}
	return &Runner{
		cfg:         cfg,
		checkoutMgr: checkoutMgr,
	}
}

// StartCleanup 启动 checkout 缓存清理任务。
func (r *Runner) StartCleanup(ctx context.Context) {
	if r == nil || r.checkoutMgr == nil {
		return
	}
	r.checkoutMgr.StartCleanup(ctx)
}

// Wait 等待所有活跃 checkout 释放。
func (r *Runner) Wait() {
	if r == nil || r.checkoutMgr == nil {
		return
	}
	r.checkoutMgr.Wait()
}

// Prepare 根据任务和选项装配一次可执行的 review。
func (r *Runner) Prepare(job Job, opts RunOptions) (*PreparedRun, error) {
	if r.cfg == nil {
		return nil, fmt.Errorf("review runner requires config")
	}

	selectedIDs, err := r.resolveReviewerIDs(opts.ReviewerIDs)
	if err != nil {
		return nil, err
	}

	reviewers, err := r.buildReviewers(selectedIDs)
	if err != nil {
		return nil, err
	}
	analyzer, analyzerProvider, err := r.buildSpecialReviewer("analyzer", r.cfg.Analyzer)
	if err != nil {
		return nil, err
	}
	summarizer, summarizerProvider, err := r.buildSpecialReviewer("summarizer", r.cfg.Summarizer)
	if err != nil {
		return nil, err
	}

	var (
		contextGatherer orchestrator.ContextGathererInterface
		checkoutResult  checkout.Result
		release         func()
	)

	if job.Type == "pr" && r.checkoutMgr != nil && strings.TrimSpace(job.Repo) != "" {
		checkoutResult = r.checkoutMgr.Checkout(checkout.Params{
			Platform: opts.CheckoutPlatform,
			Repo:     job.Repo,
			MRNumber: job.MRNumber,
			Host:     opts.CheckoutHost,
		})
		if checkoutResult.Available {
			release = checkoutResult.Release
		}
	}

	cwd := "."
	if checkoutResult.Available {
		cwd = checkoutResult.RepoDir
	}
	for i := range reviewers {
		provider.SetCwdIfSupported(reviewers[i].Provider, cwd)
	}
	provider.SetCwdIfSupported(analyzerProvider, cwd)
	provider.SetCwdIfSupported(summarizerProvider, cwd)

	hasContext := !opts.SkipContext && r.cfg.ContextGatherer != nil && r.cfg.ContextGatherer.Enabled
	if hasContext {
		contextModel := r.cfg.ContextGatherer.Model
		if contextModel == "" {
			contextModel = r.cfg.Analyzer.Model
		}
		contextProvider, err := provider.CreateProvider(contextModel, "", "", "", r.cfg)
		if err != nil {
			util.Warnf("Failed to create context gatherer provider: %v", err)
			hasContext = false
		} else {
			contextGatherer = appctx.NewContextGathererAdapter(contextProvider, r.cfg.ContextGatherer, opts.HistoryProvider)
			if cg, ok := contextGatherer.(interface{ SetCwd(string) }); ok {
				cg.SetCwd(cwd)
			}
		}
	}

	if checkoutResult.Available {
		job.Prompt += "\n\nNote: The full repository source code is available in your working directory.\nYou can browse files, read implementations, and examine the broader codebase context beyond the diff."
	}

	maxRounds := r.cfg.Defaults.MaxRounds
	if opts.MaxRoundsOverride > 0 {
		maxRounds = opts.MaxRoundsOverride
	}
	isSolo := len(reviewers) == 1
	if isSolo {
		maxRounds = 1
	}
	checkConvergence := !isSolo && !opts.DisableConvergence && r.cfg.Defaults.CheckConvergence

	orch := orchestrator.New(orchestrator.OrchestratorConfig{
		Reviewers:       reviewers,
		Analyzer:        analyzer,
		Summarizer:      summarizer,
		ContextGatherer: contextGatherer,
		Options: orchestrator.OrchestratorOptions{
			MaxRounds:        maxRounds,
			CheckConvergence: checkConvergence,
			Language:         r.cfg.Defaults.Language,
			StructurizeMode:  r.cfg.Defaults.StructurizeMode,
		},
	})

	return &PreparedRun{
		job:              job,
		reviewerIDs:      selectedIDs,
		maxRounds:        maxRounds,
		checkConvergence: checkConvergence,
		hasContext:       hasContext,
		orchestrator:     orch,
		release:          release,
	}, nil
}

func (r *Runner) resolveReviewerIDs(requested []string) ([]string, error) {
	if len(requested) == 0 {
		ids := make([]string, 0, len(r.cfg.Reviewers))
		for id := range r.cfg.Reviewers {
			ids = append(ids, id)
		}
		return ids, nil
	}

	ids := make([]string, 0, len(requested))
	for _, id := range requested {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if _, ok := r.cfg.Reviewers[trimmed]; !ok {
			return nil, fmt.Errorf("unknown reviewer: %s", trimmed)
		}
		ids = append(ids, trimmed)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no reviewers selected")
	}
	return ids, nil
}

func (r *Runner) buildReviewers(ids []string) ([]orchestrator.Reviewer, error) {
	reviewers := make([]orchestrator.Reviewer, 0, len(ids))
	for _, id := range ids {
		rc := r.cfg.Reviewers[id]
		p, err := provider.CreateProvider(rc.Model, rc.ModelName, rc.ReasoningEffort, rc.Provider, r.cfg)
		if err != nil {
			return nil, fmt.Errorf("failed to create provider for reviewer %s: %w", id, err)
		}
		reviewers = append(reviewers, orchestrator.Reviewer{
			ID:           id,
			Provider:     p,
			SystemPrompt: rc.Prompt,
		})
	}
	return reviewers, nil
}

func (r *Runner) buildSpecialReviewer(id string, rc config.ReviewerConfig) (orchestrator.Reviewer, provider.AIProvider, error) {
	p, err := provider.CreateProvider(rc.Model, rc.ModelName, rc.ReasoningEffort, rc.Provider, r.cfg)
	if err != nil {
		return orchestrator.Reviewer{}, nil, fmt.Errorf("failed to create %s provider: %w", id, err)
	}
	return orchestrator.Reviewer{
		ID:           id,
		Provider:     p,
		SystemPrompt: rc.Prompt,
	}, p, nil
}
