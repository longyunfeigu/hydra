package github

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var hunkHeaderRegex = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// ParseDiffLines extracts valid right-side line numbers from a unified diff patch.
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
			// Left side only - don't increment right line counter
		} else if strings.HasPrefix(line, " ") {
			// Context line - valid on both sides
			lines[rightLine] = true
			rightLine++
		}
		// Skip anything else ("\ No newline at end of file", trailing empty lines)
	}

	return lines
}

type diffFile struct {
	Filename string `json:"filename"`
	Patch    string `json:"patch"`
}

// GetDiffInfo returns file -> valid line numbers mapping for a PR.
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

	diffInfo := make(map[string]map[int]bool)
	for _, f := range files {
		if f.Patch != "" {
			diffInfo[f.Filename] = ParseDiffLines(f.Patch)
		} else {
			diffInfo[f.Filename] = make(map[int]bool)
		}
	}

	return diffInfo, nil
}
