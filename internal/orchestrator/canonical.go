package orchestrator

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// CanonicalizeMergedIssues links per-reviewer issue views into canonical issues.
// It keeps richer attribution than RaisedBy alone and then projects SupportedBy back into RaisedBy.
func CanonicalizeMergedIssues(issues []MergedIssue) []MergedIssue {
	if len(issues) == 0 {
		return nil
	}

	ordered := append([]MergedIssue(nil), issues...)
	sort.SliceStable(ordered, func(i, j int) bool {
		ri := firstMentionRound(ordered[i])
		rj := firstMentionRound(ordered[j])
		if ri != rj {
			return ri < rj
		}
		si := severityOrder[ordered[i].Severity]
		sj := severityOrder[ordered[j].Severity]
		if si != sj {
			return si < sj
		}
		if ordered[i].File != ordered[j].File {
			return ordered[i].File < ordered[j].File
		}
		return ordered[i].Title < ordered[j].Title
	})

	var canonical []MergedIssue
	for _, issue := range ordered {
		normalized := normalizeMergedIssue(issue)
		bestIdx := -1
		bestScore := 0.0
		for i := range canonical {
			score, ok := canonicalMatchScore(canonical[i].ReviewIssue, normalized.ReviewIssue)
			if ok && score > bestScore {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			canonical = append(canonical, normalized)
			continue
		}
		mergeCanonicalIssue(&canonical[bestIdx], normalized)
	}

	final := make([]MergedIssue, 0, len(canonical))
	for _, issue := range canonical {
		finalized := finalizeCanonicalIssue(issue)
		if len(finalized.SupportedBy) == 0 {
			continue
		}
		final = append(final, finalized)
	}

	sort.SliceStable(final, func(i, j int) bool {
		si := severityOrder[final[i].Severity]
		sj := severityOrder[final[j].Severity]
		if si != sj {
			return si < sj
		}
		if len(final[i].SupportedBy) != len(final[j].SupportedBy) {
			return len(final[i].SupportedBy) > len(final[j].SupportedBy)
		}
		if final[i].File != final[j].File {
			return final[i].File < final[j].File
		}
		return final[i].Title < final[j].Title
	})

	return final
}

func ApplyCanonicalSignals(issues []MergedIssue, signals []CanonicalSignal) []MergedIssue {
	if len(issues) == 0 || len(signals) == 0 {
		return issues
	}

	result := append([]MergedIssue(nil), issues...)
	signals = append([]CanonicalSignal(nil), signals...)
	sort.SliceStable(signals, func(i, j int) bool {
		if signals[i].Round != signals[j].Round {
			return signals[i].Round < signals[j].Round
		}
		if signals[i].ReviewerID != signals[j].ReviewerID {
			return signals[i].ReviewerID < signals[j].ReviewerID
		}
		if signals[i].IssueRef != signals[j].IssueRef {
			return signals[i].IssueRef < signals[j].IssueRef
		}
		return signals[i].Action < signals[j].Action
	})

	for si := range signals {
		for i := range result {
			if !issueContainsRef(result[i], signals[si].IssueRef) {
				continue
			}
			applySignalToIssue(&result[i], signals[si])
			break
		}
	}

	for i := range result {
		result[i] = finalizeCanonicalIssue(result[i])
	}

	filtered := result[:0]
	for _, issue := range result {
		if len(issue.SupportedBy) == 0 {
			continue
		}
		filtered = append(filtered, issue)
	}
	return filtered
}

func BuildCanonicalIssueSummary(issues []MergedIssue) string {
	if len(issues) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("| Canonical ID | Severity | File:Line | Title | Supporters | Withdrawn | Contested | Issue Refs |\n")
	b.WriteString("|--------------|----------|-----------|-------|------------|-----------|-----------|------------|\n")
	for _, issue := range issues {
		b.WriteString(fmt.Sprintf(
			"| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			sanitizePipe(issue.CanonicalID),
			sanitizePipe(issue.Severity),
			sanitizePipe(issueFileLine(issue)),
			sanitizePipe(issue.Title),
			sanitizePipe(strings.Join(issue.SupportedBy, ", ")),
			sanitizePipe(strings.Join(issue.WithdrawnBy, ", ")),
			sanitizePipe(strings.Join(issue.ContestedBy, ", ")),
			sanitizePipe(strings.Join(issueRefs(issue), ", ")),
		))
	}
	return b.String()
}

func canonicalMatchScore(a, b ReviewIssue) (float64, bool) {
	if a.File != b.File {
		return 0, false
	}

	titleA := filterStopWords(tokenize(strings.ToLower(a.Title)))
	titleB := filterStopWords(tokenize(strings.ToLower(b.Title)))
	descA := filterStopWords(firstN(tokenize(strings.ToLower(a.Description)), 60))
	descB := filterStopWords(firstN(tokenize(strings.ToLower(b.Description)), 60))

	titleSim := jaccardSimilarity(titleA, titleB)
	descSim := jaccardSimilarity(descA, descB)
	lineCompatible := canonicalLineCompatible(&a, &b, titleSim, descSim)
	if !lineCompatible {
		return 0, false
	}

	categoryBonus := 0.0
	if strings.TrimSpace(a.Category) != "" && a.Category == b.Category {
		categoryBonus = 0.05
	}
	score := titleSim*0.6 + descSim*0.35 + categoryBonus

	switch {
	case normalizedIssueTitle(a.Title) == normalizedIssueTitle(b.Title) && descSim >= 0.12:
		return score + 0.15, true
	case titleSim >= 0.58 && descSim >= 0.18:
		return score, true
	case titleSim >= 0.45 && descSim >= 0.34:
		return score, true
	case titleSim >= 0.75 && descSim >= 0.10:
		return score, true
	default:
		return 0, false
	}
}

func canonicalLineCompatible(a, b *ReviewIssue, titleSim, descSim float64) bool {
	if a.Line == nil && b.Line == nil {
		return titleSim >= 0.45 || descSim >= 0.35
	}
	if a.Line == nil || b.Line == nil {
		return titleSim >= 0.78 && descSim >= 0.18
	}

	aStart, aEnd := issueLineRange(a)
	bStart, bEnd := issueLineRange(b)
	if aStart <= bEnd+8 && bStart <= aEnd+8 {
		return true
	}

	if titleSim >= 0.88 && descSim >= 0.15 {
		return true
	}
	return false
}

func mergeCanonicalIssue(dst *MergedIssue, src MergedIssue) {
	if severityOrder[src.Severity] < severityOrder[dst.Severity] {
		dst.ReviewIssue = src.ReviewIssue
	} else {
		if dst.Line == nil && src.Line != nil {
			dst.Line = src.Line
		}
		if dst.SuggestedFix == "" && src.SuggestedFix != "" {
			dst.SuggestedFix = src.SuggestedFix
		}
		if strings.TrimSpace(dst.Category) == "" && strings.TrimSpace(src.Category) != "" {
			dst.Category = src.Category
		}
	}

	dst.Descriptions = append(dst.Descriptions, src.Descriptions...)
	dst.IntroducedBy = append(dst.IntroducedBy, src.IntroducedBy...)
	dst.SupportedBy = append(dst.SupportedBy, src.SupportedBy...)
	dst.WithdrawnBy = append(dst.WithdrawnBy, src.WithdrawnBy...)
	dst.ContestedBy = append(dst.ContestedBy, src.ContestedBy...)
	dst.Mentions = append(dst.Mentions, src.Mentions...)
}

func applySignalToIssue(issue *MergedIssue, signal CanonicalSignal) {
	switch signal.Action {
	case "support":
		issue.SupportedBy = append(issue.SupportedBy, signal.ReviewerID)
		issue.RaisedBy = append(issue.RaisedBy, signal.ReviewerID)
		issue.WithdrawnBy = removeString(issue.WithdrawnBy, signal.ReviewerID)
		issue.ContestedBy = removeString(issue.ContestedBy, signal.ReviewerID)
	case "withdraw":
		issue.SupportedBy = removeString(issue.SupportedBy, signal.ReviewerID)
		issue.RaisedBy = removeString(issue.RaisedBy, signal.ReviewerID)
		issue.WithdrawnBy = append(issue.WithdrawnBy, signal.ReviewerID)
		issue.ContestedBy = removeString(issue.ContestedBy, signal.ReviewerID)
	case "contest":
		issue.SupportedBy = removeString(issue.SupportedBy, signal.ReviewerID)
		issue.RaisedBy = removeString(issue.RaisedBy, signal.ReviewerID)
		issue.WithdrawnBy = removeString(issue.WithdrawnBy, signal.ReviewerID)
		issue.ContestedBy = append(issue.ContestedBy, signal.ReviewerID)
	default:
		return
	}
	issue.Mentions = append(issue.Mentions, IssueMention{
		ReviewerID: signal.ReviewerID,
		Round:      signal.Round,
		Status:     signal.Action,
	})
}

func normalizeMergedIssue(issue MergedIssue) MergedIssue {
	issue.Descriptions = uniqueStrings(issue.Descriptions)
	issue.IntroducedBy = uniqueSorted(issue.IntroducedBy)
	issue.SupportedBy = uniqueSorted(issue.SupportedBy)
	issue.WithdrawnBy = uniqueSorted(issue.WithdrawnBy)
	issue.ContestedBy = uniqueSorted(issue.ContestedBy)
	issue.Mentions = uniqueMentions(issue.Mentions)

	if len(issue.SupportedBy) == 0 && len(issue.RaisedBy) > 0 {
		issue.SupportedBy = uniqueSorted(issue.RaisedBy)
	}
	if len(issue.IntroducedBy) == 0 {
		issue.IntroducedBy = uniqueSorted(issue.SupportedBy)
	}
	issue.RaisedBy = uniqueSorted(issue.SupportedBy)
	return issue
}

func finalizeCanonicalIssue(issue MergedIssue) MergedIssue {
	issue = normalizeMergedIssue(issue)

	if len(issue.Mentions) > 0 {
		earliest := firstMentionRound(issue)
		introduced := make([]string, 0, len(issue.Mentions))
		for _, mention := range issue.Mentions {
			if mention.Round == earliest {
				introduced = append(introduced, mention.ReviewerID)
			}
		}
		if len(introduced) > 0 {
			issue.IntroducedBy = uniqueSorted(introduced)
		}
	}

	if len(issue.IntroducedBy) == 0 {
		issue.IntroducedBy = uniqueSorted(issue.SupportedBy)
	}

	issue.WithdrawnBy = difference(uniqueSorted(issue.WithdrawnBy), uniqueSorted(issue.SupportedBy))
	issue.RaisedBy = uniqueSorted(issue.SupportedBy)
	issue.CanonicalID = buildCanonicalID(issue.ReviewIssue)
	issue.Descriptions = uniqueStrings(issue.Descriptions)
	issue.Mentions = uniqueMentions(issue.Mentions)
	return issue
}

func buildCanonicalID(issue ReviewIssue) string {
	key := strings.Join([]string{
		strings.TrimSpace(strings.ToLower(issue.File)),
		strings.TrimSpace(strings.ToLower(issue.Category)),
		normalizedIssueTitle(issue.Title),
		truncateCanonicalText(normalizedIssueDescription(issue.Description), 160),
	}, "|")
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:8])
}

func normalizedIssueTitle(s string) string {
	return strings.Join(filterStopWords(tokenize(strings.ToLower(s))), " ")
}

func normalizedIssueDescription(s string) string {
	return strings.Join(filterStopWords(firstN(tokenize(strings.ToLower(s)), 60)), " ")
}

func truncateCanonicalText(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

func issueLineRange(issue *ReviewIssue) (int, int) {
	if issue.Line == nil {
		return 0, 0
	}
	start := *issue.Line
	end := start
	if issue.EndLine != nil && *issue.EndLine >= start {
		end = *issue.EndLine
	}
	return start, end
}

func firstMentionRound(issue MergedIssue) int {
	if len(issue.Mentions) == 0 {
		return 0
	}
	minRound := issue.Mentions[0].Round
	for _, mention := range issue.Mentions[1:] {
		if minRound == 0 || (mention.Round > 0 && mention.Round < minRound) {
			minRound = mention.Round
		}
	}
	return minRound
}

func issueFileLine(issue MergedIssue) string {
	if issue.Line == nil {
		return issue.File
	}
	return fmt.Sprintf("%s:%d", issue.File, *issue.Line)
}

func issueRefs(issue MergedIssue) []string {
	refs := make([]string, 0, len(issue.Mentions))
	for _, mention := range issue.Mentions {
		if mention.LocalIssueID == "" {
			continue
		}
		refs = append(refs, issueRef(mention.ReviewerID, mention.LocalIssueID))
	}
	return uniqueSorted(refs)
}

func issueContainsRef(issue MergedIssue, ref string) bool {
	for _, candidate := range issueRefs(issue) {
		if candidate == ref {
			return true
		}
	}
	return false
}

func issueRef(reviewerID, localIssueID string) string {
	if strings.TrimSpace(reviewerID) == "" || strings.TrimSpace(localIssueID) == "" {
		return ""
	}
	return reviewerID + ":" + localIssueID
}

func uniqueStrings(items []string) []string {
	seen := make(map[string]struct{}, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result
}

func removeString(items []string, target string) []string {
	if len(items) == 0 {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item == target {
			continue
		}
		result = append(result, item)
	}
	return result
}

func uniqueSorted(items []string) []string {
	items = uniqueStrings(items)
	sort.Strings(items)
	return items
}

func difference(items, remove []string) []string {
	if len(items) == 0 {
		return nil
	}
	blocked := make(map[string]struct{}, len(remove))
	for _, item := range remove {
		blocked[item] = struct{}{}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if _, ok := blocked[item]; ok {
			continue
		}
		result = append(result, item)
	}
	return result
}

func uniqueMentions(mentions []IssueMention) []IssueMention {
	if len(mentions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(mentions))
	result := make([]IssueMention, 0, len(mentions))
	for _, mention := range mentions {
		key := fmt.Sprintf("%s|%s|%d|%s", mention.ReviewerID, mention.LocalIssueID, mention.Round, mention.Status)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, mention)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Round != result[j].Round {
			return result[i].Round < result[j].Round
		}
		if result[i].ReviewerID != result[j].ReviewerID {
			return result[i].ReviewerID < result[j].ReviewerID
		}
		if result[i].LocalIssueID != result[j].LocalIssueID {
			return result[i].LocalIssueID < result[j].LocalIssueID
		}
		return result[i].Status < result[j].Status
	})
	return result
}
