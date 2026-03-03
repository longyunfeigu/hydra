// Package config 负责加载、解析和验证 Hydra 的 YAML 配置文件。
// 配置文件定义了所有 AI 提供者、审查者角色、分析器、汇总器以及上下文收集器的参数。
// 支持通过 ${ENV_VAR} 语法在配置中引用环境变量，保护敏感信息安全。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// HydraConfig 是 Hydra 的顶层配置结构体。
// 它定义了所有 AI 提供者、审查者、分析器和汇总器的配置，
// 是整个多模型对抗式代码审查系统的核心配置入口。
type HydraConfig struct {
	Providers       map[string]CLIProviderConfig `yaml:"providers,omitempty"`    // AI 提供者配置映射（如 claude、codex 等）
	Mock            bool                         `yaml:"mock,omitempty"`         // 是否启用全局模拟模式（用于测试）
	Defaults        DefaultsConfig               `yaml:"defaults"`              // 默认运行参数
	Reviewers       map[string]ReviewerConfig    `yaml:"reviewers"`             // 审查者配置映射，每个审查者使用不同的模型和提示词
	Analyzer        ReviewerConfig               `yaml:"analyzer"`              // 分析器配置，负责分析代码变更
	Summarizer      ReviewerConfig               `yaml:"summarizer"`            // 汇总器配置，负责汇总多轮审查结果
	ContextGatherer *ContextGathererConfig       `yaml:"contextGatherer,omitempty"` // 上下文收集器配置（可选）
	Platform        *PlatformConfig              `yaml:"platform,omitempty"`    // 平台配置（可选，默认自动检测）
}

// PlatformConfig 配置代码托管平台（GitHub 或 GitLab）。
type PlatformConfig struct {
	Type string `yaml:"type,omitempty"` // "auto"(默认) | "github" | "gitlab"
	Host string `yaml:"host,omitempty"` // 自托管域名，如 "gitlab.company.com"
}

// CLIProviderConfig 保存 CLI 提供者的可选配置。
// 每个 AI 提供者（如 Claude Code CLI、Codex CLI）可以有独立的启用状态、API 密钥和基础 URL。
type CLIProviderConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"` // 是否启用该提供者
	APIKey  string `yaml:"api_key,omitempty"` // API 密钥，支持环境变量替换（如 ${ANTHROPIC_API_KEY}）
	BaseURL string `yaml:"base_url,omitempty"` // API 基础 URL，可用于自定义端点
}

// DefaultsConfig 保存默认运行参数。
// 这些参数控制审查流程的基本行为，如最大轮数、输出格式等。
type DefaultsConfig struct {
	MaxRounds        int    `yaml:"max_rounds"`                  // 最大审查轮数，控制对抗式审查的迭代次数
	OutputFormat     string `yaml:"output_format"`               // 输出格式（如 markdown、json 等）
	CheckConvergence bool   `yaml:"check_convergence"`           // 是否检查审查意见的收敛性（提前终止）
	SkipPermissions  *bool  `yaml:"skip_permissions,omitempty"`  // 是否跳过 CLI 权限提示（非交互模式必须为 true）
}

// ReviewerConfig 定义审查者、分析器或汇总器的配置。
// 每个角色需要指定使用的 AI 模型和系统提示词，以引导其审查行为。
type ReviewerConfig struct {
	Model     string `yaml:"model"`                // AI 提供者标识（如 "claude-code"、"codex-cli"、"gpt-4o"）
	ModelName string `yaml:"model_name,omitempty"`  // 底层模型名称，传给 CLI 的 --model 参数（如 "claude-sonnet-4-5-20250514"）
	Prompt          string `yaml:"prompt"`                      // 系统提示词，指导模型的审查角度和风格
	ReasoningEffort string `yaml:"reasoning_effort,omitempty"`  // 推理深度（none|low|medium|high|xhigh），仅 OpenAI 推理模型有效
}

// ContextGathererConfig 保存上下文收集器的配置。
// 上下文收集器在审查前自动收集相关代码上下文（调用链、历史记录、文档），
// 帮助审查者更全面地理解代码变更。
type ContextGathererConfig struct {
	Enabled   bool              `yaml:"enabled"`              // 是否启用上下文收集
	History   *HistoryConfig    `yaml:"history,omitempty"`    // Git 历史记录分析配置
	Docs      *DocsConfig       `yaml:"docs,omitempty"`       // 相关文档收集配置
	Model     string            `yaml:"model,omitempty"`      // 用于上下文分析的 AI 模型
}

// HistoryConfig 配置 Git 历史记录分析的参数。
// 通过分析最近的提交和 PR，了解代码的演进背景。
type HistoryConfig struct {
	MaxDays int `yaml:"maxDays,omitempty"` // 最多回溯的天数
	MaxPRs  int `yaml:"maxPRs,omitempty"`  // 最多分析的 PR 数量
}

// DocsConfig 配置文档收集的参数。
// 自动查找与变更文件相关的文档内容。
type DocsConfig struct {
	Patterns []string `yaml:"patterns,omitempty"` // 文档文件的匹配模式（如 "*.md"、"docs/**"）
	MaxSize  int      `yaml:"maxSize,omitempty"`  // 单个文档的最大字节数
}

// envVarRegex 匹配 ${VAR_NAME} 格式的环境变量占位符
var envVarRegex = regexp.MustCompile(`\$\{(\w+)\}`)

// expandEnvVars 将字符串中的 ${VAR_NAME} 占位符替换为对应的环境变量值。
// 这允许用户在配置文件中安全地引用敏感信息（如 API 密钥），
// 而不必将它们直接写入配置文件。
func expandEnvVars(s string) string {
	return envVarRegex.ReplaceAllStringFunc(s, func(match string) string {
		// 提取 ${...} 中的变量名（去掉 ${ 和 } 字符）
		varName := match[2 : len(match)-1]
		return os.Getenv(varName)
	})
}

// expandEnvVarsInConfig 对整个配置结构体中的所有字符串字段执行环境变量替换。
// 遍历所有提供者、审查者、分析器和汇总器的配置，确保所有字段都经过环境变量展开。
func expandEnvVarsInConfig(cfg *HydraConfig) {
	// 替换所有提供者配置中的环境变量
	for k, v := range cfg.Providers {
		v.APIKey = expandEnvVars(v.APIKey)
		v.BaseURL = expandEnvVars(v.BaseURL)
		cfg.Providers[k] = v
	}
	// 替换所有审查者配置中的环境变量
	for k, v := range cfg.Reviewers {
		v.Model = expandEnvVars(v.Model)
		v.ModelName = expandEnvVars(v.ModelName)
		v.Prompt = expandEnvVars(v.Prompt)
		v.ReasoningEffort = expandEnvVars(v.ReasoningEffort)
		cfg.Reviewers[k] = v
	}
	// 替换分析器和汇总器配置中的环境变量
	cfg.Analyzer.Model = expandEnvVars(cfg.Analyzer.Model)
	cfg.Analyzer.ModelName = expandEnvVars(cfg.Analyzer.ModelName)
	cfg.Analyzer.Prompt = expandEnvVars(cfg.Analyzer.Prompt)
	cfg.Analyzer.ReasoningEffort = expandEnvVars(cfg.Analyzer.ReasoningEffort)
	cfg.Summarizer.Model = expandEnvVars(cfg.Summarizer.Model)
	cfg.Summarizer.ModelName = expandEnvVars(cfg.Summarizer.ModelName)
	cfg.Summarizer.Prompt = expandEnvVars(cfg.Summarizer.Prompt)
	cfg.Summarizer.ReasoningEffort = expandEnvVars(cfg.Summarizer.ReasoningEffort)
}

// GetConfigPath 返回配置文件的路径。
// 如果用户指定了自定义路径则使用该路径，否则使用默认路径 ~/.hydra/config.yaml。
func GetConfigPath(customPath string) string {
	if customPath != "" {
		return customPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hydra", "config.yaml")
}

// LoadConfig 加载并验证配置文件。
// 处理流程：读取 YAML 文件 -> 解析为结构体 -> 展开环境变量 -> 设置默认值 -> 验证配置完整性。
func LoadConfig(configPath string) (*HydraConfig, error) {
	path := GetConfigPath(configPath)

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found: %s", path)
	}

	var cfg HydraConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// 展开配置中所有的环境变量引用
	expandEnvVarsInConfig(&cfg)

	// 默认将 skip_permissions 设置为 true，因为非交互式 CLI 操作需要跳过权限提示
	if cfg.Defaults.SkipPermissions == nil {
		t := true
		cfg.Defaults.SkipPermissions = &t
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validateReviewerConfig 验证单个审查者/分析器/汇总器的配置是否完整。
// 确保 model 和 prompt 字段都已设置，因为这两个字段是 AI 审查的必要参数。
func validateReviewerConfig(name string, rc ReviewerConfig) error {
	if rc.Model == "" {
		return fmt.Errorf("config error: %s is missing a \"model\" field", name)
	}
	if strings.TrimSpace(rc.Prompt) == "" {
		return fmt.Errorf("config error: %s is missing a \"prompt\" field", name)
	}
	return nil
}

// validateConfig 对整个配置进行完整性验证。
// 检查项包括：最大轮数必须大于零、至少定义一个审查者、
// 所有审查者/分析器/汇总器都必须配置了模型和提示词。
func validateConfig(cfg *HydraConfig) error {
	// 验证最大审查轮数
	if cfg.Defaults.MaxRounds <= 0 {
		return fmt.Errorf("config error: defaults.max_rounds must be > 0")
	}

	// 验证至少有一个审查者
	if len(cfg.Reviewers) == 0 {
		return fmt.Errorf("config error: at least one reviewer must be defined")
	}

	// 逐一验证每个审查者的配置
	for id, rc := range cfg.Reviewers {
		if err := validateReviewerConfig("reviewers."+id, rc); err != nil {
			return err
		}
	}

	// 验证汇总器配置
	if err := validateReviewerConfig("summarizer", cfg.Summarizer); err != nil {
		return err
	}

	// 验证分析器配置
	if err := validateReviewerConfig("analyzer", cfg.Analyzer); err != nil {
		return err
	}

	return nil
}
