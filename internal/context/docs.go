package context

import (
	"os"
	"path/filepath"
	"strings"
)

var defaultDocPatterns = []string{
	"docs",
	"README.md",
	"ARCHITECTURE.md",
	"DESIGN.md",
	"CONTRIBUTING.md",
}

var skipDirs = map[string]bool{
	"node_modules": true, ".git": true, "dist": true,
	"build": true, "vendor": true, "__pycache__": true,
}

// CollectDocs finds and reads documentation files matching the given patterns.
func CollectDocs(patterns []string, maxSize int, cwd string) ([]RawDoc, error) {
	if len(patterns) == 0 {
		patterns = defaultDocPatterns
	}
	if maxSize <= 0 {
		maxSize = 50000
	}

	var docs []RawDoc
	seen := make(map[string]bool)

	for _, pattern := range patterns {
		fullPath := filepath.Join(cwd, pattern)

		info, err := os.Stat(fullPath)
		if err != nil {
			continue
		}

		if info.IsDir() {
			mdFiles := findMarkdownFiles(fullPath, maxSize)
			for _, file := range mdFiles {
				relPath, _ := filepath.Rel(cwd, file)
				if seen[relPath] {
					continue
				}
				seen[relPath] = true

				content, err := os.ReadFile(file)
				if err != nil {
					continue
				}
				docs = append(docs, RawDoc{Path: relPath, Content: string(content)})
			}
		} else if info.Size() <= int64(maxSize) {
			relPath, _ := filepath.Rel(cwd, fullPath)
			if seen[relPath] {
				continue
			}
			seen[relPath] = true

			content, err := os.ReadFile(fullPath)
			if err != nil {
				continue
			}
			docs = append(docs, RawDoc{Path: relPath, Content: string(content)})
		}
	}

	return docs, nil
}

func findMarkdownFiles(dir string, maxSize int) []string {
	var files []string

	entries, err := os.ReadDir(dir)
	if err != nil {
		return files
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())

		if entry.IsDir() {
			if skipDirs[entry.Name()] {
				continue
			}
			files = append(files, findMarkdownFiles(fullPath, maxSize)...)
		} else if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".md") {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if info.Size() <= int64(maxSize) {
				files = append(files, fullPath)
			}
		}
	}

	return files
}
