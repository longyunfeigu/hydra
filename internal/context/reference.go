package context

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var symbolPatterns = []*regexp.Regexp{
	// Go: func Name(
	regexp.MustCompile(`(?m)^\+.*func\s+(?:\([^)]+\)\s+)?([a-zA-Z_][a-zA-Z0-9_]*)\s*\(`),
	// Go: type Name struct/interface
	regexp.MustCompile(`(?m)^\+.*type\s+([a-zA-Z_][a-zA-Z0-9_]*)\s+(?:struct|interface)`),
	// JS/TS: function name(, async function name(
	regexp.MustCompile(`(?m)^\+.*(?:function|async function)\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*\(`),
	// JS/TS: const/let/var name = (
	regexp.MustCompile(`(?m)^\+.*(?:const|let|var)\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*(?:async\s*)?\(`),
	// JS/TS: arrow function
	regexp.MustCompile(`(?m)^\+.*(?:const|let|var)\s+([a-zA-Z_][a-zA-Z0-9_]*)\s*=\s*(?:async\s*)?\([^)]*\)\s*=>`),
	// Class definitions
	regexp.MustCompile(`(?m)^\+.*class\s+([a-zA-Z_][a-zA-Z0-9_]*)`),
	// JS/TS: export
	regexp.MustCompile(`(?m)^\+.*export\s+(?:const|let|var|function|class|async function)\s+([a-zA-Z_][a-zA-Z0-9_]*)`),
}

var skipSymbols = map[string]bool{
	"get": true, "set": true, "new": true, "for": true,
	"if": true, "do": true, "var": true, "nil": true,
	"err": true, "ok": true,
}

// ExtractSymbolsFromDiff extracts function/class/method names from diff + lines.
func ExtractSymbolsFromDiff(diff string) []string {
	seen := make(map[string]bool)
	var symbols []string

	for _, pattern := range symbolPatterns {
		matches := pattern.FindAllStringSubmatch(diff, -1)
		for _, m := range matches {
			name := m[1]
			if len(name) <= 2 || skipSymbols[name] || seen[name] {
				continue
			}
			seen[name] = true
			symbols = append(symbols, name)
		}
	}

	return symbols
}

// FindReferences uses ripgrep to find all occurrences of symbols in the codebase.
func FindReferences(symbols []string, cwd string) []RawReference {
	var references []RawReference

	for _, symbol := range symbols {
		// Use ripgrep: -n line numbers, -H filename, --no-heading no grouping
		cmd := exec.Command("rg", "-n", "-H", "--no-heading", symbol)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil || len(out) == 0 {
			continue
		}

		var locations []ReferenceLocation
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if line == "" {
				continue
			}
			// Format: file:line:content
			parts := strings.SplitN(line, ":", 3)
			if len(parts) < 3 {
				continue
			}
			lineNum := 0
			fmt.Sscanf(parts[1], "%d", &lineNum)
			locations = append(locations, ReferenceLocation{
				File:    parts[0],
				Line:    lineNum,
				Content: strings.TrimSpace(parts[2]),
			})
		}

		if len(locations) > 0 {
			references = append(references, RawReference{
				Symbol:       symbol,
				FoundInFiles: locations,
			})
		}
	}

	return references
}

// FormatCallChainForReviewer formats raw references into structured markdown for reviewers.
func FormatCallChainForReviewer(references []RawReference) string {
	if len(references) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("## Call Chain Context\n\n")

	for i, ref := range references {
		if i > 0 {
			sb.WriteString("\n\n---\n\n")
		}
		callers := ref.FoundInFiles
		if len(callers) > 10 {
			callers = callers[:10]
		}

		sb.WriteString(fmt.Sprintf("### Callers of `%s`\n", ref.Symbol))
		sb.WriteString(fmt.Sprintf("Found in %d locations:\n\n", len(ref.FoundInFiles)))

		for j, f := range callers {
			content := f.Content
			if len(content) > 150 {
				content = content[:150]
			}
			sb.WriteString(fmt.Sprintf("%d. %s:%d\n   > %s\n\n", j+1, f.File, f.Line, content))
		}
	}

	return sb.String()
}

// CollectReferences extracts symbols from the diff and finds their references.
func CollectReferences(diff, cwd string) []RawReference {
	symbols := ExtractSymbolsFromDiff(diff)
	return FindReferences(symbols, cwd)
}
