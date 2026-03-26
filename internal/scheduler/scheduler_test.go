package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
)

type fakeMessenger struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeMessenger) SendTextToChat(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return nil
}

func (f *fakeMessenger) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.texts))
	copy(out, f.texts)
	return out
}

type fakeConsole struct {
	mu        sync.Mutex
	captures  []string
	sendTexts []string
}

func (f *fakeConsole) EnsureSession(context.Context, tmuxctl.SessionSpec) (bool, error) {
	return true, nil
}

func (f *fakeConsole) SendText(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendTexts = append(f.sendTexts, text)
	return nil
}

func (f *fakeConsole) Capture(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.captures) == 0 {
		return "", nil
	}
	out := f.captures[0]
	if len(f.captures) > 1 {
		f.captures = f.captures[1:]
	}
	return out, nil
}

func TestNewRejectsInvalidSchedule(t *testing.T) {
	t.Parallel()

	_, err := New([]Job{{
		GroupID:    "oc_1",
		CWD:        "/srv/demo",
		Name:       "hourly",
		Schedule:   "bad schedule",
		PromptFile: "/tmp/hourly.md",
	}}, &fakeMessenger{}, &fakeConsole{}, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "invalid schedule") {
		t.Fatalf("New() error = %v, want invalid schedule", err)
	}
}

func TestJobRunnerPostsFinalOutput(t *testing.T) {
	t.Parallel()

	promptFile := writeTempPrompt(t, "# hourly review\nsay hello")
	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Job output line 1",
			"• Job output line 1\n• Job output line 2",
			"• Job output line 1\n• Job output line 2",
		},
	}
	messenger := &fakeMessenger{}
	job := &jobRunner{
		job: Job{
			GroupID:     "oc_1",
			CWD:         t.TempDir(),
			Name:        "hourly_review",
			Schedule:    "1 * * * *",
			PromptFile:  promptFile,
			SessionName: "imcodex-job-demo",
		},
		messenger: messenger,
		console:   console,
		logger:    slog.Default(),
		pollEvery: 5 * time.Millisecond,
		startWait: 0,
		history:   2000,
	}

	if err := job.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	if got := console.sendTexts; len(got) != 1 || !strings.Contains(got[0], "say hello") {
		t.Fatalf("sendTexts = %#v, want prompt sent once", got)
	}

	outputs := messenger.all()
	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1", len(outputs))
	}
	if got, want := outputs[0], "[job:hourly_review]\n• Job output line 1\n• Job output line 2"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
	}
}

func TestJobRunnerPostsNoOutputNotice(t *testing.T) {
	t.Parallel()

	promptFile := writeTempPrompt(t, "summarize quietly")
	console := &fakeConsole{
		captures: []string{
			"",
			"",
			"",
		},
	}
	messenger := &fakeMessenger{}
	job := &jobRunner{
		job: Job{
			GroupID:     "oc_1",
			CWD:         t.TempDir(),
			Name:        "silent_job",
			Schedule:    "1 * * * *",
			PromptFile:  promptFile,
			SessionName: "imcodex-job-silent",
		},
		messenger: messenger,
		console:   console,
		logger:    slog.Default(),
		pollEvery: 5 * time.Millisecond,
		startWait: 0,
		history:   2000,
	}

	if err := job.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	outputs := messenger.all()
	if len(outputs) != 1 || outputs[0] != "[job:silent_job] completed with no visible output." {
		t.Fatalf("outputs = %#v, want no-output notice", outputs)
	}
}

func writeTempPrompt(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/prompt.md"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(prompt) error = %v", err)
	}
	return path
}
