package context

import (
	gocontext "context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/guwanhua/hydra/internal/config"
	"github.com/guwanhua/hydra/internal/provider"
)

// GathererOptions controls context gathering behavior.
type GathererOptions struct {
	CallChain struct {
		MaxDepth          int
		MaxFilesToAnalyze int
	}
	History struct {
		MaxDays int
		MaxPRs  int
	}
	Docs struct {
		Patterns []string
		MaxSize  int
	}
}

// ContextGatherer collects and analyzes PR context using AI.
type ContextGatherer struct {
	provider provider.AIProvider
	options  GathererOptions
}

// NewContextGatherer creates a new ContextGatherer with the given provider and config.
func NewContextGatherer(p provider.AIProvider, cfg *config.ContextGathererConfig) *ContextGatherer {
	opts := GathererOptions{}

	// Set defaults
	opts.CallChain.MaxDepth = 2
	opts.CallChain.MaxFilesToAnalyze = 20
	opts.History.MaxDays = 30
	opts.History.MaxPRs = 10
	opts.Docs.Patterns = []string{"docs", "README.md", "ARCHITECTURE.md", "DESIGN.md"}
	opts.Docs.MaxSize = 50000

	// Override from config
	if cfg != nil {
		if cfg.CallChain != nil {
			if cfg.CallChain.MaxDepth > 0 {
				opts.CallChain.MaxDepth = cfg.CallChain.MaxDepth
			}
			if cfg.CallChain.MaxFilesToAnalyze > 0 {
				opts.CallChain.MaxFilesToAnalyze = cfg.CallChain.MaxFilesToAnalyze
			}
		}
		if cfg.History != nil {
			if cfg.History.MaxDays > 0 {
				opts.History.MaxDays = cfg.History.MaxDays
			}
			if cfg.History.MaxPRs > 0 {
				opts.History.MaxPRs = cfg.History.MaxPRs
			}
		}
		if cfg.Docs != nil {
			if len(cfg.Docs.Patterns) > 0 {
				opts.Docs.Patterns = cfg.Docs.Patterns
			}
			if cfg.Docs.MaxSize > 0 {
				opts.Docs.MaxSize = cfg.Docs.MaxSize
			}
		}
	}

	return &ContextGatherer{
		provider: p,
		options:  opts,
	}
}

var diffFilePattern = regexp.MustCompile(`(?m)^(?:diff --git a/(.+?) b/|--- a/(.+?)|\+\+\+ b/(.+?))$`)

// extractChangedFiles extracts file paths from a unified diff.
func extractChangedFiles(diff string) []string {
	matches := diffFilePattern.FindAllStringSubmatch(diff, -1)
	seen := make(map[string]bool)
	var files []string

	for _, m := range matches {
		file := m[1]
		if file == "" {
			file = m[2]
		}
		if file == "" {
			file = m[3]
		}
		if file == "" || seen[file] || strings.HasPrefix(file, "/dev/null") {
			continue
		}
		seen[file] = true
		files = append(files, file)
	}

	return files
}

// Gather collects context for a PR diff and returns structured analysis.
func (g *ContextGatherer) Gather(diff, prNumber, baseBranch string) (*GatheredContext, error) {
	cwd := "."
	changedFiles := extractChangedFiles(diff)

	// Collect data (references, history, docs)
	references := CollectReferences(diff, cwd)

	relatedPRs, _ := CollectHistory(changedFiles, g.options.History.MaxDays, g.options.History.MaxPRs, cwd)

	docs, _ := CollectDocs(g.options.Docs.Patterns, g.options.Docs.MaxSize, cwd)

	// Build analysis prompt and call AI
	prompt := BuildAnalysisPrompt(diff, changedFiles, references, relatedPRs, docs)

	response, err := g.provider.Chat(
		gocontext.Background(),
		[]provider.Message{{Role: "user", Content: prompt}},
		"You are a senior software architect. Analyze the PR context and respond in JSON format only.",
		nil,
	)
	if err != nil {
		// Return partial context without AI analysis
		return &GatheredContext{
			RelatedPRs:    relatedPRs,
			Summary:       "AI analysis unavailable: " + err.Error(),
			GatheredAt:    time.Now(),
			PRNumber:      prNumber,
			BaseBranch:    baseBranch,
			RawReferences: references,
		}, nil
	}

	// Parse AI response
	parsed := parseAIResponse(response)

	return &GatheredContext{
		AffectedModules: parsed.AffectedModules,
		CallChain:       parsed.CallChain,
		RelatedPRs:      relatedPRs,
		DesignPatterns:  parsed.DesignPatterns,
		Summary:         parsed.Summary,
		GatheredAt:      time.Now(),
		PRNumber:        prNumber,
		BaseBranch:      baseBranch,
		RawReferences:   references,
	}, nil
}

type aiAnalysisResult struct {
	AffectedModules []AffectedModule `json:"affectedModules"`
	CallChain       []CallChainItem  `json:"callChain"`
	DesignPatterns  []DesignPattern  `json:"designPatterns"`
	Summary         string           `json:"summary"`
}

var jsonBlockRegex = regexp.MustCompile("(?s)```json\\s*(.*?)\\s*```")
var jsonObjectRegex = regexp.MustCompile(`(?s)\{.*\}`)

func parseAIResponse(response string) aiAnalysisResult {
	var jsonStr string

	// Try to extract JSON from markdown code block
	if m := jsonBlockRegex.FindStringSubmatch(response); m != nil {
		jsonStr = m[1]
	} else if m := jsonObjectRegex.FindString(response); m != "" {
		jsonStr = m
	} else {
		// Use response as summary fallback
		summary := response
		if len(summary) > 1000 {
			summary = summary[:1000]
		}
		return aiAnalysisResult{Summary: summary}
	}

	var result aiAnalysisResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		summary := response
		if len(summary) > 1000 {
			summary = summary[:1000]
		}
		return aiAnalysisResult{Summary: summary}
	}

	return result
}
