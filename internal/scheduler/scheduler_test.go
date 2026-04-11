package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
	ensure    []tmuxctl.SessionSpec
}

func (f *fakeConsole) EnsureSession(_ context.Context, spec tmuxctl.SessionSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensure = append(f.ensure, spec)
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

func (f *fakeConsole) ensured() []tmuxctl.SessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tmuxctl.SessionSpec, len(f.ensure))
	copy(out, f.ensure)
	return out
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

func TestJobRunnerPromptSessionPassesLaunchOverride(t *testing.T) {
	t.Parallel()

	promptFile := writeTempPrompt(t, "review the latest changes")
	console := &fakeConsole{captures: []string{"", "", ""}}
	job := &jobRunner{
		job: Job{
			GroupID:       "oc_1",
			CWD:           t.TempDir(),
			Name:          "claude_review",
			Schedule:      "1 * * * *",
			PromptFile:    promptFile,
			SessionName:   "imcodex-job-claude-review",
			LaunchCommand: "exec '/srv/imcodex/imcodex' 'internal-run-docker-codex' '--workspace' '{cwd}' '--session' '{session_name}'",
		},
		messenger: &fakeMessenger{},
		console:   console,
		logger:    slog.Default(),
		pollEvery: 5 * time.Millisecond,
		startWait: 0,
		history:   2000,
	}

	if err := job.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	specs := console.ensured()
	if len(specs) != 1 {
		t.Fatalf("len(ensure) = %d, want 1", len(specs))
	}
	if got, want := specs[0].LaunchCommand, "exec '/srv/imcodex/imcodex' 'internal-run-docker-codex' '--workspace' '{cwd}' '--session' '{session_name}'"; got != want {
		t.Fatalf("LaunchCommand = %q, want %q", got, want)
	}
	if got, want := specs[0].JobName, "claude_review"; got != want {
		t.Fatalf("JobName = %q, want %q", got, want)
	}
}

func TestCommandJobPostsSummaryAndWritesLogs(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	messenger := &fakeMessenger{}
	job := &jobRunner{
		job: Job{
			GroupID:  "oc_1",
			CWD:      cwd,
			Name:     "hl_stack_cycle",
			Schedule: "1 * * * *",
			Command: strings.Join([]string{
				"printf '[1/2] doctor\\n'",
				"printf 'cycle ok\\n'",
				"printf 'cycle summary\\nartifacts: %s\\n' \"$IMCODEX_JOB_ARTIFACTS_DIR\" > \"$IMCODEX_JOB_SUMMARY_FILE\"",
			}, "; "),
		},
		messenger: messenger,
		logger:    slog.Default(),
	}

	if err := job.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	outputs := messenger.all()
	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1", len(outputs))
	}
	if !strings.Contains(outputs[0], "[job:hl_stack_cycle] completed.") {
		t.Fatalf("outputs[0] = %q, want completed prefix", outputs[0])
	}
	if !strings.Contains(outputs[0], "cycle summary") {
		t.Fatalf("outputs[0] = %q, want summary content", outputs[0])
	}

	root := filepath.Join(cwd, ".imcodex", "jobs", "hl-stack-cycle")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", root, err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1 run dir", len(entries))
	}
	runDir := filepath.Join(root, entries[0].Name())
	for _, name := range []string{"stdout.log", "stderr.log", "combined.log", "summary.md"} {
		if _, err := os.Stat(filepath.Join(runDir, name)); err != nil {
			t.Fatalf("Stat(%s) error = %v", name, err)
		}
	}
}

func TestCommandJobPostsFailureWithStageHint(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	messenger := &fakeMessenger{}
	job := &jobRunner{
		job: Job{
			GroupID:  "oc_1",
			CWD:      cwd,
			Name:     "hl_stack_cycle",
			Schedule: "1 * * * *",
			Command: strings.Join([]string{
				"printf '[1/3] doctor\\n'",
				"printf '[2/3] cache record-once\\n'",
				"printf 'cache failed\\n' >&2",
				"exit 7",
			}, "; "),
		},
		messenger: messenger,
		logger:    slog.Default(),
	}

	err := job.run(context.Background())
	if err == nil {
		t.Fatal("run() error = nil, want failure")
	}

	var reported reportedError
	if !errors.As(err, &reported) {
		t.Fatalf("run() error = %T, want reportedError", err)
	}

	outputs := messenger.all()
	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1", len(outputs))
	}
	if !strings.Contains(outputs[0], "failed during [2/3] cache record-once") {
		t.Fatalf("outputs[0] = %q, want stage hint", outputs[0])
	}
	if !strings.Contains(outputs[0], "cache failed") {
		t.Fatalf("outputs[0] = %q, want stderr tail", outputs[0])
	}
}

func TestCommandJobDoesNotReuseStaleSummaryFile(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	summaryFile := filepath.Join(cwd, "summary.md")
	if err := os.WriteFile(summaryFile, []byte("stale summary\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(summary) error = %v", err)
	}
	messenger := &fakeMessenger{}
	job := &jobRunner{
		job: Job{
			GroupID:     "oc_1",
			CWD:         cwd,
			Name:        "stale_summary",
			Schedule:    "1 * * * *",
			Command:     "printf 'fresh output\\n'",
			SummaryFile: summaryFile,
		},
		messenger: messenger,
		logger:    slog.Default(),
	}

	if err := job.run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}

	outputs := messenger.all()
	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1", len(outputs))
	}
	if strings.Contains(outputs[0], "stale summary") {
		t.Fatalf("outputs[0] = %q, want stale summary cleared before run", outputs[0])
	}
	if !strings.Contains(outputs[0], "fresh output") {
		t.Fatalf("outputs[0] = %q, want stdout fallback when summary file stays empty", outputs[0])
	}
}

func TestBufferGroupConcurrentWrite(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var combined bytes.Buffer
	writer := multiBufferWriter(&stdout, &combined)

	const workers = 8
	const writesPerWorker = 200

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < writesPerWorker; n++ {
				if _, err := writer.Write([]byte("x")); err != nil {
					t.Errorf("Write() error = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	want := workers * writesPerWorker
	if got := stdout.Len(); got != want {
		t.Fatalf("stdout.Len() = %d, want %d", got, want)
	}
	if got := combined.Len(); got != want {
		t.Fatalf("combined.Len() = %d, want %d", got, want)
	}
	if stdout.String() != combined.String() {
		t.Fatalf("buffer contents diverged:\nstdout=%q\ncombined=%q", stdout.String(), combined.String())
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
