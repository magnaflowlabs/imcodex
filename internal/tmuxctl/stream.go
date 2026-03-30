package tmuxctl

import (
	"regexp"
	"strings"
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

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
	if overlap := suffixPrefixOverlap(prev, curr); hasUsableOverlap(overlap) {
		return curr[overlap:], false
	}
	return curr, true
}

func hasUsableOverlap(overlap int) bool {
	// Tiny accidental overlaps (for example one shared punctuation mark)
	// can incorrectly classify a reset as append-only and cause repeated text.
	const minStableOverlap = 8
	return overlap >= minStableOverlap
}

func SliceAfter(base string, curr string) string {
	if curr == "" {
		return ""
	}
	if base == "" || curr == base {
		if curr == base {
			return ""
		}
		return curr
	}

	baseLines := strings.Split(base, "\n")
	currLines := strings.Split(curr, "\n")

	maxOverlap := min(len(baseLines), len(currLines))
	for n := maxOverlap; n > 0; n-- {
		if equalLines(baseLines[len(baseLines)-n:], currLines[:n]) {
			return strings.TrimLeft(strings.Join(currLines[n:], "\n"), "\n")
		}
	}

	return curr
}

func IsBusy(snapshot string) bool {
	snapshot = stripANSI(snapshot)
	lines := strings.Split(snapshot, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "esc to interrupt") {
			return true
		}
		if isTrailingBusyChrome(line) {
			continue
		}
		return false
	}
	return false
}

func IsTrustPrompt(snapshot string) bool {
	return strings.Contains(snapshot, "Do you trust the contents of this directory?") ||
		strings.Contains(snapshot, "Press enter to continue")
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
		strings.HasPrefix(line, "›"):
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

func isTrailingBusyChrome(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	if line == "›" || line == ">" {
		return true
	}
	if strings.HasPrefix(line, "›") {
		return true
	}
	return shouldIgnoreLine(line)
}

func suffixPrefixOverlap(prev string, curr string) int {
	limit := min(len(prev), len(curr))
	for size := limit; size > 0; size-- {
		if prev[len(prev)-size:] == curr[:size] {
			return size
		}
	}
	return 0
}

func equalLines(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func min(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
