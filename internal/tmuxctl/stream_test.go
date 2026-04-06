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

func TestDiffTextTreatsShortRepeatedOverlapAsReset(t *testing.T) {
	t.Parallel()

	prev := "0123456789abcdef-shared-tail"
	curr := "shared-tail-new-content"

	delta, reset := DiffText(prev, curr)
	if !reset {
		t.Fatal("reset = false, want true for short overlap to avoid tail loss")
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

func TestIsBusyUsesPromptAdjacentWindow(t *testing.T) {
	t.Parallel()

	raw := "• partial output\n• Working (28s • esc to interrupt)\n\n› Implement {feature}\n\n  gpt-5.4 high · 19% left · ~/repo"
	if !IsBusy(raw) {
		t.Fatal("IsBusy() = false, want true when working chrome is near prompt")
	}
}

func TestIsBusyFalseWhenPromptPresentWithoutWorkingChrome(t *testing.T) {
	t.Parallel()

	raw := "• final answer\n\n› Improve documentation in @filename\n\n  gpt-5.4 high · 92% left · ~/repo"
	if IsBusy(raw) {
		t.Fatal("IsBusy() = true, want false when prompt is present without working chrome")
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

func TestNormalizeSnapshotDropsPromptContinuationLines(t *testing.T) {
	t.Parallel()

	raw := `› First line from user
Second line from user
Third line from user

• Assistant reply`

	got := NormalizeSnapshot(raw)
	want := "• Assistant reply"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestNormalizeSnapshotKeepsAssistantTextMentioningInterruptPhrase(t *testing.T) {
	t.Parallel()

	raw := "• Document the phrase esc to interrupt for users"
	got := NormalizeSnapshot(raw)
	if got != raw {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, raw)
	}
}

func TestInputStatusSlot(t *testing.T) {
	t.Parallel()

	slot, ok := InputStatusSlot("• Working (2s • esc to interrupt)\n› explain")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := slot, "• Working (2s • esc to interrupt)"; got != want {
		t.Fatalf("slot = %q, want %q", got, want)
	}
}

func TestInputStatusSlotEmptyStatusLine(t *testing.T) {
	t.Parallel()

	slot, ok := InputStatusSlot("• final reply\n\n› ")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := slot, ""; got != want {
		t.Fatalf("slot = %q, want empty status slot", got)
	}
}

func TestInputStatusSlotMissingPrompt(t *testing.T) {
	t.Parallel()

	if _, ok := InputStatusSlot("• final reply only"); ok {
		t.Fatal("ok = true, want false when prompt line is missing")
	}
}
