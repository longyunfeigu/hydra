package context

import (
	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/provider"
)

// ContextGathererAdapter wraps ContextGatherer to implement orchestrator.ContextGathererInterface.
type ContextGathererAdapter struct {
	inner *ContextGatherer
}

// NewContextGathererAdapter creates a new adapter that satisfies orchestrator.ContextGathererInterface.
func NewContextGathererAdapter(p provider.AIProvider, cfg *config.ContextGathererConfig) *ContextGathererAdapter {
	return &ContextGathererAdapter{
		inner: NewContextGatherer(p, cfg),
	}
}

// Gather implements orchestrator.ContextGathererInterface.
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
