package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a default configuration file",
	RunE:  runInit,
}

const defaultConfig = `# Hydra Configuration
# Multi-model adversarial code review

providers:
  claude-code:
    enabled: true
  codex-cli:
    enabled: true

defaults:
  max_rounds: 3
  output_format: markdown
  check_convergence: true

analyzer:
  model: claude-code
  prompt: |
    You are a senior code analyst. Analyze the PR diff and provide:
    1. A summary of what the changes do
    2. Key areas of concern
    3. Potential impact on the codebase

    ## Suggested Review Focus
    - List specific areas reviewers should focus on

summarizer:
  model: claude-code
  prompt: |
    You are a senior engineering lead synthesizing code review feedback.
    Provide balanced, actionable conclusions.

reviewers:
  claude:
    model: claude-code
    prompt: |
      You are a thorough code reviewer focused on correctness, security,
      and maintainability. Review every changed file systematically.
  codex:
    model: codex-cli
    prompt: |
      You are a pragmatic code reviewer focused on performance, edge cases,
      and real-world reliability. Challenge assumptions and find issues others miss.

# Optional: context gathering
# contextGatherer:
#   enabled: true
#   callChain:
#     maxDepth: 2
#     maxFilesToAnalyze: 20
#   history:
#     maxDays: 30
#     maxPRs: 10
#   docs:
#     patterns: ["docs", "README.md", "ARCHITECTURE.md"]
#     maxSize: 50000
`

func runInit(cmd *cobra.Command, args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	configDir := filepath.Join(home, ".hydra")
	configPath := filepath.Join(configDir, "config.yaml")

	// Check if config already exists
	if _, err := os.Stat(configPath); err == nil {
		color.Yellow("Config file already exists: %s", configPath)
		fmt.Print("Overwrite? (y/N): ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	if err := os.WriteFile(configPath, []byte(defaultConfig), 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	color.Green("Config file created: %s", configPath)
	fmt.Println("Edit this file to configure your reviewers and models.")
	return nil
}
