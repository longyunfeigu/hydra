package context

import (
	"fmt"
	"strings"
)

// maxDiffLength 是发送给 AI 分析的 diff 文本最大长度。
// 超过此长度的 diff 会被截断，以避免超出 AI 模型的上下文窗口限制。
const maxDiffLength = 10000

// BuildAnalysisPrompt 构建用于 AI 上下文分析的详细提示词。
// 将 diff、变更文件列表、代码引用、历史 PR 和项目文档整合为一个结构化的提示词，
// 要求 AI 分析受影响模块、调用链、设计模式并生成摘要。
// AI 需要以 JSON 格式返回分析结果。
func BuildAnalysisPrompt(diff string, changedFiles []string, refs []RawReference, history []RelatedPR, docs []RawDoc) string {
	// 如果 diff 过长则截断，避免超出 AI 上下文窗口
	truncatedDiff := diff
	if len(truncatedDiff) > maxDiffLength {
		truncatedDiff = truncatedDiff[:maxDiffLength] + "\n... (truncated)"
	}

	// 格式化各部分数据为可读文本
	referencesText := formatReferences(refs)

	relatedPRsText := formatRelatedPRs(history)

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

// formatChangedFiles 将变更文件列表格式化为 Markdown 无序列表。
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

// formatReferences 将符号引用数据格式化为 Markdown 文本。
// 每个符号最多展示 20 个引用位置，内容截断到 100 字符。
func formatReferences(refs []RawReference) string {
	if len(refs) == 0 {
		return "No references found."
	}

	var sb strings.Builder
	for _, ref := range refs {
		// 限制每个符号最多展示 20 个引用位置
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

// formatRelatedPRs 将关联 PR 列表格式化为 Markdown 无序列表。
// 展示 PR 编号、标题、作者和关联程度。
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

// formatDocs 将文档内容格式化为 Markdown 文本。
// 每个文档内容截断到 2000 字符以控制提示词总长度。
// 多个文档之间用分隔线分隔。
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
