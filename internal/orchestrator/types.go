package orchestrator

import (
	"context"
	"time"

	"github.com/guwanhua/hydra/internal/provider"
)

// Reviewer 封装了一个AI提供者及其身份标识和系统提示词。
// 每个Reviewer代表辩论中的一个独立审查角色，拥有自己的AI模型实例。
type Reviewer struct {
	ID           string
	Provider     provider.AIProvider
	SystemPrompt string
}

// DebateMessage 表示辩论对话中的单条消息。
// 记录了发言的审查者ID、消息内容和时间戳。
type DebateMessage struct {
	ReviewerID string    `json:"reviewerId"`
	Content    string    `json:"content"`
	Timestamp  time.Time `json:"timestamp"`
}

// DebateSummary 保存审查者的最终总结。
// 在辩论结束后，每个审查者会生成一份总结性陈述。
type DebateSummary struct {
	ReviewerID string `json:"reviewerId"`
	Summary    string `json:"summary"`
}

// TokenUsage 追踪每个审查者的预估Token消耗量。
// 包括输入Token数、输出Token数和预估费用，用于成本监控。
type TokenUsage struct {
	ReviewerID    string  `json:"reviewerId"`
	InputTokens   int     `json:"inputTokens"`
	OutputTokens  int     `json:"outputTokens"`
	EstimatedCost float64 `json:"estimatedCost"`
}

// ReviewerStatus 追踪审查者在并行执行过程中的进度状态。
// 用于终端UI展示每个审查者的实时工作状态。
type ReviewerStatus struct {
	ReviewerID string
	Status     string  // "pending"（等待中）、"thinking"（思考中）、"done"（已完成）
	StartTime  int64   // Unix毫秒时间戳 - 开始时间
	EndTime    int64   // Unix毫秒时间戳 - 结束时间
	Duration   float64 // 耗时（秒）
}

// OrchestratorOptions 控制辩论行为的配置选项。
// MaxRounds 限制最大辩论轮数，CheckConvergence 决定是否启用共识检测提前终止。
type OrchestratorOptions struct {
	MaxRounds        int
	CheckConvergence bool
}

// OrchestratorConfig 保存创建DebateOrchestrator所需的全部配置。
// 包含多个审查者、一个分析器、一个总结器、可选的上下文收集器以及编排选项。
type OrchestratorConfig struct {
	Reviewers       []Reviewer               // 参与辩论的审查者列表
	Analyzer        Reviewer                 // 预分析器，负责在辩论前分析代码变更
	Summarizer      Reviewer                 // 总结器，负责判断共识和生成最终结论
	ContextGatherer ContextGathererInterface // 上下文收集器，为nil时表示禁用
	Options         OrchestratorOptions      // 辩论行为控制选项
}

// ContextGathererInterface 抽象了上下文收集功能，避免循环导入。
// 实现者负责从代码仓库中收集与变更相关的上下文信息（如调用链、受影响模块等）。
type ContextGathererInterface interface {
	Gather(ctx context.Context, diff, prNumber, baseBranch string) (*GatheredContext, error)
}

// GatheredContext 保存收集到的代码上下文数据。
// 包含变更摘要、符号引用关系、受影响的模块和相关的历史PR。
type GatheredContext struct {
	Summary         string
	RawReferences   []RawReference
	AffectedModules []AffectedModule
	RelatedPRs      []RelatedPR
}

// AffectedModule 表示受代码变更影响的模块。
// 包含模块名称、路径、受影响的文件列表和影响级别。
type AffectedModule struct {
	Name          string   // 模块名称
	Path          string   // 模块路径
	AffectedFiles []string // 受影响的文件列表
	ImpactLevel   string   // 影响级别："core"（核心）、"moderate"（中等）、"peripheral"（外围）
}

// RelatedPR 表示与当前变更相关的已合并的历史PR。
// 为审查者提供历史背景参考。
type RelatedPR struct {
	Number int
	Title  string
}

// RawReference 描述一个符号在代码仓库中被引用的位置。
// 用于追踪变更中涉及的函数/变量在其他文件中的调用关系。
type RawReference struct {
	Symbol       string
	FoundInFiles []ReferenceLocation
}

// ReferenceLocation 表示单个引用位置，包含文件路径、行号和上下文内容。
type ReferenceLocation struct {
	File    string
	Line    int
	Content string
}

// DebateResult 是辩论编排的最终输出结果。
// 汇总了整个辩论过程的所有数据：分析报告、对话消息、审查者总结、
// 最终结论、Token消耗、是否达成共识以及结构化的问题列表。
type DebateResult struct {
	PRNumber         string           `json:"prNumber"`
	Analysis         string           `json:"analysis"`
	Context          *GatheredContext `json:"context,omitempty"`
	Messages         []DebateMessage  `json:"messages"`
	Summaries        []DebateSummary  `json:"summaries"`
	FinalConclusion  string           `json:"finalConclusion"`
	TokenUsage       []TokenUsage     `json:"tokenUsage"`
	ConvergedAtRound *int             `json:"convergedAtRound,omitempty"`
	ParsedIssues     []MergedIssue    `json:"parsedIssues,omitempty"`
}

// ReviewIssue 表示审查者提出的单个结构化问题。
// 包含严重程度、分类、文件位置、标题、描述和建议修复方案。
type ReviewIssue struct {
	Severity     string   `json:"severity"`
	Category     string   `json:"category"`
	File         string   `json:"file"`
	Line         *int     `json:"line,omitempty"`
	EndLine      *int     `json:"endLine,omitempty"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	SuggestedFix string   `json:"suggestedFix,omitempty"`
	CodeSnippet  string   `json:"codeSnippet,omitempty"`
	RaisedBy     []string `json:"raisedBy,omitempty"`
}

// MergedIssue 是经过去重合并的问题，包含多个审查者的归属信息。
// 当多个审查者提出相似问题时，会合并为一个MergedIssue，保留最高严重程度。
type MergedIssue struct {
	ReviewIssue
	RaisedBy     []string `json:"raisedBy"`
	Descriptions []string `json:"descriptions"`
}

// ReviewerOutput 是从审查者响应文本中解析出的结构化输出。
// 包含问题列表、审查结论（approve/request_changes/comment）和总结。
type ReviewerOutput struct {
	Issues  []ReviewIssue `json:"issues"`
	Verdict string        `json:"verdict"`
	Summary string        `json:"summary"`
}

// DisplayCallbacks 是终端显示集成的回调接口。
// 编排器在执行过程中调用这些回调方法来实时更新终端UI。
// 包括：等待状态、消息输出、并行执行状态、轮次完成、共识判断和上下文收集完成等事件。
type DisplayCallbacks interface {
	OnWaiting(reviewerID string)
	OnMessage(reviewerID string, content string)
	OnParallelStatus(round int, statuses []ReviewerStatus)
	OnRoundComplete(round int, converged bool)
	OnConvergenceJudgment(verdict string, reasoning string)
	OnContextGathered(ctx *GatheredContext)
}
