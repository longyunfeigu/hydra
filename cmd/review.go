package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/guwanhua/hydra/internal/config"
	appctx "github.com/guwanhua/hydra/internal/context"
	"github.com/guwanhua/hydra/internal/display"
	ghub "github.com/guwanhua/hydra/internal/github"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/provider"
	"github.com/guwanhua/hydra/internal/util"
	"github.com/spf13/cobra"
)

var reviewCmd = &cobra.Command{
	Use:   "review [pr-number-or-url]",
	Short: "Review code changes with multiple AI reviewers",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runReview,
}

func init() {
	f := reviewCmd.Flags()
	f.StringP("config", "c", "", "Path to config file")
	f.IntP("rounds", "r", 0, "Maximum debate rounds (overrides config)")
	f.StringP("output", "o", "", "Output to file")
	f.StringP("format", "f", "markdown", "Output format (markdown|json)")
	f.Bool("no-converge", false, "Disable convergence detection")
	f.BoolP("local", "l", false, "Review local uncommitted changes")
	f.String("branch", "", "Review current branch vs base")
	f.StringSlice("files", nil, "Review specific files")
	f.String("reviewers", "", "Comma-separated reviewer IDs")
	f.BoolP("all", "a", false, "Use all reviewers")
	f.Bool("skip-context", false, "Skip context gathering")
	f.Bool("no-post", false, "Skip GitHub comment flow")
}

type reviewTarget struct {
	Type   string // "pr", "local", "branch", "files"
	Label  string
	Prompt string
	Repo   string // for PR reviews
}

func runReview(cmd *cobra.Command, args []string) error {
	// Set up context with Ctrl+C cancellation
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	d := display.New()
	d.SpinnerStart("Loading configuration...")

	configPath, _ := cmd.Flags().GetString("config")
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		d.SpinnerFail("Configuration error")
		return err
	}
	d.SpinnerSucceed("Configuration loaded")

	// Determine review target
	target, err := resolveTarget(cmd, args, d)
	if err != nil {
		return err
	}

	// Determine which reviewers to use
	allIDs := make([]string, 0, len(cfg.Reviewers))
	for id := range cfg.Reviewers {
		allIDs = append(allIDs, id)
	}

	selectedIDs, err := selectReviewerIDs(cmd, allIDs)
	if err != nil {
		return err
	}

	// Create reviewers
	reviewers := make([]orchestrator.Reviewer, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		rc := cfg.Reviewers[id]
		p, err := provider.CreateProvider(rc.Model, cfg)
		if err != nil {
			return fmt.Errorf("failed to create provider for reviewer %s: %w", id, err)
		}
		reviewers = append(reviewers, orchestrator.Reviewer{
			ID:           id,
			Provider:     p,
			SystemPrompt: rc.Prompt,
		})
	}

	// Create analyzer
	analyzerProvider, err := provider.CreateProvider(cfg.Analyzer.Model, cfg)
	if err != nil {
		return fmt.Errorf("failed to create analyzer provider: %w", err)
	}
	analyzer := orchestrator.Reviewer{
		ID:           "analyzer",
		Provider:     analyzerProvider,
		SystemPrompt: cfg.Analyzer.Prompt,
	}

	// Create summarizer
	summarizerProvider, err := provider.CreateProvider(cfg.Summarizer.Model, cfg)
	if err != nil {
		return fmt.Errorf("failed to create summarizer provider: %w", err)
	}
	summarizer := orchestrator.Reviewer{
		ID:           "summarizer",
		Provider:     summarizerProvider,
		SystemPrompt: cfg.Summarizer.Prompt,
	}

	// Create context gatherer
	skipContext, _ := cmd.Flags().GetBool("skip-context")
	var contextGatherer orchestrator.ContextGathererInterface
	if !skipContext && cfg.ContextGatherer != nil && cfg.ContextGatherer.Enabled {
		contextModel := cfg.ContextGatherer.Model
		if contextModel == "" {
			contextModel = cfg.Analyzer.Model
		}
		contextProvider, err := provider.CreateProvider(contextModel, cfg)
		if err != nil {
			util.Warnf("Failed to create context gatherer provider: %v", err)
		} else {
			contextGatherer = appctx.NewContextGathererAdapter(contextProvider, cfg.ContextGatherer)
		}
	}

	// Calculate max rounds
	maxRounds := cfg.Defaults.MaxRounds
	if r, _ := cmd.Flags().GetInt("rounds"); r > 0 {
		maxRounds = r
	}
	isSolo := len(reviewers) == 1
	if isSolo {
		maxRounds = 1
	}

	noConverge, _ := cmd.Flags().GetBool("no-converge")
	checkConvergence := !isSolo && !noConverge && cfg.Defaults.CheckConvergence

	// Display review header
	d.ReviewHeader(target.Label, selectedIDs, maxRounds, checkConvergence, contextGatherer != nil)

	// Set up orchestrator
	oCfg := orchestrator.OrchestratorConfig{
		Reviewers:       reviewers,
		Analyzer:        analyzer,
		Summarizer:      summarizer,
		ContextGatherer: contextGatherer,
		Options: orchestrator.OrchestratorOptions{
			MaxRounds:        maxRounds,
			CheckConvergence: checkConvergence,
		},
	}

	orch := orchestrator.New(oCfg)
	result, err := orch.RunStreaming(ctx, target.Label, target.Prompt, d)
	if err != nil {
		return fmt.Errorf("review failed: %w", err)
	}

	// Display final conclusion
	d.FinalConclusion(result.FinalConclusion)

	// Display issues table
	if len(result.ParsedIssues) > 0 {
		d.IssuesTable(result.ParsedIssues)
	}

	// Post comments to GitHub
	noPost, _ := cmd.Flags().GetBool("no-post")
	if !noPost && target.Type == "pr" && len(result.ParsedIssues) > 0 {
		prNum := extractPRNumber(target.Label)
		if prNum != "" {
			d.SpinnerStart("Posting comments to GitHub...")
			ghIssues := convertIssuesToGitHub(result.ParsedIssues)
			postResult := ghub.PostIssuesAsComments(prNum, ghIssues, target.Repo)
			d.SpinnerSucceed(fmt.Sprintf("Posted %d comments (%d inline, %d file-level, %d global, %d failed, %d skipped)",
				postResult.Posted, postResult.Inline, postResult.FileLevel, postResult.Global, postResult.Failed, postResult.Skipped))
		}
	}

	// Display token usage
	d.TokenUsage(result.TokenUsage, result.ConvergedAtRound)

	// Save output to file
	outputFile, _ := cmd.Flags().GetString("output")
	if outputFile != "" {
		format, _ := cmd.Flags().GetString("format")
		if err := saveOutput(outputFile, format, result); err != nil {
			return fmt.Errorf("failed to save output: %w", err)
		}
		color.Green("\n  Output saved to: %s", outputFile)
	}

	_ = ctx // consume ctx to avoid unused warning
	return nil
}

func resolveTarget(cmd *cobra.Command, args []string, d *display.Display) (*reviewTarget, error) {
	isLocal, _ := cmd.Flags().GetBool("local")
	branchBase, _ := cmd.Flags().GetString("branch")
	files, _ := cmd.Flags().GetStringSlice("files")

	if isLocal {
		return resolveLocalTarget(d)
	}
	if cmd.Flags().Changed("branch") {
		if branchBase == "" {
			branchBase = "main"
		}
		return resolveBranchTarget(branchBase)
	}
	if len(files) > 0 {
		return &reviewTarget{
			Type:   "files",
			Label:  fmt.Sprintf("Files: %s", strings.Join(files, ", ")),
			Prompt: fmt.Sprintf("Review the following files: %s.", strings.Join(files, ", ")),
		}, nil
	}
	if len(args) > 0 {
		return resolvePRTarget(args[0])
	}

	return nil, fmt.Errorf("please specify a PR number or use --local, --branch, or --files")
}

func resolveLocalTarget(d *display.Display) (*reviewTarget, error) {
	diff, err := exec.Command("git", "diff", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository or git is not available")
	}

	diffStr := string(diff)
	label := "Local Changes"
	isLastCommit := false

	if strings.TrimSpace(diffStr) == "" {
		diff, err = exec.Command("git", "diff", "HEAD~1", "HEAD").Output()
		if err != nil || strings.TrimSpace(string(diff)) == "" {
			return nil, fmt.Errorf("no changes found. Make some changes or commits first")
		}
		diffStr = string(diff)
		isLastCommit = true
		commitMsg, _ := exec.Command("git", "log", "-1", "--pretty=%s").Output()
		label = fmt.Sprintf("Last Commit: %s", strings.TrimSpace(string(commitMsg)))
	}

	var prompt string
	if isLastCommit {
		prompt = fmt.Sprintf("Please review the following code changes from the last commit:\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.", diffStr)
	} else {
		prompt = fmt.Sprintf("Please review the following local code changes (uncommitted diff):\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.", diffStr)
	}

	return &reviewTarget{Type: "local", Label: label, Prompt: prompt}, nil
}

func resolveBranchTarget(baseBranch string) (*reviewTarget, error) {
	currentBranch, err := exec.Command("git", "branch", "--show-current").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get current branch: %w", err)
	}
	branch := strings.TrimSpace(string(currentBranch))

	diff, err := exec.Command("git", "diff", baseBranch+"..."+branch).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get branch diff: %w", err)
	}

	diffStr := string(diff)
	if strings.TrimSpace(diffStr) == "" {
		return nil, fmt.Errorf("no differences found between %s and %s", baseBranch, branch)
	}

	prompt := fmt.Sprintf("Please review the changes in branch \"%s\" compared to \"%s\":\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.", branch, baseBranch, diffStr)

	return &reviewTarget{
		Type:   "branch",
		Label:  fmt.Sprintf("Branch: %s", branch),
		Prompt: prompt,
	}, nil
}

func resolvePRTarget(pr string) (*reviewTarget, error) {
	var prNumber, prURL, prRepo string

	if strings.HasPrefix(pr, "http") {
		prURL = pr
		re := regexp.MustCompile(`/pull/(\d+)`)
		if m := re.FindStringSubmatch(pr); len(m) > 1 {
			prNumber = m[1]
		} else {
			prNumber = pr
		}
		reRepo := regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/`)
		if m := reRepo.FindStringSubmatch(pr); len(m) > 1 {
			prRepo = m[1]
		}
	} else {
		prNumber = pr
		// Use gh to resolve PR URL (handles forks)
		out, err := exec.Command("gh", "pr", "view", prNumber, "--json", "url", "--jq", ".url").Output()
		if err == nil {
			prURL = strings.TrimSpace(string(out))
			reRepo := regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/`)
			if m := reRepo.FindStringSubmatch(prURL); len(m) > 1 {
				prRepo = m[1]
			}
		}
		if prURL == "" {
			// Fallback: try to detect from git remote
			out, err := exec.Command("git", "remote", "get-url", "origin").Output()
			if err == nil {
				reRemote := regexp.MustCompile(`github\.com[:/]([^/]+/[^/.]+)`)
				if m := reRemote.FindStringSubmatch(string(out)); len(m) > 1 {
					prRepo = m[1]
					prURL = fmt.Sprintf("https://github.com/%s/pull/%s", prRepo, prNumber)
				}
			}
			if prURL == "" {
				prURL = fmt.Sprintf("PR #%s", prNumber)
			}
		}
	}

	// Pre-fetch PR diff
	var prDiff, prTitle, prBody string
	diffOut, err := exec.Command("gh", "pr", "diff", prURL).Output()
	if err == nil {
		prDiff = string(diffOut)
	}

	infoOut, err := exec.Command("gh", "pr", "view", prURL, "--json", "title,body").Output()
	if err == nil {
		var info struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if json.Unmarshal(infoOut, &info) == nil {
			prTitle = info.Title
			prBody = info.Body
		}
	}

	var prompt string
	if prDiff != "" {
		prompt = fmt.Sprintf("Please review %s.\n\nTitle: %s\n\nDescription:\n%s\n\nHere is the full PR diff:\n\n```diff\n%s```\n\nAnalyze these changes and provide your feedback. You already have the complete diff above — do NOT attempt to fetch it again.",
			prURL, prTitle, prBody, prDiff)
	} else {
		prompt = fmt.Sprintf("Please review %s. Get the PR details and diff using any method available to you, then analyze the changes.", prURL)
	}

	return &reviewTarget{
		Type:   "pr",
		Label:  fmt.Sprintf("PR #%s", prNumber),
		Prompt: prompt,
		Repo:   prRepo,
	}, nil
}

func selectReviewerIDs(cmd *cobra.Command, allIDs []string) ([]string, error) {
	reviewersFlag, _ := cmd.Flags().GetString("reviewers")
	useAll, _ := cmd.Flags().GetBool("all")

	if reviewersFlag != "" {
		ids := strings.Split(reviewersFlag, ",")
		for i := range ids {
			ids[i] = strings.TrimSpace(ids[i])
		}
		idSet := make(map[string]bool)
		for _, id := range allIDs {
			idSet[id] = true
		}
		for _, id := range ids {
			if !idSet[id] {
				return nil, fmt.Errorf("unknown reviewer: %s (available: %s)", id, strings.Join(allIDs, ", "))
			}
		}
		return ids, nil
	}

	if useAll {
		return allIDs, nil
	}

	// Default: use all reviewers
	return allIDs, nil
}

func extractPRNumber(label string) string {
	re := regexp.MustCompile(`\d+`)
	if m := re.FindString(label); m != "" {
		return m
	}
	return ""
}

func saveOutput(path, format string, result *orchestrator.DebateResult) error {
	var data []byte
	var err error

	if format == "json" {
		data, err = json.MarshalIndent(result, "", "  ")
	} else {
		data = []byte(display.FormatMarkdown(result))
	}
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func convertIssuesToGitHub(issues []orchestrator.MergedIssue) []ghub.IssueForComment {
	result := make([]ghub.IssueForComment, 0, len(issues))
	for _, issue := range issues {
		raisedBy := ""
		if len(issue.RaisedBy) > 0 {
			raisedBy = strings.Join(issue.RaisedBy, ", ")
		}
		result = append(result, ghub.IssueForComment{
			File:         issue.File,
			Line:         issue.Line,
			Title:        issue.Title,
			Description:  issue.Description,
			Severity:     issue.Severity,
			SuggestedFix: issue.SuggestedFix,
			RaisedBy:     raisedBy,
		})
	}
	return result
}

// Ensure reviewers flag string is used with GetString
var _ = strconv.Itoa
