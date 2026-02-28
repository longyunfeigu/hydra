package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var (
	validSeverities = map[string]bool{
		"critical": true,
		"high":     true,
		"medium":   true,
		"low":      true,
		"nitpick":  true,
	}

	severityOrder = map[string]int{
		"critical": 0,
		"high":     1,
		"medium":   2,
		"low":      3,
		"nitpick":  4,
	}

	validVerdicts = map[string]bool{
		"approve":         true,
		"request_changes": true,
		"comment":         true,
	}

	stopWords = map[string]bool{
		"the": true, "a": true, "in": true, "of": true,
		"is": true, "to": true, "and": true, "for": true,
		"with": true, "this": true, "that": true, "it": true,
	}

	jsonFenceRe = regexp.MustCompile("(?s)```json\\s*(.*?)\\s*```")
	rawJSONRe   = regexp.MustCompile(`(?s)\{[\s\S]*"issues"\s*:\s*\[[\s\S]*\][\s\S]*\}`)
	focusRe     = regexp.MustCompile(`(?s)## Suggested Review Focus\s*\n(.*?)(?:\n##|\z)`)
)

// ParseReviewerOutput parses structured ReviewerOutput from a reviewer's response text.
// Looks for a ```json block containing { issues, verdict, summary }.
// Returns nil if no valid JSON block found.
func ParseReviewerOutput(response string) *ReviewerOutput {
	// Try ```json fenced block first
	var jsonStr string
	if m := jsonFenceRe.FindStringSubmatch(response); len(m) > 1 {
		jsonStr = m[1]
	}

	// Fallback: raw JSON object with "issues" array
	if jsonStr == "" {
		if m := rawJSONRe.FindString(response); m != "" {
			jsonStr = m
		}
	}

	if jsonStr == "" {
		return nil
	}

	// Parse into a generic map first for flexible validation
	var raw struct {
		Issues  []json.RawMessage `json:"issues"`
		Verdict string            `json:"verdict"`
		Summary string            `json:"summary"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &raw); err != nil {
		return nil
	}

	if raw.Issues == nil {
		return nil
	}

	verdict := raw.Verdict
	if !validVerdicts[verdict] {
		verdict = "comment"
	}

	var issues []ReviewIssue
	for _, rawIssue := range raw.Issues {
		var m map[string]interface{}
		if err := json.Unmarshal(rawIssue, &m); err != nil {
			continue
		}

		severity, _ := m["severity"].(string)
		if !validSeverities[severity] {
			continue
		}

		file, _ := m["file"].(string)
		if file == "" {
			continue
		}

		title, _ := m["title"].(string)
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}

		description, _ := m["description"].(string)
		description = strings.TrimSpace(description)
		if description == "" {
			continue
		}

		category, _ := m["category"].(string)
		if category == "" {
			category = "general"
		}

		issue := ReviewIssue{
			Severity:    severity,
			Category:    category,
			File:        file,
			Title:       title,
			Description: description,
		}

		// Optional line
		if lineVal, ok := m["line"].(float64); ok && lineVal > 0 {
			line := int(lineVal)
			issue.Line = &line
		}

		// Optional endLine
		if endVal, ok := m["endLine"].(float64); ok && endVal > 0 {
			endLine := int(endVal)
			if issue.Line != nil && endLine >= *issue.Line {
				issue.EndLine = &endLine
			}
		}

		if sf, ok := m["suggestedFix"].(string); ok {
			issue.SuggestedFix = sf
		}
		if cs, ok := m["codeSnippet"].(string); ok {
			issue.CodeSnippet = cs
		}
		if rb, ok := m["raisedBy"].([]interface{}); ok {
			for _, v := range rb {
				if s, ok := v.(string); ok {
					issue.RaisedBy = append(issue.RaisedBy, s)
				}
			}
		}

		issues = append(issues, issue)
	}

	return &ReviewerOutput{
		Issues:  issues,
		Verdict: verdict,
		Summary: raw.Summary,
	}
}

// ParseFocusAreas extracts suggested review focus areas from analyzer output.
// Looks for a "## Suggested Review Focus" section with bullet points.
func ParseFocusAreas(analysis string) []string {
	m := focusRe.FindStringSubmatch(analysis)
	if len(m) < 2 {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(m[1]), "\n")
	var areas []string
	for _, line := range lines {
		// Strip bullet prefix
		line = strings.TrimLeft(line, " \t")
		if len(line) > 0 && (line[0] == '-' || line[0] == '*') {
			line = strings.TrimSpace(line[1:])
		}
		line = strings.TrimSpace(line)
		if line != "" {
			areas = append(areas, line)
		}
	}
	return areas
}

// DeduplicateIssues merges similar issues across multiple reviewers.
// Issues with the same file + overlapping line + similar title are merged.
// Merged issues keep the highest severity and track all contributing reviewers.
func DeduplicateIssues(issuesByReviewer map[string][]ReviewIssue) []MergedIssue {
	var merged []MergedIssue

	for reviewerID, issues := range issuesByReviewer {
		for _, issue := range issues {
			found := false
			for i := range merged {
				if isSimilarIssue(&merged[i].ReviewIssue, &issue) {
					merged[i].RaisedBy = append(merged[i].RaisedBy, reviewerID)
					merged[i].Descriptions = append(merged[i].Descriptions, issue.Description)
					// Keep highest severity
					if severityOrder[issue.Severity] < severityOrder[merged[i].Severity] {
						merged[i].Severity = issue.Severity
					}
					// Keep suggested fix if we don't have one
					if merged[i].SuggestedFix == "" && issue.SuggestedFix != "" {
						merged[i].SuggestedFix = issue.SuggestedFix
					}
					found = true
					break
				}
			}
			if !found {
				merged = append(merged, MergedIssue{
					ReviewIssue:  issue,
					RaisedBy:     []string{reviewerID},
					Descriptions: []string{issue.Description},
				})
			}
		}
	}

	// Sort by severity (critical first)
	sort.Slice(merged, func(i, j int) bool {
		return severityOrder[merged[i].Severity] < severityOrder[merged[j].Severity]
	})

	return merged
}

// isSimilarIssue checks if two issues are similar enough to merge.
func isSimilarIssue(a, b *ReviewIssue) bool {
	// Must be same file
	if a.File != b.File {
		return false
	}

	// Check line range overlap
	if !linesOverlap(a, b) {
		return false
	}

	// Check title similarity (with stop words filtered)
	wordsA := filterStopWords(tokenize(strings.ToLower(a.Title)))
	wordsB := filterStopWords(tokenize(strings.ToLower(b.Title)))
	titleSim := jaccardSimilarity(wordsA, wordsB)

	// Check description similarity (first 50 words)
	descWordsA := filterStopWords(firstN(tokenize(strings.ToLower(a.Description)), 50))
	descWordsB := filterStopWords(firstN(tokenize(strings.ToLower(b.Description)), 50))
	descSim := jaccardSimilarity(descWordsA, descWordsB)

	return titleSim*0.7+descSim*0.3 > 0.35
}

// linesOverlap checks if two line ranges overlap or are within 5 lines proximity.
func linesOverlap(a, b *ReviewIssue) bool {
	if a.Line == nil || b.Line == nil {
		return true // No line info -> don't reject
	}
	aStart := *a.Line
	aEnd := aStart
	if a.EndLine != nil {
		aEnd = *a.EndLine
	}
	bStart := *b.Line
	bEnd := bStart
	if b.EndLine != nil {
		bEnd = *b.EndLine
	}
	// Overlap or within 5 lines of each other
	return aStart <= bEnd+5 && bStart <= aEnd+5
}

// jaccardSimilarity computes the Jaccard similarity between two word slices.
func jaccardSimilarity(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, w := range a {
		setA[w] = true
	}
	setB := make(map[string]bool, len(b))
	for _, w := range b {
		setB[w] = true
	}

	intersection := 0
	for w := range setA {
		if setB[w] {
			intersection++
		}
	}

	// Union = |A| + |B| - |intersection|
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// filterStopWords removes common English stop words.
func filterStopWords(words []string) []string {
	var result []string
	for _, w := range words {
		if w != "" && !stopWords[w] {
			result = append(result, w)
		}
	}
	return result
}

// tokenize splits a string into words on whitespace.
func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r)
	})
}

// firstN returns the first n elements of a slice.
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// FormatCallChainForReviewer formats raw references into a readable call chain section.
func FormatCallChainForReviewer(references []RawReference) string {
	if len(references) == 0 {
		return ""
	}

	var sections []string
	for _, ref := range references {
		callers := ref.FoundInFiles
		if len(callers) > 10 {
			callers = callers[:10]
		}

		var callerLines []string
		for i, f := range callers {
			content := f.Content
			if len(content) > 150 {
				content = content[:150]
			}
			callerLines = append(callerLines, fmt.Sprintf("%d. %s:%d\n   > %s", i+1, f.File, f.Line, content))
		}

		section := fmt.Sprintf("### Callers of `%s`\nFound in %d locations:\n\n%s",
			ref.Symbol, len(ref.FoundInFiles), strings.Join(callerLines, "\n\n"))
		sections = append(sections, section)
	}

	return "## Call Chain Context\n\n" + strings.Join(sections, "\n\n---\n\n")
}
