package orchestrator

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guwanhua/hydra/internal/prompt"
	"github.com/guwanhua/hydra/internal/provider"
	"github.com/guwanhua/hydra/internal/schema"
	"github.com/guwanhua/hydra/internal/util"
	"golang.org/x/sync/errgroup"
)

// tokenCount 追踪单个审查者的输入/输出Token计数。
// 用于估算API调用成本。
type tokenCount struct {
	input  int
	output int
}

// DebateOrchestrator 运行多审查者辩论式代码审查的核心编排器。
// 它协调多个AI审查者进行多轮辩论，通过对抗性讨论发现代码中的问题。
//
// 核心流程：
//  1. 并行执行上下文收集和代码预分析
//  2. 多轮辩论：每轮所有审查者并行执行，互相挑战对方的观点
//  3. 收集总结并生成最终结论和结构化问题列表
type DebateOrchestrator struct {
	reviewers       []Reviewer               // 参与辩论的审查者列表
	analyzer        Reviewer                 // 预分析器
	summarizer      Reviewer                 // 总结器/共识判断器
	contextGatherer ContextGathererInterface // 上下文收集器

	options OrchestratorOptions // 辩论行为配置

	// RunStreaming 不再复用这些字段；单次执行状态保存在 debateRun 中。
	// 保留这些字段是为了兼容同包测试和直接调用内部 helper 的场景。
	conversationHistory []DebateMessage
	tokenUsage          map[string]*tokenCount
	analysis            string
	gatheredContext     *GatheredContext
	taskPrompt          string
	lastSeenIndex       map[string]int
	issueLedgers        map[string]*IssueLedger
	canonicalSignals    []CanonicalSignal
}

// debateRun 保存单次 review 执行期的可变状态。
type debateRun struct {
	*DebateOrchestrator
	conversationHistory []DebateMessage        // 完整的辩论对话历史
	tokenUsage          map[string]*tokenCount // 每个审查者的Token使用量
	analysis            string                 // 预分析结果
	gatheredContext     *GatheredContext       // 收集到的代码上下文
	taskPrompt          string                 // 原始任务提示词（包含diff等）
	lastSeenIndex       map[string]int         // 每个审查者最后看到的消息索引
	issueLedgers        map[string]*IssueLedger
	canonicalSignals    []CanonicalSignal

	mu sync.Mutex // 保护tokenUsage的并发访问锁
}

// New 根据给定的配置创建一个新的DebateOrchestrator实例。
// 初始化Token使用量跟踪和消息已读索引。
func New(cfg OrchestratorConfig) *DebateOrchestrator {
	return &DebateOrchestrator{
		reviewers:       cfg.Reviewers,
		analyzer:        cfg.Analyzer,
		summarizer:      cfg.Summarizer,
		contextGatherer: cfg.ContextGatherer,
		options:         cfg.Options,
		tokenUsage:      make(map[string]*tokenCount),
		lastSeenIndex:   make(map[string]int),
	}
}

func (o *DebateOrchestrator) newRun(prompt string) *debateRun {
	return &debateRun{
		DebateOrchestrator: o,
		tokenUsage:         make(map[string]*tokenCount),
		lastSeenIndex:      make(map[string]int),
		taskPrompt:         prompt,
	}
}

func (o *DebateOrchestrator) legacyRun() *debateRun {
	tokenUsage := o.tokenUsage
	if tokenUsage == nil {
		tokenUsage = make(map[string]*tokenCount)
	}
	lastSeen := o.lastSeenIndex
	if lastSeen == nil {
		lastSeen = make(map[string]int)
	}
	return &debateRun{
		DebateOrchestrator:  o,
		conversationHistory: o.conversationHistory,
		tokenUsage:          tokenUsage,
		analysis:            o.analysis,
		gatheredContext:     o.gatheredContext,
		taskPrompt:          o.taskPrompt,
		lastSeenIndex:       lastSeen,
		issueLedgers:        o.issueLedgers,
		canonicalSignals:    o.canonicalSignals,
	}
}

func (o *DebateOrchestrator) syncLegacyRun(run *debateRun) {
	o.conversationHistory = run.conversationHistory
	o.tokenUsage = run.tokenUsage
	o.analysis = run.analysis
	o.gatheredContext = run.gatheredContext
	o.taskPrompt = run.taskPrompt
	o.lastSeenIndex = run.lastSeenIndex
	o.issueLedgers = run.issueLedgers
	o.canonicalSignals = run.canonicalSignals
}

// RunStreaming 执行完整的辩论循环，支持并行审查者执行和流式输出。
// 这是Hydra的核心算法，包含三个阶段：
//
//	阶段1：并行执行上下文收集和代码预分析
//	阶段2：多轮辩论，每轮所有审查者并行执行，可选共识检测提前终止
//	阶段3：收集审查者总结，生成最终结论，提取结构化问题
//
// 参数：
//   - label: 任务标识（如PR编号），用于会话标记
//   - prompt: 包含代码diff的完整审查提示词
//   - display: 终端UI回调接口，用于实时更新显示
func (o *DebateOrchestrator) RunStreaming(ctx context.Context, label, prompt string, display DisplayCallbacks) (*DebateResult, error) {
	run := o.newRun(prompt)
	run.startSessions(label)
	defer run.endAllSessions()

	// 阶段1: 并行执行上下文收集和预分析
	if err := run.runAnalysisPhase(ctx, label, prompt, display); err != nil {
		return nil, err
	}

	// 阶段2: 多轮辩论
	convergedAtRound, err := run.runDebatePhase(ctx, display)
	if err != nil {
		return nil, err
	}

	// 阶段3: 总结 + 结论 + 问题提取
	return run.runSummaryPhase(ctx, label, display, convergedAtRound)
}

func (o *DebateOrchestrator) checkConvergence(ctx context.Context, display DisplayCallbacks) (bool, error) {
	run := o.legacyRun()
	converged, err := run.checkConvergence(ctx, display)
	o.syncLegacyRun(run)
	return converged, err
}

func (o *DebateOrchestrator) collectSummaries(ctx context.Context, display DisplayCallbacks) ([]DebateSummary, error) {
	run := o.legacyRun()
	summaries, err := run.collectSummaries(ctx, display)
	o.syncLegacyRun(run)
	return summaries, err
}

func (o *DebateOrchestrator) structurizeIssues(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	run := o.legacyRun()
	issues := run.structurizeIssues(ctx, display)
	o.syncLegacyRun(run)
	return issues
}

func (o *DebateOrchestrator) extractRoundIssueDeltas(ctx context.Context, round int, roundOutputs map[string]string, display DisplayCallbacks) {
	run := o.legacyRun()
	run.extractRoundIssueDeltas(ctx, round, roundOutputs, display)
	o.syncLegacyRun(run)
}

func (o *DebateOrchestrator) structurizeIssuesFromLedgers(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	run := o.legacyRun()
	issues := run.structurizeIssuesFromLedgers(ctx, display)
	o.syncLegacyRun(run)
	return issues
}

func (o *DebateOrchestrator) initIssueLedgers() {
	run := o.legacyRun()
	run.initIssueLedgers()
	o.syncLegacyRun(run)
}

func (o *DebateOrchestrator) mergeAllLedgers() []MergedIssue {
	run := o.legacyRun()
	issues := run.mergeAllLedgers()
	o.syncLegacyRun(run)
	return issues
}

// startSessions 为支持会话的AI提供者启动会话。
// 会话模式下，提供者可以维护对话上下文，避免重复发送完整历史。
func (o *debateRun) startSessions(label string) {
	for _, r := range o.reviewers {
		if sp, ok := r.Provider.(provider.SessionProvider); ok {
			sp.StartSession(fmt.Sprintf("Hydra | %s | reviewer:%s", label, r.ID))
		}
	}
	if sp, ok := o.analyzer.Provider.(provider.SessionProvider); ok {
		sp.StartSession(fmt.Sprintf("Hydra | %s | analyzer", label))
	}
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.StartSession(fmt.Sprintf("Hydra | %s | summarizer", label))
	}
}

// runAnalysisPhase 并行执行上下文收集和代码预分析。
// 上下文收集和预分析互不依赖，可以并行执行以减少总耗时。
func (o *debateRun) runAnalysisPhase(ctx context.Context, label, prompt string, display DisplayCallbacks) error {
	g, gctx := errgroup.WithContext(ctx)

	// 上下文收集：从代码仓库中提取与变更相关的调用链、模块关系等信息
	if o.contextGatherer != nil {
		g.Go(func() error {
			display.OnWaiting("context-gatherer")
			diff := extractDiffFromPrompt(prompt)
			gathered, err := o.contextGatherer.Gather(gctx, diff, label, "main")
			if err != nil {
				// 上下文收集失败是非致命的，不影响核心审查流程
				return nil
			}
			o.gatheredContext = gathered
			return nil
		})
	}

	// 预分析：使用分析器对代码变更进行初步分析，为审查者提供关注重点
	g.Go(func() error {
		display.OnWaiting("analyzer")
		msgs := []provider.Message{{Role: "user", Content: prompt}}

		ch, errCh := o.analyzer.Provider.ChatStream(gctx, msgs, o.analyzer.SystemPrompt)
		var sb strings.Builder
		for chunk := range ch {
			sb.WriteString(chunk)
			display.OnMessageChunk("analyzer", chunk)
		}
		if err := <-errCh; err != nil {
			return fmt.Errorf("analyzer failed: %w", err)
		}
		o.analysis = sb.String()
		display.OnMessage("analyzer", o.analysis)
		o.trackTokens("analyzer", prompt+o.analyzer.SystemPrompt, o.analysis)
		return nil
	})

	if err := g.Wait(); err != nil {
		return err
	}

	// 通知UI层上下文收集已完成，可以展示相关信息
	if o.gatheredContext != nil {
		display.OnContextGathered(o.gatheredContext)
	}

	return nil
}

// runDebatePhase 执行多轮辩论，返回达成共识的轮次（nil表示未达成）。
// 每轮辩论中，所有审查者并行执行，然后检查是否达成共识。
func (o *debateRun) runDebatePhase(ctx context.Context, display DisplayCallbacks) (*int, error) {
	var convergedAtRound *int
	if o.useLedgerStructurize() {
		o.initIssueLedgers()
	}

	for round := 1; round <= o.options.MaxRounds; round++ {
		roundOutputs, err := o.runDebateRound(ctx, round, display)
		if err != nil {
			return nil, err
		}
		if o.useLedgerStructurize() {
			o.extractRoundIssueDeltas(ctx, round, roundOutputs, display)
		}

		// 共识检测：从第1轮开始（且不是最后一轮）检查审查者是否已达成共识
		// Round 1 独立审查后也可能达成一致，无需强制进入第2轮
		converged := false
		if o.options.CheckConvergence && round >= 1 && round < o.options.MaxRounds {
			display.OnWaiting("convergence-check")
			var err error
			converged, err = o.checkConvergence(ctx, display)
			if err != nil {
				// 共识检测失败是非致命的，继续下一轮辩论
				converged = false
			}
			if converged {
				r := round
				convergedAtRound = &r
			}
		}

		display.OnRoundComplete(round, converged)

		if converged {
			break
		}
	}

	return convergedAtRound, nil
}

// runDebateRound 执行单轮辩论：构建消息、并行执行所有审查者、收集结果到对话历史。
func (o *debateRun) runDebateRound(ctx context.Context, round int, display DisplayCallbacks) (map[string]string, error) {
	// 在执行前为所有审查者构建消息（快照，确保所有审查者看到相同的信息）
	// 这样避免了先执行的审查者的输出影响后执行审查者的输入
	type reviewerTask struct {
		reviewer    Reviewer
		messages    []provider.Message
		inputText   string
		inputTokens int
	}
	tasks := make([]reviewerTask, len(o.reviewers))
	for i, r := range o.reviewers {
		messages := o.buildMessages(r.ID)
		var inputParts []string
		for _, m := range messages {
			inputParts = append(inputParts, m.Content)
		}
		inputText := strings.Join(inputParts, "\n") + r.SystemPrompt
		tasks[i] = reviewerTask{
			reviewer:    r,
			messages:    messages,
			inputText:   inputText,
			inputTokens: estimateTokens(inputText),
		}
	}

	// 初始化每个审查者的状态追踪，用于UI实时显示
	statuses := make([]ReviewerStatus, len(o.reviewers))
	var statusesMu sync.Mutex
	for i, r := range o.reviewers {
		statuses[i] = ReviewerStatus{
			ReviewerID: r.ID,
			Status:     "pending",
		}
	}

	display.OnWaiting(fmt.Sprintf("round-%d", round))
	display.OnParallelStatus(round, copyStatuses(statuses))

	// 并行执行所有审查者，每个审查者在独立的goroutine中运行
	type roundResult struct {
		reviewer     Reviewer
		fullResponse string
		inputText    string
	}
	results := make([]roundResult, len(tasks))

	rg, rgctx := errgroup.WithContext(ctx)
	for i, task := range tasks {
		i, task := i, task
		rg.Go(func() error {
			// 标记审查者状态为"思考中"，记录开始时间
			startTime := time.Now().UnixMilli()
			statusesMu.Lock()
			statuses[i] = ReviewerStatus{
				ReviewerID:  task.reviewer.ID,
				Status:      "thinking",
				StartTime:   startTime,
				InputTokens: task.inputTokens,
			}
			statusSnapshot := copyStatuses(statuses)
			statusesMu.Unlock()
			display.OnParallelStatus(round, statusSnapshot)

			// 流式接收审查者的响应，逐块读取
			ch, errCh := task.reviewer.Provider.ChatStream(rgctx, task.messages, task.reviewer.SystemPrompt)
			var sb strings.Builder
			for chunk := range ch {
				sb.WriteString(chunk)
				display.OnMessageChunk(task.reviewer.ID, chunk)
			}
			if err := <-errCh; err != nil {
				return fmt.Errorf("reviewer %s failed: %w", task.reviewer.ID, err)
			}

			fullResponse := sb.String()
			outputTokens := estimateTokens(fullResponse)

			// 标记审查者状态为"已完成"，记录结束时间和耗时
			endTime := time.Now().UnixMilli()
			statusesMu.Lock()
			statuses[i] = ReviewerStatus{
				ReviewerID:    task.reviewer.ID,
				Status:        "done",
				StartTime:     startTime,
				EndTime:       endTime,
				Duration:      float64(endTime-startTime) / 1000.0,
				InputTokens:   task.inputTokens,
				OutputTokens:  outputTokens,
				EstimatedCost: float64(task.inputTokens+outputTokens) * 0.00001,
			}
			statusSnapshot = copyStatuses(statuses)
			statusesMu.Unlock()
			display.OnParallelStatus(round, statusSnapshot)

			results[i] = roundResult{
				reviewer:     task.reviewer,
				fullResponse: fullResponse,
				inputText:    task.inputText,
			}
			return nil
		})
	}

	if err := rg.Wait(); err != nil {
		return nil, err
	}

	// 所有审查者完成后，将结果添加到对话历史并通知UI
	// 注意：必须在所有审查者完成后统一添加，保证消息顺序一致
	for _, r := range results {
		o.trackTokens(r.reviewer.ID, r.inputText, r.fullResponse)
		o.conversationHistory = append(o.conversationHistory, DebateMessage{
			ReviewerID: r.reviewer.ID,
			Round:      round,
			Content:    r.fullResponse,
			Timestamp:  time.Now(),
		})
		o.markAsSeen(r.reviewer.ID)
		display.OnMessage(r.reviewer.ID, r.fullResponse)
	}

	roundOutputs := make(map[string]string, len(results))
	for _, r := range results {
		roundOutputs[r.reviewer.ID] = r.fullResponse
	}
	return roundOutputs, nil
}

// runSummaryPhase 收集审查者总结，生成最终结论和结构化问题列表。
func (o *debateRun) runSummaryPhase(ctx context.Context, label string, display DisplayCallbacks, convergedAtRound *int) (*DebateResult, error) {
	// 辩论结束后，收集每个审查者的最终总结，然后由总结器生成统一结论
	display.OnWaiting("reviewer-summaries")
	summaries, err := o.collectSummaries(ctx, display)
	if err != nil {
		return nil, fmt.Errorf("collecting summaries: %w", err)
	}

	// 结束总结器会话，后续的 getFinalConclusion 和 structurizeIssues 都构建全新消息，
	// 不依赖会话上下文，可以安全地并行执行
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}

	var (
		finalConclusion string
		parsedIssues    []MergedIssue
	)

	display.OnWaiting("final-conclusion")
	if o.useLedgerStructurize() {
		finalConclusion, err = o.getFinalConclusion(ctx, summaries, display)
		if err != nil {
			return nil, fmt.Errorf("final conclusion: %w", err)
		}
		parsedIssues = o.structurizeIssuesFromLedgers(ctx, display)
	} else {
		finalConclusion, err = o.getFinalConclusion(ctx, summaries, display)
		if err != nil {
			return nil, fmt.Errorf("final conclusion: %w", err)
		}
		parsedIssues = o.structurizeIssuesLegacy(ctx, display)
	}

	return &DebateResult{
		PRNumber:         label,
		Analysis:         o.analysis,
		Context:          o.gatheredContext,
		Messages:         o.conversationHistory,
		Summaries:        summaries,
		FinalConclusion:  finalConclusion,
		TokenUsage:       o.getTokenUsage(),
		ConvergedAtRound: convergedAtRound,
		ParsedIssues:     parsedIssues,
	}, nil
}

func (o *DebateOrchestrator) useLedgerStructurize() bool {
	return strings.EqualFold(strings.TrimSpace(o.options.StructurizeMode), "ledger")
}

func (o *debateRun) initIssueLedgers() {
	o.issueLedgers = make(map[string]*IssueLedger, len(o.reviewers))
	for _, reviewer := range o.reviewers {
		o.issueLedgers[reviewer.ID] = NewIssueLedger(reviewer.ID)
	}
}

func (o *debateRun) extractRoundIssueDeltas(ctx context.Context, round int, roundOutputs map[string]string, display DisplayCallbacks) {
	if len(o.issueLedgers) == 0 || len(roundOutputs) == 0 {
		return
	}

	display.OnWaiting("structurizer")
	currentCanonicalSummary := BuildCanonicalIssueSummary(o.currentCanonicalIssues())

	g, gctx := errgroup.WithContext(ctx)
	for _, reviewer := range o.reviewers {
		reviewerID := reviewer.ID
		roundContent := strings.TrimSpace(roundOutputs[reviewerID])
		ledger := o.issueLedgers[reviewerID]
		if roundContent == "" || ledger == nil {
			continue
		}

		g.Go(func() error {
			delta, err := o.extractIssueDelta(gctx, reviewerID, round, roundContent, ledger.BuildSummary(), currentCanonicalSummary)
			if err != nil {
				util.Warnf("extractRoundIssueDeltas: reviewer %s round %d skipped: %v", reviewerID, round, err)
				return nil
			}
			ledger.ApplyDelta(delta, round)
			o.applyCanonicalActions(reviewerID, round, delta)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		util.Warnf("extractRoundIssueDeltas: unexpected error: %v", err)
	}
}

func (o *debateRun) extractIssueDelta(ctx context.Context, reviewerID string, round int, roundContent, ledgerSummary, canonicalSummary string) (*StructurizeDelta, error) {
	deltaSchema := schema.GetSchemaString("issues_delta")
	basePrompt := prompt.MustRender("structurize_delta.tmpl", map[string]any{
		"ReviewerID":       reviewerID,
		"Round":            round,
		"RoundContent":     roundContent,
		"LedgerSummary":    ledgerSummary,
		"CanonicalSummary": canonicalSummary,
		"Schema":           deltaSchema,
		"Language":         o.options.Language,
	})
	systemPrompt := prompt.MustRender("structurize_delta_system.tmpl", nil)

	chatOpts := &provider.ChatOptions{DisableTools: true, MaxTokens: 8192}
	const maxAttempts = 3
	var lastValidationErrors string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attemptPrompt := basePrompt
		if attempt > 1 {
			attemptPrompt = fmt.Sprintf("Previous output had validation errors:\n%s\n\n%s", lastValidationErrors, basePrompt)
		}

		msgs := []provider.Message{{Role: "user", Content: attemptPrompt}}
		response, err := o.summarizer.Provider.Chat(ctx, msgs, systemPrompt, chatOpts)
		if err != nil {
			lastValidationErrors = fmt.Sprintf("Chat error: %v", err)
			continue
		}
		o.trackTokens("summarizer", attemptPrompt+systemPrompt, response)

		parsed := ParseStructurizeDelta(response)
		switch {
		case parsed.ParseError != nil:
			lastValidationErrors = fmt.Sprintf("JSON parse error: %v", parsed.ParseError)
		case len(parsed.SchemaErrors) > 0:
			vr := &schema.ValidationResult{Errors: parsed.SchemaErrors}
			lastValidationErrors = schema.FormatErrorsForRetry(vr)
		case parsed.Output == nil:
			lastValidationErrors = "JSON was valid but output is empty."
		default:
			return parsed.Output, nil
		}
	}

	return nil, fmt.Errorf("failed after %d attempts: %s", maxAttempts, lastValidationErrors)
}

func (o *debateRun) structurizeIssuesFromLedgers(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	allIssues := o.collectCanonicalInputsFromLedgers()
	if len(allIssues) > 0 {
		return ApplyCanonicalSignals(CanonicalizeMergedIssues(allIssues), o.canonicalSignals)
	}

	// 兜底：当增量提取全失败时，回退到 legacy 一次性提取。
	if o.hasReviewerMessages() {
		util.Warnf("structurizeIssuesFromLedgers: no issues from ledgers, falling back to legacy structurizer")
		return o.structurizeIssuesLegacy(ctx, display)
	}
	return nil
}

func (o *debateRun) mergeAllLedgers() []MergedIssue {
	if len(o.issueLedgers) == 0 {
		return nil
	}
	reviewerIDs := make([]string, 0, len(o.issueLedgers))
	for reviewerID := range o.issueLedgers {
		reviewerIDs = append(reviewerIDs, reviewerID)
	}
	sort.Strings(reviewerIDs)

	var merged []MergedIssue
	for _, reviewerID := range reviewerIDs {
		merged = append(merged, o.issueLedgers[reviewerID].ToMergedIssues()...)
	}
	return merged
}

func (o *debateRun) collectCanonicalInputsFromLedgers() []MergedIssue {
	if len(o.issueLedgers) == 0 {
		return nil
	}
	reviewerIDs := make([]string, 0, len(o.issueLedgers))
	for reviewerID := range o.issueLedgers {
		reviewerIDs = append(reviewerIDs, reviewerID)
	}
	sort.Strings(reviewerIDs)

	var merged []MergedIssue
	for _, reviewerID := range reviewerIDs {
		merged = append(merged, o.issueLedgers[reviewerID].ToCanonicalInputs()...)
	}
	return merged
}

func (o *debateRun) currentCanonicalIssues() []MergedIssue {
	return ApplyCanonicalSignals(CanonicalizeMergedIssues(o.collectCanonicalInputsFromLedgers()), o.canonicalSignals)
}

func (o *debateRun) applyCanonicalActions(reviewerID string, round int, delta *StructurizeDelta) {
	if delta == nil {
		return
	}
	seen := make(map[string]struct{}, len(delta.Support)+len(delta.Withdraw)+len(delta.Contest))
	actions := make([]CanonicalSignal, 0, len(delta.Support)+len(delta.Withdraw)+len(delta.Contest))
	for _, item := range delta.Support {
		ref := strings.TrimSpace(item.IssueRef)
		if ref == "" {
			continue
		}
		key := "support|" + ref
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		actions = append(actions, CanonicalSignal{
			ReviewerID: reviewerID,
			IssueRef:   ref,
			Round:      round,
			Action:     "support",
		})
	}
	for _, item := range delta.Withdraw {
		ref := strings.TrimSpace(item.IssueRef)
		if ref == "" {
			continue
		}
		key := "withdraw|" + ref
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		actions = append(actions, CanonicalSignal{
			ReviewerID: reviewerID,
			IssueRef:   ref,
			Round:      round,
			Action:     "withdraw",
		})
	}
	for _, item := range delta.Contest {
		ref := strings.TrimSpace(item.IssueRef)
		if ref == "" {
			continue
		}
		key := "contest|" + ref
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		actions = append(actions, CanonicalSignal{
			ReviewerID: reviewerID,
			IssueRef:   ref,
			Round:      round,
			Action:     "contest",
		})
	}
	if len(actions) == 0 {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	o.canonicalSignals = append(o.canonicalSignals, actions...)
}

func (o *debateRun) hasReviewerMessages() bool {
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID != "user" {
			return true
		}
	}
	return false
}

// ========== 消息构建 ==========

// buildMessages 根据当前轮次为指定审查者构建消息列表。
// 第1轮：构建包含任务、上下文、分析结果和关注重点的独立审查提示
// 第2轮及之后：
//   - 会话模式：仅发送上一轮其他审查者的新消息（增量更新）
//   - 非会话模式：发送完整上下文，包含所有历史消息
func (o *debateRun) buildMessages(currentReviewerID string) []provider.Message {
	reviewer := o.findReviewer(currentReviewerID)
	hasSession := false
	if reviewer != nil {
		_, hasSession = reviewer.Provider.(provider.SessionProvider)
	}
	lastSeen := -1
	if v, ok := o.lastSeenIndex[currentReviewerID]; ok {
		lastSeen = v
	}
	isFirstCall := lastSeen < 0

	// 第1轮：独立审查
	if isFirstCall {
		return o.buildFirstRoundMessages(currentReviewerID)
	}

	// 第2轮及之后：收集辩论上下文
	var otherIDs []string
	for _, r := range o.reviewers {
		if r.ID != currentReviewerID {
			otherIDs = append(otherIDs, r.ID)
		}
	}

	myMessageCount, previousRoundsMessages := o.collectPreviousRoundsMessages(currentReviewerID)

	if hasSession {
		return o.buildSessionDebateMessages(currentReviewerID, myMessageCount, previousRoundsMessages)
	}
	return o.buildFullContextDebateMessages(currentReviewerID, otherIDs, previousRoundsMessages)
}

// buildFirstRoundMessages 构建第一轮独立审查的消息。
// 每个审查者独立审查代码，不受其他审查者影响。
func (o *debateRun) buildFirstRoundMessages(currentReviewerID string) []provider.Message {
	var contextSection string
	if o.gatheredContext != nil && o.gatheredContext.Summary != "" {
		contextSection = fmt.Sprintf("\n## System Context\n%s\n\n", o.gatheredContext.Summary)
	}

	var focusSection string
	focusAreas := ParseFocusAreas(o.analysis)
	if len(focusAreas) > 0 {
		focusSection = fmt.Sprintf("\nThe analyzer suggests focusing on: %s.\nThese are suggestions -- also flag anything else you notice beyond these areas.\n",
			strings.Join(focusAreas, "; "))
	}

	var callChainSection string
	if o.gatheredContext != nil && len(o.gatheredContext.RawReferences) > 0 {
		callChainSection = "\n" + FormatCallChainForReviewer(o.gatheredContext.RawReferences) + "\n"
	}

	// 只传 focus bullets，不传 analyzer 全文，避免锚定 reviewer 思维
	p := prompt.MustRender("reviewer_first_round.tmpl", map[string]any{
		"TaskPrompt":       o.taskPrompt,
		"ContextSection":   contextSection,
		"FocusSection":     focusSection,
		"CallChainSection": callChainSection,
		"ReviewerID":       currentReviewerID,
		"Language":         o.options.Language,
	})

	return []provider.Message{{Role: "user", Content: p}}
}

// collectPreviousRoundsMessages 收集之前轮次的消息，用于辩论上下文。
// 返回当前审查者的消息计数和其他审查者的消息列表。
func (o *debateRun) collectPreviousRoundsMessages(currentReviewerID string) (int, []DebateMessage) {
	myMessageCount := 0
	for _, m := range o.conversationHistory {
		if m.ReviewerID == currentReviewerID {
			myMessageCount++
		}
	}

	// 只获取之前轮次的消息（排除当前审查者自己的消息，因为会作为assistant消息单独添加）
	messageCountByReviewer := make(map[string]int)
	var previousRoundsMessages []DebateMessage
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID == currentReviewerID {
			continue // 排除自己的消息
		}
		if msg.ReviewerID == "user" {
			previousRoundsMessages = append(previousRoundsMessages, msg)
			continue
		}
		count := messageCountByReviewer[msg.ReviewerID]
		if count < myMessageCount {
			messageCountByReviewer[msg.ReviewerID] = count + 1
			previousRoundsMessages = append(previousRoundsMessages, msg)
		}
	}

	return myMessageCount, previousRoundsMessages
}

// buildSessionDebateMessages 为会话模式构建增量消息。
// 仅发送最新一轮的新消息（增量更新，因为会话已有之前的上下文）。
func (o *debateRun) buildSessionDebateMessages(currentReviewerID string, myMessageCount int, previousRoundsMessages []DebateMessage) []provider.Message {
	prevRoundCount := myMessageCount - 1
	messageCountByReviewer2 := make(map[string]int)
	var newMessages []DebateMessage
	for _, msg := range previousRoundsMessages {
		if msg.ReviewerID == "user" {
			newMessages = append(newMessages, msg)
			continue
		}
		count := messageCountByReviewer2[msg.ReviewerID]
		messageCountByReviewer2[msg.ReviewerID] = count + 1
		if count >= prevRoundCount {
			newMessages = append(newMessages, msg)
		}
	}

	if len(newMessages) == 0 {
		return []provider.Message{{Role: "user", Content: "Please continue with your review."}}
	}

	var parts []string
	for _, m := range newMessages {
		parts = append(parts, fmt.Sprintf("[%s]: %s", m.ReviewerID, m.Content))
	}
	newContent := strings.Join(parts, "\n\n---\n\n")

	p := prompt.MustRender("reviewer_debate_session.tmpl", map[string]any{
		"ReviewerID": currentReviewerID,
		"NewContent": newContent,
		"Language":   o.options.Language,
	})

	return []provider.Message{{Role: "user", Content: p}}
}

// buildFullContextDebateMessages 为非会话模式构建包含完整上下文的消息。
// 发送完整上下文，包含所有历史轮次的消息。
func (o *debateRun) buildFullContextDebateMessages(currentReviewerID string, otherIDs []string, previousRoundsMessages []DebateMessage) []provider.Message {
	otherLabel := fmt.Sprintf("[%s]", strings.Join(otherIDs, "], ["))
	isPlural := len(otherIDs) > 1
	otherWord := "is"
	if isPlural {
		otherWord = "are"
	}

	p := prompt.MustRender("reviewer_debate_full.tmpl", map[string]any{
		"TaskPrompt": o.taskPrompt,
		"Analysis":   o.analysis,
		"ReviewerID": currentReviewerID,
		"OtherLabel": otherLabel,
		"PluralS":    pluralS(isPlural),
		"OtherWord":  otherWord,
		"Language":   o.options.Language,
	})

	messages := []provider.Message{{Role: "user", Content: p}}

	// 添加之前轮次的其他审查者消息作为user角色
	for _, msg := range previousRoundsMessages {
		prefix := fmt.Sprintf("[%s]: ", msg.ReviewerID)
		if msg.ReviewerID == "user" {
			prefix = "[Human]: "
		}
		messages = append(messages, provider.Message{
			Role:    "user",
			Content: prefix + msg.Content,
		})
	}

	// 添加审查者自己之前的消息作为assistant角色，维持对话的连贯性
	for _, m := range o.conversationHistory {
		if m.ReviewerID == currentReviewerID {
			messages = append(messages, provider.Message{
				Role:    "assistant",
				Content: m.Content,
			})
		}
	}

	return messages
}

// ========== 共识检测与总结 ==========

// checkConvergence 请求总结器判断审查者是否已达成共识。
// 共识条件非常严格：所有审查者必须就最终结论达成一致，
// 关键问题必须被所有人确认，不能有被忽略的意见分歧。
// 支持从 Round 1 开始检测（独立审查达成一致也是有效共识）。
func (o *debateRun) checkConvergence(ctx context.Context, display DisplayCallbacks) (bool, error) {
	if len(o.conversationHistory) < len(o.reviewers) {
		return false, nil
	}

	roundsCompleted := len(o.conversationHistory) / len(o.reviewers)

	// 使用所有已完成轮次的消息，避免仅看最后一轮导致误判提前收敛
	roundByReviewer := make(map[string]int)
	var parts []string
	for _, m := range o.conversationHistory {
		if m.ReviewerID == "user" {
			continue
		}
		roundByReviewer[m.ReviewerID]++
		parts = append(parts, fmt.Sprintf("[%s] Round %d: %s", m.ReviewerID, roundByReviewer[m.ReviewerID], m.Content))
	}
	messagesText := strings.Join(parts, "\n\n---\n\n")

	convergencePrompt := prompt.MustRender("convergence_check.tmpl", map[string]any{
		"ReviewerCount":   len(o.reviewers),
		"RoundsCompleted": roundsCompleted,
		"IsFirstRound":    roundsCompleted == 1,
		"MessagesText":    messagesText,
	})

	systemPrompt := prompt.MustRender("convergence_system.tmpl", nil)

	msgs := []provider.Message{{Role: "user", Content: convergencePrompt}}
	response, err := o.summarizer.Provider.Chat(ctx, msgs, systemPrompt, nil)
	if err != nil {
		return false, err
	}

	// 解析响应：提取最后一条非空行的首词作为判定结果（CONVERGED/NOT_CONVERGED）
	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		reasoning := "Convergence judge returned empty response."
		display.OnConvergenceJudgment("NOT_CONVERGED", reasoning)
		util.Warnf("checkConvergence: empty response, treating as NOT_CONVERGED")
		return false, nil
	}

	lines := strings.Split(trimmed, "\n")
	verdictLineIdx := -1
	verdict := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		verdict = strings.ToUpper(fields[0])
		verdictLineIdx = i
		break
	}

	if verdict == "" {
		reasoning := "Convergence judge returned no verdict token."
		display.OnConvergenceJudgment("NOT_CONVERGED", reasoning)
		util.Warnf("checkConvergence: no verdict token found, treating as NOT_CONVERGED")
		return false, nil
	}

	isConverged := verdict == "CONVERGED"
	if verdict != "CONVERGED" && verdict != "NOT_CONVERGED" {
		util.Warnf("checkConvergence: invalid verdict token %q, treating as NOT_CONVERGED", verdict)
		verdict = "NOT_CONVERGED"
		isConverged = false
	}

	reasoning := trimmed
	if verdictLineIdx > 0 {
		reasoning = strings.TrimSpace(strings.Join(lines[:verdictLineIdx], "\n"))
	}
	if reasoning == "" {
		reasoning = trimmed
	}
	display.OnConvergenceJudgment(verdict, reasoning)

	return isConverged, nil
}

// collectSummaries 并行请求每个审查者提供最终总结。
// 在辩论结束后，每个审查者总结自己的核心观点和结论，不透露身份。
func (o *debateRun) collectSummaries(ctx context.Context, display DisplayCallbacks) ([]DebateSummary, error) {
	// 预分配结果切片，每个 goroutine 写入自己的索引位置，无需加锁
	summaries := make([]DebateSummary, len(o.reviewers))
	statuses := make([]ReviewerStatus, len(o.reviewers))
	var statusesMu sync.Mutex
	for i, reviewer := range o.reviewers {
		statuses[i] = ReviewerStatus{
			ReviewerID: reviewer.ID,
			Status:     "pending",
		}
	}

	g, gctx := errgroup.WithContext(ctx)
	displayStatuses := func() {
		statusesMu.Lock()
		snapshot := copyStatuses(statuses)
		statusesMu.Unlock()
		display.OnSummaryStatus(snapshot)
	}
	displayStatuses()
	for i, reviewer := range o.reviewers {
		i, reviewer := i, reviewer
		g.Go(func() error {
			messages := o.buildMessages(reviewer.ID)
			summaryPrompt := prompt.MustRender("reviewer_summary.tmpl", map[string]any{
				"Language": o.options.Language,
			})

			// 会话模式下续传只会发送最后一条 user 消息，将最新上下文与总结要求合并到同一条消息中
			if _, ok := reviewer.Provider.(provider.SessionProvider); ok {
				var contextParts []string
				for _, m := range messages {
					content := strings.TrimSpace(m.Content)
					if content == "" {
						continue
					}
					contextParts = append(contextParts, content)
				}
				if len(contextParts) > 0 {
					summaryPrompt = fmt.Sprintf("Latest debate context:\n\n%s\n\n%s",
						strings.Join(contextParts, "\n\n---\n\n"), summaryPrompt)
				}
				messages = []provider.Message{{Role: "user", Content: summaryPrompt}}
			} else {
				messages = append(messages, provider.Message{Role: "user", Content: summaryPrompt})
			}

			var inputParts []string
			for _, m := range messages {
				inputParts = append(inputParts, m.Content)
			}
			inputText := strings.Join(inputParts, "\n") + reviewer.SystemPrompt
			inputTokens := estimateTokens(inputText)

			startTime := time.Now().UnixMilli()
			statusesMu.Lock()
			statuses[i] = ReviewerStatus{
				ReviewerID:  reviewer.ID,
				Status:      "thinking",
				StartTime:   startTime,
				InputTokens: inputTokens,
			}
			statusesMu.Unlock()
			displayStatuses()

			ch, errCh := reviewer.Provider.ChatStream(gctx, messages, reviewer.SystemPrompt)
			var sb strings.Builder
			for chunk := range ch {
				sb.WriteString(chunk)
				display.OnMessageChunk("summary:"+reviewer.ID, chunk)
			}
			if err := <-errCh; err != nil {
				return fmt.Errorf("summary from %s: %w", reviewer.ID, err)
			}

			summary := sb.String()
			display.OnMessage("summary:"+reviewer.ID, summary)

			outputTokens := estimateTokens(summary)
			endTime := time.Now().UnixMilli()
			statusesMu.Lock()
			statuses[i] = ReviewerStatus{
				ReviewerID:    reviewer.ID,
				Status:        "done",
				StartTime:     startTime,
				EndTime:       endTime,
				Duration:      float64(endTime-startTime) / 1000.0,
				InputTokens:   inputTokens,
				OutputTokens:  outputTokens,
				EstimatedCost: float64(inputTokens+outputTokens) * 0.00001,
			}
			statusesMu.Unlock()
			displayStatuses()

			o.trackTokens(reviewer.ID, inputText, summary)

			summaries[i] = DebateSummary{
				ReviewerID: reviewer.ID,
				Summary:    summary,
			}
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return summaries, nil
}

// getFinalConclusion 请求总结器根据所有审查者的匿名总结生成最终结论。
// 最终结论包含：共识点、分歧点分析、以及推荐的行动项。
func (o *debateRun) getFinalConclusion(ctx context.Context, summaries []DebateSummary, display DisplayCallbacks) (string, error) {
	var parts []string
	for i, s := range summaries {
		parts = append(parts, fmt.Sprintf("Reviewer %d:\n%s", i+1, s.Summary))
	}
	summaryText := strings.Join(parts, "\n\n---\n\n")
	debateText := o.buildDebateTranscript(16000)

	conclusionPrompt := prompt.MustRender("final_conclusion.tmpl", map[string]any{
		"ReviewerCount": len(summaries),
		"SummaryText":   summaryText,
		"DebateText":    debateText,
		"Language":      o.options.Language,
	})

	msgs := []provider.Message{{Role: "user", Content: conclusionPrompt}}
	ch, errCh := o.summarizer.Provider.ChatStream(ctx, msgs, o.summarizer.SystemPrompt)
	var sb strings.Builder
	for chunk := range ch {
		sb.WriteString(chunk)
		display.OnMessageChunk("summarizer", chunk)
	}
	if err := <-errCh; err != nil {
		return "", err
	}

	response := sb.String()
	display.OnMessage("summarizer", response)

	o.trackTokens("summarizer", conclusionPrompt+o.summarizer.SystemPrompt, response)
	return response, nil
}

// ========== 问题提取 ==========

// structurizeIssues 使用AI从审查文本中提取结构化的问题列表。
// 如果JSON解析失败，最多重试3次。每次重试会使用更明确的提示词引导模型输出正确格式。
// 提取的问题包含：严重程度、分类、文件位置、描述和建议修复等信息。
func (o *debateRun) structurizeIssuesLegacy(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	// 收集每个审查者的所有消息（按轮次），避免只取最后一条导致早期发现的问题丢失
	allMessages := make(map[string][]string) // reviewerID -> []content (按轮次顺序)
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID == "user" {
			continue
		}
		allMessages[msg.ReviewerID] = append(allMessages[msg.ReviewerID], msg.Content)
	}

	if len(allMessages) == 0 {
		util.Warnf("structurizeIssues: no reviewer messages found in conversation history (total messages: %d)", len(o.conversationHistory))
		return nil
	}
	util.Debugf("structurizeIssues: collected messages from %d reviewers", len(allMessages))

	var reviewerIDs []string
	for id := range allMessages {
		reviewerIDs = append(reviewerIDs, id)
	}
	sort.Strings(reviewerIDs)

	var reviewParts []string
	for _, id := range reviewerIDs {
		rounds := allMessages[id]
		if len(rounds) == 1 {
			reviewParts = append(reviewParts, fmt.Sprintf("[%s]:\n%s", id, rounds[0]))
		} else {
			var roundParts []string
			for i, content := range rounds {
				roundParts = append(roundParts, fmt.Sprintf("[%s] Round %d:\n%s", id, i+1, content))
			}
			reviewParts = append(reviewParts, strings.Join(roundParts, "\n\n"))
		}
	}
	reviewText := strings.Join(reviewParts, "\n\n---\n\n")
	reviewerIDsStr := strings.Join(reviewerIDs, ", ")

	issuesSchema := schema.GetSchemaString("issues")
	basePrompt := prompt.MustRender("structurize_issues.tmpl", map[string]any{
		"ReviewText":  reviewText,
		"ReviewerIDs": reviewerIDsStr,
		"Schema":      issuesSchema,
		"Language":    o.options.Language,
	})

	systemPrompt := prompt.MustRender("structurize_system.tmpl", nil)
	// 显式设置 MaxTokens：structurizer 需要输出大量结构化 JSON（每个 issue 包含完整描述+代码），
	// 不设置时 OpenAI 默认 max_tokens 仅约 4096，会导致 JSON 输出截断 → "unexpected end of JSON input"
	chatOpts := &provider.ChatOptions{DisableTools: true, MaxTokens: 32768}
	const maxAttempts = 3

	var lastValidationErrors string
	var bestEffort []MergedIssue

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		display.OnWaiting("structurizer")

		var attemptPrompt string
		if attempt == 1 {
			attemptPrompt = basePrompt
		} else {
			attemptPrompt = prompt.MustRender("structurize_retry.tmpl", map[string]any{
				"ValidationErrors": lastValidationErrors,
				"ReviewText":       reviewText,
				"ReviewerIDs":      reviewerIDsStr,
				"Schema":           issuesSchema,
			})
		}

		// 打印 structurizer 的完整请求（system + user），便于复现与单元测试。
		// 通过 -v/--show-tool-trace 可在终端看到完整内容。
		structurizerTrace := fmt.Sprintf(
			"## Structurizer Request (Attempt %d/%d)\n\n### System Prompt\n```text\n%s\n```\n\n### User Prompt\n```text\n%s\n```",
			attempt, maxAttempts, systemPrompt, attemptPrompt,
		)
		display.OnMessage("structurizer-request", structurizerTrace)
		util.Debugf("structurizeIssues: attempt %d/%d full request:\n%s", attempt, maxAttempts, structurizerTrace)

		msgs := []provider.Message{{Role: "user", Content: attemptPrompt}}
		response, err := o.summarizer.Provider.Chat(ctx, msgs, systemPrompt, chatOpts)
		if err != nil {
			util.Warnf("structurizeIssues: attempt %d/%d Chat error: %v", attempt, maxAttempts, err)
			lastValidationErrors = fmt.Sprintf("Chat error: %v", err)
			continue
		}
		o.trackTokens("summarizer", attemptPrompt, response)

		// 截取前 500 字符用于调试
		preview := response
		if len(preview) > 500 {
			preview = preview[:500] + "..."
		}
		util.Debugf("structurizeIssues: attempt %d/%d response preview: %s", attempt, maxAttempts, preview)

		parsed := ParseReviewerOutput(response)
		var parsedIssues []MergedIssue
		if parsed.Output != nil && len(parsed.Output.Issues) > 0 {
			parsedIssues = DeduplicateMergedIssues(issuesToMerged(parsed.Output.Issues))
			if len(parsedIssues) > len(bestEffort) {
				bestEffort = parsedIssues
			}
		}

		// 根据解析结果决定是否重试
		switch {
		case parsed.ParseError != nil:
			util.Warnf("structurizeIssues: attempt %d/%d parse error: %v", attempt, maxAttempts, parsed.ParseError)
			lastValidationErrors = fmt.Sprintf("JSON parse error: %v", parsed.ParseError)
		case len(parsed.SchemaErrors) > 0:
			vr := &schema.ValidationResult{Errors: parsed.SchemaErrors}
			lastValidationErrors = schema.FormatErrorsForRetry(vr)
			util.Warnf("structurizeIssues: attempt %d/%d schema errors: %s", attempt, maxAttempts, lastValidationErrors)
		case parsed.Output == nil || len(parsed.Output.Issues) == 0:
			util.Warnf("structurizeIssues: attempt %d/%d parsed OK but 0 valid issues", attempt, maxAttempts)
			lastValidationErrors = "JSON was valid but contained 0 issues. Please include all issues found in the review."
		default:
			// 成功
			return parsedIssues
		}
	}

	if len(bestEffort) > 0 {
		util.Warnf("structurizeIssues: returning best-effort issues after retries (%d issues)", len(bestEffort))
		return bestEffort
	}

	return nil
}

// structurizeIssues 兼容旧测试和旧调用，内部委托给 legacy 路径。
func (o *debateRun) structurizeIssues(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	return o.structurizeIssuesLegacy(ctx, display)
}

// issuesToMerged 将 ReviewIssue 列表转换为 MergedIssue 列表。
// 注意：ClaimedBy 是模型在 JSON 输出中自称的归属（可能不准确），
// 这里直接用作 MergedIssue.RaisedBy，因为 structurizer 已被要求按 reviewer ID 归属。
// 后续 DeduplicateIssues 会基于真实的 reviewer 输出进行交叉验证和合并。
func issuesToMerged(issues []ReviewIssue) []MergedIssue {
	result := make([]MergedIssue, 0, len(issues))
	for _, issue := range issues {
		supportedBy := uniqueSorted(issue.ClaimedBy)
		if len(supportedBy) == 0 {
			supportedBy = []string{"summarizer"}
		}
		result = append(result, MergedIssue{
			ReviewIssue:  issue,
			RaisedBy:     supportedBy,
			IntroducedBy: append([]string(nil), supportedBy...),
			SupportedBy:  append([]string(nil), supportedBy...),
			Descriptions: []string{issue.Description},
			Mentions: func() []IssueMention {
				mentions := make([]IssueMention, 0, len(supportedBy))
				for _, reviewerID := range supportedBy {
					mentions = append(mentions, IssueMention{
						ReviewerID: reviewerID,
						Status:     "active",
					})
				}
				return mentions
			}(),
		})
	}
	return result
}

// ========== 辅助方法 ==========

// findReviewer 根据ID查找审查者，未找到返回nil。
func (o *DebateOrchestrator) findReviewer(id string) *Reviewer {
	for i := range o.reviewers {
		if o.reviewers[i].ID == id {
			return &o.reviewers[i]
		}
	}
	return nil
}

// markAsSeen 将当前对话历史的最后一条消息标记为该审查者已读。
// 用于在会话模式下确定需要发送哪些增量消息。
func (o *debateRun) markAsSeen(reviewerID string) {
	o.lastSeenIndex[reviewerID] = len(o.conversationHistory) - 1
}

// trackTokens 追踪指定审查者的Token使用量（线程安全）。
// 使用estimateTokens进行粗略估算。
func (o *debateRun) trackTokens(reviewerID, input, output string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	tc, ok := o.tokenUsage[reviewerID]
	if !ok {
		tc = &tokenCount{}
		o.tokenUsage[reviewerID] = tc
	}
	tc.input += estimateTokens(input)
	tc.output += estimateTokens(output)
}

// getTokenUsage 返回所有审查者的Token使用量汇总（线程安全）。
// 预估费用按照每Token 0.00001美元计算。
func (o *debateRun) getTokenUsage() []TokenUsage {
	o.mu.Lock()
	defer o.mu.Unlock()
	var usage []TokenUsage
	for id, tc := range o.tokenUsage {
		usage = append(usage, TokenUsage{
			ReviewerID:    id,
			InputTokens:   tc.input,
			OutputTokens:  tc.output,
			EstimatedCost: float64(tc.input+tc.output) * 0.00001,
		})
	}
	return usage
}

// endAllSessions 结束所有AI提供者的会话。
// 在辩论完成或出错时调用，释放会话资源。
func (o *debateRun) endAllSessions() {
	for _, r := range o.reviewers {
		if sp, ok := r.Provider.(provider.SessionProvider); ok {
			sp.EndSession()
		}
	}
	if sp, ok := o.analyzer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}
}

func (o *debateRun) buildDebateTranscript(maxLen int) string {
	if len(o.conversationHistory) == 0 {
		return ""
	}

	roundByReviewer := make(map[string]int)
	var parts []string
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID == "user" {
			continue
		}
		roundByReviewer[msg.ReviewerID]++
		parts = append(parts, fmt.Sprintf("[%s] Round %d:\n%s", msg.ReviewerID, roundByReviewer[msg.ReviewerID], msg.Content))
	}
	transcript := strings.Join(parts, "\n\n---\n\n")
	if maxLen > 0 && len(transcript) > maxLen {
		return fmt.Sprintf("[Truncated: showing last %d chars of transcript]\n\n%s", maxLen, transcript[len(transcript)-maxLen:])
	}
	return transcript
}

// GetReviewers 暴露审查者列表，用于辩论后的后续讨论。
func (o *DebateOrchestrator) GetReviewers() []Reviewer {
	return o.reviewers
}

// estimateTokens 估算文本的Token数量。
// CJK字符（中日韩）按约0.7 Token/字符计算，英文按约0.25 Token/字符（即4字符≈1Token）计算。
func estimateTokens(text string) int {
	cjkCount := 0
	for _, r := range text {
		if isCJK(r) {
			cjkCount++
		}
	}
	return int(math.Ceil(float64(cjkCount)*0.7 + float64(len(text)-cjkCount)/4.0))
}

// isCJK 检查一个字符是否为CJK（中日韩）字符。
// 包括汉字、CJK符号标点（U+3000-U+303F）和全角/半角字符（U+FF00-U+FFEF）。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK符号和标点
		(r >= 0xFF00 && r <= 0xFFEF) // 半角和全角字符
}

var diffFenceRe = regexp.MustCompile("(?s)```diff\\n(.*?)```")

// extractDiffFromPrompt 从包含```diff代码块的提示词中提取diff内容。
// 如果未找到diff代码块，返回原始提示词。
func extractDiffFromPrompt(prompt string) string {
	if m := diffFenceRe.FindStringSubmatch(prompt); len(m) > 1 {
		return m[1]
	}
	return prompt
}

// copyStatuses 创建ReviewerStatus切片的浅拷贝，确保传递给UI回调时数据安全。
// 避免并发goroutine修改状态时影响UI显示。
func copyStatuses(statuses []ReviewerStatus) []ReviewerStatus {
	cp := make([]ReviewerStatus, len(statuses))
	copy(cp, statuses)
	return cp
}

// pluralS 返回英文复数后缀"s"（如果是复数），否则返回空字符串。
// 用于构建语法正确的英文提示词。
func pluralS(plural bool) string {
	if plural {
		return "s"
	}
	return ""
}
