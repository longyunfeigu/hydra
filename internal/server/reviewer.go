package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/guwanhua/hydra/internal/display"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/review"
	"github.com/guwanhua/hydra/internal/reviewpost"
)

type reviewPlatform interface {
	platform.Named
	platform.MRMetadataProvider
	platform.IssueCommenter
	platform.HistoryProvider
}

const hydraSummaryMarker = reviewpost.HydraSummaryMarker

// RunServerReview 执行 server 模式下的审查流程。
func RunServerReview(ctx context.Context, event *MergeRequestEvent, plat reviewPlatform, runner *review.Runner, checkoutHost string, logger *slog.Logger) error {
	repo := event.Project.PathWithNamespace
	mrID := fmt.Sprintf("%d", event.ObjectAttributes.IID)

	logger.Info("starting review", "repo", repo, "mr", mrID)

	job, err := review.BuildServerMRJob(review.MRRef{
		ID:   mrID,
		Repo: repo,
		URL:  event.ObjectAttributes.URL,
	}, plat)
	if err != nil {
		return err
	}

	prepared, err := runner.Prepare(*job, review.RunOptions{
		HistoryProvider:  plat,
		CheckoutPlatform: plat.Name(),
		CheckoutHost:     checkoutHost,
	})
	if err != nil {
		return err
	}
	defer prepared.Close()

	result, err := prepared.Run(ctx, display.NewNoopDisplay(logger))
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	logger.Info("review completed", "repo", repo, "mr", mrID, "issues", len(result.ParsedIssues))

	if len(result.ParsedIssues) > 0 {
		platIssues := convertIssuesToPlatform(result.ParsedIssues)
		postResult := plat.PostIssuesAsComments(mrID, platIssues, repo)
		logger.Info("posted comments",
			"posted", postResult.Posted, "inline", postResult.Inline, "file_level", postResult.FileLevel,
			"global", postResult.Global, "failed", postResult.Failed, "skipped", postResult.Skipped)
	}

	if shouldPostServerSummary(result.FinalConclusion, plat) {
		summaryBody := buildServerSummaryNoteBody(result.FinalConclusion)
		if err := upsertServerSummaryNote(plat, mrID, repo, summaryBody); err != nil {
			logger.Error("failed to post summary note", "error", err)
		} else {
			logger.Info("summary note posted", "mr", mrID)
		}
	} else if strings.TrimSpace(result.FinalConclusion) != "" {
		logger.Info("skipped summary note, platform unsupported", "platform", plat.Name())
	}

	return nil
}

// convertIssuesToPlatform 将 MergedIssue 列表转换为平台通用的评论格式。
func convertIssuesToPlatform(issues []orchestrator.MergedIssue) []platform.IssueForComment {
	return reviewpost.ConvertIssuesToPlatform(issues)
}

func shouldPostServerSummary(finalConclusion string, plat platform.Named) bool {
	return strings.TrimSpace(finalConclusion) != "" && supportsServerSummaryPosting(plat)
}

func buildServerSummaryNoteBody(finalConclusion string) string {
	return reviewpost.BuildSummaryNoteBody(finalConclusion)
}

func upsertServerSummaryNote(plat platform.Named, mrID, repo, body string) error {
	return reviewpost.UpsertSummaryNote(plat, mrID, repo, body)
}

func supportsServerSummaryPosting(plat platform.Named) bool {
	return reviewpost.SupportsSummaryPosting(plat)
}
