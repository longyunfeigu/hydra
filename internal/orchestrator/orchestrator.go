package orchestrator

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/guwanhua/hydra/internal/provider"
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
//   1. 并行执行上下文收集和代码预分析
//   2. 多轮辩论：每轮所有审查者并行执行，互相挑战对方的观点
//   3. 收集总结并生成最终结论和结构化问题列表
type DebateOrchestrator struct {
	reviewers       []Reviewer               // 参与辩论的审查者列表
	analyzer        Reviewer                  // 预分析器
	summarizer      Reviewer                  // 总结器/共识判断器
	contextGatherer ContextGathererInterface  // 上下文收集器

	options         OrchestratorOptions       // 辩论行为配置

	conversationHistory []DebateMessage       // 完整的辩论对话历史
	tokenUsage          map[string]*tokenCount // 每个审查者的Token使用量
	analysis            string                 // 预分析结果
	gatheredContext      *GatheredContext      // 收集到的代码上下文
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
//   阶段1：并行执行上下文收集和代码预分析
//   阶段2：多轮辩论，每轮所有审查者并行执行，可选共识检测提前终止
//   阶段3：收集审查者总结，生成最终结论，提取结构化问题
//
// 参数：
//   - label: 任务标识（如PR编号），用于会话标记
//   - prompt: 包含代码diff的完整审查提示词
//   - display: 终端UI回调接口，用于实时更新显示
func (o *DebateOrchestrator) RunStreaming(ctx context.Context, label, prompt string, display DisplayCallbacks) (*DebateResult, error) {
	// 重置所有状态，确保每次执行都是干净的
	o.conversationHistory = nil
	o.tokenUsage = make(map[string]*tokenCount)
	o.lastSeenIndex = make(map[string]int)
	o.analysis = ""
	o.gatheredContext = nil
	o.taskPrompt = prompt

	var convergedAtRound *int // 记录在哪一轮达成共识（nil表示未达成）

	// 为支持会话的AI提供者启动会话
	// 会话模式下，提供者可以维护对话上下文，避免重复发送完整历史
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

	defer o.endAllSessions() // 确保所有会话在函数退出时被清理

	// ========== 阶段1：并行执行上下文收集和代码预分析 ==========
	// 上下文收集和预分析互不依赖，可以并行执行以减少总耗时
	g, gctx := errgroup.WithContext(ctx)

	// 上下文收集：从代码仓库中提取与变更相关的调用链、模块关系等信息
	if o.contextGatherer != nil {
		g.Go(func() error {
			display.OnWaiting("context-gatherer")
			diff := extractDiffFromPrompt(prompt)
			gathered, err := o.contextGatherer.Gather(diff, label, "main")
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
		return nil, err
	}

	// 通知UI层上下文收集已完成，可以展示相关信息
	if o.gatheredContext != nil {
		display.OnContextGathered(o.gatheredContext)
	}

	// ========== 阶段2：多轮辩论 ==========
	// 每轮辩论中，所有审查者并行执行，然后检查是否达成共识
	for round := 1; round <= o.options.MaxRounds; round++ {
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
		type reviewerResult struct {
			reviewer     Reviewer
			fullResponse string
			inputText    string
		}
		results := make([]reviewerResult, len(tasks))

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

				results[i] = reviewerResult{
					reviewer:     task.reviewer,
					fullResponse: sb.String(),
					inputText:    inputText,
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
				Content:    r.fullResponse,
				Timestamp:  time.Now(),
			})
			o.markAsSeen(r.reviewer.ID)
			display.OnMessage(r.reviewer.ID, r.fullResponse)
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

	// ========== 阶段3：收集总结和最终结论 ==========
	// 辩论结束后，收集每个审查者的最终总结，然后由总结器生成统一结论
	display.OnWaiting("summarizer")
	summaries, err := o.collectSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting summaries: %w", err)
	}

	finalConclusion, err := o.getFinalConclusion(ctx, summaries)
	if err != nil {
		return nil, fmt.Errorf("final conclusion: %w", err)
	}

	// 在结构化提取之前结束总结器会话，确保后续JSON提取不受会话上下文干扰
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}

	// 从审查文本中提取结构化问题（支持重试），转换为可直接用于GitHub评论的格式
	parsedIssues := o.structurizeIssues(ctx, display)

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

	var otherIDs []string
	for _, r := range o.reviewers {
		if r.ID != currentReviewerID {
			otherIDs = append(otherIDs, r.ID)
		}
	}

	// 第1轮：独立审查 - 每个审查者独立审查代码，不受其他审查者影响
	if isFirstCall {
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

		prompt := fmt.Sprintf(`Task: %s
%s%s%sHere is the analysis:

%s

You are [%s]. Review EVERY changed file and EVERY changed function/block -- do not skip any.
For each change, check: correctness, security, performance, error handling, edge cases, maintainability.
If you reviewed a file and found no issues, say so briefly. Do not stop early.`,
			o.taskPrompt, contextSection, focusSection, callChainSection, o.analysis, currentReviewerID)

		return []provider.Message{{Role: "user", Content: prompt}}
	}

	// 第2轮及之后：审查者可以看到之前轮次的讨论内容
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

	if hasSession {
		// 会话模式：仅发送最新一轮的新消息（增量更新，因为会话已有之前的上下文）
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

		return []provider.Message{{
			Role: "user",
			Content: fmt.Sprintf(`You are [%s]. Here's what others said in the previous round:

%s

Do three things:
1. Continue your own exhaustive review -- are there changed files or functions you haven't covered yet? Cover them now.
2. Point out what the other reviewers MISSED -- which files or changes did they skip or gloss over?
3. Respond to their points -- agree where valid, challenge where you disagree.`, currentReviewerID, newContent),
		}}
	}

	// 非会话模式：发送包含所有历史轮次的完整上下文
	otherLabel := fmt.Sprintf("[%s]", strings.Join(otherIDs, "], ["))
	isPlural := len(otherIDs) > 1
	otherWord := "is"
	if isPlural {
		otherWord = "are"
	}

	debateContext := fmt.Sprintf(`You are [%s] in a code review debate with %s.
Your shared goal: find ALL real issues in the code -- leave nothing uncovered.

IMPORTANT:
- You are [%s], the other reviewer%s %s %s
- Continue your own exhaustive review -- cover any changed files or functions you haven't addressed yet
- Point out what others MISSED -- which files or changes did they skip or gloss over?
- Challenge weak arguments - don't agree just to be polite
- Acknowledge good points and build on them
- If you disagree, explain why with evidence`,
		currentReviewerID, otherLabel,
		currentReviewerID, pluralS(isPlural), otherWord, otherLabel)

	prompt := fmt.Sprintf(`Task: %s

Here is the analysis:

%s

%s

Previous rounds discussion:`, o.taskPrompt, o.analysis, debateContext)

	messages := []provider.Message{{Role: "user", Content: prompt}}

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

	// 获取最后一轮的所有审查者消息用于共识判断
	lastRoundMessages := o.conversationHistory[len(o.conversationHistory)-len(o.reviewers):]
	var parts []string
	for _, m := range lastRoundMessages {
		parts = append(parts, fmt.Sprintf("[%s]: %s", m.ReviewerID, m.Content))
	}
	messagesText := strings.Join(parts, "\n\n---\n\n")

	prompt := fmt.Sprintf(`You are a strict consensus judge. Analyze whether these %d reviewers have reached TRUE CONSENSUS.

IMPORTANT: This is Round %d. Reviewers have now seen each other's opinions.

TRUE CONSENSUS requires ALL of the following:
1. All reviewers agree on the SAME final verdict (all approve OR all request changes)
2. Critical/blocking issues identified by ANY reviewer are acknowledged by ALL others
3. No reviewer has raised a concern that others have ignored or dismissed without addressing
4. They explicitly agree on what actions to take (not just "no disagreement")

NOT CONSENSUS if ANY of these:
- One reviewer identified a Critical/Important issue that others didn't address
- Reviewers found DIFFERENT sets of issues without cross-validating each other's findings
- One reviewer says "I disagree" or challenges another's reasoning
- Reviewers give different verdicts or severity assessments
- Silence on another's point (not responding to it) - silence is NOT agreement
- They list problems but haven't confirmed they agree on the complete list

Reviews from Round %d:
%s

First, provide a brief reasoning (2-3 sentences) explaining your judgment.
Then on the LAST line, respond with EXACTLY one word: CONVERGED or NOT_CONVERGED`,
		len(o.reviewers), roundsCompleted, roundsCompleted, messagesText)

	systemPrompt := "You are a strict consensus judge. Be VERY conservative - if there is ANY doubt, respond NOT_CONVERGED. Provide brief reasoning, then on the last line respond with exactly one word: CONVERGED or NOT_CONVERGED."

	msgs := []provider.Message{{Role: "user", Content: prompt}}
	response, err := o.summarizer.Provider.Chat(ctx, msgs, systemPrompt, nil)
	if err != nil {
		return false, err
	}

	// 解析响应：提取最后一行的判定结果（CONVERGED或NOT_CONVERGED）和推理过程
	lines := strings.Split(strings.TrimSpace(response), "\n")
	lastLine := strings.TrimSpace(lines[len(lines)-1])
	verdict := strings.ToUpper(strings.Fields(lastLine)[0])
	isConverged := verdict == "CONVERGED"

	// 提取推理过程（除最后一行外的所有内容）
	reasoning := strings.TrimSpace(strings.Join(lines[:len(lines)-1], "\n"))
	if reasoning == "" {
		reasoning = strings.TrimSpace(response)
	}

	verdictStr := "NOT_CONVERGED"
	if isConverged {
		verdictStr = "CONVERGED"
	}
	display.OnConvergenceJudgment(verdictStr, reasoning)

	return isConverged, nil
}

// collectSummaries 请求每个审查者提供最终总结。
// 在辩论结束后，每个审查者总结自己的核心观点和结论，不透露身份。
func (o *DebateOrchestrator) collectSummaries(ctx context.Context) ([]DebateSummary, error) {
	summaryPrompt := "Please summarize your key points and conclusions. Do not reveal your identity or role."
	var summaries []DebateSummary

	for _, reviewer := range o.reviewers {
		messages := o.buildMessages(reviewer.ID)
		messages = append(messages, provider.Message{Role: "user", Content: summaryPrompt})

		summary, err := reviewer.Provider.Chat(ctx, messages, reviewer.SystemPrompt, nil)
		if err != nil {
			return nil, fmt.Errorf("summary from %s: %w", reviewer.ID, err)
		}

		var inputParts []string
		for _, m := range messages {
			inputParts = append(inputParts, m.Content)
		}
		o.trackTokens(reviewer.ID, strings.Join(inputParts, "\n")+reviewer.SystemPrompt, summary)

		summaries = append(summaries, DebateSummary{
			ReviewerID: reviewer.ID,
			Summary:    summary,
		})
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

	prompt := fmt.Sprintf(`There are exactly %d reviewers in this debate. Based on their anonymous summaries below, provide a final conclusion including:
- Points of consensus
- Points of disagreement with analysis
- Recommended action items

%s`, len(summaries), summaryText)

	msgs := []provider.Message{{Role: "user", Content: prompt}}
	response, err := o.summarizer.Provider.Chat(ctx, msgs, o.summarizer.SystemPrompt, nil)
	if err != nil {
		return "", err
	}

	o.trackTokens("summarizer", prompt+o.summarizer.SystemPrompt, response)
	return response, nil
}

// structurizeIssues 使用AI从审查文本中提取结构化的问题列表。
// 如果JSON解析失败，最多重试3次。每次重试会使用更明确的提示词引导模型输出正确格式。
// 提取的问题包含：严重程度、分类、文件位置、描述和建议修复等信息。
func (o *DebateOrchestrator) structurizeIssues(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	// 收集每个审查者的最后一条消息（代表其最终观点）
	lastMessages := make(map[string]string)
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID == "user" {
			continue
		}
		lastMessages[msg.ReviewerID] = msg.Content
	}

	if len(lastMessages) == 0 {
		return nil
	}

	var reviewParts []string
	var reviewerIDs []string
	for id, content := range lastMessages {
		reviewParts = append(reviewParts, fmt.Sprintf("[%s]:\n%s", id, content))
		reviewerIDs = append(reviewerIDs, id)
	}
	reviewText := strings.Join(reviewParts, "\n\n---\n\n")
	reviewerIDsStr := strings.Join(reviewerIDs, ", ")

	basePrompt := fmt.Sprintf(`Based on these code review discussions, extract ALL concrete issues mentioned by the reviewers into a structured JSON format.

%s

Output ONLY a JSON block (no other text):
`+"```json"+`
{
  "issues": [
    {
      "severity": "critical|high|medium|low|nitpick",
      "category": "security|performance|error-handling|style|correctness|architecture",
      "file": "path/to/file",
      "line": 42,
      "title": "One-line summary",
      "description": "Detailed markdown explanation (see rules below)",
      "suggestedFix": "Brief one-line fix summary",
      "raisedBy": ["reviewer-id-1", "reviewer-id-2"]
    }
  ]
}
`+"```"+`

Rules:
- Include every issue mentioned by any reviewer
- The "description" field will be posted as a GitHub PR comment. Make it comprehensive markdown covering: (1) What the problem is, (2) Why it matters (impact/risk), (3) The original problematic code quoted in a code block, (4) The suggested fix shown as code, (5) Why the fix is correct
- If multiple reviewers mention the same issue, list all their IDs in raisedBy
- Use the exact reviewer IDs: %s
- If a file path or line number is mentioned, include it; otherwise omit the field
- Severity: critical = blocks merge, high = should fix, medium = worth fixing, low = minor, nitpick = style only`, reviewText, reviewerIDsStr)

	systemPrompt := "You extract structured issues from code review text. Output only valid JSON."
	chatOpts := &provider.ChatOptions{DisableTools: true}
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		display.OnWaiting("structurizer")

		var prompt string
		if attempt == 1 {
			prompt = basePrompt
		} else {
			prompt = fmt.Sprintf(`Your previous response was not valid JSON. Output ONLY a fenced JSON block with the issues array. No other text.

Here are the review discussions again:

%s

Required JSON format:
`+"```json"+`
{"issues": [{"severity": "critical|high|medium|low|nitpick", "category": "string", "file": "path", "title": "summary", "description": "details", "raisedBy": ["%s"]}]}
`+"```"+`
Use reviewer IDs: %s`, reviewText, reviewerIDs[0], reviewerIDsStr)
		}

		msgs := []provider.Message{{Role: "user", Content: prompt}}
		response, err := o.summarizer.Provider.Chat(ctx, msgs, systemPrompt, chatOpts)
		if err != nil {
			continue
		}
		o.trackTokens("summarizer", prompt, response)

		parsed := ParseReviewerOutput(response)
		if parsed != nil && len(parsed.Issues) > 0 {
			result := make([]MergedIssue, 0, len(parsed.Issues))
			for _, issue := range parsed.Issues {
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
	}

	return nil
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
