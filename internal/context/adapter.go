package context

import (
	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/provider"
)

// ContextGathererAdapter 使用适配器模式包装 ContextGatherer，
// 使其实现 orchestrator.ContextGathererInterface 接口。
// 这样做的目的是解耦 context 包和 orchestrator 包的类型依赖，
// 避免循环导入，同时让 orchestrator 可以通过接口调用上下文收集功能。
type ContextGathererAdapter struct {
	inner *ContextGatherer // 被包装的实际上下文收集器
}

// NewContextGathererAdapter 创建一个满足 orchestrator.ContextGathererInterface 接口的适配器。
// 内部会创建 ContextGatherer 实例并持有其引用。
func NewContextGathererAdapter(p provider.AIProvider, cfg *config.ContextGathererConfig) *ContextGathererAdapter {
	return &ContextGathererAdapter{
		inner: NewContextGatherer(p, cfg),
	}
}

// Gather 实现 orchestrator.ContextGathererInterface 接口。
// 委托给内部的 ContextGatherer 执行实际的上下文收集，
// 然后将本包的类型转换为 orchestrator 包的类型。
func (a *ContextGathererAdapter) Gather(diff, prNumber, baseBranch string) (*orchestrator.GatheredContext, error) {
	gc, err := a.inner.Gather(diff, prNumber, baseBranch)
	if err != nil {
		return nil, err
	}
	return convertToOrchestrator(gc), nil
}

// convertToOrchestrator 将 context 包的 GatheredContext 转换为 orchestrator 包的同名类型。
// 逐字段映射受影响模块、关联 PR 和原始引用数据。
func convertToOrchestrator(gc *GatheredContext) *orchestrator.GatheredContext {
	result := &orchestrator.GatheredContext{
		Summary: gc.Summary,
	}

	// 转换受影响模块列表
	for _, mod := range gc.AffectedModules {
		result.AffectedModules = append(result.AffectedModules, orchestrator.AffectedModule{
			Name:          mod.Name,
			Path:          mod.Path,
			AffectedFiles: mod.AffectedFiles,
			ImpactLevel:   mod.ImpactLevel,
		})
	}

	// 转换关联 PR 列表
	for _, pr := range gc.RelatedPRs {
		result.RelatedPRs = append(result.RelatedPRs, orchestrator.RelatedPR{
			Number: pr.Number,
			Title:  pr.Title,
		})
	}

	// 转换原始符号引用数据
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
