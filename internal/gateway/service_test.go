package gateway

import (
	"context"
	"errors"
	"log/slog"
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
	mu            sync.Mutex
	captures      []string
	captureErrors []error
	sendTexts     []string
	ensureErrors  []error
	sendErrors    []error
}

func (f *fakeConsole) EnsureSession(context.Context, tmuxctl.SessionSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ensureErrors) > 0 {
		err := f.ensureErrors[0]
		if len(f.ensureErrors) > 1 {
			f.ensureErrors = f.ensureErrors[1:]
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (f *fakeConsole) SendText(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendTexts = append(f.sendTexts, text)
	if len(f.sendErrors) > 0 {
		err := f.sendErrors[0]
		if len(f.sendErrors) > 1 {
			f.sendErrors = f.sendErrors[1:]
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeConsole) Capture(context.Context, string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.captureErrors) > 0 {
		err := f.captureErrors[0]
		if len(f.captureErrors) > 1 {
			f.captureErrors = f.captureErrors[1:]
		}
		if err != nil {
			return "", err
		}
	}
	if len(f.captures) == 0 {
		return "", nil
	}
	out := f.captures[0]
	if len(f.captures) > 1 {
		f.captures = f.captures[1:]
	}
	return out, nil
}

func (f *fakeConsole) allSendTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sendTexts))
	copy(out, f.sendTexts)
	return out
}

func TestServiceBridgesMessageToConsole(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Hel",
			"• Hello\n\n  gpt-5.4 xhigh · 100% left · /srv/demo",
			"• Hello\n\n  gpt-5.4 xhigh · 100% left · /srv/demo",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "Just answer hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		joined := strings.Join(messenger.all(), "\n")
		return strings.Contains(joined, "[working]") && strings.Contains(joined, "Hello")
	})

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "›") {
		t.Fatalf("messages = %#v, want prompt lines hidden", messenger.all())
	}
}

func TestServiceRejectsUnknownGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &fakeMessenger{}
	console := &fakeConsole{}
	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_other",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	if got := messenger.all(); len(got) != 0 {
		t.Fatalf("messages = %#v, want ignored unknown-group message", got)
	}
}

func TestServiceStreamsScrolledSnapshotWithoutRepeatingPrefix(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Alpha\n• Beta",
			"• Beta\n• Gamma",
			"• Beta\n• Gamma",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "summarize progress",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 2
	})

	outputs := nonStatusMessages(messenger.all())
	if got, want := outputs[0], "• Alpha\n• Beta"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
	}
	if got, want := outputs[1], "• Gamma"; got != want {
		t.Fatalf("outputs[1] = %q, want %q", got, want)
	}
}

func TestServiceDoesNotReplayPreviousHistoryOnNewRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• old reply one\n\n• old reply two",
			"• old reply one\n\n• old reply two\n\n• Working (1s • esc to interrupt)",
			"• old reply one\n\n• old reply two\n\n• new reply start",
			"• old reply one\n\n• old reply two\n\n• new reply start\n\n• new reply final",
			"• old reply one\n\n• old reply two\n\n• new reply start\n\n• new reply final",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "new question",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 1
	})

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "old reply one") || strings.Contains(joined, "old reply two") {
		t.Fatalf("messages = %#v, want previous history excluded", messenger.all())
	}
	if !strings.Contains(joined, "new reply start") {
		t.Fatalf("messages = %#v, want current reply forwarded", messenger.all())
	}
}

func TestServiceQueuesSecondMessageUntilBusyClears(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Working (2s • esc to interrupt)",
			"",
			"",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 20 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "first"}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_2", GroupID: "oc_1", Text: "second"}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	time.Sleep(30 * time.Millisecond)
	if got := console.allSendTexts(); len(got) != 1 || got[0] != "first" {
		t.Fatalf("sendTexts = %#v, want only first while busy", got)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 2 && got[1] == "second"
	})
}

func TestServiceIgnoresDuplicateMessageIDs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{captures: []string{"", ""}}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	msg := IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "hello"}
	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	if got := console.allSendTexts(); len(got) != 1 {
		t.Fatalf("sendTexts = %#v, want single dispatch", got)
	}
}

func TestServiceRetriesAfterEnsureSessionFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures:     []string{"", ""},
		ensureErrors: []error{errors.New("tmux unavailable"), nil},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 10 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "hello"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	joined := strings.Join(messenger.all(), "\n")
	if !strings.Contains(joined, "tmux unavailable") {
		t.Fatalf("messages = %#v, want startup failure surfaced", messenger.all())
	}
}

func TestServiceRequeuesMessageAfterSendFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures:   []string{"", ""},
		sendErrors: []error{errors.New("send failed"), nil},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 10 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "hello"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 2 && got[0] == "hello" && got[1] == "hello"
	})
}

func nonStatusMessages(texts []string) []string {
	out := make([]string, 0, len(texts))
	for _, text := range texts {
		if strings.HasPrefix(text, "[working]") || strings.HasPrefix(text, "[imcodex]") {
			continue
		}
		out = append(out, text)
	}
	return out
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
