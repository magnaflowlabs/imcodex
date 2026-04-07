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
	mu       sync.Mutex
	texts    []string
	sendErrs []error
}

func (f *fakeMessenger) SendTextToChat(_ context.Context, _ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sendErrs) > 0 {
		err := f.sendErrs[0]
		if len(f.sendErrs) > 1 {
			f.sendErrs = f.sendErrs[1:]
		}
		if err != nil {
			return err
		}
	}
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
	sendErrs []error
	editErrs []error
	delErrs  []error
	edits    int
}

func (f *fakeEditableMessenger) SendTextToChat(ctx context.Context, groupID string, text string) error {
	_, err := f.SendTextToChatWithID(ctx, groupID, text)
	return err
}

func (f *fakeEditableMessenger) SendTextToChatWithID(_ context.Context, _ string, text string) (SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sendErrs) > 0 {
		err := f.sendErrs[0]
		if len(f.sendErrs) > 1 {
			f.sendErrs = f.sendErrs[1:]
		}
		if err != nil {
			return SentMessage{}, err
		}
	}

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

func (f *fakeEditableMessenger) DeleteMessageInChat(_ context.Context, _ string, messageID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.delErrs) > 0 {
		err := f.delErrs[0]
		f.delErrs = f.delErrs[1:]
		if err != nil {
			return err
		}
	}
	for i := range f.messages {
		if f.messages[i].messageID != messageID {
			continue
		}
		f.messages = append(f.messages[:i], f.messages[i+1:]...)
		f.events = append(f.events, "delete:"+messageID)
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
	ensureSpecs   []tmuxctl.SessionSpec
	interrupts    []string
	ensureErrors  []error
	sendErrors    []error
	ensureEntered chan struct{}
	ensureBlock   <-chan struct{}
}

func (f *fakeConsole) EnsureSession(_ context.Context, spec tmuxctl.SessionSpec) (bool, error) {
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
	f.ensureSpecs = append(f.ensureSpecs, spec)
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

func (f *fakeConsole) ensured() []tmuxctl.SessionSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tmuxctl.SessionSpec, len(f.ensureSpecs))
	copy(out, f.ensureSpecs)
	return out
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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

func TestServiceDoesNotForwardMultilinePromptEchoTail(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"› line one\nline two\nline three\n\n• final reply",
			"› line one\nline two\nline three\n\n• final reply",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "line one\nline two\nline three",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) >= 1
	})

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "line two") || strings.Contains(joined, "line three") {
		t.Fatalf("messages = %#v, want prompt echo tail suppressed", messenger.all())
	}
	if !strings.Contains(joined, "• final reply") {
		t.Fatalf("messages = %#v, want final reply forwarded", messenger.all())
	}
}

func TestServiceEnsureSessionPassesLaunchOverride(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{captures: []string{""}}
	svc := NewService(context.Background(), Options{
		GroupID:        "oc_1",
		CWD:            "/srv/demo",
		SessionName:    "imcodex-demo",
		SessionCommand: "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent claude",
	}, &fakeMessenger{}, console, nil, slog.Default())

	rt := svc.ensureRuntime()
	if err := svc.ensureSession(rt); err != nil {
		t.Fatalf("ensureSession() error = %v", err)
	}

	specs := console.ensured()
	if len(specs) != 1 {
		t.Fatalf("len(ensureSpecs) = %d, want 1", len(specs))
	}
	if got, want := specs[0].LaunchCommand, "/usr/local/bin/imcodex-agent-run --workspace '{cwd}' --session '{session_name}' --agent claude"; got != want {
		t.Fatalf("LaunchCommand = %q, want %q", got, want)
	}
	if got, want := specs[0].GroupID, "oc_1"; got != want {
		t.Fatalf("GroupID = %q, want %q", got, want)
	}
}

func TestServiceDoesNotForwardWrappedSingleLinePromptEchoTail(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"› this is a very long single-line prompt\nthat wrapped in the terminal\n\n• final reply",
			"› this is a very long single-line prompt\nthat wrapped in the terminal\n\n• final reply",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.flushIdleTicks = 2

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "this is a very long single-line prompt that wrapped in the terminal",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) >= 1
	})

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "that wrapped in the terminal") {
		t.Fatalf("messages = %#v, want wrapped prompt echo suppressed", messenger.all())
	}
	if !strings.Contains(joined, "• final reply") {
		t.Fatalf("messages = %#v, want final reply forwarded", messenger.all())
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

func TestSuppressPromptEchoPrefixRequiresFullPrefixMatch(t *testing.T) {
	t.Parallel()

	text, consumed := suppressPromptEchoPrefix("line two\nline three\n\n• reply", "line two\nline three")
	if !consumed {
		t.Fatal("consumed = false, want true on exact full-prefix echo")
	}
	if got, want := text, "• reply"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestSuppressPromptEchoPrefixDoesNotStripSuffixOnlyMatch(t *testing.T) {
	t.Parallel()

	text, consumed := suppressPromptEchoPrefix("line three\n\n• reply", "line two\nline three")
	if consumed {
		t.Fatal("consumed = true, want false on suffix-only match")
	}
	if got, want := text, "line three\n\n• reply"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestServiceEditableMessengerKeepsWorkingMessageSeparateFromReply(t *testing.T) {
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
		t.Fatalf("messages = %#v, want single final reply body", got)
	}
	events := messenger.allEvents()
	joinedEvents := strings.Join(events, "\n")
	if !strings.Contains(joinedEvents, "send:") || !strings.Contains(joinedEvents, workingStatusText) || !strings.Contains(joinedEvents, "• final reply") {
		t.Fatalf("events = %#v, want working send and body send", events)
	}
	if !strings.Contains(joinedEvents, "delete:1") {
		t.Fatalf("events = %#v, want working message cleaned up on idle", events)
	}
}

func TestServiceEditableOutputRespectsSyncInterval(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 50 * time.Millisecond

	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            5,
		nextRunID:        5,
		outputBuffer:     "• first",
		outputBufferedAt: time.Now(),
	}

	svc.flushOutputBuffer(rt)
	if got := len(nonStatusMessages(messenger.all())); got != 1 {
		t.Fatalf("len(messages) = %d, want first body sent", got)
	}

	rt.outputBuffer = "\n• second"
	rt.outputBufferedAt = time.Now()
	beforeEdits := messenger.editCount()
	svc.flushOutputBuffer(rt)

	if got := messenger.editCount(); got != beforeEdits {
		t.Fatalf("editCount = %d, want no edit inside sync interval", got)
	}
	if got := strings.TrimSpace(rt.outputBuffer); got != "• second" {
		t.Fatalf("outputBuffer = %q, want pending tail retained inside sync interval", got)
	}

	time.Sleep(60 * time.Millisecond)
	svc.flushOutputBuffer(rt)

	if got := messenger.editCount(); got <= beforeEdits {
		t.Fatalf("editCount = %d, want edit after sync interval", got)
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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

func TestServiceEditableMessengerFlushesImmediatelyOnRolloverThreshold(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const reply = "abcdefghijklmnop"
	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			reply + "\n\n• Working (2s • esc to interrupt)",
			reply + "\n\n• Working (3s • esc to interrupt)",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 2000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.busyFlushAfter = time.Hour
	svc.flushIdleTicks = 200
	svc.editRolloverAt = 10

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "long reply",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 2 && got[0] == "abcdefghij" && got[1] == "klmnop"
	})
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
			"• first reply",
			"",
			"• Working (1s • esc to interrupt)",
			"• second reply",
			"• second reply",
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
	if !strings.Contains(events, "send:2:• alpha") {
		t.Fatalf("events = %#v, want busy flush for first partial reply", messenger.allEvents())
	}
	if !strings.Contains(events, "edit:2:• alpha\n\n• beta") {
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
	svc.editableSyncEvery = 5 * time.Millisecond
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
		sendErrs: []error{
			nil,
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 1 retry_after=1"),
			nil,
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
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
		return strings.Contains(strings.Join(messenger.allEvents(), "\n"), workingStatusText)
	})
	time.Sleep(200 * time.Millisecond)
	if got := len(nonStatusMessages(messenger.all())); got != 0 {
		t.Fatalf("len(messages) after early backoff window = %d, want 0 body chunks before retry", got)
	}

	waitFor(t, 2*time.Second, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) == 1 && got[0] == "• first\n\n• second"
	})
	if events := strings.Join(messenger.allEvents(), "\n"); !strings.Contains(events, "send:2:• first\n\n• second") {
		t.Fatalf("events = %#v, want body send retried after retry_after window", messenger.allEvents())
	}
}

func TestServiceSyncEditableOutputPrunesStaleMessagesOnSegmentShrink(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 3,
		messages: []trackedMessage{
			{messageID: "1", text: "seg-1"},
			{messageID: "2", text: "seg-2"},
			{messageID: "3", text: "seg-3"},
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.editRolloverAt = 100
	rt := &groupRuntime{
		opts: svc.opts,
		outputMessages: []trackedMessage{
			{messageID: "1", text: "seg-1"},
			{messageID: "2", text: "seg-2"},
			{messageID: "3", text: "seg-3"},
		},
	}

	if err := svc.syncEditableOutput(rt, messenger, "final single segment", false); err != nil {
		t.Fatalf("syncEditableOutput() error = %v", err)
	}

	if got := len(rt.outputMessages); got != 1 {
		t.Fatalf("len(outputMessages) = %d, want 1 after shrink", got)
	}
	if got := len(messenger.all()); got != 1 {
		t.Fatalf("len(messages) = %d, want stale segments removed", got)
	}
	events := strings.Join(messenger.allEvents(), "\n")
	if !strings.Contains(events, "delete:2") || !strings.Contains(events, "delete:3") {
		t.Fatalf("events = %#v, want delete events for stale segments", messenger.allEvents())
	}
}

func TestServiceEditableRateLimitBlocksDetachedFlushViaSharedOutputBackoff(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
		editErrs: []error{
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 5 retry_after=5"),
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            5,
		nextRunID:        5,
		outputText:       "• first",
		outputBuffer:     "\n\n• second",
		outputBufferedAt: time.Now(),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}

	svc.flushOutputBuffer(rt) // trigger editable 429
	if rt.outputBackoffUntil.IsZero() {
		t.Fatal("outputBackoffUntil = zero, want shared backoff after editable 429")
	}

	rt.enqueueDetachedOutput(5, "detached chunk")
	before := len(messenger.all())
	svc.flushDetachedOutputs(rt)
	after := len(messenger.all())

	if after != before {
		t.Fatalf("detached flush sent during shared backoff: before=%d after=%d", before, after)
	}
	if got := len(rt.detachedOutputs); got == 0 {
		t.Fatalf("len(detachedOutputs) = %d, want queue retained during shared backoff", got)
	}
	if !rt.deferBodyUntilIdle {
		t.Fatal("deferBodyUntilIdle = false, want true after editable 429")
	}
}

func TestServiceDetachedRateLimitBlocksEditableFlushViaSharedOutputBackoff(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		sendErrs: []error{
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 5 retry_after=5"),
		},
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts: svc.opts,
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
		outputText: "• first",
	}

	rt.enqueueDetachedOutput(5, "detached chunk")
	svc.flushDetachedOutputs(rt) // trigger detached 429
	if rt.outputBackoffUntil.IsZero() {
		t.Fatal("outputBackoffUntil = zero, want shared backoff after detached 429")
	}

	rt.outputBuffer = "\n• second"
	rt.outputBufferedAt = time.Now()
	before := messenger.editCount()
	svc.flushOutputBuffer(rt)
	after := messenger.editCount()

	if after != before {
		t.Fatalf("editable flush sent during shared backoff: before=%d after=%d", before, after)
	}
	if got := strings.TrimSpace(rt.outputBuffer); got != "• second" {
		t.Fatalf("outputBuffer = %q, want buffered tail retained while shared backoff active", got)
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
		sendErrs: []error{
			nil,
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 1 retry_after=1"),
			nil,
		},
	}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
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
		return strings.Contains(strings.Join(messenger.allEvents(), "\n"), workingStatusText)
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

func TestServiceFlushOutputBufferDefersWhileRunInFlightAfterEditableRateLimit(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:               svc.opts,
		runID:              7,
		nextRunID:          7,
		outputText:         "• first",
		outputBuffer:       "\n• second",
		outputBufferedAt:   time.Now(),
		editBackoffUntil:   time.Now().Add(time.Minute),
		deferBodyUntilIdle: true,
		lastBusy:           true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}

	before := messenger.editCount()
	svc.flushOutputBuffer(rt)
	after := messenger.editCount()

	if after != before {
		t.Fatalf("editCount = %d (before=%d), want no editable flush while run in-flight and deferBodyUntilIdle=true", after, before)
	}
	if got := strings.TrimSpace(rt.outputBuffer); got != "• second" {
		t.Fatalf("outputBuffer = %q, want preserved while deferred", got)
	}
}

func TestServiceFlushOutputBufferAllowsIdleFlushAfterEditableRateLimit(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:               svc.opts,
		runID:              7,
		nextRunID:          7,
		outputText:         "• first",
		outputBuffer:       "\n• second",
		outputBufferedAt:   time.Now(),
		deferBodyUntilIdle: true,
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
	}

	before := messenger.editCount()
	svc.flushOutputBuffer(rt)
	after := messenger.editCount()

	if after <= before {
		t.Fatalf("editCount = %d (before=%d), want editable flush once run is idle", after, before)
	}
	if rt.deferBodyUntilIdle {
		t.Fatal("deferBodyUntilIdle = true, want cleared after successful idle flush")
	}
}

func TestServicePollDuringDeferredBodyUntilIdleTracksLatestSnapshotOnly(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• step\n• Working (1s • esc to interrupt)",
			"• step\n• Running sleep 10\n• Working (2s • esc to interrupt)",
			"• step\n• Working (3s • esc to interrupt)",
		},
	}
	messenger := &fakeEditableMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.busyFlushAfter = time.Hour
	svc.flushIdleTicks = 100

	rt := &groupRuntime{
		opts:               svc.opts,
		session:            svc.opts.SessionName,
		sessionReady:       true,
		outputArmed:        true,
		deferBodyUntilIdle: true,
		editBackoffUntil:   time.Now().Add(time.Minute),
		lastBusy:           true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
	}

	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)

	if got, want := strings.TrimSpace(rt.outputBuffer), "• step"; got != want {
		t.Fatalf("outputBuffer = %q, want latest rewritten snapshot only %q", got, want)
	}
	if strings.Contains(rt.outputBuffer, "Running sleep 10") {
		t.Fatalf("outputBuffer = %q, want transient rewritten lines dropped", rt.outputBuffer)
	}
}

func TestServiceDetachedOutputRetriesChunkWithoutReplay(t *testing.T) {
	t.Parallel()

	messenger := &fakeMessenger{
		sendErrs: []error{
			nil,
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 1 retry_after=1"),
			nil,
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{
		opts: svc.opts,
	}

	original := strings.Repeat("x", maxMessageRunes+16)
	rt.enqueueDetachedOutput(1, original)
	if got, want := len(rt.detachedOutputs), 2; got != want {
		t.Fatalf("len(detachedOutputs) = %d, want %d chunked detached queue", got, want)
	}

	svc.flushDetachedOutputs(rt)
	firstPass := messenger.all()
	if got, want := len(firstPass), 1; got != want {
		t.Fatalf("len(messages) after first pass = %d, want %d", got, want)
	}
	if got, want := len(rt.detachedOutputs), 1; got != want {
		t.Fatalf("len(detachedOutputs) after 429 = %d, want %d pending chunk", got, want)
	}

	svc.flushDetachedOutputs(rt)
	if got, want := len(messenger.all()), 1; got != want {
		t.Fatalf("len(messages) during retry_after backoff = %d, want %d (no replay)", got, want)
	}

	rt.detachedBackoffUntil = time.Time{}
	rt.outputBackoffUntil = time.Time{}
	svc.flushDetachedOutputs(rt)

	final := messenger.all()
	if got, want := len(final), 2; got != want {
		t.Fatalf("len(messages) after retry = %d, want %d", got, want)
	}
	if got := final[0] + final[1]; got != original {
		t.Fatalf("reassembled output = %q, want exact original text", got)
	}
}

func TestServiceDetachedOutputPreservesBoundaryNewlines(t *testing.T) {
	t.Parallel()

	messenger := &fakeMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{opts: svc.opts}

	original := strings.Repeat("a", maxMessageRunes-1) + "\n" + strings.Repeat("b", 16)
	rt.enqueueDetachedOutput(1, original)
	svc.flushDetachedOutputs(rt)

	reassembled := strings.Join(messenger.all(), "")
	if reassembled != original {
		t.Fatalf("reassembled output mismatch:\n got=%q\nwant=%q", reassembled, original)
	}
}

func TestServiceEditableMessengerVeryLongReplyNoLoss(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	part1 := strings.Repeat("alpha-", 700)
	part2 := part1 + strings.Repeat("beta-", 700)
	final := part2 + strings.Repeat("gamma-", 700)
	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			part1 + "\n\n• Working (2s • esc to interrupt)",
			part2 + "\n\n• Working (3s • esc to interrupt)",
			final,
			final,
		},
	}
	messenger := &fakeEditableMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.pollEvery = 5 * time.Millisecond
	svc.history = 4000
	svc.startWait = 0
	svc.workingAfter = 5 * time.Millisecond
	svc.busyFlushAfter = 10 * time.Millisecond
	svc.flushIdleTicks = 2
	svc.editRolloverAt = 2800

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_long",
		GroupID:   "oc_1",
		Text:      "long",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 800*time.Millisecond, func() bool {
		joined := strings.Join(nonStatusMessages(messenger.all()), "")
		return joined == final
	})
}

func TestServiceDetachedOutputKeepsQueueOnRepeatedNonRateLimitErrors(t *testing.T) {
	t.Parallel()

	messenger := &fakeMessenger{
		sendErrs: []error{
			errors.New("network timeout"),
			errors.New("network timeout"),
			nil,
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{opts: svc.opts}
	rt.enqueueDetachedOutput(1, "chunk-1\nchunk-2")

	svc.flushDetachedOutputs(rt) // error #1, keep queue
	if got := len(rt.detachedOutputs); got == 0 {
		t.Fatalf("len(detachedOutputs) = %d, want > 0 after first non-429 error", got)
	}
	rt.detachedBackoffUntil = time.Time{}

	svc.flushDetachedOutputs(rt) // error #2, keep queue
	if got := len(rt.detachedOutputs); got == 0 {
		t.Fatalf("len(detachedOutputs) = %d, want > 0 after second non-429 error", got)
	}
	rt.detachedBackoffUntil = time.Time{}

	svc.flushDetachedOutputs(rt) // success
	if got := len(rt.detachedOutputs); got != 0 {
		t.Fatalf("len(detachedOutputs) = %d, want 0 after eventual success", got)
	}
	if got := len(messenger.all()); got == 0 {
		t.Fatalf("len(messages) = %d, want delivered chunks after retries", got)
	}
}

func TestServiceDetachedOutputDoesNotDropByCursor(t *testing.T) {
	t.Parallel()

	messenger := &fakeMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts: svc.opts,
		runCursorCommitted: map[uint64]int{
			2: 2,
		},
		detachedOutputs: []detachedOutput{
			{runID: 2, cursor: 2, text: "dup", enqueuedAt: time.Now().Add(-time.Second)},
			{runID: 2, cursor: 3, text: "new", enqueuedAt: time.Now().Add(-time.Second)},
		},
	}

	svc.flushDetachedOutputs(rt)

	got := messenger.all()
	if len(got) != 2 || got[0] != "dup" || got[1] != "new" {
		t.Fatalf("messages = %#v, want both queued chunks delivered", got)
	}
	if len(rt.detachedOutputs) != 0 {
		t.Fatalf("len(detachedOutputs) = %d, want 0", len(rt.detachedOutputs))
	}
}

func TestServiceFlushOutputBufferRedirectsToDetachedQueueWhenBacklogExists(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		messages: []trackedMessage{
			{messageID: "1", text: "• synced"},
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            5,
		nextRunID:        5,
		outputText:       "• synced",
		outputBuffer:     "\n• new tail",
		outputBufferedAt: time.Now(),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• synced"},
		},
		detachedOutputs: []detachedOutput{
			{runID: 5, cursor: 1, text: "• pending", enqueuedAt: time.Now()},
		},
	}

	svc.flushOutputBuffer(rt)

	if got := messenger.editCount(); got != 0 {
		t.Fatalf("editCount = %d, want 0 when detached backlog exists", got)
	}
	if got := len(rt.detachedOutputs); got < 2 {
		t.Fatalf("len(detachedOutputs) = %d, want new tail appended to detached queue", got)
	}
	last := rt.detachedOutputs[len(rt.detachedOutputs)-1].text
	if last != "• new tail" {
		t.Fatalf("last detached chunk = %q, want %q", last, "• new tail")
	}
}

func TestServiceOutputWatchdogKeepsBufferedTailWhenEditBackoffBlocks(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.outputWatchdogAfter = 10 * time.Millisecond

	rt := &groupRuntime{
		opts:               svc.opts,
		runID:              7,
		nextRunID:          7,
		runCursorCommitted: map[uint64]int{7: 1},
		outputText:         "• old",
		outputBuffer:       "\n\n• tail",
		outputBufferedAt:   time.Now().Add(-time.Second),
		editBackoffUntil:   time.Now().Add(time.Minute),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• old"},
		},
	}

	svc.applyOutputWatchdog(rt, time.Now())

	if !rt.hasBufferedOutput() {
		t.Fatalf("outputBuffer = %q, want kept while editable backoff is active", rt.outputBuffer)
	}
	if got := len(rt.detachedOutputs); got != 0 {
		t.Fatalf("len(detachedOutputs) = %d, want 0 while watchdog waits for editable retry", got)
	}
	if got := len(nonStatusMessages(messenger.all())); got != 0 {
		t.Fatalf("messages = %#v, want no detached plain sends while editable backoff is active", messenger.all())
	}
}

func TestServiceOutputWatchdogKeepsBufferedTailDuringActiveRunWhenEditBackoffBlocks(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	svc.outputWatchdogAfter = 10 * time.Millisecond

	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            7,
		nextRunID:        7,
		outputText:       "• old",
		outputBuffer:     "\n\n• tail",
		outputBufferedAt: time.Now().Add(-time.Second),
		editBackoffUntil: time.Now().Add(time.Minute),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• old"},
		},
		lastBusy: true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
	}

	svc.applyOutputWatchdog(rt, time.Now())

	if !rt.hasBufferedOutput() {
		t.Fatalf("outputBuffer = %q, want kept for editable retry while run is active", rt.outputBuffer)
	}
	if got := len(rt.detachedOutputs); got != 0 {
		t.Fatalf("len(detachedOutputs) = %d, want 0 while active run is in-flight", got)
	}
	if got := len(nonStatusMessages(messenger.all())); got != 0 {
		t.Fatalf("messages = %#v, want no detached plain sends during active run", messenger.all())
	}
}

func TestServiceFallsBackToPlainOutputAfterRepeatedEditableRateLimits(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		messages: []trackedMessage{
			{messageID: "1", text: "• synced"},
		},
		editErrs: []error{
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 2 retry_after=2"),
			errors.New("telegram api failed: http=429 code=429 desc=Too Many Requests: retry after 2 retry_after=2"),
		},
	}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            9,
		nextRunID:        9,
		outputText:       "• synced",
		outputBuffer:     "\n• new tail",
		outputBufferedAt: time.Now(),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• synced"},
		},
	}

	svc.flushOutputBuffer(rt)
	if rt.forcePlainOutput {
		t.Fatal("forcePlainOutput = true, want editable retry after first 429")
	}
	rt.outputBackoffUntil = time.Time{}
	rt.editBackoffUntil = time.Time{}

	svc.flushOutputBuffer(rt)
	if !rt.forcePlainOutput {
		t.Fatal("forcePlainOutput = false, want fallback after second 429")
	}
	rt.outputBackoffUntil = time.Time{}
	rt.editBackoffUntil = time.Time{}

	svc.flushOutputBuffer(rt)

	got := nonStatusMessages(messenger.all())
	if len(got) != 2 || got[0] != "• synced" || got[1] != "• new tail" {
		t.Fatalf("messages = %#v, want prior editable body plus plain fallback tail", got)
	}
}

func TestServiceDispatchNextDrainsBufferedTailBeforeRunSwitchEvenDuringEditBackoff(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{captures: []string{""}}
	messenger := &fakeEditableMessenger{}
	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:               svc.opts,
		session:            svc.opts.SessionName,
		sessionReady:       true,
		runID:              3,
		nextRunID:          3,
		runCursorCommitted: map[uint64]int{3: 1},
		outputText:         "• first",
		outputBuffer:       "\n\n• second tail",
		outputBufferedAt:   time.Now().Add(-time.Second),
		editBackoffUntil:   time.Now().Add(time.Minute),
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "next"},
		},
	}

	svc.dispatchNext(rt)
	svc.flushDetachedOutputs(rt)

	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "next" {
		t.Fatalf("sendTexts = %#v, want next prompt dispatched", sendTexts)
	}
	got := nonStatusMessages(messenger.all())
	if len(got) != 1 || got[0] != "• second tail" {
		t.Fatalf("messages = %#v, want previous run tail detached and delivered once", got)
	}
	if rt.runID != 4 {
		t.Fatalf("runID = %d, want switched to new run 4", rt.runID)
	}
}

func TestServiceEditableMessengerDoesNotRetryWhenMessageNotModified(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
		editErrs: []error{
			errors.New("telegram api failed: http=400 code=400 desc=Bad Request: message is not modified"),
		},
	}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            3,
		nextRunID:        3,
		outputBuffer:     "• first",
		outputBufferedAt: time.Now(),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• stale"},
		},
	}

	svc.flushOutputBuffer(rt)

	if got := messenger.editCount(); got != 1 {
		t.Fatalf("editCount = %d, want single attempt without retry loop", got)
	}
	if got := rt.outputText; got != "• first" {
		t.Fatalf("outputText = %q, want committed candidate text after message-not-modified", got)
	}
}

func TestServicePollSkipsUnarmedOutputUntilFirstDispatch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• stale one\n• stale two",
			"• stale one\n• stale two",
			"• stale one\n• stale two\n• fresh reply",
			"• stale one\n• stale two\n• fresh reply",
			"• stale one\n• stale two\n• fresh reply\n• unsolicited",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.flushIdleTicks = 1

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		lastText:     "• stale one",
		outputArmed:  false,
	}

	svc.poll(rt)
	if got := nonStatusMessages(messenger.all()); len(got) != 0 {
		t.Fatalf("messages before first dispatch = %#v, want ignored stale output", got)
	}

	if err := svc.dispatchPrepared(rt, &activeRequest{
		messageID: "om_1",
		input:     "hello",
	}); err != nil {
		t.Fatalf("dispatchPrepared() error = %v", err)
	}
	svc.poll(rt)

	got := nonStatusMessages(messenger.all())
	if len(got) != 1 || got[0] != "• fresh reply" {
		t.Fatalf("messages after armed poll = %#v, want only fresh delta", got)
	}
	if !rt.outputArmed {
		t.Fatalf("outputArmed = false, want armed after first dispatch")
	}

	svc.poll(rt)
	if got := nonStatusMessages(messenger.all()); len(got) != 1 {
		t.Fatalf("messages after stable poll = %#v, want no duplicate forwarding", got)
	}

	svc.poll(rt)
	if got := nonStatusMessages(messenger.all()); len(got) != 2 || got[1] != "• unsolicited" {
		t.Fatalf("messages after first dispatch = %#v, want ongoing forwarding while armed", got)
	}
}

func TestServiceEditableMessengerResetsThreadWhenMessageToEditMissing(t *testing.T) {
	t.Parallel()

	messenger := &fakeEditableMessenger{
		nextID: 1,
		messages: []trackedMessage{
			{messageID: "1", text: "• first"},
		},
		editErrs: []error{
			errors.New("telegram api failed: http=400 code=400 desc=Bad Request: message to edit not found"),
		},
	}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, &fakeConsole{}, nil, slog.Default())
	svc.editableSyncEvery = 5 * time.Millisecond
	rt := &groupRuntime{
		opts:             svc.opts,
		runID:            4,
		nextRunID:        4,
		outputBuffer:     "• first\n\n• second",
		outputBufferedAt: time.Now(),
		outputMessages: []trackedMessage{
			{messageID: "1", text: "• stale"},
		},
	}

	svc.flushOutputBuffer(rt)

	events := strings.Join(messenger.allEvents(), "\n")
	if !strings.Contains(events, "send:2:• first\n\n• second") {
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

func TestServiceKeepsRunInFlightAcrossCaptureFailureWithoutOutputYet(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"",
			"",
		},
		captureErrors: []error{
			errors.New("tmux temporary failure"),
			nil,
			nil,
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.silentBusyGrace = time.Hour

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		lastBusy:     true,
		busySince:    time.Now(),
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
	}

	svc.poll(rt)
	if rt.active == nil {
		t.Fatal("active = nil after capture failure, want run preserved")
	}
	if !rt.lastBusy {
		t.Fatal("lastBusy = false after capture failure, want in-flight run preserved")
	}

	svc.poll(rt)
	if rt.active == nil {
		t.Fatal("active = nil after recovery poll, want run still in-flight")
	}
	if rt.busySince.IsZero() {
		t.Fatal("busySince = zero after recovery poll, want silent-run grace preserved")
	}
}

func TestServiceRecoversSessionAfterCaptureFailureEvenWithoutBufferedDelta(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",             // baseline
			"",             // first poll: still no output
			"• final tail", // after recovery
			"• final tail",
		},
		captureErrors: []error{
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
	svc.flushIdleTicks = 1

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "om_1",
		GroupID:   "oc_1",
		Text:      "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 800*time.Millisecond, func() bool {
		got := nonStatusMessages(messenger.all())
		return len(got) >= 1 && strings.Contains(strings.Join(got, "\n"), "• final tail")
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
	svc.silentBusyGrace = 0
	svc.idleConfirmTicks = 1
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
	svc.editableSyncEvery = 5 * time.Millisecond
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

func TestServiceDispatchNextFinalizesLateTailWithEmptyBuffers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• late tail",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		baseText:     "",
		lastText:     "",
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.dispatchNext(rt)

	got := nonStatusMessages(messenger.all())
	if len(got) != 1 || got[0] != "• late tail" {
		t.Fatalf("messages = %#v, want late tail flushed before next dispatch", got)
	}
	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want second prompt dispatched", sendTexts)
	}
}

func TestServiceDispatchNextDefersWhenFinalizeSnapshotStillBusy(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• Working (1s • esc to interrupt)",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.dispatchNext(rt)

	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts = %#v, want no dispatch while finalize snapshot is busy", got)
	}
	if got := len(rt.pending); got != 1 {
		t.Fatalf("len(pending) = %d, want request kept queued until idle", got)
	}
	if !rt.lastBusy {
		t.Fatalf("lastBusy = false, want busy=true from finalize snapshot")
	}
}

func TestServiceDispatchNextDefersOnFinalizeCaptureErrorWithoutDroppingPending(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• first reply\n\n• tail reply",
			"• first reply\n\n• tail reply",
		},
		captureErrors: []error{
			errors.New("tmux capture failure"),
			nil,
			nil,
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		lastText:     "• first reply",
		outputText:   "• first reply",
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.dispatchNext(rt)
	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts after capture error = %#v, want no dispatch", got)
	}
	if got := len(rt.pending); got != 1 {
		t.Fatalf("len(pending) after capture error = %d, want 1", got)
	}

	svc.dispatchNext(rt)

	outputs := nonStatusMessages(messenger.all())
	if len(outputs) == 0 || outputs[0] != "• tail reply" {
		t.Fatalf("messages = %#v, want finalized tail forwarded before dispatch", outputs)
	}
	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want pending prompt dispatched after finalize succeeds", sendTexts)
	}
}

func TestServiceDispatchNextRetriesAfterBusyFinalizeWithoutDroppingPending(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• Working (1s • esc to interrupt)",
			"• first reply\n\n• tail reply",
			"• first reply\n\n• tail reply",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		lastText:     "• first reply",
		outputText:   "• first reply",
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.dispatchNext(rt) // busy boundary: must defer
	if got := len(rt.pending); got != 1 {
		t.Fatalf("len(pending) after busy boundary = %d, want 1", got)
	}
	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts after busy boundary = %#v, want no dispatch", got)
	}

	svc.dispatchNext(rt) // idle boundary: finalize tail then dispatch
	outputs := nonStatusMessages(messenger.all())
	if len(outputs) == 0 || outputs[0] != "• tail reply" {
		t.Fatalf("messages = %#v, want finalized tail forwarded before dispatch", outputs)
	}
	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want pending prompt dispatched after busy clears", sendTexts)
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
			"• partial reply\n• Working (2s • esc to interrupt)",
			"• partial reply",
			"• partial reply",
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

func TestServicePollResetMergesExistingBufferedTailInsteadOfReplacing(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• beta\n• gamma\n• delta",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.busyFlushAfter = time.Hour
	svc.flushIdleTicks = 100
	svc.outputWatchdogAfter = 0

	rt := &groupRuntime{
		opts:             svc.opts,
		session:          svc.opts.SessionName,
		sessionReady:     true,
		baseText:         "",
		lastText:         "• alpha\n• beta\n• gamma\n• unsent-tail",
		outputArmed:      true,
		outputText:       "• alpha\n• beta\n• gamma",
		outputBuffer:     "\n• unsent-tail",
		outputBufferedAt: time.Now(),
		lastBusy:         true,
	}

	svc.poll(rt)

	if !strings.Contains(rt.outputBuffer, "• unsent-tail") {
		t.Fatalf("outputBuffer = %q, want existing unsent tail preserved", rt.outputBuffer)
	}
	if !strings.Contains(rt.outputBuffer, "• delta") {
		t.Fatalf("outputBuffer = %q, want new reset delta merged", rt.outputBuffer)
	}
}

func TestServiceDoesNotDispatchPendingOnSingleIdleFlickerWhileRunInFlight(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"",
			"",
			"",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.idleConfirmTicks = 3

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
		lastBusy: true,
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.poll(rt) // idle flicker
	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts after single idle flicker = %#v, want no premature dispatch", got)
	}

	svc.poll(rt) // busy again
	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts after busy resume = %#v, want still no dispatch", got)
	}

	// Confirm idle for enough ticks, then dispatch should happen once.
	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)
	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want dispatch after confirmed idle", sendTexts)
	}
}

func TestServiceDoesNotDispatchPendingDuringSilentRunBeforeGraceExpires(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"",
			"",
			"",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.idleConfirmTicks = 1
	svc.silentBusyGrace = time.Hour

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		busySince:    time.Now(),
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
		lastBusy: true,
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)

	if got := console.allSendTexts(); len(got) != 0 {
		t.Fatalf("sendTexts = %#v, want no dispatch while silent-run grace is active", got)
	}
	if rt.active == nil {
		t.Fatal("active = nil, want run kept in-flight during silent-run grace")
	}
}

func TestServiceDispatchesPendingAfterSilentRunGraceExpires(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.idleConfirmTicks = 1
	svc.silentBusyGrace = 10 * time.Minute

	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		busySince:    time.Now().Add(-11 * time.Minute),
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
		lastBusy: true,
		pending: []IncomingMessage{
			{MessageID: "om_2", GroupID: "oc_1", Text: "second"},
		},
	}

	svc.poll(rt)

	sendTexts := console.allSendTexts()
	if len(sendTexts) != 1 || sendTexts[0] != "second" {
		t.Fatalf("sendTexts = %#v, want dispatch after silent-run grace expires", sendTexts)
	}
	if rt.active == nil {
		// dispatchNext will immediately start the next run, so active should not stay nil.
		t.Fatal("active = nil, want next run dispatched")
	}
}

func TestServiceResetBufferedOutputIgnoresShrinkingResetWhileRunInFlight(t *testing.T) {
	t.Parallel()

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, &fakeMessenger{}, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{
		outputText: "• alpha\n• beta\n• gamma",
		active: &activeRequest{
			messageID: "om_1",
			input:     "first",
		},
	}

	svc.resetBufferedOutput(rt, "• alpha\n• beta")

	if got, want := rt.outputText, "• alpha\n• beta\n• gamma"; got != want {
		t.Fatalf("outputText = %q, want baseline unchanged on shrinking reset", got)
	}
	if got := rt.outputBuffer; got != "" {
		t.Fatalf("outputBuffer = %q, want unchanged empty buffer", got)
	}
}

func TestServiceResetBufferedOutputUnsyncedResetReplacesBufferInsteadOfMerging(t *testing.T) {
	t.Parallel()

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, &fakeMessenger{}, &fakeConsole{}, nil, slog.Default())
	rt := &groupRuntime{
		outputBuffer:     "• step1\n• step2",
		outputBufferedAt: time.Now(),
	}

	svc.resetBufferedOutput(rt, "• step1 (rewritten)")

	if got, want := rt.outputBuffer, "• step1 (rewritten)"; got != want {
		t.Fatalf("outputBuffer = %q, want latest rewritten snapshot %q", got, want)
	}
}

func TestServicePollDeferredShrinkingResetDoesNotAdvanceLastTextAndStillFlushesTail(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• alpha\n• beta\n• Working (1s • esc to interrupt)",
			"• alpha\n• beta\n• gamma\n• delta",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.idleConfirmTicks = 1
	svc.flushIdleTicks = 1
	svc.busyFlushAfter = time.Hour

	const baseline = "• alpha\n• beta\n• gamma"
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		lastText:     baseline,
		outputText:   baseline,
		lastBusy:     true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "prompt",
		},
	}

	svc.poll(rt)
	if got := rt.lastText; got != baseline {
		t.Fatalf("lastText after deferred shrinking reset = %q, want unchanged baseline %q", got, baseline)
	}
	if got := nonStatusMessages(messenger.all()); len(got) != 0 {
		t.Fatalf("messages after deferred shrinking reset = %#v, want none yet", got)
	}

	svc.poll(rt)

	got := nonStatusMessages(messenger.all())
	if len(got) != 1 || got[0] != "• delta" {
		t.Fatalf("messages after idle reconciliation = %#v, want tail forwarded without pending trigger", got)
	}
}

func TestServicePollEmptyBodyJitterWhileWorkingDoesNotReplayOldOutput(t *testing.T) {
	t.Parallel()

	console := &fakeConsole{
		captures: []string{
			"• Working (1s • esc to interrupt)",
			"• stable body\n• Working (2s • esc to interrupt)",
			"• Working (3s • esc to interrupt)",
			"• stable body\n• Working (4s • esc to interrupt)",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(context.Background(), Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, nil, slog.Default())
	svc.flushIdleTicks = 1
	svc.busyFlushAfter = 5 * time.Millisecond
	svc.idleConfirmTicks = 3

	const stable = "• stable body"
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
		outputArmed:  true,
		lastText:     stable,
		outputText:   stable,
		lastBusy:     true,
		active: &activeRequest{
			messageID: "om_1",
			input:     "prompt",
		},
	}

	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)

	if got := nonStatusMessages(messenger.all()); len(got) != 0 {
		t.Fatalf("messages = %#v, want no replay when only working-line jitter happens", got)
	}
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
	svc.silentBusyGrace = 0
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
