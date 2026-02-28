package context

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CollectHistory 收集与变更文件相关的历史 PR 信息。
// 通过分析 git 提交历史中的 PR 引用（如 #123），找到最近 N 天内
// 涉及相同文件的历史 PR，并获取其详细信息。
// 这些数据帮助审查者了解相关代码区域的近期变更历史。
func CollectHistory(changedFiles []string, maxDays, maxPRs int, cwd string) ([]RelatedPR, error) {
	if len(changedFiles) == 0 {
		return nil, nil
	}

	// 设置默认值
	if maxDays <= 0 {
		maxDays = 30
	}
	if maxPRs <= 0 {
		maxPRs = 10
	}

	// 从 git 提交历史中提取涉及这些文件的 PR 编号
	prNumbers := extractPRNumbers(changedFiles, maxDays, cwd)

	// 逐个获取 PR 详情，直到达到最大数量限制
	var relatedPRs []RelatedPR
	for _, prNum := range prNumbers {
		if len(relatedPRs) >= maxPRs {
			break
		}
		pr := getPRDetails(prNum, changedFiles, cwd)
		if pr != nil {
			relatedPRs = append(relatedPRs, *pr)
		}
	}

	return relatedPRs, nil
}

// extractPRNumbers 从 git log 中提取涉及指定文件的 PR 编号。
// 使用 git log --since 限制时间范围，从提交消息中搜索 #N 格式的 PR 引用。
func extractPRNumbers(files []string, maxDays int, cwd string) []int {
	// 构建 git log 命令参数
	fileArgs := make([]string, 0, len(files)+6)
	fileArgs = append(fileArgs, "log",
		fmt.Sprintf("--since=%d days ago", maxDays),
		"--pretty=format:%s",
		"--name-only",
		"--",
	)
	fileArgs = append(fileArgs, files...)

	cmd := exec.Command("git", fileArgs...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		return nil
	}

	// 从提交消息中解析 PR 编号，去重
	seen := make(map[int]bool)
	var prNumbers []int

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 匹配常见的 PR 引用格式：(#123)、PR #123、pull/123
		for _, idx := range findPRNumbers(line) {
			if !seen[idx] {
				seen[idx] = true
				prNumbers = append(prNumbers, idx)
			}
		}
	}

	return prNumbers
}

// findPRNumbers 从单条提交消息中查找所有 #N 格式的 PR 编号。
// 逐字符扫描，找到 '#' 后提取后续数字组成 PR 编号。
func findPRNumbers(message string) []int {
	var numbers []int
	// 查找 #N 模式（如 (#123)、PR #456）
	for i := 0; i < len(message)-2; i++ {
		if message[i] == '#' {
			j := i + 1
			for j < len(message) && message[j] >= '0' && message[j] <= '9' {
				j++
			}
			if j > i+1 {
				n, err := strconv.Atoi(message[i+1 : j])
				if err == nil && n > 0 {
					numbers = append(numbers, n)
				}
			}
		}
	}
	return numbers
}

// ghPRView 是 gh pr view 命令 JSON 输出的反序列化结构体。
type ghPRView struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	MergedAt string `json:"mergedAt"`
	Author   struct {
		Login string `json:"login"`
	} `json:"author"`
	Files []struct {
		Path string `json:"path"`
	} `json:"files"`
}

// getPRDetails 通过 gh CLI 获取单个 PR 的详细信息。
// 计算与当前变更文件的重叠文件列表，并根据是否有重叠文件
// 判断关联程度（"direct" 表示直接修改相同文件，"same-module" 表示同模块）。
func getPRDetails(prNumber int, changedFiles []string, cwd string) *RelatedPR {
	cmd := exec.Command("gh", "pr", "view", strconv.Itoa(prNumber),
		"--json", "number,title,author,mergedAt,files",
	)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}

	var data ghPRView
	if json.Unmarshal(out, &data) != nil {
		return nil
	}

	// 计算当前 PR 变更文件与历史 PR 文件的重叠情况
	changedSet := make(map[string]bool, len(changedFiles))
	for _, f := range changedFiles {
		changedSet[f] = true
	}

	var overlapping []string
	for _, f := range data.Files {
		if changedSet[f.Path] {
			overlapping = append(overlapping, f.Path)
		}
	}

	// 根据是否有重叠文件确定关联程度
	relevance := "same-module"
	if len(overlapping) > 0 {
		relevance = "direct"
	}

	return &RelatedPR{
		Number:           data.Number,
		Title:            data.Title,
		Author:           data.Author.Login,
		MergedAt:         data.MergedAt,
		OverlappingFiles: overlapping,
		Relevance:        relevance,
	}
}
