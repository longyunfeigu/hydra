package context

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// CollectHistory gathers git history and related PRs for the changed files.
func CollectHistory(changedFiles []string, maxDays, maxPRs int, cwd string) ([]RelatedPR, error) {
	if len(changedFiles) == 0 {
		return nil, nil
	}

	if maxDays <= 0 {
		maxDays = 30
	}
	if maxPRs <= 0 {
		maxPRs = 10
	}

	// Get commits that touched these files in the last N days
	prNumbers := extractPRNumbers(changedFiles, maxDays, cwd)

	// Fetch PR details
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

func extractPRNumbers(files []string, maxDays int, cwd string) []int {
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

	// Parse PR numbers from commit messages
	seen := make(map[int]bool)
	var prNumbers []int

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Common patterns: (#123), PR #123, pull/123
		for _, idx := range findPRNumbers(line) {
			if !seen[idx] {
				seen[idx] = true
				prNumbers = append(prNumbers, idx)
			}
		}
	}

	return prNumbers
}

func findPRNumbers(message string) []int {
	var numbers []int
	// Look for (#N) pattern
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

	// Determine overlapping files and relevance
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
