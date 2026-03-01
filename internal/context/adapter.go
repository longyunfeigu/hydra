package context

import (
	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/provider"
)

// ContextGathererAdapter 使用适配器模式包装 ContextGatherer，
// 使其实现 orchestrator.ContextGathererInterface 接口。
type ContextGathererAdapter struct {
	inner *ContextGatherer
}

// NewContextGathererAdapter 创建一个满足 orchestrator.ContextGathererInterface 接口的适配器。
// plat 可为 nil（当平台检测失败或不在 PR 模式时）。
func NewContextGathererAdapter(p provider.AIProvider, cfg *config.ContextGathererConfig, plat platform.Platform) *ContextGathererAdapter {
	var historyProvider platform.HistoryProvider
	if plat != nil {
		historyProvider = plat
	}
	return &ContextGathererAdapter{
		inner: NewContextGatherer(p, cfg, historyProvider),
	}
}

// Gather 实现 orchestrator.ContextGathererInterface 接口。
func (a *ContextGathererAdapter) Gather(diff, prNumber, baseBranch string) (*orchestrator.GatheredContext, error) {
	gc, err := a.inner.Gather(diff, prNumber, baseBranch)
	if err != nil {
		return nil, err
	}
	return convertToOrchestrator(gc), nil
}

func convertToOrchestrator(gc *GatheredContext) *orchestrator.GatheredContext {
	result := &orchestrator.GatheredContext{
		Summary: gc.Summary,
	}

	for _, mod := range gc.AffectedModules {
		result.AffectedModules = append(result.AffectedModules, orchestrator.AffectedModule{
			Name:          mod.Name,
			Path:          mod.Path,
			AffectedFiles: mod.AffectedFiles,
			ImpactLevel:   mod.ImpactLevel,
		})
	}

	for _, pr := range gc.RelatedPRs {
		result.RelatedPRs = append(result.RelatedPRs, orchestrator.RelatedPR{
			Number: pr.Number,
			Title:  pr.Title,
		})
	}

	for _, ref := range gc.RawReferences {
		oRef := orchestrator.RawReference{
			Symbol: ref.Symbol,
		}
		for _, loc := range ref.FoundInFiles {
			oRef.FoundInFiles = append(oRef.FoundInFiles, orchestrator.ReferenceLocation{
				File:    loc.File,
				Line:    loc.Line,
				Content: loc.Content,
			})
		}
		result.RawReferences = append(result.RawReferences, oRef)
	}

	return result
}
