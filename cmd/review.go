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

// reviewCmd 定义了 "review" 子命令，这是 Hydra 的核心功能命令。
// 它接受一个可选的位置参数（PR 编号或 URL），支持多种审查模式：
//   - 直接指定 GitHub PR 编号或 URL 进行审查
//   - 使用 --local 审查本地未提交的变更
//   - 使用 --branch 审查当前分支与基准分支的差异
//   - 使用 --files 审查指定的文件列表
var reviewCmd = &cobra.Command{
	Use:   "review [pr-number-or-url]",
	Short: "Review code changes with multiple AI reviewers",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runReview,
}

// init 注册 review 命令的所有命令行标志（flags）。
// 这些标志控制审查的行为，包括配置文件路径、辩论轮数、输出格式、
// 审查目标类型、审查者选择以及是否跳过上下文收集和 GitHub 评论发布。
func init() {
	f := reviewCmd.Flags()
	f.StringP("config", "c", "", "Path to config file")                // 指定配置文件路径，不指定则使用默认路径
	f.IntP("rounds", "r", 0, "Maximum debate rounds (overrides config)") // 覆盖配置中的最大辩论轮数
	f.StringP("output", "o", "", "Output to file")                     // 将审查结果输出到文件
	f.StringP("format", "f", "markdown", "Output format (markdown|json)") // 输出格式：markdown 或 json
	f.Bool("no-converge", false, "Disable convergence detection")      // 禁用收敛检测，强制完成所有辩论轮次
	f.BoolP("local", "l", false, "Review local uncommitted changes")   // 审查本地未提交的代码变更
	f.String("branch", "", "Review current branch vs base")            // 审查当前分支与指定基准分支的差异
	f.StringSlice("files", nil, "Review specific files")               // 审查指定的文件列表
	f.String("reviewers", "", "Comma-separated reviewer IDs")          // 以逗号分隔指定使用哪些审查者
	f.BoolP("all", "a", false, "Use all reviewers")                    // 使用配置中定义的所有审查者
	f.Bool("skip-context", false, "Skip context gathering")            // 跳过上下文收集阶段以加快审查速度
	f.Bool("no-post", false, "Skip GitHub comment flow")               // 跳过将审查结果发布为 GitHub 评论
}

// reviewTarget 封装了审查目标的所有必要信息。
// 它统一了不同审查模式（PR、本地变更、分支对比、文件列表）的数据结构，
// 使得 runReview 函数无需关心目标是如何解析出来的。
type reviewTarget struct {
	Type   string // 审查类型："pr"（GitHub PR）、"local"（本地变更）、"branch"（分支对比）、"files"（指定文件）
	Label  string // 用于显示的标签，如 "PR #123" 或 "Local Changes"
	Prompt string // 发送给 AI 审查者的完整提示词，包含代码差异和审查指令
	Repo   string // GitHub 仓库标识（owner/repo 格式），仅在 PR 审查模式下使用
}

// runReview 是 review 命令的主执行函数，协调整个代码审查流程。
// 完整的执行流程如下：
//  1. 加载配置文件并解析审查目标（PR/本地变更/分支/文件）
//  2. 根据用户选择创建 AI 审查者、分析器和总结器实例
//  3. 可选地创建上下文收集器，用于收集代码库的相关上下文信息
//  4. 通过编排器（orchestrator）执行多轮对抗式审查辩论
//  5. 展示最终结论、问题列表，并可选地将评论发布到 GitHub
//  6. 输出 token 使用统计，并可选地保存结果到文件
func runReview(cmd *cobra.Command, args []string) error {
	// 设置带有 Ctrl+C 取消支持的上下文，允许用户随时中断审查过程
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	d := display.New()
	d.SpinnerStart("Loading configuration...")

	// 加载配置文件：优先使用命令行指定的路径，否则使用默认配置路径
	configPath, _ := cmd.Flags().GetString("config")
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		d.SpinnerFail("Configuration error")
		return err
	}
	d.SpinnerSucceed("Configuration loaded")

	// 解析审查目标：根据命令行参数和标志确定要审查的内容（PR、本地变更、分支或文件）
	target, err := resolveTarget(cmd, args, d)
	if err != nil {
		return err
	}

	// 从配置中提取所有审查者的 ID 列表，用于后续的审查者选择
	allIDs := make([]string, 0, len(cfg.Reviewers))
	for id := range cfg.Reviewers {
		allIDs = append(allIDs, id)
	}

	// 根据用户指定的 --reviewers 或 --all 标志选择参与本次审查的审查者
	selectedIDs, err := selectReviewerIDs(cmd, allIDs)
	if err != nil {
		return err
	}

	// 为每个选中的审查者创建对应的 AI 提供者实例（如 claude-code、codex-cli 等）
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

	// 创建分析器：在审查开始前对代码变更进行预分析，为审查者提供重点关注方向
	analyzerProvider, err := provider.CreateProvider(cfg.Analyzer.Model, cfg)
	if err != nil {
		return fmt.Errorf("failed to create analyzer provider: %w", err)
	}
	analyzer := orchestrator.Reviewer{
		ID:           "analyzer",
		Provider:     analyzerProvider,
		SystemPrompt: cfg.Analyzer.Prompt,
	}

	// 创建总结器：在辩论结束后综合所有审查者的意见，生成最终审查结论
	summarizerProvider, err := provider.CreateProvider(cfg.Summarizer.Model, cfg)
	if err != nil {
		return fmt.Errorf("failed to create summarizer provider: %w", err)
	}
	summarizer := orchestrator.Reviewer{
		ID:           "summarizer",
		Provider:     summarizerProvider,
		SystemPrompt: cfg.Summarizer.Prompt,
	}

	// 创建上下文收集器（可选）：自动收集与变更相关的代码上下文信息，
	// 包括调用链、历史变更记录和文档等，帮助审查者更好地理解代码背景。
	// 如果用户指定了 --skip-context 或配置中未启用，则跳过此步骤。
	skipContext, _ := cmd.Flags().GetBool("skip-context")
	var contextGatherer orchestrator.ContextGathererInterface
	if !skipContext && cfg.ContextGatherer != nil && cfg.ContextGatherer.Enabled {
		contextModel := cfg.ContextGatherer.Model
		if contextModel == "" {
			contextModel = cfg.Analyzer.Model // 未指定模型时复用分析器的模型
		}
		contextProvider, err := provider.CreateProvider(contextModel, cfg)
		if err != nil {
			util.Warnf("Failed to create context gatherer provider: %v", err)
		} else {
			contextGatherer = appctx.NewContextGathererAdapter(contextProvider, cfg.ContextGatherer)
		}
	}

	// 计算最大辩论轮数：命令行指定的值优先于配置文件中的值。
	// 当只有一个审查者时（solo 模式），强制设为 1 轮，因为辩论需要至少两个参与者。
	maxRounds := cfg.Defaults.MaxRounds
	if r, _ := cmd.Flags().GetInt("rounds"); r > 0 {
		maxRounds = r
	}
	isSolo := len(reviewers) == 1
	if isSolo {
		maxRounds = 1
	}

	// 收敛检测：当审查者们的意见趋于一致时提前结束辩论，节省时间和 token。
	// 仅在多审查者模式下且未被用户禁用时启用。
	noConverge, _ := cmd.Flags().GetBool("no-converge")
	checkConvergence := !isSolo && !noConverge && cfg.Defaults.CheckConvergence

	// 在终端显示审查信息头部：包含审查目标、审查者列表、轮数等关键参数
	d.ReviewHeader(target.Label, selectedIDs, maxRounds, checkConvergence, contextGatherer != nil)

	// 组装编排器配置并启动审查流程。编排器负责协调审查者之间的多轮辩论，
	// 包括并行发起审查请求、收集响应、检测收敛，最终调用总结器生成结论。
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

	// 展示最终审查结论
	d.FinalConclusion(result.FinalConclusion)

	// 如果发现了代码问题，以表格形式展示问题列表（包含严重级别、文件位置等信息）
	if len(result.ParsedIssues) > 0 {
		d.IssuesTable(result.ParsedIssues)
	}

	// 将审查发现的问题作为评论发布到 GitHub PR。
	// 仅在审查目标为 PR、存在问题且用户未禁用 (--no-post) 的情况下执行。
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

	// 显示 token 使用统计信息，帮助用户了解 API 调用成本
	d.TokenUsage(result.TokenUsage, result.ConvergedAtRound)

	// 如果用户指定了 --output，将审查结果保存到文件（支持 markdown 和 json 格式）
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

// resolveTarget 根据命令行参数确定审查目标。
// 优先级从高到低为：--local > --branch > --files > 位置参数（PR 编号/URL）。
// 如果没有指定任何审查目标，返回错误提示用户指定目标。
func resolveTarget(cmd *cobra.Command, args []string, d *display.Display) (*reviewTarget, error) {
	isLocal, _ := cmd.Flags().GetBool("local")
	branchBase, _ := cmd.Flags().GetString("branch")
	files, _ := cmd.Flags().GetStringSlice("files")

	if isLocal {
		return resolveLocalTarget(d)
	}
	if cmd.Flags().Changed("branch") {
		if branchBase == "" {
			branchBase = "main" // 未指定基准分支时默认与 main 分支对比
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

// resolveLocalTarget 处理本地变更的审查目标解析。
// 首先尝试获取工作区中未提交的变更（git diff HEAD），如果没有未提交变更，
// 则回退到最后一次提交的变更（git diff HEAD~1 HEAD）。
// 这种设计确保用户在刚提交代码后仍然可以使用 --local 审查最近的改动。
func resolveLocalTarget(d *display.Display) (*reviewTarget, error) {
	// 获取工作区相对于 HEAD 的差异（包括暂存和未暂存的变更）
	diff, err := exec.Command("git", "diff", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("not a git repository or git is not available")
	}

	diffStr := string(diff)
	label := "Local Changes"
	isLastCommit := false

	// 如果没有未提交的变更，回退到审查最后一次提交
	if strings.TrimSpace(diffStr) == "" {
		diff, err = exec.Command("git", "diff", "HEAD~1", "HEAD").Output()
		if err != nil || strings.TrimSpace(string(diff)) == "" {
			return nil, fmt.Errorf("no changes found. Make some changes or commits first")
		}
		diffStr = string(diff)
		isLastCommit = true
		// 获取最后一次提交的消息作为显示标签
		commitMsg, _ := exec.Command("git", "log", "-1", "--pretty=%s").Output()
		label = fmt.Sprintf("Last Commit: %s", strings.TrimSpace(string(commitMsg)))
	}

	// 根据变更来源构造不同的提示词，让审查者了解变更的性质
	var prompt string
	if isLastCommit {
		prompt = fmt.Sprintf("Please review the following code changes from the last commit:\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.", diffStr)
	} else {
		prompt = fmt.Sprintf("Please review the following local code changes (uncommitted diff):\n\n```diff\n%s\n```\n\nAnalyze these changes and provide your feedback.", diffStr)
	}

	return &reviewTarget{Type: "local", Label: label, Prompt: prompt}, nil
}

// resolveBranchTarget 处理分支对比模式的审查目标解析。
// 使用三点 diff（baseBranch...currentBranch）获取当前分支相对于基准分支的所有变更。
// 三点 diff 只显示当前分支引入的变更，不包含基准分支上的后续提交，
// 这确保审查结果聚焦在当前分支的改动上。
func resolveBranchTarget(baseBranch string) (*reviewTarget, error) {
	currentBranch, err := exec.Command("git", "branch", "--show-current").Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get current branch: %w", err)
	}
	branch := strings.TrimSpace(string(currentBranch))

	// 使用三点 diff 语法获取分支间的差异
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

// resolvePRTarget 处理 GitHub PR 审查模式的目标解析。
// 支持两种输入方式：
//  1. 完整的 GitHub PR URL（如 https://github.com/owner/repo/pull/123）
//  2. 纯 PR 编号（如 123），此时通过 gh CLI 工具或 git remote 自动推断仓库信息
//
// 该函数还会预先获取 PR 的 diff、标题和描述，将它们嵌入到提示词中，
// 避免审查者在运行时再去拉取这些信息，提高审查效率和可靠性。
func resolvePRTarget(pr string) (*reviewTarget, error) {
	var prNumber, prURL, prRepo string

	if strings.HasPrefix(pr, "http") {
		// 用户直接提供了 PR URL，从 URL 中正则提取 PR 编号和仓库信息
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
		// 用户提供了纯 PR 编号，需要通过 gh CLI 或 git remote 推断完整 URL
		prNumber = pr
		// 优先使用 gh CLI 解析 PR URL，它能正确处理 fork 的情况
		out, err := exec.Command("gh", "pr", "view", prNumber, "--json", "url", "--jq", ".url").Output()
		if err == nil {
			prURL = strings.TrimSpace(string(out))
			reRepo := regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/`)
			if m := reRepo.FindStringSubmatch(prURL); len(m) > 1 {
				prRepo = m[1]
			}
		}
		if prURL == "" {
			// 回退方案：从 git remote origin URL 中提取仓库信息
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

	// 预先获取 PR 的 diff 内容，直接嵌入提示词中以减少审查时的网络请求
	var prDiff, prTitle, prBody string
	diffOut, err := exec.Command("gh", "pr", "diff", prURL).Output()
	if err == nil {
		prDiff = string(diffOut)
	}

	// 获取 PR 的标题和描述，为审查者提供更完整的变更上下文
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

	// 构造提示词：如果成功预获取了 diff，则将完整 diff 嵌入提示词并明确告知审查者
	// 不要再次获取；否则让审查者自行获取 diff
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

// selectReviewerIDs 根据命令行标志决定本次审查使用哪些审查者。
// 选择逻辑如下：
//  1. 如果指定了 --reviewers 标志，解析逗号分隔的 ID 列表，并验证每个 ID 是否在配置中存在
//  2. 如果指定了 --all 标志，使用配置中定义的全部审查者
//  3. 默认情况下也使用全部审查者（与 --all 行为一致）
func selectReviewerIDs(cmd *cobra.Command, allIDs []string) ([]string, error) {
	reviewersFlag, _ := cmd.Flags().GetString("reviewers")
	useAll, _ := cmd.Flags().GetBool("all")

	if reviewersFlag != "" {
		// 解析用户指定的审查者列表，并去除每个 ID 两端的空白字符
		ids := strings.Split(reviewersFlag, ",")
		for i := range ids {
			ids[i] = strings.TrimSpace(ids[i])
		}
		// 构建合法 ID 集合，用于快速校验用户输入的 ID 是否有效
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

	// 默认使用所有审查者
	return allIDs, nil
}

// extractPRNumber 从审查目标标签（如 "PR #123"）中提取纯数字的 PR 编号。
// 用于在发布 GitHub 评论时确定目标 PR。
func extractPRNumber(label string) string {
	re := regexp.MustCompile(`\d+`)
	if m := re.FindString(label); m != "" {
		return m
	}
	return ""
}

// saveOutput 将审查结果保存到指定路径的文件中。
// 支持两种格式：
//   - "json"：以带缩进的 JSON 格式保存完整的 DebateResult 结构体
//   - 其他（默认 "markdown"）：以 Markdown 格式保存可读性更好的审查报告
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

// convertIssuesToGitHub 将编排器产出的 MergedIssue 列表转换为 GitHub 评论所需的格式。
// 主要的转换工作是将 RaisedBy 字段从字符串切片合并为逗号分隔的单个字符串，
// 以便在 GitHub 评论中展示哪些审查者共同发现了该问题。
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

// 确保 strconv 包被引用（用于 reviewers 标志的字符串处理），避免编译器报未使用的导入错误
var _ = strconv.Itoa
