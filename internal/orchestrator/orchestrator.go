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

	conversationHistory []DebateMessage        // 完整的辩论对话历史
	tokenUsage          map[string]*tokenCount // 每个审查者的Token使用量
	analysis            string                 // 预分析结果
	gatheredContext     *GatheredContext       // 收集到的代码上下文
	taskPrompt          string                 // 原始任务提示词（包含diff等）
	lastSeenIndex       map[string]int         // 每个审查者最后看到的消息索引

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
	o.reset(prompt)
	o.startSessions(label)
	defer o.endAllSessions()

	// 阶段1: 并行执行上下文收集和预分析
	if err := o.runAnalysisPhase(ctx, label, prompt, display); err != nil {
		return nil, err
	}

	// 阶段2: 多轮辩论
	convergedAtRound, err := o.runDebatePhase(ctx, display)
	if err != nil {
		return nil, err
	}

	// 阶段3: 总结 + 结论 + 问题提取
	return o.runSummaryPhase(ctx, label, display, convergedAtRound)
}

// reset 重置所有状态，确保每次执行都是干净的。
func (o *DebateOrchestrator) reset(prompt string) {
	o.conversationHistory = nil
	o.tokenUsage = make(map[string]*tokenCount)
	o.lastSeenIndex = make(map[string]int)
	o.analysis = ""
	o.gatheredContext = nil
	o.taskPrompt = prompt
}

// startSessions 为支持会话的AI提供者启动会话。
// 会话模式下，提供者可以维护对话上下文，避免重复发送完整历史。
func (o *DebateOrchestrator) startSessions(label string) {
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
func (o *DebateOrchestrator) runAnalysisPhase(ctx context.Context, label, prompt string, display DisplayCallbacks) error {
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
			display.OnMessage("analyzer", chunk)
		}
		if err := <-errCh; err != nil {
			return fmt.Errorf("analyzer failed: %w", err)
		}
		o.analysis = sb.String()
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
func (o *DebateOrchestrator) runDebatePhase(ctx context.Context, display DisplayCallbacks) (*int, error) {
	var convergedAtRound *int

	for round := 1; round <= o.options.MaxRounds; round++ {
		if err := o.runDebateRound(ctx, round, display); err != nil {
			return nil, err
		}

		// 共识检测：从第2轮开始（且不是最后一轮）检查审查者是否已达成共识
		// 如果达成共识则提前终止辩论，节省Token消耗
		converged := false
		if o.options.CheckConvergence && round >= 2 && round < o.options.MaxRounds {
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
func (o *DebateOrchestrator) runDebateRound(ctx context.Context, round int, display DisplayCallbacks) error {
	// 在执行前为所有审查者构建消息（快照，确保所有审查者看到相同的信息）
	// 这样避免了先执行的审查者的输出影响后执行审查者的输入
	type reviewerTask struct {
		reviewer Reviewer
		messages []provider.Message
	}
	tasks := make([]reviewerTask, len(o.reviewers))
	for i, r := range o.reviewers {
		tasks[i] = reviewerTask{
			reviewer: r,
			messages: o.buildMessages(r.ID),
		}
	}

	// 初始化每个审查者的状态追踪，用于UI实时显示
	statuses := make([]ReviewerStatus, len(o.reviewers))
	for i, r := range o.reviewers {
		statuses[i] = ReviewerStatus{
			ReviewerID: r.ID,
			Status:     "pending",
		}
	}

	display.OnWaiting(fmt.Sprintf("round-%d", round))
	display.OnParallelStatus(round, statuses)

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
			statuses[i] = ReviewerStatus{
				ReviewerID: task.reviewer.ID,
				Status:     "thinking",
				StartTime:  startTime,
			}
			display.OnParallelStatus(round, copyStatuses(statuses))

			// 流式接收审查者的响应，逐块读取
			ch, errCh := task.reviewer.Provider.ChatStream(rgctx, task.messages, task.reviewer.SystemPrompt)
			var sb strings.Builder
			for chunk := range ch {
				sb.WriteString(chunk)
			}
			if err := <-errCh; err != nil {
				return fmt.Errorf("reviewer %s failed: %w", task.reviewer.ID, err)
			}

			// 标记审查者状态为"已完成"，记录结束时间和耗时
			endTime := time.Now().UnixMilli()
			statuses[i] = ReviewerStatus{
				ReviewerID: task.reviewer.ID,
				Status:     "done",
				StartTime:  startTime,
				EndTime:    endTime,
				Duration:   float64(endTime-startTime) / 1000.0,
			}
			display.OnParallelStatus(round, copyStatuses(statuses))

			var inputParts []string
			for _, m := range task.messages {
				inputParts = append(inputParts, m.Content)
			}
			inputText := strings.Join(inputParts, "\n") + task.reviewer.SystemPrompt

			results[i] = roundResult{
				reviewer:     task.reviewer,
				fullResponse: sb.String(),
				inputText:    inputText,
			}
			return nil
		})
	}

	if err := rg.Wait(); err != nil {
		return err
	}

	// 所有审查者完成后，将结果添加到对话历史并通知UI
	// 注意：必须在所有审查者完成后统一添加，保证消息顺序一致
	for _, r := range results {
		o.trackTokens(r.reviewer.ID, r.inputText, r.fullResponse)
		o.conversationHistory = append(o.conversationHistory, DebateMessage{
			ReviewerID: r.reviewer.ID,
			Content:    r.fullResponse,
			Timestamp:  time.Now(),
		})
		o.markAsSeen(r.reviewer.ID)
		display.OnMessage(r.reviewer.ID, r.fullResponse)
	}

	return nil
}

// runSummaryPhase 收集审查者总结，生成最终结论和结构化问题列表。
func (o *DebateOrchestrator) runSummaryPhase(ctx context.Context, label string, display DisplayCallbacks, convergedAtRound *int) (*DebateResult, error) {
	// 辩论结束后，收集每个审查者的最终总结，然后由总结器生成统一结论
	display.OnWaiting("summarizer")
	summaries, err := o.collectSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting summaries: %w", err)
	}

	// 结束总结器会话，后续的 getFinalConclusion 和 structurizeIssues 都构建全新消息，
	// 不依赖会话上下文，可以安全地并行执行
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}

	// getFinalConclusion（依赖 summaries）和 structurizeIssues（只读 conversationHistory）并行执行
	var finalConclusion string
	var parsedIssues []MergedIssue

	g3, g3ctx := errgroup.WithContext(ctx)
	g3.Go(func() error {
		var err error
		finalConclusion, err = o.getFinalConclusion(g3ctx, summaries)
		if err != nil {
			return fmt.Errorf("final conclusion: %w", err)
		}
		return nil
	})
	g3.Go(func() error {
		parsedIssues = o.structurizeIssues(g3ctx, display)
		return nil
	})
	if err := g3.Wait(); err != nil {
		return nil, err
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

// ========== 消息构建 ==========

// buildMessages 根据当前轮次为指定审查者构建消息列表。
// 第1轮：构建包含任务、上下文、分析结果和关注重点的独立审查提示
// 第2轮及之后：
//   - 会话模式：仅发送上一轮其他审查者的新消息（增量更新）
//   - 非会话模式：发送完整上下文，包含所有历史消息
func (o *DebateOrchestrator) buildMessages(currentReviewerID string) []provider.Message {
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
func (o *DebateOrchestrator) buildFirstRoundMessages(currentReviewerID string) []provider.Message {
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

	p := prompt.MustRender("reviewer_first_round.tmpl", map[string]any{
		"TaskPrompt":       o.taskPrompt,
		"ContextSection":   contextSection,
		"FocusSection":     focusSection,
		"CallChainSection": callChainSection,
		"Analysis":         o.analysis,
		"ReviewerID":       currentReviewerID,
	})

	return []provider.Message{{Role: "user", Content: p}}
}

// collectPreviousRoundsMessages 收集之前轮次的消息，用于辩论上下文。
// 返回当前审查者的消息计数和其他审查者的消息列表。
func (o *DebateOrchestrator) collectPreviousRoundsMessages(currentReviewerID string) (int, []DebateMessage) {
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
func (o *DebateOrchestrator) buildSessionDebateMessages(currentReviewerID string, myMessageCount int, previousRoundsMessages []DebateMessage) []provider.Message {
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
	})

	return []provider.Message{{Role: "user", Content: p}}
}

// buildFullContextDebateMessages 为非会话模式构建包含完整上下文的消息。
// 发送完整上下文，包含所有历史轮次的消息。
func (o *DebateOrchestrator) buildFullContextDebateMessages(currentReviewerID string, otherIDs []string, previousRoundsMessages []DebateMessage) []provider.Message {
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
// 仅在完成至少2轮辩论后才进行检测。
func (o *DebateOrchestrator) checkConvergence(ctx context.Context, display DisplayCallbacks) (bool, error) {
	if len(o.conversationHistory) < len(o.reviewers) {
		return false, nil
	}

	roundsCompleted := len(o.conversationHistory) / len(o.reviewers)
	if roundsCompleted < 2 {
		return false, nil
	}

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
func (o *DebateOrchestrator) collectSummaries(ctx context.Context) ([]DebateSummary, error) {
	// 预分配结果切片，每个 goroutine 写入自己的索引位置，无需加锁
	summaries := make([]DebateSummary, len(o.reviewers))

	g, gctx := errgroup.WithContext(ctx)
	for i, reviewer := range o.reviewers {
		i, reviewer := i, reviewer
		g.Go(func() error {
			messages := o.buildMessages(reviewer.ID)
			summaryPrompt := prompt.MustRender("reviewer_summary.tmpl", nil)

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

			summary, err := reviewer.Provider.Chat(gctx, messages, reviewer.SystemPrompt, nil)
			if err != nil {
				return fmt.Errorf("summary from %s: %w", reviewer.ID, err)
			}

			var inputParts []string
			for _, m := range messages {
				inputParts = append(inputParts, m.Content)
			}
			o.trackTokens(reviewer.ID, strings.Join(inputParts, "\n")+reviewer.SystemPrompt, summary)

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
func (o *DebateOrchestrator) getFinalConclusion(ctx context.Context, summaries []DebateSummary) (string, error) {
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
	})

	msgs := []provider.Message{{Role: "user", Content: conclusionPrompt}}
	response, err := o.summarizer.Provider.Chat(ctx, msgs, o.summarizer.SystemPrompt, nil)
	if err != nil {
		return "", err
	}

	o.trackTokens("summarizer", conclusionPrompt+o.summarizer.SystemPrompt, response)
	return response, nil
}

// ========== 问题提取 ==========

// structurizeIssues 使用AI从审查文本中提取结构化的问题列表。
// 如果JSON解析失败，最多重试3次。每次重试会使用更明确的提示词引导模型输出正确格式。
// 提取的问题包含：严重程度、分类、文件位置、描述和建议修复等信息。
func (o *DebateOrchestrator) structurizeIssues(ctx context.Context, display DisplayCallbacks) []MergedIssue {
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
	})

	systemPrompt := prompt.MustRender("structurize_system.tmpl", nil)
	chatOpts := &provider.ChatOptions{DisableTools: true}
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

// issuesToMerged 将 ReviewIssue 列表转换为 MergedIssue 列表。
func issuesToMerged(issues []ReviewIssue) []MergedIssue {
	result := make([]MergedIssue, 0, len(issues))
	for _, issue := range issues {
		raisedBy := issue.RaisedBy
		if len(raisedBy) == 0 {
			raisedBy = []string{"summarizer"}
		}
		result = append(result, MergedIssue{
			ReviewIssue:  issue,
			RaisedBy:     raisedBy,
			Descriptions: []string{issue.Description},
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
func (o *DebateOrchestrator) markAsSeen(reviewerID string) {
	o.lastSeenIndex[reviewerID] = len(o.conversationHistory) - 1
}

// trackTokens 追踪指定审查者的Token使用量（线程安全）。
// 使用estimateTokens进行粗略估算。
func (o *DebateOrchestrator) trackTokens(reviewerID, input, output string) {
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
func (o *DebateOrchestrator) getTokenUsage() []TokenUsage {
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
func (o *DebateOrchestrator) endAllSessions() {
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

func (o *DebateOrchestrator) buildDebateTranscript(maxLen int) string {
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
