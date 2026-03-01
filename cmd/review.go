package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"

	"github.com/fatih/color"
	"github.com/guwanhua/hydra/internal/config"
	appctx "github.com/guwanhua/hydra/internal/context"
	"github.com/guwanhua/hydra/internal/display"
	"github.com/guwanhua/hydra/internal/orchestrator"
	"github.com/guwanhua/hydra/internal/platform"
	"github.com/guwanhua/hydra/internal/platform/detect"
	"github.com/guwanhua/hydra/internal/provider"
	"github.com/guwanhua/hydra/internal/util"
	"github.com/spf13/cobra"
)

// reviewCmd 定义了 "review" 子命令，这是 Hydra 的核心功能命令。
// 它接受一个可选的位置参数（PR/MR 编号或 URL），支持多种审查模式：
//   - 直接指定 PR/MR 编号或 URL 进行审查
//   - 使用 --local 审查本地未提交的变更
//   - 使用 --branch 审查当前分支与基准分支的差异
//   - 使用 --files 审查指定的文件列表
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
	f.StringP("branch", "b", "", "Review current branch vs base")
	f.StringSlice("files", nil, "Review specific files")
	f.String("reviewers", "", "Comma-separated reviewer IDs")
	f.BoolP("all", "a", false, "Use all reviewers")
	f.Bool("skip-context", false, "Skip context gathering")
	f.Bool("no-post", false, "Skip posting review comments")
}

// reviewTarget 封装了审查目标的所有必要信息。
type reviewTarget struct {
	Type   string // "pr"、"local"、"branch"、"files"
	Label  string // 显示标签，如 "PR #123"、"MR !456"
	Prompt string // AI 审查提示词
	Repo   string // 仓库标识（owner/repo 或 group/project）
}

func runReview(cmd *cobra.Command, args []string) error {
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

	// 检测平台
	var platformType, platformHost string
	if cfg.Platform != nil {
		platformType = cfg.Platform.Type
		platformHost = cfg.Platform.Host
	}
	plat, platErr := detect.FromRemote(platformType, platformHost)

	// 解析审查目标
	target, err := resolveTarget(cmd, args, d, plat)
	if err != nil {
		return err
	}

	allIDs := make([]string, 0, len(cfg.Reviewers))
	for id := range cfg.Reviewers {
		allIDs = append(allIDs, id)
	}

	selectedIDs, err := selectReviewerIDs(cmd, allIDs)
	if err != nil {
		return err
	}

	reviewers := make([]orchestrator.Reviewer, 0, len(selectedIDs))
	for _, id := range selectedIDs {
		rc := cfg.Reviewers[id]
		p, err := provider.CreateProvider(rc.Model, rc.ModelName, cfg)
		if err != nil {
			return fmt.Errorf("failed to create provider for reviewer %s: %w", id, err)
		}
		reviewers = append(reviewers, orchestrator.Reviewer{
			ID:           id,
			Provider:     p,
			SystemPrompt: rc.Prompt,
		})
	}

	analyzerProvider, err := provider.CreateProvider(cfg.Analyzer.Model, cfg.Analyzer.ModelName, cfg)
	if err != nil {
		return fmt.Errorf("failed to create analyzer provider: %w", err)
	}
	analyzer := orchestrator.Reviewer{
		ID:           "analyzer",
		Provider:     analyzerProvider,
		SystemPrompt: cfg.Analyzer.Prompt,
	}

	summarizerProvider, err := provider.CreateProvider(cfg.Summarizer.Model, cfg.Summarizer.ModelName, cfg)
	if err != nil {
		return fmt.Errorf("failed to create summarizer provider: %w", err)
	}
	summarizer := orchestrator.Reviewer{
		ID:           "summarizer",
		Provider:     summarizerProvider,
		SystemPrompt: cfg.Summarizer.Prompt,
	}

	skipContext, _ := cmd.Flags().GetBool("skip-context")
	var contextGatherer orchestrator.ContextGathererInterface
	if !skipContext && cfg.ContextGatherer != nil && cfg.ContextGatherer.Enabled {
		contextModel := cfg.ContextGatherer.Model
		if contextModel == "" {
			contextModel = cfg.Analyzer.Model
		}
		contextProvider, err := provider.CreateProvider(contextModel, "", cfg)
		if err != nil {
			util.Warnf("Failed to create context gatherer provider: %v", err)
		} else {
			contextGatherer = appctx.NewContextGathererAdapter(contextProvider, cfg.ContextGatherer, plat)
		}
	}

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

	d.ReviewHeader(target.Label, selectedIDs, maxRounds, checkConvergence, contextGatherer != nil)

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

	d.FinalConclusion(result.FinalConclusion)

	if len(result.ParsedIssues) > 0 {
		d.IssuesTable(result.ParsedIssues)
	}

	// 将审查发现的问题作为评论发布到 PR/MR
	noPost, _ := cmd.Flags().GetBool("no-post")
	if !noPost && target.Type == "pr" && len(result.ParsedIssues) > 0 && plat != nil {
		prNum := extractPRNumber(target.Label)
		if prNum != "" {
			d.SpinnerStart("Posting review comments...")
			platIssues := convertIssuesToPlatform(result.ParsedIssues)
			postResult := plat.PostIssuesAsComments(prNum, platIssues, target.Repo)
			d.SpinnerSucceed(fmt.Sprintf("Posted %d comments (%d inline, %d file-level, %d global, %d failed, %d skipped)",
				postResult.Posted, postResult.Inline, postResult.FileLevel, postResult.Global, postResult.Failed, postResult.Skipped))
		}
	} else if !noPost && target.Type == "pr" {
		// 诊断：打印跳过发布的原因
		if plat == nil {
			util.Warnf("Skipped posting comments: platform not detected (check platform config)")
		}
		if len(result.ParsedIssues) == 0 {
			util.Warnf("Skipped posting comments: no structured issues parsed from reviewers")
		}
	}

	d.TokenUsage(result.TokenUsage, result.ConvergedAtRound)

	outputFile, _ := cmd.Flags().GetString("output")
	if outputFile != "" {
		format, _ := cmd.Flags().GetString("format")
		if err := saveOutput(outputFile, format, result); err != nil {
			return fmt.Errorf("failed to save output: %w", err)
		}
		color.Green("\n  Output saved to: %s", outputFile)
	}

	_ = ctx
	_ = platErr // 平台检测失败时仅影响 PR 模式和评论发布
	return nil
}

// resolveTarget 根据命令行参数确定审查目标。
func resolveTarget(cmd *cobra.Command, args []string, d *display.Display, plat platform.Platform) (*reviewTarget, error) {
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
		return resolveMRTarget(args[0], plat)
	}

	return nil, fmt.Errorf("please specify a PR/MR number or use --local, --branch, or --files")
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

	annotatedDiff := platform.AnnotateDiffWithLineNumbers(diffStr)
	prompt := fmt.Sprintf("Please review the changes in branch \"%s\" compared to \"%s\":\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.\nWhen reporting issues, always reference the line number shown at the beginning of each line.", branch, baseBranch, annotatedDiff)

	return &reviewTarget{
		Type:   "branch",
		Label:  fmt.Sprintf("Branch: %s", branch),
		Prompt: prompt,
	}, nil
}

// resolveMRTarget 处理 PR/MR 审查模式的目标解析。
// 支持 GitHub PR URL、GitLab MR URL 以及纯编号。
func resolveMRTarget(input string, plat platform.Platform) (*reviewTarget, error) {
	var mrNumber, mrURL, mrRepo string

	if strings.HasPrefix(input, "http") {
		// 用户提供了完整 URL
		mrURL = input
		if plat != nil {
			repo, id, err := plat.ParseMRURL(input)
			if err == nil {
				mrNumber = id
				mrRepo = repo
			}
		}
		// 回退：尝试通用正则提取
		if mrNumber == "" {
			// GitHub: /pull/123
			re := regexp.MustCompile(`/pull/(\d+)`)
			if m := re.FindStringSubmatch(input); len(m) > 1 {
				mrNumber = m[1]
			}
			// GitLab: /-/merge_requests/123
			re2 := regexp.MustCompile(`/merge_requests/(\d+)`)
			if m := re2.FindStringSubmatch(input); len(m) > 1 {
				mrNumber = m[1]
			}
		}
		if mrNumber == "" {
			mrNumber = input
		}
	} else {
		// 用户提供了纯编号
		mrNumber = input
		if plat != nil {
			repo, err := plat.DetectRepoFromRemote()
			if err == nil {
				mrRepo = repo
				mrURL = plat.BuildMRURL(repo, mrNumber)
			}
		}
		if mrURL == "" {
			mrURL = fmt.Sprintf("MR/PR #%s", mrNumber)
		}
	}

	// 通过 Platform 接口获取 diff 和信息
	var mrDiff, mrTitle, mrBody string
	if plat != nil {
		if diff, err := plat.GetDiff(mrNumber, mrRepo); err == nil {
			mrDiff = diff
		}
		if info, err := plat.GetInfo(mrNumber, mrRepo); err == nil {
			mrTitle = info.Title
			mrBody = info.Description
		}
	}

	var prompt string
	if mrDiff != "" {
		annotatedDiff := platform.AnnotateDiffWithLineNumbers(mrDiff)
		prompt = fmt.Sprintf("Please review %s.\n\nTitle: %s\n\nDescription:\n%s\n\nHere is the full diff (each line is prefixed with its new-file line number for reference):\n\n```diff\n%s```\n\nAnalyze these changes and provide your feedback. You already have the complete diff above — do NOT attempt to fetch it again.\nWhen reporting issues, always reference the line number shown at the beginning of each line (e.g. \"line 263\").",
			mrURL, mrTitle, mrBody, annotatedDiff)
	} else {
		prompt = fmt.Sprintf("Please review %s. Get the details and diff using any method available to you, then analyze the changes.", mrURL)
	}

	// 确定标签格式
	label := fmt.Sprintf("PR #%s", mrNumber)
	if plat != nil && plat.Name() == "gitlab" {
		label = fmt.Sprintf("MR !%s", mrNumber)
	}

	return &reviewTarget{
		Type:   "pr",
		Label:  label,
		Prompt: prompt,
		Repo:   mrRepo,
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

// convertIssuesToPlatform 将编排器产出的 MergedIssue 列表转换为平台通用的评论格式。
func convertIssuesToPlatform(issues []orchestrator.MergedIssue) []platform.IssueForComment {
	result := make([]platform.IssueForComment, 0, len(issues))
	for _, issue := range issues {
		raisedBy := ""
		if len(issue.RaisedBy) > 0 {
			raisedBy = strings.Join(issue.RaisedBy, ", ")
		}
		result = append(result, platform.IssueForComment{
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
