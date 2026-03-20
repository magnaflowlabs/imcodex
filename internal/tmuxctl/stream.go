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

func DiffText(prev string, curr string) string {
	if curr == prev {
		return ""
	}
	if prev == "" {
		return curr
	}
	if curr == "" {
		return ""
	}

	prevLines := strings.Split(prev, "\n")
	currLines := strings.Split(curr, "\n")

	maxOverlap := min(len(prevLines), len(currLines))
	for n := maxOverlap; n > 0; n-- {
		if equalLines(prevLines[len(prevLines)-n:], currLines[:n]) {
			return strings.Join(currLines[n:], "\n")
		}
	}

	if strings.HasSuffix(curr, prev) {
		return ""
	}
	return curr
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

func OutputBody(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	blocks := splitBlocks(strings.Split(text, "\n"))
	start := trimLeadingPromptBlocks(blocks, 0)
	if start >= len(blocks) {
		return ""
	}

	end := trimTrailingPromptBlocks(blocks, len(blocks), start)
	if end <= start {
		return ""
	}

	kept := make([]string, 0, end-start)
	afterPrompt := false
	for _, block := range blocks[start:end] {
		if isPromptBlock(block) {
			afterPrompt = true
			continue
		}
		if isIndentedBlock(block) {
			if afterPrompt || len(kept) == 0 {
				continue
			}
		}
		kept = append(kept, strings.Join(block, "\n"))
		afterPrompt = false
	}
	return strings.TrimSpace(strings.Join(kept, "\n\n"))
}

func AppendOnlyDelta(prev string, curr string) (string, bool) {
	if curr == prev {
		return "", true
	}
	if prev == "" {
		return curr, true
	}
	if curr == "" {
		return "", true
	}

	prevLines := strings.Split(prev, "\n")
	currLines := strings.Split(curr, "\n")

	maxOverlap := min(len(prevLines), len(currLines))
	for n := maxOverlap; n > 0; n-- {
		if equalLines(prevLines[len(prevLines)-n:], currLines[:n]) {
			return strings.Join(currLines[n:], "\n"), true
		}
	}

	return "", false
}

func IsBusy(snapshot string) bool {
	lines := recentNonEmptyLines(snapshot, 3)
	if len(lines) == 0 {
		return false
	}

	last := strings.ToLower(lines[len(lines)-1])
	if strings.Contains(last, "esc to interrupt") {
		return true
	}
	if len(lines) >= 2 && isPromptCursorLine(last) {
		return strings.Contains(strings.ToLower(lines[len(lines)-2]), "esc to interrupt")
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
		strings.HasPrefix(line, "Press enter to continue"):
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

func recentNonEmptyLines(snapshot string, maxLines int) []string {
	snapshot = stripANSI(snapshot)
	lines := strings.Split(snapshot, "\n")

	filtered := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			filtered = append(filtered, line)
		}
	}
	if maxLines > 0 && len(filtered) > maxLines {
		filtered = filtered[len(filtered)-maxLines:]
	}
	return filtered
}

func isPromptCursorLine(line string) bool {
	line = strings.TrimSpace(line)
	return line == "›" || line == ">"
}

func isPromptLine(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "›") || strings.HasPrefix(line, ">")
}

func isPromptBlock(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		return isPromptLine(trimmed)
	}
	return false
}

func isIndentedBlock(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		return line[0] == ' ' || line[0] == '\t'
	}
	return false
}

func trimLeadingPromptBlocks(blocks [][]string, start int) int {
	skippedPrompt := false
	for start < len(blocks) {
		switch {
		case isPromptBlock(blocks[start]):
			start++
			skippedPrompt = true
		case skippedPrompt && isIndentedBlock(blocks[start]):
			start++
		default:
			return start
		}
	}
	return start
}

func trimTrailingPromptBlocks(blocks [][]string, end int, floor int) int {
	for end > floor {
		if !isPromptBlock(blocks[end-1]) {
			return end
		}
		end--
	}
	return end
}

func splitBlocks(lines []string) [][]string {
	var blocks [][]string
	var current []string
	flush := func() {
		if len(current) == 0 {
			return
		}
		block := make([]string, len(current))
		copy(block, current)
		blocks = append(blocks, block)
		current = nil
	}

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()
	return blocks
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
