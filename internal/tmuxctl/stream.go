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
	droppingPromptEcho := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			if droppingPromptEcho {
				droppingPromptEcho = false
				continue
			}
			if len(out) == 0 || prevBlank {
				continue
			}
			out = append(out, "")
			prevBlank = true
			continue
		}
		if isPromptEchoStart(trimmed) {
			droppingPromptEcho = true
			continue
		}
		if droppingPromptEcho {
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
	if overlap := suffixPrefixOverlap(prev, curr); hasUsableOverlap(prev, curr, overlap) {
		return curr[overlap:], false
	}
	return curr, true
}

func hasUsableOverlap(prev string, curr string, overlap int) bool {
	// Tiny accidental overlaps (for example one shared punctuation mark)
	// can incorrectly classify a reset as append-only and drop fresh output.
	const minStableOverlap = 8
	if overlap < minStableOverlap || overlap > len(prev) || overlap > len(curr) {
		return false
	}
	// Only trust overlaps aligned to whole-line boundaries. This keeps
	// pane-scroll dedupe while avoiding accidental mid-token truncation.
	prevStart := len(prev) - overlap
	prevBoundary := prevStart == 0 || prev[prevStart-1] == '\n'
	currBoundary := overlap == len(curr) || curr[overlap] == '\n'
	return prevBoundary && currBoundary
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
	if promptIdx := lastPromptLineIndex(lines); promptIdx >= 0 {
		start := promptIdx - 8
		if start < 0 {
			start = 0
		}
		end := promptIdx + 2
		if end >= len(lines) {
			end = len(lines) - 1
		}
		for i := start; i <= end; i++ {
			lower := strings.ToLower(strings.TrimSpace(lines[i]))
			if strings.Contains(lower, "esc to interrupt") {
				return true
			}
		}
		return false
	}
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

// InputStatusSlot returns the text on the line directly above the input prompt line.
// The boolean indicates whether a prompt line was found in the snapshot.
func InputStatusSlot(snapshot string) (string, bool) {
	snapshot = stripANSI(snapshot)
	lines := strings.Split(snapshot, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimRight(lines[i], " \t\r")
		if !isPromptLine(strings.TrimSpace(line)) {
			continue
		}
		if i == 0 {
			return "", true
		}
		return strings.TrimSpace(strings.TrimRight(lines[i-1], " \t\r")), true
	}
	return "", false
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
		strings.HasPrefix(line, "Do you trust the contents of this directory?"),
		strings.HasPrefix(line, "comes with higher risk of prompt injection."),
		strings.HasPrefix(line, "1. Yes, continue"),
		strings.HasPrefix(line, "2. No, quit"),
		strings.HasPrefix(line, "Press enter to continue"):
		return true
	case looksLikeModelStatusLine(line),
		looksLikeWorkingChromeLine(line):
		return true
	default:
		return false
	}
}

func isPromptEchoStart(line string) bool {
	line = strings.TrimSpace(line)
	return line == "›" || strings.HasPrefix(line, "› ")
}

func looksLikeWorkingChromeLine(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(line))
	if !strings.Contains(lower, "esc to interrupt") {
		return false
	}
	return strings.HasPrefix(lower, "• working") || strings.HasPrefix(lower, "working (")
}

func looksLikeModelStatusLine(line string) bool {
	line = strings.TrimSpace(line)
	lower := strings.ToLower(line)
	if !strings.Contains(line, "% left ·") {
		return false
	}
	if !strings.Contains(line, "· /") && !strings.Contains(line, "· ~/") {
		return false
	}
	return strings.Contains(lower, "gpt-") ||
		strings.Contains(lower, "codex") ||
		strings.Contains(lower, "o1") ||
		strings.Contains(lower, "o3") ||
		strings.Contains(lower, "o4")
}

func isTrailingBusyChrome(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return true
	}
	if isPromptLine(line) {
		return true
	}
	return shouldIgnoreLine(line)
}

func isPromptLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	return line == "›" || line == ">" || strings.HasPrefix(line, "›") || strings.HasPrefix(line, ">")
}

func lastPromptLineIndex(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if isPromptLine(strings.TrimSpace(lines[i])) {
			return i
		}
	}
	return -1
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
