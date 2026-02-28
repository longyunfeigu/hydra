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

// tokenCount tracks input/output tokens for a reviewer.
type tokenCount struct {
	input  int
	output int
}

// DebateOrchestrator runs a multi-reviewer debate on a code review task.
type DebateOrchestrator struct {
	reviewers       []Reviewer
	analyzer        Reviewer
	summarizer      Reviewer
	contextGatherer ContextGathererInterface
	options         OrchestratorOptions

	conversationHistory []DebateMessage
	tokenUsage          map[string]*tokenCount
	analysis            string
	gatheredContext      *GatheredContext
	taskPrompt          string
	lastSeenIndex       map[string]int

	mu sync.Mutex // protects tokenUsage
}

// New creates a new DebateOrchestrator from the given config.
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

// RunStreaming runs the full debate loop with parallel reviewer execution.
func (o *DebateOrchestrator) RunStreaming(ctx context.Context, label, prompt string, display DisplayCallbacks) (*DebateResult, error) {
	// Reset state
	o.conversationHistory = nil
	o.tokenUsage = make(map[string]*tokenCount)
	o.lastSeenIndex = make(map[string]int)
	o.analysis = ""
	o.gatheredContext = nil
	o.taskPrompt = prompt

	var convergedAtRound *int

	// Start sessions for providers that support it
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

	defer o.endAllSessions()

	// Phase 1: Run context gathering and analysis in parallel
	g, gctx := errgroup.WithContext(ctx)

	// Context gathering
	if o.contextGatherer != nil {
		g.Go(func() error {
			display.OnWaiting("context-gatherer")
			diff := extractDiffFromPrompt(prompt)
			gathered, err := o.contextGatherer.Gather(diff, label, "main")
			if err != nil {
				// Context gathering failure is non-fatal
				return nil
			}
			o.gatheredContext = gathered
			return nil
		})
	}

	// Pre-analysis
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

	// Notify display about gathered context
	if o.gatheredContext != nil {
		display.OnContextGathered(o.gatheredContext)
	}

	// Phase 2: Debate rounds
	for round := 1; round <= o.options.MaxRounds; round++ {
		// Build messages for ALL reviewers BEFORE execution (snapshot, same info)
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

		// Initialize status tracking
		statuses := make([]ReviewerStatus, len(o.reviewers))
		for i, r := range o.reviewers {
			statuses[i] = ReviewerStatus{
				ReviewerID: r.ID,
				Status:     "pending",
			}
		}

		display.OnWaiting(fmt.Sprintf("round-%d", round))
		display.OnParallelStatus(round, statuses)

		// Execute all reviewers in parallel
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
				// Mark as thinking
				startTime := time.Now().UnixMilli()
				statuses[i] = ReviewerStatus{
					ReviewerID: task.reviewer.ID,
					Status:     "thinking",
					StartTime:  startTime,
				}
				display.OnParallelStatus(round, copyStatuses(statuses))

				// Stream response
				ch, errCh := task.reviewer.Provider.ChatStream(rgctx, task.messages, task.reviewer.SystemPrompt)
				var sb strings.Builder
				for chunk := range ch {
					sb.WriteString(chunk)
				}
				if err := <-errCh; err != nil {
					return fmt.Errorf("reviewer %s failed: %w", task.reviewer.ID, err)
				}

				// Mark as done
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

		// Add results to history and notify display (after all complete)
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

		// Check convergence (round >= 2 and not the last round)
		converged := false
		if o.options.CheckConvergence && round >= 2 && round < o.options.MaxRounds {
			display.OnWaiting("convergence-check")
			var err error
			converged, err = o.checkConvergence(ctx, display)
			if err != nil {
				// Convergence check failure is non-fatal
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

	// Phase 3: Collect summaries and conclusion
	display.OnWaiting("summarizer")
	summaries, err := o.collectSummaries(ctx)
	if err != nil {
		return nil, fmt.Errorf("collecting summaries: %w", err)
	}

	finalConclusion, err := o.getFinalConclusion(ctx, summaries)
	if err != nil {
		return nil, fmt.Errorf("final conclusion: %w", err)
	}

	// End summarizer session before structurization for clean JSON extraction
	if sp, ok := o.summarizer.Provider.(provider.SessionProvider); ok {
		sp.EndSession()
	}

	// Extract structured issues (with retry)
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

// buildMessages constructs the message list for a reviewer based on round.
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

	// Round 1: independent review
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

	// Round 2+: see previous rounds
	myMessageCount := 0
	for _, m := range o.conversationHistory {
		if m.ReviewerID == currentReviewerID {
			myMessageCount++
		}
	}

	// Get messages from previous rounds only
	messageCountByReviewer := make(map[string]int)
	var previousRoundsMessages []DebateMessage
	for _, msg := range o.conversationHistory {
		if msg.ReviewerID == currentReviewerID {
			continue // Exclude own messages
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
		// Session mode: send only new messages from the latest round
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

	// Non-session mode: full context with all previous rounds
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

	// Add previous rounds messages
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

	// Add own previous messages as assistant
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

// checkConvergence asks the summarizer to judge if reviewers have converged.
func (o *DebateOrchestrator) checkConvergence(ctx context.Context, display DisplayCallbacks) (bool, error) {
	if len(o.conversationHistory) < len(o.reviewers) {
		return false, nil
	}

	roundsCompleted := len(o.conversationHistory) / len(o.reviewers)
	if roundsCompleted < 2 {
		return false, nil
	}

	// Get last round's messages
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

	// Parse response
	lines := strings.Split(strings.TrimSpace(response), "\n")
	lastLine := strings.TrimSpace(lines[len(lines)-1])
	verdict := strings.ToUpper(strings.Fields(lastLine)[0])
	isConverged := verdict == "CONVERGED"

	// Extract reasoning (everything except the last line)
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

// collectSummaries asks each reviewer for a final summary.
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

// getFinalConclusion asks the summarizer to produce a final conclusion from reviewer summaries.
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

// structurizeIssues uses AI to extract structured issues from review text.
// Retries up to 3 times if JSON parsing fails.
func (o *DebateOrchestrator) structurizeIssues(ctx context.Context, display DisplayCallbacks) []MergedIssue {
	// Collect last message from each reviewer
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

// Helper methods

func (o *DebateOrchestrator) findReviewer(id string) *Reviewer {
	for i := range o.reviewers {
		if o.reviewers[i].ID == id {
			return &o.reviewers[i]
		}
	}
	return nil
}

func (o *DebateOrchestrator) markAsSeen(reviewerID string) {
	o.lastSeenIndex[reviewerID] = len(o.conversationHistory) - 1
}

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

// GetReviewers exposes reviewers for post-review discussion.
func (o *DebateOrchestrator) GetReviewers() []Reviewer {
	return o.reviewers
}

// estimateTokens estimates token count from text.
// CJK characters ~0.7 tokens/char, English ~0.25 tokens/char.
func estimateTokens(text string) int {
	cjkCount := 0
	for _, r := range text {
		if isCJK(r) {
			cjkCount++
		}
	}
	return int(math.Ceil(float64(cjkCount)*0.7 + float64(len(text)-cjkCount)/4.0))
}

// isCJK checks if a rune is a CJK character.
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		(r >= 0x3000 && r <= 0x303F) || // CJK Symbols and Punctuation
		(r >= 0xFF00 && r <= 0xFFEF) // Halfwidth and Fullwidth Forms
}

var diffFenceRe = regexp.MustCompile("(?s)```diff\\n(.*?)```")

// extractDiffFromPrompt extracts diff content from a prompt containing ```diff blocks.
func extractDiffFromPrompt(prompt string) string {
	if m := diffFenceRe.FindStringSubmatch(prompt); len(m) > 1 {
		return m[1]
	}
	return prompt
}

// copyStatuses creates a shallow copy of a ReviewerStatus slice for safe display callback.
func copyStatuses(statuses []ReviewerStatus) []ReviewerStatus {
	cp := make([]ReviewerStatus, len(statuses))
	copy(cp, statuses)
	return cp
}

// pluralS returns "s" if plural, empty string otherwise.
func pluralS(plural bool) string {
	if plural {
		return "s"
	}
	return ""
}
