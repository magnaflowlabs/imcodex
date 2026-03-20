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
			"› hello\n\n• Hello.\n\n  gpt-5.4 xhigh · 100% left · /srv/demo",
			"› hello\n\n• Hello.\n\n  gpt-5.4 xhigh · 100% left · /srv/demo",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.all(), "\n"), "Hello.")
	})

	joined := strings.Join(messenger.all(), "\n")
	if strings.Contains(joined, "› hello") {
		t.Fatalf("messages = %#v, want prompt echo hidden", messenger.all())
	}
}

func TestServiceOnlySendsNewContentAfterSnapshotScroll(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"• Alpha\n• Beta",
			"• Alpha\n• Beta",
			"• Beta\n• Gamma",
			"• Beta\n• Gamma",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "summarize progress",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		outputs := nonStatusMessages(messenger.all())
		return len(outputs) >= 2
	})

	outputs := nonStatusMessages(messenger.all())
	if got, want := outputs[0], "• Alpha\n• Beta"; got != want {
		t.Fatalf("outputs[0] = %q, want %q", got, want)
	}
	if got, want := outputs[1], "• Gamma"; got != want {
		t.Fatalf("outputs[1] = %q, want %q", got, want)
	}
}

func TestServiceSkipsPromptEchoAndPlaceholder(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"› hi\n\n• Hi.\n\n› 在是？\n\n› Write tests for @filename",
			"› hi\n\n• Hi.\n\n› 在是？\n\n• 在。说事。\n\n› Write tests for @filename",
			"› hi\n\n• Hi.\n\n› 在是？\n\n• 在。说事。\n\n› Write tests for @filename",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_1",
		Text:    "在是？",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.all(), "\n"), "在。说事。")
	})

	joined := strings.Join(nonStatusMessages(messenger.all()), "\n")
	if strings.Contains(joined, "› hi") || strings.Contains(joined, "› 在是？") || strings.Contains(joined, "› Write tests for @filename") {
		t.Fatalf("messages = %#v, want prompt area hidden", messenger.all())
	}
}

func TestServicePollBatchesSmallBusyUpdatesUntilQuiet(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"• Current directory: /srv/demo\n• Working (1s • esc to interrupt)",
			"• Current directory: /srv/demo\n| name | type |\n• Working (1s • esc to interrupt)",
			"• Current directory: /srv/demo\n| name | type |\n| --- | --- |\n| alpha | dir |\n| beta | dir |\n• Working (1s • esc to interrupt)",
			"• Current directory: /srv/demo\n| name | type |\n| --- | --- |\n| alpha | dir |\n| beta | dir |",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
	}

	svc.poll(rt)
	svc.poll(rt)
	svc.poll(rt)
	if got := nonStatusMessages(messenger.all()); len(got) != 0 {
		t.Fatalf("messages after busy deltas = %#v, want none yet", got)
	}

	svc.poll(rt)
	got := nonStatusMessages(messenger.all())
	if len(got) != 1 {
		t.Fatalf("messages after quiet flush = %#v, want single batch", got)
	}
	want := "• Current directory: /srv/demo\n| name | type |\n| --- | --- |\n| alpha | dir |\n| beta | dir |"
	if got[0] != want {
		t.Fatalf("batched output = %q, want %q", got[0], want)
	}
}

func TestServicePollStreamsLargeBusyOutput(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	large := "• " + strings.Repeat("x", streamFlushRunes+20)
	console := &fakeConsole{
		captures: []string{
			large + "\n• Working (1s • esc to interrupt)",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	rt := &groupRuntime{
		opts:         svc.opts,
		session:      svc.opts.SessionName,
		sessionReady: true,
	}

	svc.poll(rt)

	got := nonStatusMessages(messenger.all())
	if len(got) != 1 {
		t.Fatalf("messages = %#v, want one streamed chunk while busy", got)
	}
	if got[0] != large {
		t.Fatalf("streamed output = %q, want %q", got[0], large)
	}
	if !rt.busy {
		t.Fatal("rt.busy = false, want true because codex is still working")
	}
}

func TestServiceBridgesRealisticCodexSnapshotAsSingleBlock(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"",
			"• Working (1s • esc to interrupt)",
			"› 哈哈，你能看到我在发什么吗？\n\n• 能看到。你发的是：“哈哈，你能看到我在发什么吗？”\n\n› 1. 今天星期几？\n  2. 假如你是小a，你叫什么名字？\n\n  4. 好了。\n\n• 你在问两个直接问题，我先用系统时间确认今天的具体日期和星期，再一起回答。\n\n• Working (2s • esc to interrupt)",
			"› 哈哈，你能看到我在发什么吗？\n\n• 能看到。你发的是：“哈哈，你能看到我在发什么吗？”\n\n› 1. 今天星期几？\n  2. 假如你是小a，你叫什么名字？\n\n  4. 好了。\n\n• 你在问两个直接问题，我先用系统时间确认今天的具体日期和星期，再一起回答。\n\n• Ran TZ=Asia/Shanghai date '+%Y-%m-%d %A'\n  └ 2026-03-20 Friday\n\n────────────────────────────────────────────────────────────────────────────────\n\n• 1. 今天是星期五，日期是 2026 年 3 月 20 日。\n  2. 假如我是小a，那我就叫小a。",
			"› 哈哈，你能看到我在发什么吗？\n\n• 能看到。你发的是：“哈哈，你能看到我在发什么吗？”\n\n› 1. 今天星期几？\n  2. 假如你是小a，你叫什么名字？\n\n  4. 好了。\n\n• 你在问两个直接问题，我先用系统时间确认今天的具体日期和星期，再一起回答。\n\n• Ran TZ=Asia/Shanghai date '+%Y-%m-%d %A'\n  └ 2026-03-20 Friday\n\n────────────────────────────────────────────────────────────────────────────────\n\n• 1. 今天是星期五，日期是 2026 年 3 月 20 日。\n  2. 假如我是小a，那我就叫小a。",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{GroupID: "oc_1", Text: "哈哈，你能看到我在发什么吗？\n\n1. 今天星期几？\n2. 假如你是小a，你叫什么名字？\n\n4. 好了。"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 1
	})

	outputs := nonStatusMessages(messenger.all())
	if len(outputs) != 1 {
		t.Fatalf("outputs = %#v, want one block", outputs)
	}
	joined := outputs[0]
	if strings.Contains(joined, "› 1. 今天星期几？") || strings.Contains(joined, "  4. 好了。") {
		t.Fatalf("output leaked prompt block = %q", joined)
	}
	if !strings.Contains(joined, "────────────────") {
		t.Fatalf("output missing divider = %q", joined)
	}
	if !strings.Contains(joined, "• 1. 今天是星期五，日期是 2026 年 3 月 20 日。") {
		t.Fatalf("output missing final answer = %q", joined)
	}
}

func TestServiceSkipsSessionHistoryBeforeCurrentRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	console := &fakeConsole{
		captures: []string{
			"› Summarize recent commits\n\n› Explain this code",
			"› Summarize recent commits\n\n› Explain this code\n\n• Working (1s • esc to interrupt)",
			"› Summarize recent commits\n\n› Explain this code\n\n› hello\n\n• hi there",
			"› Summarize recent commits\n\n› Explain this code\n\n› hello\n\n• hi there",
		},
	}
	messenger := &fakeMessenger{}

	svc := NewService(ctx, Options{GroupID: "oc_1", CWD: "/srv/demo", SessionName: "imcodex-demo"}, messenger, console, slog.Default())
	svc.pollEvery = 5 * time.Millisecond
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{GroupID: "oc_1", Text: "hello"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return len(nonStatusMessages(messenger.all())) >= 1
	})

	outputs := nonStatusMessages(messenger.all())
	if len(outputs) != 1 {
		t.Fatalf("outputs = %#v, want one reply block", outputs)
	}
	if strings.Contains(outputs[0], "Summarize recent commits") || strings.Contains(outputs[0], "Explain this code") {
		t.Fatalf("outputs = %#v, want previous session history excluded", outputs)
	}
	if strings.Contains(outputs[0], "› hello") || !strings.Contains(outputs[0], "• hi there") {
		t.Fatalf("outputs = %#v, want assistant reply only", outputs)
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
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{GroupID: "oc_1", Text: "first"}); err != nil {
		t.Fatalf("HandleMessage(first) error = %v", err)
	}
	if err := svc.HandleMessage(context.Background(), IncomingMessage{GroupID: "oc_1", Text: "second"}); err != nil {
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
	svc.startWait = 0

	if err := svc.HandleMessage(context.Background(), IncomingMessage{GroupID: "oc_1", Text: "hello"}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		return strings.Contains(strings.Join(messenger.all(), "\n"), "tmux unavailable")
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
