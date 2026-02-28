package context

import "time"

// GatheredContext is the full context gathered for a PR.
type GatheredContext struct {
	AffectedModules []AffectedModule `json:"affectedModules"`
	CallChain       []CallChainItem  `json:"callChain"`
	RelatedPRs      []RelatedPR      `json:"relatedPRs"`
	DesignPatterns  []DesignPattern  `json:"designPatterns"`
	Summary         string           `json:"summary"`
	GatheredAt      time.Time        `json:"gatheredAt"`
	PRNumber        string           `json:"prNumber"`
	BaseBranch      string           `json:"baseBranch"`
	RawReferences   []RawReference   `json:"rawReferences,omitempty"`
}

// AffectedModule represents a logical module affected by the PR.
type AffectedModule struct {
	Name          string   `json:"name"`
	Path          string   `json:"path"`
	Description   string   `json:"description"`
	AffectedFiles []string `json:"affectedFiles"`
	TotalFiles    int      `json:"totalFiles"`
	ImpactLevel   string   `json:"impactLevel"` // "core", "moderate", "peripheral"
}

// CallChainItem represents a symbol and its callers.
type CallChainItem struct {
	Symbol  string       `json:"symbol"`
	File    string       `json:"file"`
	Callers []CallerInfo `json:"callers"`
}

// CallerInfo describes a caller of a symbol.
type CallerInfo struct {
	Symbol  string `json:"symbol"`
	File    string `json:"file"`
	Context string `json:"context"`
}

// RelatedPR represents a historically related pull request.
type RelatedPR struct {
	Number           int      `json:"number"`
	Title            string   `json:"title"`
	Author           string   `json:"author"`
	MergedAt         string   `json:"mergedAt"`
	OverlappingFiles []string `json:"overlappingFiles"`
	Relevance        string   `json:"relevance"` // "direct", "same-module"
}

// DesignPattern represents a design pattern or convention found in the codebase.
type DesignPattern struct {
	Pattern     string `json:"pattern"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Source      string `json:"source"` // "documentation", "inferred"
}

// RawReference represents grep results for a symbol.
type RawReference struct {
	Symbol       string              `json:"symbol"`
	FoundInFiles []ReferenceLocation `json:"foundInFiles"`
}

// ReferenceLocation is a single location where a symbol was found.
type ReferenceLocation struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Content string `json:"content"`
}

// RawDoc represents a documentation file's content.
type RawDoc struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
