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
	mu           sync.Mutex
	captures     []string
	sendTexts    []string
	ensureErrors []error
	sendErrors   []error
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
	if len(f.captures) == 0 {
		return "", nil
	}
	out := f.captures[0]
	if len(f.captures) > 1 {
		f.captures = f.captures[1:]
	}
	return out, nil
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
		GroupID: "oc_1",
		Text:    "Just answer hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		texts := messenger.all()
		joined := strings.Join(texts, "\n")
		if strings.Contains(joined, "Codex is processing") && strings.Contains(joined, "Hello") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("messages = %#v, want processing + output", messenger.all())
}

func TestServiceRejectsUnknownGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &fakeMessenger{}
	console := &fakeConsole{}
	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_other",
		Text:    "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	texts := messenger.all()
	if len(texts) != 0 {
		t.Fatalf("messages = %#v, want ignored unknown-group message", texts)
	}
}

func TestServiceDoesNotDispatchSecondMessageImmediately(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{captures: []string{""}}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 50 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "first",
	}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "second",
	}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	time.Sleep(20 * time.Millisecond)

	console.mu.Lock()
	gotNow := append([]string(nil), console.sendTexts...)
	console.mu.Unlock()
	if len(gotNow) != 1 || gotNow[0] != "first" {
		t.Fatalf("immediate sendTexts = %#v, want only first message dispatched", gotNow)
	}

	time.Sleep(80 * time.Millisecond)

	console.mu.Lock()
	gotLater := append([]string(nil), console.sendTexts...)
	console.mu.Unlock()
	if len(gotLater) < 2 || gotLater[1] != "second" {
		t.Fatalf("later sendTexts = %#v, want second message dispatched after poll", gotLater)
	}
}

func TestServicePreservesMessageFormatting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{captures: []string{""}}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	want := "line1\n\n"
	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    want,
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		console.mu.Lock()
		got := append([]string(nil), console.sendTexts...)
		console.mu.Unlock()
		if len(got) > 0 {
			if got[0] != want {
				t.Fatalf("sendTexts[0] = %q, want %q", got[0], want)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for sendTexts")
}

func TestServiceIgnoresDuplicateMessageIDs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{captures: []string{""}}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	msg := IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "only once",
	}
	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(duplicate) error = %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		console.mu.Lock()
		got := append([]string(nil), console.sendTexts...)
		console.mu.Unlock()
		if len(got) > 0 {
			time.Sleep(20 * time.Millisecond)
			console.mu.Lock()
			got = append([]string(nil), console.sendTexts...)
			console.mu.Unlock()
			if len(got) != 1 || got[0] != "only once" {
				t.Fatalf("sendTexts = %#v, want single dispatch for duplicate message_id", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for sendTexts")
}

func TestServiceAllowsRetryAfterQueueFullForSameMessageID(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &fakeMessenger{}
	console := &fakeConsole{}
	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.runtime = &groupRuntime{
		opts:    svc.opts,
		session: svc.opts.SessionName,
		queue:   make(chan string, 1),
	}
	svc.runtime.queue <- "already queued"

	msg := IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "retry after full",
	}
	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(queue full) error = %v", err)
	}

	<-svc.runtime.queue

	if err := svc.HandleMessage(context.Background(), msg); err != nil {
		t.Fatalf("HandleMessage(retry) error = %v", err)
	}

	select {
	case got := <-svc.runtime.queue:
		if got != "retry after full" {
			t.Fatalf("queued text = %q, want retried payload", got)
		}
	default:
		t.Fatal("queue = empty, want retried payload enqueued")
	}
}

func TestServiceRetriesPendingMessageAfterSessionStartFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures:     []string{""},
		ensureErrors: []error{errors.New("tmux unavailable"), nil},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "retry me",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		console.mu.Lock()
		got := append([]string(nil), console.sendTexts...)
		console.mu.Unlock()
		if len(got) > 0 {
			if got[0] != "retry me" {
				t.Fatalf("sendTexts[0] = %q, want retried message", got[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for retry send")
}

func TestServiceRetriesFailedSendWithoutDroppingMessage(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures:   []string{""},
		sendErrors: []error{errors.New("paste failed"), nil},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "retry send",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		console.mu.Lock()
		got := append([]string(nil), console.sendTexts...)
		console.mu.Unlock()
		if len(got) >= 2 {
			if got[0] != "retry send" || got[1] != "retry send" {
				t.Fatalf("sendTexts = %#v, want retried send of same message", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("timed out waiting for send retry")
}

func TestServiceSendChunkedPreservesLeadingSpaces(t *testing.T) {
	t.Parallel()

	messenger := &fakeMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, slog.Default())

	want := "    fmt.Println(\"hello\")"
	svc.sendChunked("oc_1", want)

	got := messenger.all()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("messages = %#v, want preserved leading spaces", got)
	}
}
