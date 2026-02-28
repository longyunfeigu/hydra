// Package display 负责处理 Hydra 代码审查过程中的所有终端输出和显示逻辑。
// 包括进度旋转动画、审查状态展示、结果表格、Token 用量统计等。
package display

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/fatih/color"
	"github.com/guwanhua/hydra/internal/orchestrator"
)

// Display 负责管理审查过程中的所有终端输出。
// 包含旋转动画器（spinner）用于显示等待状态，
// 以及当前审查者和轮次信息用于格式化输出。
type Display struct {
	spin            *spinner.Spinner // 终端旋转动画器，用于显示等待/处理中状态
	currentReviewer string          // 当前正在展示输出的审查者 ID
	currentRound    int             // 当前审查轮次
	maxRounds       int             // 最大审查轮次数
}

// New 创建一个新的 Display 实例。
// 初始化旋转动画器（使用字符集 14，120ms 刷新间隔），初始轮次设为 1。
func New() *Display {
	s := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
	return &Display{
		spin:         s,
		currentRound: 1,
	}
}

// --- 旋转动画方法 ---

// SpinnerStart 启动旋转动画并显示指定文本。
// 用于在等待 AI 响应等耗时操作时向用户显示进度。
func (d *Display) SpinnerStart(text string) {
	d.spin.Suffix = "  " + text
	d.spin.Start()
}

// SpinnerSucceed 停止旋转动画并显示绿色成功消息。
func (d *Display) SpinnerSucceed(text string) {
	d.spin.Stop()
	color.Green("  %s %s", color.GreenString("✓"), text)
}

// SpinnerFail 停止旋转动画并显示红色失败消息。
func (d *Display) SpinnerFail(text string) {
	d.spin.Stop()
	color.Red("  %s %s", color.RedString("✗"), text)
}

// SpinnerStop 停止旋转动画但不打印任何消息。
func (d *Display) SpinnerStop() {
	d.spin.Stop()
}

// --- 审查生命周期方法 ---

// SetMaxRounds 更新最大轮次数，用于显示目的。
func (d *Display) SetMaxRounds(maxRounds int) {
	d.maxRounds = maxRounds
}

// ReviewHeader 打印审查头部信息，包含配置详情。
// 展示审查目标、审查者列表、最大轮次数以及是否启用收敛检查和上下文收集。
func (d *Display) ReviewHeader(label string, reviewerIDs []string, maxRounds int, checkConvergence, contextEnabled bool) {
	d.maxRounds = maxRounds

	fmt.Println()
	color.Cyan("  %s", strings.Repeat("=", 50))
	color.New(color.FgCyan, color.Bold).Printf("  Hydra Code Review\n")
	color.Cyan("  %s", strings.Repeat("=", 50))
	fmt.Println()

	color.White("  Target:      %s", label)
	color.White("  Reviewers:   %s", strings.Join(reviewerIDs, ", "))
	color.White("  Max Rounds:  %d", maxRounds)

	if checkConvergence {
		color.White("  Convergence: enabled")
	}
	if contextEnabled {
		color.White("  Context:     enabled")
	}

	fmt.Println()
}

// --- DisplayCallbacks 接口方法 ---
// 以下方法实现了 orchestrator 的回调接口，用于在审查过程的各个阶段更新终端显示。

// OnWaiting 在等待审查者、分析器或摘要器响应时显示旋转动画。
// 根据不同的 reviewerID 显示不同的等待提示文本，并附带随机冷笑话缓解等待焦虑。
func (d *Display) OnWaiting(reviewerID string) {
	d.spin.Stop()

	if reviewerID == "convergence-check" {
		color.New(color.FgYellow, color.Bold).Printf("\n┌─ Convergence Judge %s\n", strings.Repeat("─", 30))
	}

	var label string
	switch {
	case reviewerID == "context-gatherer":
		label = "Gathering system context"
	case reviewerID == "analyzer":
		label = "Analyzing changes"
	case reviewerID == "summarizer":
		label = "Generating final summary"
	case reviewerID == "convergence-check":
		label = "Evaluating if reviewers reached consensus"
	case reviewerID == "structurizer":
		label = "Extracting structured issues"
	case strings.HasPrefix(reviewerID, "round-"):
		roundNum := strings.TrimPrefix(reviewerID, "round-")
		label = fmt.Sprintf("Round %s: Starting parallel review", roundNum)
	default:
		label = fmt.Sprintf("%s is thinking", reviewerID)
	}

	joke := getRandomJoke()
	d.spin.Suffix = fmt.Sprintf("  %s... | %s", label, color.HiBlackString(joke))
	d.spin.Start()
}

// OnMessage 显示审查者的响应内容。
// 当审查者切换时打印新的审查者标题头，然后渲染 Markdown 格式的响应内容。
func (d *Display) OnMessage(reviewerID string, content string) {
	d.spin.Stop()

	if reviewerID != d.currentReviewer {
		d.currentReviewer = reviewerID
		if reviewerID == "analyzer" {
			color.New(color.FgMagenta, color.Bold).Printf("\n%s\n", strings.Repeat("─", 50))
			color.New(color.FgMagenta, color.Bold).Printf("  Analysis\n")
			color.New(color.FgMagenta, color.Bold).Printf("%s\n\n", strings.Repeat("─", 50))
		} else {
			color.New(color.FgCyan, color.Bold).Printf("\n┌─ %s ", reviewerID)
			fmt.Printf("%s\n", color.HiBlackString("[Round %d/%d]", d.currentRound, d.maxRounds))
			color.Cyan("│")
		}
	}

	rendered := RenderTerminalMarkdown(content)
	fmt.Print(rendered)
}

// OnParallelStatus 更新旋转动画以显示并行执行的进度。
// 展示每个审查者的状态（已完成/思考中/等待中）和耗时。
func (d *Display) OnParallelStatus(round int, statuses []orchestrator.ReviewerStatus) {
	statusLine := formatParallelStatus(round, statuses)
	joke := getRandomJoke()
	d.spin.Suffix = fmt.Sprintf("  %s | %s", statusLine, color.HiBlackString(joke))
}

// OnRoundComplete 显示审查轮次完成状态。
// 如果审查者达成共识（converged=true），显示绿色的 CONVERGED 标记并提示提前结束；
// 否则显示红色的 NOT CONVERGED 标记，继续下一轮。
func (d *Display) OnRoundComplete(round int, converged bool) {
	fmt.Println()
	if converged {
		fmt.Printf("%s %s\n", color.YellowString("└─ Verdict:"), color.New(color.FgGreen, color.Bold).Sprint("CONVERGED"))
		color.New(color.FgGreen, color.Bold).Printf("\n  Round %d/%d - CONSENSUS REACHED\n", round, d.maxRounds)
		color.Green("   Stopping early to save tokens.\n")
	} else {
		fmt.Printf("%s %s\n", color.YellowString("└─ Verdict:"), color.New(color.FgRed, color.Bold).Sprint("NOT CONVERGED"))
		fmt.Printf("\n%s\n\n", color.HiBlackString("── Round %d/%d complete ──", round, d.maxRounds))
	}
	d.currentRound = round + 1
}

// OnConvergenceJudgment 展示收敛判断者的推理过程。
// 以灰色文本逐行显示判断理由，帮助用户理解为何审查提前结束或继续。
func (d *Display) OnConvergenceJudgment(verdict string, reasoning string) {
	if reasoning == "" {
		return
	}
	lines := strings.Split(reasoning, "\n")
	for _, line := range lines {
		fmt.Println(color.HiBlackString("│ %s", line))
	}
}

// OnContextGathered 展示收集到的系统上下文信息。
// 包括受影响模块（按影响级别着色）、关联 PR 列表和 AI 生成的上下文摘要。
func (d *Display) OnContextGathered(ctx *orchestrator.GatheredContext) {
	d.spin.Stop()

	color.New(color.FgMagenta, color.Bold).Printf("\n%s\n", strings.Repeat("─", 50))
	color.New(color.FgMagenta, color.Bold).Printf("  System Context\n")
	color.New(color.FgMagenta, color.Bold).Printf("%s\n\n", strings.Repeat("─", 50))

	if len(ctx.AffectedModules) > 0 {
		fmt.Println(color.HiBlackString("Affected Modules:"))
		for _, mod := range ctx.AffectedModules {
			var impact string
			switch mod.ImpactLevel {
			case "core":
				impact = color.RedString("●")
			case "moderate":
				impact = color.YellowString("●")
			default:
				impact = color.GreenString("●")
			}
			fmt.Printf("  %s %s (%d files)\n", impact, color.HiBlackString(mod.Name), len(mod.AffectedFiles))
		}
		fmt.Println()
	}

	if len(ctx.RelatedPRs) > 0 {
		fmt.Println(color.HiBlackString("Related PRs:"))
		limit := len(ctx.RelatedPRs)
		if limit > 5 {
			limit = 5
		}
		for _, pr := range ctx.RelatedPRs[:limit] {
			fmt.Printf("  %s #%d: %s\n", color.HiBlackString("•"), pr.Number, color.HiBlackString(pr.Title))
		}
		fmt.Println()
	}

	if ctx.Summary != "" {
		rendered := RenderTerminalMarkdown(ctx.Summary)
		fmt.Print(rendered)
	}
}

// --- 结果展示方法 ---

// FinalConclusion 显示最终审查结论。
// 使用绿色粗体的双线分隔框突出显示，并渲染 Markdown 格式的结论文本。
func (d *Display) FinalConclusion(text string) {
	d.spin.Stop()

	color.New(color.FgGreen, color.Bold).Printf("\n%s\n", strings.Repeat("═", 50))
	color.New(color.FgGreen, color.Bold).Printf("  Final Conclusion\n")
	color.New(color.FgGreen, color.Bold).Printf("%s\n\n", strings.Repeat("═", 50))

	rendered := RenderTerminalMarkdown(text)
	fmt.Print(rendered)
}

// IssuesTable 以表格形式展示审查过程中发现的结构化问题。
// 按严重等级着色显示（critical=红色粗体, high=红色, medium=黄色, low=蓝色, nitpick=灰色），
// 包含问题标题、文件位置、提出者和修复建议（如果有）。
func (d *Display) IssuesTable(issues []orchestrator.MergedIssue) {
	totalRaw := 0
	for _, issue := range issues {
		totalRaw += len(issue.RaisedBy)
	}

	color.New(color.FgMagenta, color.Bold).Printf("\n%s\n", strings.Repeat("─", 50))
	color.New(color.FgMagenta, color.Bold).Printf("  Issues Found (%d unique, %d total across reviewers)\n", len(issues), totalRaw)
	color.New(color.FgMagenta, color.Bold).Printf("%s\n\n", strings.Repeat("─", 50))

	severityColor := map[string]func(string, ...interface{}) string{
		"critical": color.New(color.FgRed, color.Bold).Sprintf,
		"high":     color.RedString,
		"medium":   color.YellowString,
		"low":      color.BlueString,
		"nitpick":  color.HiBlackString,
	}

	for i, issue := range issues {
		colorFn, ok := severityColor[issue.Severity]
		if !ok {
			colorFn = color.WhiteString
		}

		location := issue.File
		if issue.Line != nil && *issue.Line > 0 {
			location = fmt.Sprintf("%s:%d", issue.File, *issue.Line)
		}

		reviewers := make([]string, len(issue.RaisedBy))
		for j, r := range issue.RaisedBy {
			reviewers[j] = color.CyanString(r)
		}

		fmt.Println(colorFn("  %2d. [%-8s] %s", i+1, strings.ToUpper(issue.Severity), issue.Title))
		fmt.Printf("      %s  [%s]\n", color.HiBlackString(location), strings.Join(reviewers, ", "))
		if issue.SuggestedFix != "" {
			fix := issue.SuggestedFix
			if len(fix) > 100 {
				fix = fix[:100] + "..."
			}
			color.Green("      Fix: %s", fix)
		}
		fmt.Println()
	}
}

// TokenUsage 展示 Token 使用量统计信息。
// 逐行显示每个审查者的输入/输出 Token 数，最后汇总显示总量和估算费用。
// 如果审查提前收敛，还会显示收敛轮次。
func (d *Display) TokenUsage(usage []orchestrator.TokenUsage, convergedAt *int) {
	fmt.Println(color.HiBlackString("\n%s", strings.Repeat("─", 50)))
	fmt.Println(color.HiBlackString("  Token Usage (Estimated)"))
	fmt.Println(color.HiBlackString("%s", strings.Repeat("─", 50)))

	var totalInput, totalOutput int
	var totalCost float64

	for _, u := range usage {
		totalInput += u.InputTokens
		totalOutput += u.OutputTokens
		totalCost += u.EstimatedCost

		pad := 12 - len(u.ReviewerID)
		if pad < 1 {
			pad = 1
		}
		fmt.Println(color.HiBlackString("  %s%s%8s in  %8s out",
			u.ReviewerID, strings.Repeat(" ", pad),
			formatNumber(u.InputTokens), formatNumber(u.OutputTokens)))
	}

	fmt.Println(color.HiBlackString("%s", strings.Repeat("─", 50)))
	color.Yellow("  Total%s%8s in  %8s out  ~$%.4f",
		strings.Repeat(" ", 6), formatNumber(totalInput), formatNumber(totalOutput), totalCost)

	if convergedAt != nil {
		color.Green("\n  ✓ Converged at round %d", *convergedAt)
	}
	fmt.Println()
}

// --- 辅助函数 ---

// formatParallelStatus 格式化并行审查的状态显示文本。
// 每个审查者用不同颜色标识状态：绿色=已完成（附耗时），黄色=思考中，灰色=等待中。
func formatParallelStatus(round int, statuses []orchestrator.ReviewerStatus) string {
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		switch s.Status {
		case "done":
			parts[i] = color.GreenString("✓ %s", s.ReviewerID) +
				color.HiBlackString(" (%.1fs)", s.Duration)
		case "thinking":
			parts[i] = color.YellowString("⋯ %s", s.ReviewerID)
		default:
			parts[i] = color.HiBlackString("○ %s", s.ReviewerID)
		}
	}
	return fmt.Sprintf("Round %d: [%s]", round, strings.Join(parts, " | "))
}

// formatNumber 将整数格式化为带千分位逗号的字符串。
// 例如：1234 -> "1,234"，1234567 -> "1,234,567"
func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d,%03d,%03d", n/1000000, (n/1000)%1000, n%1000)
}

// coldJokes 是等待 AI 响应时显示的程序员冷笑话集合。
// 在旋转动画旁随机展示，缓解用户等待时的无聊感。
var coldJokes = []string{
	"Why do programmers confuse Halloween and Christmas? Because Oct 31 = Dec 25",
	`A SQL query walks into a bar, walks up to two tables and asks: "Can I join you?"`,
	"Why do programmers hate nature? It has too many bugs.",
	"There are only 10 types of people: those who understand binary and those who don't",
	"Why do Java developers wear glasses? Because they can't C#",
	"Why did the developer go broke? Because he used up all his cache.",
	"99 little bugs in the code, take one down, patch it around... 127 little bugs in the code.",
	"There's no place like 127.0.0.1",
	"Why did the functions stop calling each other? They had too many arguments.",
	"I would tell you a UDP joke, but you might not get it.",
	"How many programmers does it take to change a light bulb? None, that's a hardware problem.",
	"The best thing about a boolean is that even if you're wrong, you're only off by a bit.",
	"In order to understand recursion, you must first understand recursion.",
	"There are two hard things in computer science: cache invalidation, naming things, and off-by-one errors.",
	"What's the object-oriented way to become wealthy? Inheritance.",
	"Debugging: Being the detective in a crime movie where you are also the murderer.",
	"It works on my machine! Then we'll ship your machine.",
	"Copy-paste is not a design pattern.",
	"Real programmers count from 0.",
	`Git commit -m "fixed it for real this time"`,
}

// getRandomJoke 从冷笑话集合中随机选取一条返回。
func getRandomJoke() string {
	return coldJokes[rand.Intn(len(coldJokes))]
}
