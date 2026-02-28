package context

import (
	"fmt"
	"strings"
)

const maxDiffLength = 10000

// BuildAnalysisPrompt constructs a detailed prompt for AI context analysis.
func BuildAnalysisPrompt(diff string, changedFiles []string, refs []RawReference, history []RelatedPR, docs []RawDoc) string {
	// Truncate diff if too long
	truncatedDiff := diff
	if len(truncatedDiff) > maxDiffLength {
		truncatedDiff = truncatedDiff[:maxDiffLength] + "\n... (truncated)"
	}

	// Format references
	referencesText := formatReferences(refs)

	// Format related PRs
	relatedPRsText := formatRelatedPRs(history)

	// Format docs
	docsText := formatDocs(docs)

	return fmt.Sprintf(`You are a senior software architect analyzing a PR's impact on the system.

## PR Diff
`+"```diff\n%s\n```"+`

## Changed Files
%s

## Code References (grep results)
These are all the places where the changed functions/classes are referenced:

%s

## Related Recent PRs
%s

## Project Documentation
%s

---

Based on the above information, analyze and provide:

1. **Affected Modules**: Identify which logical modules/features this PR affects. For each:
   - name: module name
   - path: base path
   - description: what this module does
   - affectedFiles: which PR files belong to this module
   - impactLevel: "core" (critical path), "moderate" (important but not critical), or "peripheral" (utilities/helpers)

2. **Call Chain Analysis**: From the grep results, identify the REAL call chains (not just string matches). For key functions/classes being modified:
   - Who calls them? (callers)
   - What's the calling context? (API endpoint, background job, test, etc.)

3. **Design Patterns**: Based on the code and documentation:
   - What design patterns are used in the affected areas?
   - Are there any conventions that this PR should follow?
   - Note if the pattern was found in documentation or inferred from code.

4. **Summary**: Write a 2-3 paragraph summary for code reviewers explaining:
   - What system areas this PR touches
   - What the impact and risks are
   - What reviewers should pay attention to

Respond in JSON format:
`+"```json\n"+`{
  "affectedModules": [...],
  "callChain": [...],
  "designPatterns": [...],
  "summary": "..."
}
`+"```",
		truncatedDiff,
		formatChangedFiles(changedFiles),
		referencesText,
		relatedPRsText,
		docsText,
	)
}

func formatChangedFiles(files []string) string {
	if len(files) == 0 {
		return "No files changed."
	}
	var sb strings.Builder
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("- %s\n", f))
	}
	return sb.String()
}

func formatReferences(refs []RawReference) string {
	if len(refs) == 0 {
		return "No references found."
	}

	var sb strings.Builder
	for _, ref := range refs {
		files := ref.FoundInFiles
		if len(files) > 20 {
			files = files[:20]
		}
		sb.WriteString(fmt.Sprintf("### %s\n", ref.Symbol))
		sb.WriteString(fmt.Sprintf("Found in %d locations:\n", len(ref.FoundInFiles)))
		for _, f := range files {
			content := f.Content
			if len(content) > 100 {
				content = content[:100]
			}
			sb.WriteString(fmt.Sprintf("- %s:%d: %s\n", f.File, f.Line, content))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func formatRelatedPRs(prs []RelatedPR) string {
	if len(prs) == 0 {
		return "No related PRs found."
	}

	var sb strings.Builder
	for _, pr := range prs {
		sb.WriteString(fmt.Sprintf("- PR #%d: \"%s\" by %s (%s)\n", pr.Number, pr.Title, pr.Author, pr.Relevance))
	}
	return sb.String()
}

func formatDocs(docs []RawDoc) string {
	if len(docs) == 0 {
		return "No documentation found."
	}

	var sb strings.Builder
	for i, doc := range docs {
		if i > 0 {
			sb.WriteString("\n\n---\n\n")
		}
		content := doc.Content
		if len(content) > 2000 {
			content = content[:2000] + "\n... (truncated)"
		}
		sb.WriteString(fmt.Sprintf("### %s\n%s", doc.Path, content))
	}
	return sb.String()
}
