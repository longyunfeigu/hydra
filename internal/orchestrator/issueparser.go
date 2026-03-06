package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/guwanhua/hydra/internal/schema"
)

var (
	// validSeverities 定义有效的问题严重程度级别。
	// critical=阻塞合并, high=应该修复, medium=值得修复, low=次要, nitpick=风格问题
	validSeverities = map[string]bool{
		"critical": true,
		"high":     true,
		"medium":   true,
		"low":      true,
		"nitpick":  true,
	}

	// severityOrder 定义严重程度的排序优先级，数值越小越严重。
	// 用于合并重复问题时保留最高严重程度，以及最终排序时将关键问题排在前面。
	severityOrder = map[string]int{
		"critical": 0,
		"high":     1,
		"medium":   2,
		"low":      3,
		"nitpick":  4,
	}

	// validVerdicts 定义有效的审查结论类型。
	// approve=批准, request_changes=要求修改, comment=仅评论
	validVerdicts = map[string]bool{
		"approve":         true,
		"request_changes": true,
		"comment":         true,
	}

	// stopWords 定义英文停用词集合，在计算文本相似度时过滤掉这些常见词。
	// 过滤停用词可以提高Jaccard相似度计算的准确性。
	stopWords = map[string]bool{
		"the": true, "a": true, "in": true, "of": true,
		"is": true, "to": true, "and": true, "for": true,
		"with": true, "this": true, "that": true, "it": true,
	}

	// jsonFenceRe 匹配```json ... ```格式的JSON代码块
	jsonFenceRe = regexp.MustCompile("(?s)```json\\s*(.*?)\\s*```")
	// rawJSONRe 匹配包含"issues"数组的原始JSON对象（无代码块包裹的情况）
	rawJSONRe = regexp.MustCompile(`(?s)\{[\s\S]*"issues"\s*:\s*\[[\s\S]*\][\s\S]*\}`)
	// rawDeltaJSONRe 匹配包含 delta 关键字段的 JSON 对象（无代码块包裹的情况）
	rawDeltaJSONRe = regexp.MustCompile(`(?s)\{[\s\S]*"(add|retract|update|support|withdraw|contest)"\s*:[\s\S]*\}`)
	// focusRe 匹配"## Suggested Review Focus"章节，提取审查关注重点
	focusRe = regexp.MustCompile(`(?s)## Suggested Review Focus\s*\n(.*?)(?:\n##|\z)`)
	// fileLineRe 匹配文件路径中嵌入的行号范围，如 "file.py:37" 或 "file.py:37-48"
	fileLineRe = regexp.MustCompile(`^(.+):(\d+)(?:-(\d+))?$`)
)

// ParseResult 包含解析结果、原始 JSON 和可能的错误信息。
type ParseResult struct {
	Output       *ReviewerOutput          // 成功解析的结构化输出
	RawJSON      string                   // 提取到的原始 JSON
	SchemaErrors []schema.ValidationError // schema 校验错误
	ParseError   error                    // JSON 语法级错误
}

// DeltaParseResult 包含 issue delta 解析结果。
type DeltaParseResult struct {
	Output       *StructurizeDelta        // 成功解析的 delta
	RawJSON      string                   // 提取到的原始 JSON
	SchemaErrors []schema.ValidationError // schema 校验错误
	ParseError   error                    // JSON 语法级错误
}

// ParseReviewerOutput 从审查者的响应文本中解析结构化的ReviewerOutput。
// 解析策略：
//  1. 优先查找```json代码块中的JSON内容
//  2. 回退方案：查找包含"issues"数组的原始JSON对象
//  3. 使用 JSON Schema 校验结构有效性
//
// 对每个问题进行严格验证：必须包含有效的严重程度、文件路径、标题和描述。
// 返回 ParseResult，包含解析结果和错误详情，便于重试时提供具体反馈。
func ParseReviewerOutput(response string) *ParseResult {
	result := &ParseResult{}

	result.RawJSON = extractJSON(response, rawJSONRe)

	if result.RawJSON == "" {
		result.ParseError = fmt.Errorf("no JSON block found in response")
		return result
	}

	// Schema 校验：在深度解析之前检查结构有效性
	vr := schema.ValidateIssuesJSON(result.RawJSON)
	if !vr.Valid {
		result.SchemaErrors = vr.Errors
		// 即使 schema 校验失败，仍然尝试手动解析以提取尽可能多的有效 issue
	}

	// 先解析为通用结构体进行灵活验证，避免严格结构体解析导致丢失数据
	var raw struct {
		Issues  []json.RawMessage `json:"issues"`
		Verdict string            `json:"verdict"`
		Summary string            `json:"summary"`
	}
	if err := json.Unmarshal([]byte(result.RawJSON), &raw); err != nil {
		result.ParseError = fmt.Errorf("JSON syntax error: %w", err)
		return result
	}

	if raw.Issues == nil {
		result.ParseError = fmt.Errorf("missing 'issues' key in JSON")
		return result
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

		// 从文件路径中提取嵌入的行号范围，如 "file.py:37-48" → file="file.py", line=37, endLine=48
		var fileLine, fileEndLine *int
		if match := fileLineRe.FindStringSubmatch(file); match != nil {
			file = match[1]
			if v, err := strconv.Atoi(match[2]); err == nil && v > 0 {
				fileLine = &v
			}
			if match[3] != "" {
				if v, err := strconv.Atoi(match[3]); err == nil && v > 0 {
					fileEndLine = &v
				}
			}
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

		// 可选字段：起始行号（JSON 中的 "line" 字段优先，路径中提取的行号作为 fallback）
		if lineVal, ok := m["line"].(float64); ok && lineVal > 0 {
			line := int(lineVal)
			issue.Line = &line
		} else if fileLine != nil {
			issue.Line = fileLine
		}

		// 可选字段：结束行号（JSON 中的 "endLine" 字段优先，路径中提取的结束行号作为 fallback）
		if endVal, ok := m["endLine"].(float64); ok && endVal > 0 {
			endLine := int(endVal)
			if issue.Line != nil && endLine >= *issue.Line {
				issue.EndLine = &endLine
			}
		} else if fileEndLine != nil && issue.Line != nil && *fileEndLine >= *issue.Line {
			issue.EndLine = fileEndLine
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
					issue.ClaimedBy = append(issue.ClaimedBy, s)
				}
			}
		}

		issues = append(issues, issue)
	}

	result.Output = &ReviewerOutput{
		Issues:  issues,
		Verdict: verdict,
		Summary: raw.Summary,
	}
	return result
}

// ParseStructurizeDelta 从模型响应中解析单轮增量 delta。
func ParseStructurizeDelta(response string) *DeltaParseResult {
	result := &DeltaParseResult{}
	result.RawJSON = extractJSON(response, rawDeltaJSONRe)

	if result.RawJSON == "" {
		result.ParseError = fmt.Errorf("no JSON block found in response")
		return result
	}

	vr := schema.ValidateJSON("issues_delta", result.RawJSON)
	if !vr.Valid {
		result.SchemaErrors = vr.Errors
	}

	var raw struct {
		Add      []json.RawMessage `json:"add"`
		Retract  []string          `json:"retract"`
		Update   []json.RawMessage `json:"update"`
		Support  []json.RawMessage `json:"support"`
		Withdraw []json.RawMessage `json:"withdraw"`
		Contest  []json.RawMessage `json:"contest"`
	}
	if err := json.Unmarshal([]byte(result.RawJSON), &raw); err != nil {
		result.ParseError = fmt.Errorf("JSON syntax error: %w", err)
		return result
	}

	if raw.Add == nil || raw.Retract == nil || raw.Update == nil || raw.Support == nil || raw.Withdraw == nil || raw.Contest == nil {
		result.ParseError = fmt.Errorf("missing one of required keys: add/retract/update/support/withdraw/contest")
		return result
	}

	out := &StructurizeDelta{
		Retract:  make([]string, 0, len(raw.Retract)),
		Support:  make([]DeltaIssueRefAction, 0, len(raw.Support)),
		Withdraw: make([]DeltaIssueRefAction, 0, len(raw.Withdraw)),
		Contest:  make([]DeltaIssueRefAction, 0, len(raw.Contest)),
	}

	for _, id := range raw.Retract {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		out.Retract = append(out.Retract, id)
	}

	for _, rawAdd := range raw.Add {
		var m map[string]any
		if err := json.Unmarshal(rawAdd, &m); err != nil {
			continue
		}

		severity := parseStringField(m, "severity")
		if !validSeverities[severity] {
			continue
		}
		file := parseStringField(m, "file")
		title := parseStringField(m, "title")
		description := parseStringField(m, "description")
		if file == "" || title == "" || description == "" {
			continue
		}

		add := DeltaAddIssue{
			Severity:    severity,
			Category:    parseStringField(m, "category"),
			File:        file,
			Title:       title,
			Description: description,
		}
		if suggestedFix, ok := parseOptionalStringField(m, "suggestedFix"); ok {
			add.SuggestedFix = suggestedFix
		}
		if line, ok := parsePositiveIntField(m, "line"); ok {
			add.Line = &line
		}
		out.Add = append(out.Add, add)
	}

	for _, rawUpdate := range raw.Update {
		var m map[string]any
		if err := json.Unmarshal(rawUpdate, &m); err != nil {
			continue
		}

		update := DeltaUpdateIssue{
			ID: parseStringField(m, "id"),
		}
		if update.ID == "" {
			continue
		}

		if severity, ok := parseOptionalStringField(m, "severity"); ok {
			if !validSeverities[severity] {
				continue
			}
			update.Severity = &severity
		}
		if category, ok := parseOptionalStringField(m, "category"); ok {
			update.Category = &category
		}
		if file, ok := parseOptionalStringField(m, "file"); ok && file != "" {
			update.File = &file
		}
		if title, ok := parseOptionalStringField(m, "title"); ok && title != "" {
			update.Title = &title
		}
		if description, ok := parseOptionalStringField(m, "description"); ok && description != "" {
			update.Description = &description
		}
		if suggestedFix, ok := parseOptionalStringField(m, "suggestedFix"); ok {
			update.SuggestedFix = &suggestedFix
		}
		if line, ok := parsePositiveIntField(m, "line"); ok {
			update.Line = &line
		}

		out.Update = append(out.Update, update)
	}

	for _, action := range parseDeltaIssueRefActions(raw.Support) {
		out.Support = append(out.Support, action)
	}
	for _, action := range parseDeltaIssueRefActions(raw.Withdraw) {
		out.Withdraw = append(out.Withdraw, action)
	}
	for _, action := range parseDeltaIssueRefActions(raw.Contest) {
		out.Contest = append(out.Contest, action)
	}

	result.Output = out
	return result
}

func parseDeltaIssueRefActions(items []json.RawMessage) []DeltaIssueRefAction {
	actions := make([]DeltaIssueRefAction, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, rawItem := range items {
		var m map[string]any
		if err := json.Unmarshal(rawItem, &m); err != nil {
			continue
		}
		ref := parseStringField(m, "issueRef")
		if ref == "" {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		actions = append(actions, DeltaIssueRefAction{IssueRef: ref})
	}
	return actions
}

func extractJSON(response string, rawPattern *regexp.Regexp) string {
	// 优先尝试匹配```json代码块格式
	if m := jsonFenceRe.FindStringSubmatch(response); len(m) > 1 {
		return m[1]
	}
	// 回退方案：尝试匹配未包裹在代码块中的原始 JSON 对象
	if m := rawPattern.FindString(response); m != "" {
		return m
	}
	return ""
}

func parseStringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func parseOptionalStringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(s), true
}

func parsePositiveIntField(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	i := int(f)
	if i <= 0 || float64(i) != f {
		return 0, false
	}
	return i, true
}

// ParseFocusAreas 从分析器输出中提取建议的审查关注重点。
// 查找"## Suggested Review Focus"章节中的要点列表（支持-和*作为列表标记）。
// 返回提取到的关注重点字符串切片，未找到则返回nil。
func ParseFocusAreas(analysis string) []string {
	m := focusRe.FindStringSubmatch(analysis)
	if len(m) < 2 {
		return nil
	}

	lines := strings.Split(strings.TrimSpace(m[1]), "\n")
	var areas []string
	for _, line := range lines {
		// 去除列表项前缀（- 或 *）和多余空白
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

// DeduplicateIssues 跨多个审查者合并相似的问题。
// 合并条件：相同文件 + 行号范围重叠 + 标题/描述相似度超过阈值。
// 合并规则：
//   - 保留最高严重程度
//   - 记录所有提出该问题的审查者ID
//   - 保留所有描述（用于生成更全面的问题说明）
//   - 如果已有修复建议为空则使用新发现的修复建议
//
// 最终结果按严重程度排序（critical在前）。
func DeduplicateIssues(issuesByReviewer map[string][]ReviewIssue) []MergedIssue {
	var merged []MergedIssue

	for reviewerID, issues := range issuesByReviewer {
		for _, issue := range issues {
			found := false
			for i := range merged {
				if isSimilarIssue(&merged[i].ReviewIssue, &issue) {
					merged[i].RaisedBy = append(merged[i].RaisedBy, reviewerID)
					merged[i].SupportedBy = append(merged[i].SupportedBy, reviewerID)
					merged[i].Descriptions = append(merged[i].Descriptions, issue.Description)
					merged[i].Mentions = append(merged[i].Mentions, IssueMention{
						ReviewerID: reviewerID,
						Status:     "active",
					})
					// 保留最高严重程度（数值越小越严重）
					if severityOrder[issue.Severity] < severityOrder[merged[i].Severity] {
						merged[i].Severity = issue.Severity
					}
					// 如果当前合并问题没有修复建议，则使用新发现的
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
					IntroducedBy: []string{reviewerID},
					SupportedBy:  []string{reviewerID},
					Descriptions: []string{issue.Description},
					Mentions: []IssueMention{{
						ReviewerID: reviewerID,
						Status:     "active",
					}},
				})
			}
		}
	}

	// 按严重程度排序，critical排在最前面
	sort.Slice(merged, func(i, j int) bool {
		return severityOrder[merged[i].Severity] < severityOrder[merged[j].Severity]
	})

	for i := range merged {
		merged[i] = finalizeCanonicalIssue(merged[i])
	}

	return merged
}

// isSimilarIssue 检查两个问题是否足够相似可以合并。
// 判断逻辑：
//  1. 必须是同一个文件（完全匹配）
//  2. 行号范围必须重叠或相邻（5行以内）
//  3. 标题和描述的加权Jaccard相似度 > 0.35（标题权重0.7，描述权重0.3）
func isSimilarIssue(a, b *ReviewIssue) bool {
	// 必须是同一个文件
	if a.File != b.File {
		return false
	}

	// 检查行号范围是否重叠
	if !linesOverlap(a, b) {
		return false
	}

	// 计算标题相似度（过滤停用词后的Jaccard相似度）
	wordsA := filterStopWords(tokenize(strings.ToLower(a.Title)))
	wordsB := filterStopWords(tokenize(strings.ToLower(b.Title)))
	titleSim := jaccardSimilarity(wordsA, wordsB)

	// 计算描述相似度（仅取前50个词以控制计算量）
	descWordsA := filterStopWords(firstN(tokenize(strings.ToLower(a.Description)), 50))
	descWordsB := filterStopWords(firstN(tokenize(strings.ToLower(b.Description)), 50))
	descSim := jaccardSimilarity(descWordsA, descWordsB)

	// 加权相似度：标题权重70%，描述权重30%，阈值0.35
	return titleSim*0.7+descSim*0.3 > 0.35
}

// linesOverlap 检查两个问题的行号范围是否重叠或在5行的邻近范围内。
// 合并策略：
//   - 两边都有行号：要求范围重叠或相邻（5行内）
//   - 两边都没有行号：允许继续由文本相似度决定
//   - 仅一边有行号：视为不重叠，避免过度合并
func linesOverlap(a, b *ReviewIssue) bool {
	if a.Line == nil && b.Line == nil {
		return true
	}
	if a.Line == nil || b.Line == nil {
		return false
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
	// 判断两个范围是否重叠或在5行邻近距离内
	return aStart <= bEnd+5 && bStart <= aEnd+5
}

// jaccardSimilarity 计算两个词列表之间的Jaccard相似度。
// Jaccard系数 = |交集| / |并集|，范围[0,1]，1表示完全相同，0表示完全不同。
// 两个空集合的相似度定义为0。
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

	// 并集大小 = |A| + |B| - |交集|
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// filterStopWords 过滤常见的英文停用词。
// 停用词对文本语义贡献小，过滤后可以提高相似度计算的准确性。
func filterStopWords(words []string) []string {
	var result []string
	for _, w := range words {
		if w != "" && !stopWords[w] {
			result = append(result, w)
		}
	}
	return result
}

// tokenize 将文本分割为词列表，只保留字母和数字。
// 这会去除标点符号，避免 "vulnerability." 与 "vulnerability" 被视为不同词。
func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// firstN 返回切片的前n个元素。
// 如果切片长度不足n，返回整个切片。用于限制相似度计算的文本长度。
func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DeduplicateMergedIssues 对扁平的 MergedIssue 列表进行去重和按严重程度排序。
// 复用 isSimilarIssue() 比较逻辑，将相似问题合并（保留最高严重程度、合并 raisedBy 和描述）。
// 最终结果按 severity 排序（critical 在前）。
func DeduplicateMergedIssues(issues []MergedIssue) []MergedIssue {
	var merged []MergedIssue

	for _, issue := range issues {
		found := false
		for i := range merged {
			if isSimilarIssue(&merged[i].ReviewIssue, &issue.ReviewIssue) {
				// 合并 raisedBy（去重）
				seen := make(map[string]bool)
				for _, r := range merged[i].RaisedBy {
					seen[r] = true
				}
				for _, r := range issue.RaisedBy {
					if !seen[r] {
						merged[i].RaisedBy = append(merged[i].RaisedBy, r)
					}
				}
				merged[i].SupportedBy = append(merged[i].SupportedBy, issue.SupportedBy...)
				merged[i].IntroducedBy = append(merged[i].IntroducedBy, issue.IntroducedBy...)
				merged[i].WithdrawnBy = append(merged[i].WithdrawnBy, issue.WithdrawnBy...)
				merged[i].ContestedBy = append(merged[i].ContestedBy, issue.ContestedBy...)
				// 合并描述
				merged[i].Descriptions = append(merged[i].Descriptions, issue.Descriptions...)
				merged[i].Mentions = append(merged[i].Mentions, issue.Mentions...)
				// 保留最高严重程度
				if severityOrder[issue.Severity] < severityOrder[merged[i].Severity] {
					merged[i].Severity = issue.Severity
				}
				// 补充修复建议
				if merged[i].SuggestedFix == "" && issue.SuggestedFix != "" {
					merged[i].SuggestedFix = issue.SuggestedFix
				}
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, issue)
		}
	}

	// 按严重程度排序，critical 排在最前面
	sort.Slice(merged, func(i, j int) bool {
		return severityOrder[merged[i].Severity] < severityOrder[merged[j].Severity]
	})

	for i := range merged {
		merged[i] = finalizeCanonicalIssue(merged[i])
	}

	return merged
}

// FormatCallChainForReviewer 将原始引用数据格式化为可读的调用链Markdown文档。
// 为每个被修改的符号（函数/变量），列出其在代码仓库中被引用的位置（最多10个）。
// 输出格式适合直接嵌入审查者的提示词中，帮助审查者理解变更的影响范围。
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
