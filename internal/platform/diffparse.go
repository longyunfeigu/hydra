package platform

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// hunkHeaderRegex 匹配 unified diff 的 hunk 头部行。
var hunkHeaderRegex = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ParseDiffLines 从 unified diff 补丁文本中解析出右侧（新文件）的有效行号集合。
func ParseDiffLines(patch string) map[int]bool {
	lines := make(map[int]bool)
	patchLines := strings.Split(patch, "\n")
	rightLine := 0

	for _, line := range patchLines {
		if m := hunkHeaderRegex.FindStringSubmatch(line); m != nil {
			rightLine, _ = strconv.Atoi(m[1])
			continue
		}

		if rightLine == 0 {
			continue
		}

		if strings.HasPrefix(line, "+") {
			lines[rightLine] = true
			rightLine++
		} else if strings.HasPrefix(line, "-") {
			// 删除行不递增右侧行号
		} else if strings.HasPrefix(line, " ") {
			lines[rightLine] = true
			rightLine++
		}
	}

	return lines
}

// ClassifyCommentsByDiff 根据 diff 信息对评论进行分类。
// diffInfo 是 map[filename]map[lineNumber]bool 格式的 diff 信息。
func ClassifyCommentsByDiff(comments []ReviewCommentInput, diffInfo map[string]map[int]bool) []ClassifiedComment {
	classified := make([]ClassifiedComment, 0, len(comments))
	for _, c := range comments {
		// 先尝试精确匹配，再尝试后缀匹配（reviewer 可能输出短路径如 "base_parser.py"）
		resolvedPath := resolvePathByDiff(c.Path, diffInfo)
		resolved := c
		resolved.Path = resolvedPath

		fileLines, fileInDiff := diffInfo[resolvedPath]
		if fileInDiff && c.Line != nil && fileLines[*c.Line] {
			// 行号在 diff 范围内：inline
			classified = append(classified, ClassifiedComment{Input: resolved, Mode: "inline"})
		} else if fileInDiff {
			// 文件在 diff 中但没有行号或行号不在范围内：
			// 自动分配第一个变更行，尝试 inline
			if firstLine := firstDiffLine(fileLines); firstLine > 0 {
				resolved.Line = &firstLine
				classified = append(classified, ClassifiedComment{Input: resolved, Mode: "inline"})
			} else {
				classified = append(classified, ClassifiedComment{Input: resolved, Mode: "file"})
			}
		} else {
			classified = append(classified, ClassifiedComment{Input: resolved, Mode: "global"})
		}
	}
	return classified
}

// firstDiffLine 返回 diff 行号集合中最小的行号。
func firstDiffLine(lines map[int]bool) int {
	min := 0
	for line := range lines {
		if min == 0 || line < min {
			min = line
		}
	}
	return min
}

// resolvePathByDiff 将评论中的文件路径解析为 diff 中的完整路径。
// 策略：精确匹配 → 后缀匹配（唯一时采用）。
// 例如 "base_parser.py" 匹配 "backend/infrastructure/parsers/base_parser.py"。
func resolvePathByDiff(path string, diffInfo map[string]map[int]bool) string {
	// 精确匹配
	if _, ok := diffInfo[path]; ok {
		return path
	}

	// 后缀匹配：找出所有以 path 结尾的 diff 文件
	suffix := "/" + path
	var matches []string
	for diffPath := range diffInfo {
		if strings.HasSuffix(diffPath, suffix) || diffPath == path {
			matches = append(matches, diffPath)
		}
	}

	// 唯一匹配时采用
	if len(matches) == 1 {
		return matches[0]
	}

	return path
}

// AnnotateDiffWithLineNumbers 给 unified diff 的每一行加上显式的新文件行号前缀。
// 这样 AI reviewer 可以直接看到每行的行号，无需从 hunk header 手动推算。
// 例如: "255:  existing line", "256: +new line", "   : -removed line"
func AnnotateDiffWithLineNumbers(diff string) string {
	lines := strings.Split(diff, "\n")
	var result []string
	rightLine := 0
	maxWidth := 4 // 行号占位宽度

	for _, line := range lines {
		// hunk header: 提取起始行号，原样保留
		if m := hunkHeaderRegex.FindStringSubmatch(line); m != nil {
			rightLine, _ = strconv.Atoi(m[1])
			result = append(result, line)
			continue
		}

		// 文件级 header (diff --git, index, ---, +++, etc.)：原样保留并重置行号
		if strings.HasPrefix(line, "diff ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "new file") || strings.HasPrefix(line, "deleted file") ||
			strings.HasPrefix(line, "old mode") || strings.HasPrefix(line, "new mode") ||
			strings.HasPrefix(line, "similarity") || strings.HasPrefix(line, "rename") {
			rightLine = 0
			result = append(result, line)
			continue
		}

		// 还没进入 hunk 区域
		if rightLine == 0 {
			result = append(result, line)
			continue
		}

		if strings.HasPrefix(line, "+") {
			result = append(result, fmt.Sprintf("%*d:%s", maxWidth, rightLine, line))
			rightLine++
		} else if strings.HasPrefix(line, "-") {
			// 删除行没有新文件行号
			result = append(result, fmt.Sprintf("%*s:%s", maxWidth, "", line))
		} else if strings.HasPrefix(line, " ") {
			result = append(result, fmt.Sprintf("%*d:%s", maxWidth, rightLine, line))
			rightLine++
		} else {
			// 空行或其他（如 "\ No newline at end of file"）
			result = append(result, line)
		}
	}

	return strings.Join(result, "\n")
}

// IsDuplicateComment 检查待发布的评论是否与已存在的评论重复。
func IsDuplicateComment(comment ReviewCommentInput, existing []ExistingComment) bool {
	prefix := TruncStr(comment.Body, 100)
	for _, e := range existing {
		if e.Path != comment.Path {
			continue
		}
		if (comment.Line == nil) != (e.Line == nil) {
			continue
		}
		if comment.Line != nil && e.Line != nil && *comment.Line != *e.Line {
			continue
		}
		if TruncStr(e.Body, 100) == prefix {
			return true
		}
	}
	return false
}
