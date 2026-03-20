package tmuxctl

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

const minReliableOverlapRunes = 4

func NormalizeSnapshot(snapshot string) string {
	snapshot = stripANSI(snapshot)
	lines := strings.Split(snapshot, "\n")

	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			if len(out) == 0 || prevBlank {
				continue
			}
			out = append(out, "")
			prevBlank = true
			continue
		}
		if shouldIgnoreLine(trimmed) {
			continue
		}

		out = append(out, line)
		prevBlank = false
	}

	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func DiffText(prev string, curr string) (string, bool) {
	if curr == prev {
		return "", false
	}
	if prev == "" {
		return curr, false
	}
	if strings.HasPrefix(curr, prev) {
		return curr[len(prev):], false
	}
	if delta, ok := diffByOverlap(prev, curr); ok {
		return delta, false
	}
	return curr, true
}

func IsBusy(snapshot string) bool {
	return strings.Contains(snapshot, "esc to interrupt")
}

func IsTrustPrompt(snapshot string) bool {
	return strings.Contains(snapshot, "Do you trust the contents of this directory?") ||
		strings.Contains(snapshot, "Press enter to continue")
}

func IsApprovalPrompt(snapshot string) bool {
	lower := strings.ToLower(snapshot)

	switch {
	case strings.Contains(lower, "allow") &&
		strings.Contains(lower, "deny") &&
		(strings.Contains(lower, "command") || strings.Contains(lower, "sandbox") || strings.Contains(lower, "approval")):
		return true
	case strings.Contains(lower, "approve") && strings.Contains(lower, "command"):
		return true
	case strings.Contains(lower, "approval") && strings.Contains(lower, "command"):
		return true
	case strings.Contains(lower, "run command") &&
		(strings.Contains(lower, "allow") || strings.Contains(lower, "deny") || strings.Contains(lower, "approve")):
		return true
	default:
		return false
	}
}

func stripANSI(in string) string {
	return ansiPattern.ReplaceAllString(in, "")
}

func shouldIgnoreLine(line string) bool {
	switch {
	case strings.HasPrefix(line, "╭"),
		strings.HasPrefix(line, "│"),
		strings.HasPrefix(line, "╰"),
		strings.HasPrefix(line, "Tip:"),
		strings.HasPrefix(line, "model:"),
		strings.HasPrefix(line, "directory:"),
		strings.HasPrefix(line, "Do you trust the contents of this directory?"),
		strings.HasPrefix(line, "comes with higher risk of prompt injection."),
		strings.HasPrefix(line, "1. Yes, continue"),
		strings.HasPrefix(line, "2. No, quit"),
		strings.HasPrefix(line, "Press enter to continue"),
		line == "›":
		return true
	case strings.Contains(line, "chatgpt.com/codex"),
		strings.Contains(line, "community.openai.com"),
		strings.Contains(line, "% left ·"),
		strings.Contains(line, "esc to interrupt"):
		return true
	default:
		return false
	}
}

func diffByOverlap(prev string, curr string) (string, bool) {
	prevBoundaries := runeBoundaries(prev)
	prevRunes := len(prevBoundaries) - 1
	currRunes := utf8.RuneCountInString(curr)
	maxRunes := min(prevRunes, currRunes)

	for size := maxRunes; size > 0; size-- {
		start := prevBoundaries[prevRunes-size]
		overlap := prev[start:]
		if !isReliableOverlap(overlap, size) {
			continue
		}

		if pos := strings.Index(curr, overlap); pos >= 0 {
			return curr[pos+len(overlap):], true
		}
	}

	return "", false
}

func isReliableOverlap(overlap string, size int) bool {
	return size >= minReliableOverlapRunes || strings.ContainsRune(overlap, '\n')
}

func runeBoundaries(text string) []int {
	boundaries := make([]int, 0, utf8.RuneCountInString(text)+1)
	for idx := range text {
		boundaries = append(boundaries, idx)
	}
	return append(boundaries, len(text))
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
