package display

import (
	"log"

	"github.com/guwanhua/hydra/internal/orchestrator"
)

// NoopDisplay 实现 orchestrator.DisplayCallbacks 接口。
// 用于 Server 模式，通过 logger 记录关键事件，不输出终端 UI。
type NoopDisplay struct {
	Logger *log.Logger
}

// NewNoopDisplay 创建一个 NoopDisplay 实例。
func NewNoopDisplay(logger *log.Logger) *NoopDisplay {
	return &NoopDisplay{Logger: logger}
}

func (d *NoopDisplay) OnWaiting(reviewerID string) {
	d.Logger.Printf("[waiting] %s", reviewerID)
}

func (d *NoopDisplay) OnMessage(reviewerID string, content string) {
	truncated := content
	if len(truncated) > 200 {
		truncated = truncated[:200] + "..."
	}
	d.Logger.Printf("[message] %s: %s", reviewerID, truncated)
}

func (d *NoopDisplay) OnParallelStatus(round int, statuses []orchestrator.ReviewerStatus) {
	d.Logger.Printf("[parallel] round %d, %d reviewers", round, len(statuses))
}

func (d *NoopDisplay) OnRoundComplete(round int, converged bool) {
	d.Logger.Printf("[round] %d complete, converged=%v", round, converged)
}

func (d *NoopDisplay) OnConvergenceJudgment(verdict string, reasoning string) {
	d.Logger.Printf("[convergence] verdict=%s", verdict)
}

func (d *NoopDisplay) OnContextGathered(ctx *orchestrator.GatheredContext) {
	modules := 0
	prs := 0
	if ctx != nil {
		modules = len(ctx.AffectedModules)
		prs = len(ctx.RelatedPRs)
	}
	d.Logger.Printf("[context] %d modules, %d related PRs", modules, prs)
}
