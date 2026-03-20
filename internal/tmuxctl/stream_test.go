package tmuxctl

import "testing"

func TestNormalizeSnapshot(t *testing.T) {
	t.Parallel()

	raw := `╭─────────────────────────────────────────────╮
│ >_ OpenAI Codex (v0.115.0-alpha.27)         │
│                                             │
│ model:     gpt-5.4 xhigh   /model to change │
│ directory: /Volumes/newver/flow             │
╰─────────────────────────────────────────────╯

  Tip: New Build faster with the Codex App.

› Just answer hello

• Hello

› Write tests for @filename

    fmt.Println("hello")

  gpt-5.4 xhigh · 100% left · /Volumes/newver/flow
`

	got := NormalizeSnapshot(raw)
	want := "› Just answer hello\n\n• Hello\n\n› Write tests for @filename\n\n    fmt.Println(\"hello\")"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestDiffText(t *testing.T) {
	t.Parallel()

	prev := "• Hel"
	curr := "• Hello"

	delta, reset := DiffText(prev, curr)
	if reset {
		t.Fatal("reset = true, want false")
	}
	if got, want := delta, "lo"; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestNormalizeSnapshotPreservesLeadingIndentationOnFirstLine(t *testing.T) {
	t.Parallel()

	raw := "    first line\n  second line\n"
	got := NormalizeSnapshot(raw)
	want := "    first line\n  second line"
	if got != want {
		t.Fatalf("NormalizeSnapshot() = %q, want %q", got, want)
	}
}

func TestDiffTextHandlesShiftedWindow(t *testing.T) {
	t.Parallel()

	prev := "line1\nline2"
	curr := "line2\nline3"

	delta, reset := DiffText(prev, curr)
	if reset {
		t.Fatal("reset = true, want false")
	}
	if got, want := delta, "\nline3"; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestDiffTextReturnsCurrentSnapshotOnReset(t *testing.T) {
	t.Parallel()

	prev := "line1"
	curr := "lineA"

	delta, reset := DiffText(prev, curr)
	if !reset {
		t.Fatal("reset = false, want true")
	}
	if got, want := delta, curr; got != want {
		t.Fatalf("delta = %q, want %q", got, want)
	}
}

func TestIsBusy(t *testing.T) {
	t.Parallel()

	raw := "• Working (2s • esc to interrupt)"
	if !IsBusy(raw) {
		t.Fatal("IsBusy() = false, want true")
	}
}
