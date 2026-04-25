package gateway

import (
	"strings"
	"testing"
)

func TestReadDownloadedResourceBodyRejectsOversize(t *testing.T) {
	t.Parallel()

	_, err := readDownloadedResourceBody(strings.NewReader("abcdef"), 5)
	if err == nil {
		t.Fatal("readDownloadedResourceBody() error = nil, want oversize error")
	}
	if !strings.Contains(err.Error(), "5 byte limit") {
		t.Fatalf("error = %v, want byte limit", err)
	}
}

func TestReadDownloadedResourceBodyAllowsExactLimit(t *testing.T) {
	t.Parallel()

	data, err := readDownloadedResourceBody(strings.NewReader("abcde"), 5)
	if err != nil {
		t.Fatalf("readDownloadedResourceBody() error = %v", err)
	}
	if got := string(data); got != "abcde" {
		t.Fatalf("data = %q, want abcde", got)
	}
}
