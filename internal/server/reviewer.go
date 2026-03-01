package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/display"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/provider"
)

// RunServerReview 执行 server 模式下的审查流程。
// 镜像 cmd/review.go:runReview，但不依赖 CLI 交互。
func RunServerReview(ctx context.Context, event *MergeRequestEvent,
	plat platform.Platform, cfg *config.HydraConfig, logger *log.Logger) error {

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

	// 构建 review prompt（格式同 cmd/review.go）
	prompt := fmt.Sprintf(
		"Please review %s.\n\nTitle: %s\n\nDescription:\n%s\n\nHere is the full diff:\n\n```diff\n%s```\n\nAnalyze these changes and provide your feedback. You already have the complete diff above — do NOT attempt to fetch it again.",
		mrURL, mrInfo.Title, mrInfo.Description, mrDiff,
	)

	// 创建 reviewers
	allIDs := make([]string, 0, len(cfg.Reviewers))
	for id := range cfg.Reviewers {
		allIDs = append(allIDs, id)
	}

	reviewers := make([]orchestrator.Reviewer, 0, len(allIDs))
	for _, id := range allIDs {
		rc := cfg.Reviewers[id]
		p, err := provider.CreateProvider(rc.Model, rc.ModelName, cfg)
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
	analyzerProvider, err := provider.CreateProvider(cfg.Analyzer.Model, cfg.Analyzer.ModelName, cfg)
	if err != nil {
		return fmt.Errorf("failed to create analyzer provider: %w", err)
	}
	analyzer := orchestrator.Reviewer{
		ID:           "analyzer",
		Provider:     analyzerProvider,
		SystemPrompt: cfg.Analyzer.Prompt,
	}

	// 创建 summarizer
	summarizerProvider, err := provider.CreateProvider(cfg.Summarizer.Model, cfg.Summarizer.ModelName, cfg)
	if err != nil {
		return fmt.Errorf("failed to create summarizer provider: %w", err)
	}
	summarizer := orchestrator.Reviewer{
		ID:           "summarizer",
		Provider:     summarizerProvider,
		SystemPrompt: cfg.Summarizer.Prompt,
	}

	maxRounds := cfg.Defaults.MaxRounds
	isSolo := len(reviewers) == 1
	if isSolo {
		maxRounds = 1
	}
	checkConvergence := !isSolo && cfg.Defaults.CheckConvergence

	// 创建 orchestrator（ContextGatherer 设为 nil，server 模式无本地文件系统）
	oCfg := orchestrator.OrchestratorConfig{
		Reviewers:       reviewers,
		Analyzer:        analyzer,
		Summarizer:      summarizer,
		ContextGatherer: nil,
		Options: orchestrator.OrchestratorOptions{
			MaxRounds:        maxRounds,
			CheckConvergence: checkConvergence,
		},
	}

	orch := orchestrator.New(oCfg)
	noopDisplay := display.NewNoopDisplay(logger)

	label := fmt.Sprintf("MR !%s", mrID)
	result, err := orch.RunStreaming(ctx, label, prompt, noopDisplay)
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

	// 发布 summary note
	if result.FinalConclusion != "" {
		if noter, ok := plat.(interface {
			PostNote(mrID, repo, body string) error
		}); ok {
			summaryBody := "## Hydra Code Review Summary\n\n" + result.FinalConclusion
			if err := noter.PostNote(mrID, repo, summaryBody); err != nil {
				logger.Printf("[review] failed to post summary note: %v", err)
			} else {
				logger.Printf("[review] summary note posted to MR !%s", mrID)
			}
		}
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
