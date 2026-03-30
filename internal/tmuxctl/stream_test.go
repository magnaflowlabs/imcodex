package tmuxctl

import "testing"

func TestNormalizeSnapshot(t *testing.T) {
	t.Parallel()

	raw := `╭─────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.116.0-alpha.1)          │
│                                             │
│ model:     gpt-5.4 xhigh   /model to change │
│ directory: ~/tools                          │
╰─────────────────────────────────────────────╯

  Tip: New Build faster with the Codex App.

› hi

• Hello

› Write tests for @filename

  gpt-5.4 xhigh · 100% left · ~/tools
`

	got := NormalizeSnapshot(raw)
	want := "• Hello"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestDiffTextUsesOverlap(t *testing.T) {
	t.Parallel()

	prev := "• Alpha\n• Beta"
	curr := "• Beta\n• Gamma"

	delta, reset := DiffText(prev, curr)
	if reset {
		t.Fatal("reset = true, want false")
	}
	if got, want := delta, "\n• Gamma"; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestDiffTextReportsReset(t *testing.T) {
	t.Parallel()

	prev := "• One"
	curr := "• Two"

	delta, reset := DiffText(prev, curr)
	if !reset {
		t.Fatal("reset = false, want true")
	}
	if got, want := delta, curr; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestDiffTextTreatsTinyOverlapAsReset(t *testing.T) {
	t.Parallel()

	prev := "• alpha\n• beta"
	curr := "• alpha revised\n• gamma"

	delta, reset := DiffText(prev, curr)
	if !reset {
		t.Fatal("reset = false, want true for tiny accidental overlap")
	}
	if got, want := delta, curr; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestIsBusyHandlesTrailingPromptPlaceholder(t *testing.T) {
	t.Parallel()

	if !IsBusy("• Working (2s • esc to interrupt)\n\n› Implement {feature}") {
		t.Fatal("IsBusy() = false, want true")
	}
	if IsBusy("• Working (2s • esc to interrupt)\n\n• Final answer") {
		t.Fatal("IsBusy() = true, want false")
	}
}

func TestNormalizeSnapshotKeepsModelLikeContentLines(t *testing.T) {
	t.Parallel()

	raw := "model: gpt-5.4\n\ndirectory: /srv/demo\n\nchatgpt.com/codex\n\ncommunity.openai.com"
	got := NormalizeSnapshot(raw)
	if got != raw {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, raw)
	}
}

func TestNormalizeSnapshotKeepsNonPromptLinesAfterPromptPrefix(t *testing.T) {
	t.Parallel()

	raw := `› First line from user
Second line from user
Third line from user

• Assistant reply`

	got := NormalizeSnapshot(raw)
	want := "Second line from user\nThird line from user\n\n• Assistant reply"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}
