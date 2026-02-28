// Package github 提供与 GitHub API 交互的功能，包括发布 PR 评审评论、解析 diff 等。
package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// hunkHeaderRegex 匹配 unified diff 的 hunk 头部行。
// 格式示例：@@ -10,5 +20,8 @@ ，其中 +20 表示右侧（新文件）的起始行号。
var hunkHeaderRegex = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ParseDiffLines 从 unified diff 补丁文本中解析出右侧（新文件）的有效行号集合。
// 返回一个 map，key 为行号，value 为 true 表示该行在 diff 中存在。
// 这些行号用于确定评论是否可以作为行内评论发布（GitHub 只允许在 diff 范围内的行上发布行内评论）。
//
// 解析规则：
//   - "+" 开头的行：新增行，记录行号并递增计数器
//   - "-" 开头的行：删除行，不影响右侧行号计数器
//   - " " 开头的行：上下文行（两侧共有），记录行号并递增计数器
//   - 其他行（如 "\ No newline at end of file"）：跳过
func ParseDiffLines(patch string) map[int]bool {
	lines := make(map[int]bool)
	patchLines := strings.Split(patch, "\n")
	rightLine := 0

	for _, line := range patchLines {
		// 解析 hunk 头部，获取右侧起始行号
		if m := hunkHeaderRegex.FindStringSubmatch(line); m != nil {
			rightLine, _ = strconv.Atoi(m[1])
			continue
		}

		// 尚未遇到 hunk 头部，跳过
		if rightLine == 0 {
			continue
		}

		if strings.HasPrefix(line, "+") {
			// 新增行：记录右侧行号并递增
			lines[rightLine] = true
			rightLine++
		} else if strings.HasPrefix(line, "-") {
			// 删除行：仅存在于左侧，不递增右侧行号计数器
		} else if strings.HasPrefix(line, " ") {
			// 上下文行：两侧共有，记录行号并递增
			lines[rightLine] = true
			rightLine++
		}
		// 其他内容（如 "\ No newline at end of file"、末尾空行）直接跳过
	}

	return lines
}

// diffFile 表示 GitHub API 返回的 PR 文件信息，包含文件名和 patch 内容。
type diffFile struct {
	Filename string `json:"filename"` // 文件路径
	Patch    string `json:"patch"`    // 该文件的 unified diff 补丁内容
}

// GetDiffInfo 获取 PR 中每个文件的有效行号映射。
// 返回 map[文件名]map[行号]bool，用于判断评论能否作为行内评论发布。
// 通过 GitHub API (repos/{owner}/{repo}/pulls/{pr}/files) 获取文件列表和 patch。
func GetDiffInfo(prNumber, repo string) (map[string]map[int]bool, error) {
	out, err := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/pulls/%s/files", repo, prNumber),
		"--paginate",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get PR files: %w", err)
	}

	var files []diffFile
	if err := json.Unmarshal(out, &files); err != nil {
		return nil, fmt.Errorf("failed to parse PR files: %w", err)
	}

	// 为每个文件解析有效行号
	diffInfo := make(map[string]map[int]bool)
	for _, f := range files {
		if f.Patch != "" {
			diffInfo[f.Filename] = ParseDiffLines(f.Patch)
		} else {
			// 没有 patch 内容的文件（如二进制文件），创建空的行号映射
			diffInfo[f.Filename] = make(map[int]bool)
		}
	}

	return diffInfo, nil
}
