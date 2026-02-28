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

// Display handles all terminal output for the review process.
type Display struct {
	spin            *spinner.Spinner
	currentReviewer string
	currentRound    int
	maxRounds       int
}

// New creates a new Display instance.
func New() *Display {
	s := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
	return &Display{
		spin:         s,
		currentRound: 1,
	}
}

// --- Spinner methods ---

// SpinnerStart starts the spinner with the given text.
func (d *Display) SpinnerStart(text string) {
	d.spin.Suffix = "  " + text
	d.spin.Start()
}

// SpinnerSucceed stops the spinner and prints a success message.
func (d *Display) SpinnerSucceed(text string) {
	d.spin.Stop()
	color.Green("  %s %s", color.GreenString("✓"), text)
}

// SpinnerFail stops the spinner and prints a failure message.
func (d *Display) SpinnerFail(text string) {
	d.spin.Stop()
	color.Red("  %s %s", color.RedString("✗"), text)
}

// SpinnerStop stops the spinner without printing anything.
func (d *Display) SpinnerStop() {
	d.spin.Stop()
}

// --- Review lifecycle ---

// SetMaxRounds updates the maximum round count for display purposes.
func (d *Display) SetMaxRounds(maxRounds int) {
	d.maxRounds = maxRounds
}

// ReviewHeader prints the review header with configuration details.
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

// --- DisplayCallbacks interface methods ---

// OnWaiting shows a spinner while waiting for a reviewer/analyzer/summarizer.
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

// OnMessage displays a reviewer's response.
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

// OnParallelStatus updates the spinner to show parallel execution progress.
func (d *Display) OnParallelStatus(round int, statuses []orchestrator.ReviewerStatus) {
	statusLine := formatParallelStatus(round, statuses)
	joke := getRandomJoke()
	d.spin.Suffix = fmt.Sprintf("  %s | %s", statusLine, color.HiBlackString(joke))
}

// OnRoundComplete displays round completion status.
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

// OnConvergenceJudgment shows the judge's reasoning.
func (d *Display) OnConvergenceJudgment(verdict string, reasoning string) {
	if reasoning == "" {
		return
	}
	lines := strings.Split(reasoning, "\n")
	for _, line := range lines {
		fmt.Println(color.HiBlackString("│ %s", line))
	}
}

// OnContextGathered displays the gathered context information.
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

// --- Result display methods ---

// FinalConclusion shows the final review conclusion.
func (d *Display) FinalConclusion(text string) {
	d.spin.Stop()

	color.New(color.FgGreen, color.Bold).Printf("\n%s\n", strings.Repeat("═", 50))
	color.New(color.FgGreen, color.Bold).Printf("  Final Conclusion\n")
	color.New(color.FgGreen, color.Bold).Printf("%s\n\n", strings.Repeat("═", 50))

	rendered := RenderTerminalMarkdown(text)
	fmt.Print(rendered)
}

// IssuesTable shows structured issues found during review.
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

// TokenUsageDisplay shows token usage statistics.
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

// --- Helpers ---

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

func formatNumber(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1000000 {
		return fmt.Sprintf("%d,%03d", n/1000, n%1000)
	}
	return fmt.Sprintf("%d,%03d,%03d", n/1000000, (n/1000)%1000, n%1000)
}

// Cold jokes displayed while waiting for AI responses.
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

func getRandomJoke() string {
	return coldJokes[rand.Intn(len(coldJokes))]
}
