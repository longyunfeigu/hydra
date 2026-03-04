package display

import (
	"log/slog"

	"github.com/guwanhua/hydra/internal/orchestrator"
)

// NoopDisplay 实现 orchestrator.DisplayCallbacks 接口。
// 用于 Server 模式，通过 logger 记录关键事件，不输出终端 UI。
type NoopDisplay struct {
	Logger *slog.Logger
}

// NewNoopDisplay 创建一个 NoopDisplay 实例。
func NewNoopDisplay(logger *slog.Logger) *NoopDisplay {
	return &NoopDisplay{Logger: logger}
}

func (d *NoopDisplay) OnWaiting(reviewerID string) {
	d.Logger.Info("waiting", "reviewer", reviewerID)
}

func (d *NoopDisplay) OnMessage(reviewerID string, content string) {
	truncated := content
	if len(truncated) > 200 {
		truncated = truncated[:200] + "..."
	}
	d.Logger.Debug("message", "reviewer", reviewerID, "content", truncated)
}

func (d *NoopDisplay) OnParallelStatus(round int, statuses []orchestrator.ReviewerStatus) {
	d.Logger.Info("parallel status", "round", round, "reviewers", len(statuses))
}

func (d *NoopDisplay) OnRoundComplete(round int, converged bool) {
	d.Logger.Info("round complete", "round", round, "converged", converged)
}

func (d *NoopDisplay) OnConvergenceJudgment(verdict string, reasoning string) {
	d.Logger.Info("convergence judgment", "verdict", verdict)
}

func (d *NoopDisplay) OnContextGathered(ctx *orchestrator.GatheredContext) {
	modules := 0
	prs := 0
	if ctx != nil {
		modules = len(ctx.AffectedModules)
		prs = len(ctx.RelatedPRs)
	}
	d.Logger.Info("context gathered", "modules", modules, "related_prs", prs)
}
