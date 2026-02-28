package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// HydraConfig is the top-level configuration.
type HydraConfig struct {
	Providers       map[string]CLIProviderConfig `yaml:"providers,omitempty"`
	Mock            bool                         `yaml:"mock,omitempty"`
	Defaults        DefaultsConfig               `yaml:"defaults"`
	Reviewers       map[string]ReviewerConfig    `yaml:"reviewers"`
	Analyzer        ReviewerConfig               `yaml:"analyzer"`
	Summarizer      ReviewerConfig               `yaml:"summarizer"`
	ContextGatherer *ContextGathererConfig       `yaml:"contextGatherer,omitempty"`
}

// CLIProviderConfig holds optional config for CLI providers.
type CLIProviderConfig struct {
	Enabled bool   `yaml:"enabled,omitempty"`
	APIKey  string `yaml:"api_key,omitempty"`
	BaseURL string `yaml:"base_url,omitempty"`
}

// DefaultsConfig holds default settings.
type DefaultsConfig struct {
	MaxRounds        int    `yaml:"max_rounds"`
	OutputFormat     string `yaml:"output_format"`
	CheckConvergence bool   `yaml:"check_convergence"`
	SkipPermissions  *bool  `yaml:"skip_permissions,omitempty"`
}

// ReviewerConfig defines a reviewer/analyzer/summarizer.
type ReviewerConfig struct {
	Model  string `yaml:"model"`
	Prompt string `yaml:"prompt"`
}

// ContextGathererConfig holds context gathering settings.
type ContextGathererConfig struct {
	Enabled   bool              `yaml:"enabled"`
	CallChain *CallChainConfig  `yaml:"callChain,omitempty"`
	History   *HistoryConfig    `yaml:"history,omitempty"`
	Docs      *DocsConfig       `yaml:"docs,omitempty"`
	Model     string            `yaml:"model,omitempty"`
}

type CallChainConfig struct {
	MaxDepth          int `yaml:"maxDepth,omitempty"`
	MaxFilesToAnalyze int `yaml:"maxFilesToAnalyze,omitempty"`
}

type HistoryConfig struct {
	MaxDays int `yaml:"maxDays,omitempty"`
	MaxPRs  int `yaml:"maxPRs,omitempty"`
}

type DocsConfig struct {
	Patterns []string `yaml:"patterns,omitempty"`
	MaxSize  int      `yaml:"maxSize,omitempty"`
}

var envVarRegex = regexp.MustCompile(`\$\{(\w+)\}`)

func expandEnvVars(s string) string {
	return envVarRegex.ReplaceAllStringFunc(s, func(match string) string {
		varName := match[2 : len(match)-1]
		return os.Getenv(varName)
	})
}

func expandEnvVarsInConfig(cfg *HydraConfig) {
	for k, v := range cfg.Providers {
		v.APIKey = expandEnvVars(v.APIKey)
		v.BaseURL = expandEnvVars(v.BaseURL)
		cfg.Providers[k] = v
	}
	for k, v := range cfg.Reviewers {
		v.Model = expandEnvVars(v.Model)
		v.Prompt = expandEnvVars(v.Prompt)
		cfg.Reviewers[k] = v
	}
	cfg.Analyzer.Model = expandEnvVars(cfg.Analyzer.Model)
	cfg.Analyzer.Prompt = expandEnvVars(cfg.Analyzer.Prompt)
	cfg.Summarizer.Model = expandEnvVars(cfg.Summarizer.Model)
	cfg.Summarizer.Prompt = expandEnvVars(cfg.Summarizer.Prompt)
}

// GetConfigPath returns the config file path.
func GetConfigPath(customPath string) string {
	if customPath != "" {
		return customPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hydra", "config.yaml")
}

// LoadConfig loads and validates the config file.
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

	expandEnvVarsInConfig(&cfg)

	// Default skip_permissions to true (required for non-interactive CLI operation)
	if cfg.Defaults.SkipPermissions == nil {
		t := true
		cfg.Defaults.SkipPermissions = &t
	}

	if err := validateConfig(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func validateReviewerConfig(name string, rc ReviewerConfig) error {
	if rc.Model == "" {
		return fmt.Errorf("config error: %s is missing a \"model\" field", name)
	}
	if strings.TrimSpace(rc.Prompt) == "" {
		return fmt.Errorf("config error: %s is missing a \"prompt\" field", name)
	}
	return nil
}

func validateConfig(cfg *HydraConfig) error {
	if cfg.Defaults.MaxRounds <= 0 {
		return fmt.Errorf("config error: defaults.max_rounds must be > 0")
	}

	if len(cfg.Reviewers) == 0 {
		return fmt.Errorf("config error: at least one reviewer must be defined")
	}

	for id, rc := range cfg.Reviewers {
		if err := validateReviewerConfig("reviewers."+id, rc); err != nil {
			return err
		}
	}

	if err := validateReviewerConfig("summarizer", cfg.Summarizer); err != nil {
		return err
	}

	if err := validateReviewerConfig("analyzer", cfg.Analyzer); err != nil {
		return err
	}

	return nil
}
