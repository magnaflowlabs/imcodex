package gateway

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	interrupts    []string
	ensureErrors  []error
	sendErrors    []error
	ensureEntered chan struct{}
	ensureBlock   <-chan struct{}
}

func (f *fakeConsole) EnsureSession(context.Context, tmuxctl.SessionSpec) (bool, error) {
	if f.ensureEntered != nil {
		select {
		case f.ensureEntered <- struct{}{}:
		default:
		}
	}
	if f.ensureBlock != nil {
		<-f.ensureBlock
	}
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

func (f *fakeConsole) Interrupt(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, "esc:"+session)
	return nil
}

func (f *fakeConsole) ForceInterrupt(_ context.Context, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interrupts = append(f.interrupts, "ctrl-c:"+session)
	return nil
}

func (f *fakeConsole) allSendTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sendTexts))
	copy(out, f.sendTexts)
	return out
}

func (f *fakeConsole) allInterrupts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.interrupts))
	copy(out, f.interrupts)
	return out
}

type fakeResourceFetcher struct {
	mu        sync.Mutex
	resources map[string]DownloadedResource
	errors    map[string]error
	requests  []string
}

func (f *fakeResourceFetcher) DownloadMessageResource(_ context.Context, messageID string, resourceType string, resourceKey string) (DownloadedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := messageID + "|" + resourceType + "|" + resourceKey
	f.requests = append(f.requests, key)
	if err, ok := f.errors[key]; ok && err != nil {
		return DownloadedResource{}, err
	}
	resource, ok := f.resources[key]
	if !ok {
		return DownloadedResource{}, errors.New("resource not found")
	}
	return resource, nil
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

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
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
		return strings.Contains(joined, "Hello")
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
	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())

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

func TestServiceDownloadsAttachmentsIntoInboxAndForwardsPrompt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cwd := t.TempDir()
	console := &fakeConsole{captures: []string{"", ""}}
	messenger := &fakeMessenger{}
	resources := &fakeResourceFetcher{
		resources: map[string]DownloadedResource{
			"om_file|file|file_v3_123": {
				Data:        []byte("pdf-bytes"),
				FileName:    "report.pdf",
				ContentType: "application/pdf",
			},
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: cwd, SessionName: "imcodex-demo"}, messenger, console, resources, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_file",
		GroupID:   "oc_1",
		Attachments: []IncomingAttachment{
			{ResourceType: "file", ResourceKey: "file_v3_123", FileName: "report.pdf"},
		},
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	got := console.allSendTexts()[0]
	if !strings.Contains(got, "User attached a file:") || !strings.Contains(got, ".imcodex/inbox/") || !strings.Contains(got, "report.pdf") {
		t.Fatalf("sendTexts[0] = %q, want attachment prompt with inbox path", got)
	}

	inboxDir := filepath.Join(cwd, ".imcodex", "inbox")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", inboxDir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	data, err := os.ReadFile(filepath.Join(inboxDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("ReadFile(saved attachment) error = %v", err)
	}
	if got, want := string(data), "pdf-bytes"; got != want {
		t.Fatalf("saved attachment = %q, want %q", got, want)
	}
}

func TestServiceForwardsTextAndMultipleAttachments(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cwd := t.TempDir()
	console := &fakeConsole{captures: []string{"", ""}}
	messenger := &fakeMessenger{}
	resources := &fakeResourceFetcher{
		resources: map[string]DownloadedResource{
			"om_mix|file|file_v3_1": {
				Data:        []byte("doc-bytes"),
				FileName:    "notes.txt",
				ContentType: "text/plain",
			},
			"om_mix|image|img_v3_1": {
				Data:        []byte("png-bytes"),
				ContentType: "image/png",
			},
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: cwd, SessionName: "imcodex-demo"}, messenger, console, resources, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_mix",
		GroupID:   "oc_1",
		Text:      "Please inspect both attachments.",
		Attachments: []IncomingAttachment{
			{ResourceType: "file", ResourceKey: "file_v3_1", FileName: "notes.txt"},
			{ResourceType: "image", ResourceKey: "img_v3_1"},
		},
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	got := console.allSendTexts()[0]
	if !strings.Contains(got, "Please inspect both attachments.") {
		t.Fatalf("sendTexts[0] = %q, want original text preserved", got)
	}
	if !strings.Contains(got, "User attached a file:") || !strings.Contains(got, "notes.txt") {
		t.Fatalf("sendTexts[0] = %q, want file prompt", got)
	}
	if !strings.Contains(got, "User attached an image:") || !strings.Contains(got, ".png") {
		t.Fatalf("sendTexts[0] = %q, want image prompt with inferred extension", got)
	}

	inboxDir := filepath.Join(cwd, ".imcodex", "inbox")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", inboxDir, err)
	}
	if len(entries) != 2 {
		t.Fatalf("len(entries) = %d, want 2", len(entries))
	}
}

func TestServiceDownloadFailureDoesNotBlockNextMessage(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cwd := t.TempDir()
	console := &fakeConsole{captures: []string{"", "", ""}}
	messenger := &fakeMessenger{}
	resources := &fakeResourceFetcher{
		errors: map[string]error{
			"om_bad|file|file_missing": errors.New("download denied"),
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: cwd, SessionName: "imcodex-demo"}, messenger, console, resources, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_bad",
		GroupID:   "oc_1",
		Attachments: []IncomingAttachment{
			{ResourceType: "file", ResourceKey: "file_missing", FileName: "blocked.pdf"},
		},
	}); err != nil {
		t.Fatalf("HandleMessage(bad attachment) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.all(), "\n"), "Failed to prepare message for Codex")
	})

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_next",
		GroupID:   "oc_1",
		Text:      "after attachment failure",
	}); err != nil {
		t.Fatalf("HandleMessage(next) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	got := console.allSendTexts()
	if len(got) != 1 || got[0] != "after attachment failure" {
		t.Fatalf("sendTexts = %#v, want only follow-up text after attachment failure", got)
	}
}

func TestServiceImageWithoutFilenameUsesContentTypeExtension(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cwd := t.TempDir()
	console := &fakeConsole{captures: []string{"", ""}}
	messenger := &fakeMessenger{}
	resources := &fakeResourceFetcher{
		resources: map[string]DownloadedResource{
			"om_img|image|img_v3_only": {
				Data:        []byte("jpeg-bytes"),
				ContentType: "image/jpeg",
			},
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: cwd, SessionName: "imcodex-demo"}, messenger, console, resources, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_img",
		GroupID:   "oc_1",
		Attachments: []IncomingAttachment{
			{ResourceType: "image", ResourceKey: "img_v3_only"},
		},
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 1
	})

	got := console.allSendTexts()[0]
	if !strings.Contains(got, "User attached an image:") || !(strings.Contains(got, ".jpg") || strings.Contains(got, ".jpeg")) {
		t.Fatalf("sendTexts[0] = %q, want image prompt with jpg/jpeg extension", got)
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

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
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
		return len(nonStatusMessages(messenger.all())) >= 1
	})

	outputs := nonStatusMessages(messenger.all())
	if got, want := outputs[0], "• Alpha\n• Beta\n• Gamma"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
	}
	if len(outputs) != 1 {
		t.Fatalf("len(outputs) = %d, want 1", len(outputs))
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

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
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

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 20 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "first"}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 1 && got[0] == "first"
	})
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

func TestServiceInterruptsCurrentRunOnNewMessageWhenEnabled(t *testing.T) {
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

	svc := NewService(ctx, Options{
		GroupID:               "oc_1",
		CWD:                   "/srv/demo",
		SessionName:           "imcodex-demo",
		InterruptOnNewMessage: true,
	}, messenger, console, nil, slog.Default())
	svc.pollEvery = 20 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.interruptForceAfter = 500 * time.Millisecond

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "first"}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 1 && got[0] == "first"
	})

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_2", GroupID: "oc_1", Text: "second"}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allInterrupts()) >= 1
	})
	if got := console.allInterrupts()[0]; !strings.HasPrefix(got, "esc:") {
		t.Fatalf("interrupts[0] = %q, want esc interrupt", got)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 2 && got[1] == "second"
	})
}

func TestServiceKeepsOnlyLatestPendingMessageWhileSessionStarts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ensureBlock := make(chan struct{})
	console := &fakeConsole{
		captures:      []string{"", ""},
		ensureEntered: make(chan struct{}, 1),
		ensureBlock:   ensureBlock,
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 10 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "first"}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	select {
	case <-console.ensureEntered:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for ensure session to start")
	}

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_2", GroupID: "oc_1", Text: "second"}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_3", GroupID: "oc_1", Text: "third"}); err != nil {
		t.Fatalf("HandleMessage(third) error = %v", err)
	}

	close(ensureBlock)

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 1
	})

	if got := console.allSendTexts(); len(got) != 1 || got[0] != "third" {
		t.Fatalf("sendTexts = %#v, want only latest startup message", got)
	}
}

func TestServiceIgnoresDuplicateMessageIDs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{captures: []string{"", ""}}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
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

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
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

func TestServiceClearsQueueAfterSendFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures:   []string{"", ""},
		sendErrors: []error{errors.New("send failed"), nil},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 10 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_1", GroupID: "oc_1", Text: "hello"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.all(), "\n"), "send failed")
	})

	time.Sleep(50 * time.Millisecond)
	if got := console.allSendTexts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("sendTexts = %#v, want failed message sent once without retry", got)
	}

	if err := svc.HandleMessage(context.Background(), IncomingMessage{MessageID: "om_2", GroupID: "oc_1", Text: "after failure"}); err != nil {
		t.Fatalf("HandleMessage(after failure) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 2 && got[1] == "after failure"
	})
}

func TestSplitByRunesRespectsTelegramSafeChunkSize(t *testing.T) {
	t.Parallel()

	if maxMessageRunes > 4096 {
		t.Fatalf("maxMessageRunes = %d, want <= 4096 for Telegram sendMessage", maxMessageRunes)
	}

	text := strings.Repeat("a", maxMessageRunes*2+17)
	chunks := splitByRunes(text, maxMessageRunes)
	if len(chunks) != 3 {
		t.Fatalf("len(chunks) = %d, want 3", len(chunks))
	}
	for i, chunk := range chunks {
		if got := utf8.RuneCountInString(chunk); got > maxMessageRunes {
			t.Fatalf("chunk %d size = %d, want <= %d", i, got, maxMessageRunes)
		}
	}
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
