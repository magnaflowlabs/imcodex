package xutil

import "testing"

func TestFirstNonEmptyPreservesOriginalValue(t *testing.T) {
	t.Parallel()

	got := FirstNonEmpty("", "  line one\n    line two  ", "fallback")
	if want := "  line one\n    line two  "; got != want {
		t.Fatalf("FirstNonEmpty() = %q, want %q", got, want)
	}
}

func TestFirstNonEmptySkipsWhitespaceOnlyValues(t *testing.T) {
	t.Parallel()

	got := FirstNonEmpty("   ", "\t", "value")
	if want := "value"; got != want {
		t.Fatalf("FirstNonEmpty() = %q, want %q", got, want)
	}
}
