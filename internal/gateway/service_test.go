package gateway

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
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

type fakeEditableMessenger struct {
	mu       sync.Mutex
	nextID   int
	messages []trackedMessage
	events   []string
	editErrs []error
	edits    int
}

func (f *fakeEditableMessenger) SendTextToChat(ctx context.Context, groupID string, text string) error {
	_, err := f.SendTextToChatWithID(ctx, groupID, text)
	return err
}

func (f *fakeEditableMessenger) SendTextToChatWithID(_ context.Context, _ string, text string) (SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.nextID++
	id := strconv.Itoa(f.nextID)
	f.messages = append(f.messages, trackedMessage{messageID: id, text: text})
	f.events = append(f.events, "send:"+id+":"+text)
	return SentMessage{MessageID: id}, nil
}

func (f *fakeEditableMessenger) EditTextInChat(_ context.Context, _ string, messageID string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits++
	if len(f.editErrs) > 0 {
		err := f.editErrs[0]
		f.editErrs = f.editErrs[1:]
		if err != nil {
			return err
		}
	}

	for i := range f.messages {
		if f.messages[i].messageID != messageID {
			continue
		}
		f.messages[i].text = text
		f.events = append(f.events, "edit:"+messageID+":"+text)
		return nil
	}
	return errors.New("message not found")
}

func (f *fakeEditableMessenger) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.messages))
	for _, msg := range f.messages {
		out = append(out, msg.text)
	}
	return out
}

func (f *fakeEditableMessenger) allEvents() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.events))
	copy(out, f.events)
	return out
}

func (f *fakeEditableMessenger) editCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.edits
}

func (f *fakeEditableMessenger) SendChatAction(_ context.Context, groupID string, action string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, "action:"+groupID+":"+action)
	return nil
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
		return strings.Contains(strings.Join(messenger.all(), "\n"), "Failed to prepare request for Codex")
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

func TestServiceForwardsTransientDisconnectDuringAttachmentReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cwd := t.TempDir()
	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"stream disconnected before completion: stream closed before response.completed",
			"stream disconnected before completion: stream closed before response.completed\n\n• Working (1s • esc to interrupt)",
			"stream disconnected before completion: stream closed before response.completed\n\n• Attachment summary ready",
			"stream disconnected before completion: stream closed before response.completed\n\n• Attachment summary ready",
		},
	}
	messenger := &fakeMessenger{}
	resources := &fakeResourceFetcher{
		resources: map[string]DownloadedResource{
			"om_retry|file|file_retry": {
				Data:        []byte("report-body"),
				FileName:    "report.txt",
				ContentType: "text/plain",
			},
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: cwd, SessionName: "imcodex-demo"}, messenger, console, resources, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_retry",
		GroupID:   "oc_1",
		Attachments: []IncomingAttachment{
			{ResourceType: "file", ResourceKey: "file_retry", FileName: "report.txt"},
		},
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 1
	})

	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 {
		t.Fatalf("len(sendTexts) = %d, want 1 without content-driven retry", len(sendTexts))
	}

	outputs := nonStatusMessages(messenger.all())
	if got, want := outputs[0], "stream disconnected before completion: stream closed before response.completed\n\n• Attachment summary ready"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
	}
}

func TestServiceForwardsTransientDisconnectDuringNormalReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"stream disconnected before completion: stream closed before response.completed\n\n• Working (1s • esc to interrupt)",
			"stream disconnected before completion: stream closed before response.completed\n\n• final reply",
			"stream disconnected before completion: stream closed before response.completed\n\n• final reply",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "stream disconnected before completion: stream closed before response.completed\n\n• final reply"
	})

	outputs := nonStatusMessages(messenger.all())
	if got, want := outputs[0], "stream disconnected before completion: stream closed before response.completed\n\n• final reply"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
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

func TestServiceRefreshesBaselineBeforeDispatchingNewRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• previous reply final",
			"• previous reply final\n\nRan curl -L -s https://gmncode.cn\n<html>invite</html>\n\n• invite reply",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.flushIdleTicks = 1

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		lastText:     "• previous reply",
	}

	if err := svc.dispatchPrepared(rt, &activeRequest{
		messageID: "om_2",
		input:     "new question",
	}); err != nil {
		t.Fatalf("dispatchPrepared() error = %v", err)
	}

	svc.poll(rt)

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "previous reply") {
		t.Fatalf("messages = %#v, want stale previous reply excluded", messenger.all())
	}
	if !strings.Contains(joined, "Ran curl -L -s https://gmncode.cn") || !strings.Contains(joined, "• invite reply") {
		t.Fatalf("messages = %#v, want current request output forwarded", messenger.all())
	}
}

func TestServiceEditableMessengerEditsWorkingMessageIntoReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• final reply",
			"• final reply",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• final reply"
	})

	if got := nonStatusMessages(messenger.all()); len(got) != 1 || got[0] != "• final reply" {
		t.Fatalf("messages = %#v, want single edited final reply", got)
	}
	events := messenger.allEvents()
	joinedEvents := strings.Join(events, "\n")
	if !strings.Contains(joinedEvents, "send:") || !strings.Contains(joinedEvents, workingStatusText) || !strings.Contains(joinedEvents, "edit:") || !strings.Contains(joinedEvents, "• final reply") {
		t.Fatalf("events = %#v, want send working then edit final reply", events)
	}
}

func TestServiceSendsChatActionWhileWaitingForFirstVisibleReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Working (2s • esc to interrupt)",
			"• Working (3s • esc to interrupt)",
			"• final reply",
			"• final reply",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 10 * time.Millisecond
	svc.chatActionEvery = 10 * time.Millisecond
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.allEvents(), "\n"), "action:oc_1:typing")
	})

	if events := strings.Join(messenger.allEvents(), "\n"); !strings.Contains(events, "action:oc_1:typing") {
		t.Fatalf("events = %#v, want chat action before visible reply", messenger.allEvents())
	}
}

func TestServiceEditableMessengerRollsOverLongReply(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const reply = "abcdefghijklmnopqrstuvwx"
	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			reply,
			reply,
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 2
	svc.editRolloverAt = 10

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "long reply",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) == 3
	})

	got := nonStatusMessages(messenger.all())
	want := []string{"abcdefghij", "klmnopqrst", "uvwx"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func TestServiceEditableMessengerKeepsPreviousReplyWhenNewRequestStarts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first reply",
			"• first reply",
			"",
			"• Working (1s • esc to interrupt)",
			"• second reply",
			"• second reply",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "first",
	}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• first reply"
	})

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_2",
		GroupID:   "oc_1",
		Text:      "second",
	}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 2 && got[1] == "• second reply"
	})

	got := nonStatusMessages(messenger.all())
	want := []string{"• first reply", "• second reply"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %#v, want %#v", got, want)
	}
}

func TestServiceEditableMessengerRecoversBufferedReplyAfterReset(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• alpha\n\n• beta",
			"• alpha revised\n\n• beta\n\n• gamma",
			"• alpha revised\n\n• beta\n\n• gamma",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• alpha revised\n\n• beta\n\n• gamma"
	})

	if got := nonStatusMessages(messenger.all()); len(got) != 1 || got[0] != "• alpha revised\n\n• beta\n\n• gamma" {
		t.Fatalf("messages = %#v, want recovered final reply", got)
	}
}

func TestServiceEditableMessengerFlushesBufferedReplyWhileBusy(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• alpha\n\n• Working (1s • esc to interrupt)",
			"• alpha\n\n• Working (2s • esc to interrupt)",
			"• alpha\n\n• Working (3s • esc to interrupt)",
			"• alpha\n\n• Working (4s • esc to interrupt)",
			"• alpha\n\n• Working (5s • esc to interrupt)",
			"• alpha\n\n• beta\n\n• Working (6s • esc to interrupt)",
			"• alpha\n\n• beta\n\n• Working (7s • esc to interrupt)",
			"• alpha\n\n• beta\n\n• Working (8s • esc to interrupt)",
			"• alpha\n\n• beta\n\n• Working (9s • esc to interrupt)",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.busyFlushAfter = 15 * time.Millisecond
	svc.flushIdleTicks = 50

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• alpha\n\n• beta"
	})

	events := strings.Join(messenger.allEvents(), "\n")
	if !strings.Contains(events, "edit:1:• alpha") {
		t.Fatalf("events = %#v, want busy flush for first partial reply", messenger.allEvents())
	}
	if !strings.Contains(events, "edit:1:• alpha\n\n• beta") {
		t.Fatalf("events = %#v, want busy flush for second partial reply", messenger.allEvents())
	}
}

func TestServiceEditableMessengerFlushesBufferedReplyImmediatelyWhenBusyEnds(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• alpha\n\n• Working (2s • esc to interrupt)",
			"• alpha\n\n• omega",
			"• alpha\n\n• omega",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.busyFlushAfter = time.Hour
	svc.flushIdleTicks = 200

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• alpha\n\n• omega"
	})
}

func TestMergeBufferedOutputDeduplicatesTailOverlap(t *testing.T) {
	t.Parallel()

	got := mergeBufferedOutput("• alpha\n• beta", "• beta\n• gamma")
	want := "• alpha\n• beta\n• gamma"
	if got != want {
		t.Fatalf("mergeBufferedOutput() = %q, want %q", got, want)
	}
}

func TestMergeBufferedOutputKeepsTinyOverlapAsAppend(t *testing.T) {
	t.Parallel()

	got := mergeBufferedOutput("abcdefghij", "ij-klm")
	want := "abcdefghijij-klm"
	if got != want {
		t.Fatalf("mergeBufferedOutput() = %q, want %q", got, want)
	}
}

func TestServiceEditableMessengerBacksOffAfterRateLimit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first",
			"• first",
			"• first\n\n• second",
			"• first\n\n• second",
		},
	}
	messenger := &fakeEditableMessenger{
		editErrs: []error{
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 1 retry_after=1"),
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 1

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return messenger.editCount() >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if got := messenger.editCount(); got != 1 {
		t.Fatalf("editCount after backoff window check = %d, want 1", got)
	}

	waitFor(t, 2*time.Second, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• first\n\n• second"
	})
	if got := messenger.editCount(); got < 2 {
		t.Fatalf("editCount = %d, want retry after retry_after window", got)
	}
}

func TestServiceDispatchesNextPromptWhileEditableOutputBackedOff(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first",
			"• first",
			"• first",
			"• first",
		},
	}
	messenger := &fakeEditableMessenger{
		editErrs: []error{
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 1 retry_after=1"),
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 1

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "first",
	}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return messenger.editCount() >= 1
	})

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_2",
		GroupID:   "oc_1",
		Text:      "second",
	}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(console.allSendTexts()) >= 2
	})
	sendTexts := console.allSendTexts()
	if got, want := sendTexts[1], "second"; got != want {
		t.Fatalf("sendTexts[1] = %q, want %q while prior output is backed off", got, want)
	}

	waitFor(t, 2*time.Second, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) >= 1 && strings.Contains(strings.Join(got, "\n"), "• first")
	})
	if got := nonStatusMessages(messenger.all()); len(got) == 0 {
		t.Fatalf("messages = %#v, want detached first output delivered", got)
	}
}

func TestServiceEditableMessengerDoesNotRetryWhenMessageNotModified(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first",
			"• first",
			"• first",
			"• first",
		},
	}
	messenger := &fakeEditableMessenger{
		editErrs: []error{
			errors.New("telegram api failed: http=400 code=400 desc=Bad Request: message is not modified"),
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 1

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return messenger.editCount() >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if got := messenger.editCount(); got != 1 {
		t.Fatalf("editCount = %d, want 1 without retry loop on message-not-modified", got)
	}
}

func TestServiceEditableMessengerResetsThreadWhenMessageToEditMissing(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first",
			"• first\n\n• second",
			"• first\n\n• second",
		},
	}
	messenger := &fakeEditableMessenger{
		editErrs: []error{
			errors.New("telegram api failed: http=400 code=400 desc=Bad Request: message to edit not found"),
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.flushIdleTicks = 1

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• first\n\n• second"
	})

	events := strings.Join(messenger.allEvents(), "\n")
	if !strings.Contains(events, "send:2:• first") {
		t.Fatalf("events = %#v, want fallback send on missing editable message", messenger.allEvents())
	}
}

func TestServicePreservesBufferedOutputAcrossCaptureFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• first chunk",
			"• first chunk",
		},
		captureErrors: []error{
			nil,
			nil,
			nil,
			errors.New("tmux temporary failure"),
			nil,
			nil,
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.flushIdleTicks = 200

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 800*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) >= 1 && strings.Contains(strings.Join(got, "\n"), "• first chunk")
	})
}

func TestResetBufferedOutputKeepsPendingBufferWhenCurrentTextEmpty(t *testing.T) {
	t.Parallel()

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, &fakeMessenger{}, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{
		outputBuffer:     "• tail not sent",
		outputBufferedAt: time.Now(),
		outputText:       "• previous synced",
	}

	svc.resetBufferedOutput(rt, "")

	if got, want := rt.outputBuffer, "• tail not sent"; got != want {
		t.Fatalf("outputBuffer = %q, want %q", got, want)
	}
	if !rt.outputBufferedAt.IsZero() && strings.TrimSpace(rt.outputBuffer) == "" {
		t.Fatalf("outputBufferedAt=%v with empty buffer, want cleared together", rt.outputBufferedAt)
	}
}

func TestServiceFlushesBufferedReplyBeforeDispatchingNextMessage(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• first reply",
			"• first reply",
			"• first reply",
			"• first reply",
			"• first reply",
			"• first reply",
			"• first reply",
			"",
			"",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.pollEvery = 20 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.flushIdleTicks = 50

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "first",
	}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := console.allSendTexts()
		return len(got) >= 1 && got[0] == "first"
	})

	time.Sleep(80 * time.Millisecond)
	if got := nonStatusMessages(messenger.all()); len(got) != 0 {
		t.Fatalf("messages = %#v, want first reply still buffered", got)
	}

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_2",
		GroupID:   "oc_1",
		Text:      "second",
	}); err != nil {
		t.Fatalf("HandleMessage(second) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 1 && len(console.allSendTexts()) >= 2
	})

	if got := nonStatusMessages(messenger.all())[0]; got != "• first reply" {
		t.Fatalf("outputs[0] = %q, want flushed first reply before second dispatch", got)
	}
	if got := console.allSendTexts()[1]; got != "second" {
		t.Fatalf("sendTexts[1] = %q, want second prompt dispatched", got)
	}
}

func TestServiceDispatchNextFinalizesTailBeforeNextPrompt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• first reply\n\n• tail reply",
			"• first reply\n\n• tail reply",
		},
	}
	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first reply"},
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		baseText:     "",
		lastText:     "• first reply",
		outputText:   "• first reply",
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• first reply"},
		},
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.dispatchNext(rt)

	if got := nonStatusMessages(messenger.all()); len(got) != 1 || got[0] != "• first reply\n\n• tail reply" {
		t.Fatalf("messages = %#v, want finalized tail before next dispatch", got)
	}
	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want second prompt dispatched after tail finalize", sendTexts)
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
		if strings.HasPrefix(text, "[working]") {
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
