package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/guwanhua/hydra/internal/checkout"
	"github.com/guwanhua/hydra/internal/config"
	appctx "github.com/guwanhua/hydra/internal/context"
	"github.com/guwanhua/hydra/internal/display"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/prompt"
	"github.com/guwanhua/hydra/internal/provider"
)

const hydraSummaryMarker = "<!-- hydra:summary -->"

// RunServerReview 执行 server 模式下的审查流程。
// 镜像 cmd/review.go:runReview，但不依赖 CLI 交互。
func RunServerReview(ctx context.Context, event *MergeRequestEvent,
	plat platform.Platform, cfg *config.HydraConfig, checkoutMgr *checkout.Manager, logger *log.Logger) error {

	repo := event.Project.PathWithNamespace
	mrID := fmt.Sprintf("%d", event.ObjectAttributes.IID)
	mrURL := event.ObjectAttributes.URL

	logger.Printf("[review] starting review for %s MR !%s", repo, mrID)

	// 获取 MR diff 和信息
	mrDiff, err := plat.GetDiff(mrID, repo)
	if err != nil {
		return fmt.Errorf("failed to get MR diff: %w", err)
	}

	mrInfo, err := plat.GetInfo(mrID, repo)
	if err != nil {
		return fmt.Errorf("failed to get MR info: %w", err)
	}

	var checkoutResult checkout.Result
	if checkoutMgr != nil {
		host := ""
		if cfg.Platform != nil {
			host = cfg.Platform.Host
		}
		checkoutResult = checkoutMgr.Checkout(checkout.Params{
			Platform: plat.Name(),
			Repo:     repo,
			MRNumber: mrID,
			Host:     host,
		})
		if checkoutResult.Available {
			defer checkoutResult.Release()
		}
	}

	// 构建 review prompt（格式同 cmd/review.go）
	annotatedDiff := platform.AnnotateDiffWithLineNumbers(mrDiff)
	reviewPrompt := prompt.MustRender("server_review.tmpl", map[string]any{
		"MRURL":        mrURL,
		"Title":        mrInfo.Title,
		"Description":  mrInfo.Description,
		"Diff":         annotatedDiff,
		"HasLocalRepo": checkoutResult.Available,
	})

	// 创建 reviewers
	allIDs := make([]string, 0, len(cfg.Reviewers))
	for id := range cfg.Reviewers {
		allIDs = append(allIDs, id)
	}

	reviewers := make([]orchestrator.Reviewer, 0, len(allIDs))
	for _, id := range allIDs {
		rc := cfg.Reviewers[id]
		p, err := provider.CreateProvider(rc.Model, rc.ModelName, rc.ReasoningEffort, cfg)
		if err != nil {
			return fmt.Errorf("failed to create provider for reviewer %s: %w", id, err)
		}
		reviewers = append(reviewers, orchestrator.Reviewer{
			ID:           id,
			Provider:     p,
			SystemPrompt: rc.Prompt,
		})
	}

	// 创建 analyzer
	analyzerProvider, err := provider.CreateProvider(cfg.Analyzer.Model, cfg.Analyzer.ModelName, cfg.Analyzer.ReasoningEffort, cfg)
	if err != nil {
		return fmt.Errorf("failed to create analyzer provider: %w", err)
	}
	analyzer := orchestrator.Reviewer{
		ID:           "analyzer",
		Provider:     analyzerProvider,
		SystemPrompt: cfg.Analyzer.Prompt,
	}

	// 创建 summarizer
	summarizerProvider, err := provider.CreateProvider(cfg.Summarizer.Model, cfg.Summarizer.ModelName, cfg.Summarizer.ReasoningEffort, cfg)
	if err != nil {
		return fmt.Errorf("failed to create summarizer provider: %w", err)
	}
	summarizer := orchestrator.Reviewer{
		ID:           "summarizer",
		Provider:     summarizerProvider,
		SystemPrompt: cfg.Summarizer.Prompt,
	}

	if checkoutResult.Available {
		for i := range reviewers {
			provider.SetCwdIfSupported(reviewers[i].Provider, checkoutResult.RepoDir)
		}
		provider.SetCwdIfSupported(analyzerProvider, checkoutResult.RepoDir)
		provider.SetCwdIfSupported(summarizerProvider, checkoutResult.RepoDir)
	}

	var contextGatherer orchestrator.ContextGathererInterface
	if checkoutResult.Available && cfg.ContextGatherer != nil && cfg.ContextGatherer.Enabled {
		contextModel := cfg.ContextGatherer.Model
		if contextModel == "" {
			contextModel = cfg.Analyzer.Model
		}
		ctxProvider, err := provider.CreateProvider(contextModel, "", "", cfg)
		if err != nil {
			logger.Printf("[warn] context gatherer provider failed: %v", err)
		} else {
			adapter := appctx.NewContextGathererAdapter(ctxProvider, cfg.ContextGatherer, plat)
			adapter.SetCwd(checkoutResult.RepoDir)
			contextGatherer = adapter
		}
	}

	maxRounds := cfg.Defaults.MaxRounds
	isSolo := len(reviewers) == 1
	if isSolo {
		maxRounds = 1
	}
	checkConvergence := !isSolo && cfg.Defaults.CheckConvergence

	// 创建 orchestrator
	oCfg := orchestrator.OrchestratorConfig{
		Reviewers:       reviewers,
		Analyzer:        analyzer,
		Summarizer:      summarizer,
		ContextGatherer: contextGatherer,
		Options: orchestrator.OrchestratorOptions{
			MaxRounds:        maxRounds,
			CheckConvergence: checkConvergence,
		},
	}

	orch := orchestrator.New(oCfg)
	noopDisplay := display.NewNoopDisplay(logger)

	label := fmt.Sprintf("MR !%s", mrID)
	result, err := orch.RunStreaming(ctx, label, reviewPrompt, noopDisplay)
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	logger.Printf("[review] review completed for %s MR !%s, %d issues found",
		repo, mrID, len(result.ParsedIssues))

	// 发布 inline comments
	if len(result.ParsedIssues) > 0 {
		platIssues := convertIssuesToPlatform(result.ParsedIssues)
		postResult := plat.PostIssuesAsComments(mrID, platIssues, repo)
		logger.Printf("[review] posted %d comments (%d inline, %d file-level, %d global, %d failed, %d skipped)",
			postResult.Posted, postResult.Inline, postResult.FileLevel, postResult.Global, postResult.Failed, postResult.Skipped)
	}

	// 发布 summary note（upsert，避免重复刷屏）
	if shouldPostServerSummary(result.FinalConclusion, plat) {
		summaryBody := buildServerSummaryNoteBody(result.FinalConclusion)
		if err := upsertServerSummaryNote(plat, mrID, repo, summaryBody); err != nil {
			logger.Printf("[review] failed to post summary note: %v", err)
		} else {
			logger.Printf("[review] summary note posted to MR !%s", mrID)
		}
	} else if strings.TrimSpace(result.FinalConclusion) != "" {
		logger.Printf("[review] skipped summary note: platform %q does not support summary posting", plat.Name())
	}

	return nil
}

// convertIssuesToPlatform 将 MergedIssue 列表转换为平台通用的评论格式。
func convertIssuesToPlatform(issues []orchestrator.MergedIssue) []platform.IssueForComment {
	result := make([]platform.IssueForComment, 0, len(issues))
	for _, issue := range issues {
		raisedBy := ""
		if len(issue.RaisedBy) > 0 {
			raisedBy = strings.Join(issue.RaisedBy, ", ")
		}
		result = append(result, platform.IssueForComment{
			File:         issue.File,
			Line:         issue.Line,
			Title:        issue.Title,
			Description:  issue.Description,
			Severity:     issue.Severity,
			SuggestedFix: issue.SuggestedFix,
			RaisedBy:     raisedBy,
		})
	}
	return result
}

func shouldPostServerSummary(finalConclusion string, plat platform.Platform) bool {
	return strings.TrimSpace(finalConclusion) != "" && supportsServerSummaryPosting(plat)
}

func buildServerSummaryNoteBody(finalConclusion string) string {
	return hydraSummaryMarker + "\n## Hydra Code Review Summary\n\n" + strings.TrimSpace(finalConclusion)
}

func upsertServerSummaryNote(plat platform.Platform, mrID, repo, body string) error {
	type summaryUpserter interface {
		UpsertSummaryNote(mrID, repo, marker, body string) error
	}
	type summaryPoster interface {
		PostNote(mrID, repo, body string) error
	}

	if upserter, ok := plat.(summaryUpserter); ok {
		return upserter.UpsertSummaryNote(mrID, repo, hydraSummaryMarker, body)
	}
	if poster, ok := plat.(summaryPoster); ok {
		return poster.PostNote(mrID, repo, body)
	}
	return fmt.Errorf("platform %q does not support summary posting", plat.Name())
}

func supportsServerSummaryPosting(plat platform.Platform) bool {
	if plat == nil {
		return false
	}
	type summaryUpserter interface {
		UpsertSummaryNote(mrID, repo, marker, body string) error
	}
	type summaryPoster interface {
		PostNote(mrID, repo, body string) error
	}
	if _, ok := plat.(summaryUpserter); ok {
		return true
	}
	if _, ok := plat.(summaryPoster); ok {
		return true
	}
	return false
}
