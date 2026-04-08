package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

func TestUpdateToIncomingMessageAcceptsText(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 9,
			Chat:      Chat{ID: -100123, Type: "supergroup"},
			From:      &User{IsBot: false},
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.GroupID, "-100123"; got != want {
		t.Fatalf("GroupID = %q, want %q", got, want)
	}
	if got, want := msg.Text, "hello"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

func TestUpdateToIncomingMessagePreservesWhitespace(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		UpdateID: 2,
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: -100123, Type: "supergroup"},
			From:      &User{IsBot: false},
			Text:      "  line one\n    line two  ",
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "  line one\n    line two  "; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

func TestUpdateToIncomingMessageAcceptsPhotoWithCaption(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: -100123, Type: "group"},
			Caption:   "inspect this",
			Photo: []Photo{
				{FileID: "small", Width: 10, Height: 10},
				{FileID: "large", Width: 20, Height: 20},
			},
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "inspect this"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "image" || msg.Attachments[0].ResourceKey != "large" {
		t.Fatalf("Attachments = %#v, want largest photo", msg.Attachments)
	}
}

func TestUpdateToIncomingMessageAcceptsDocument(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 11,
			Chat:      Chat{ID: -100123, Type: "supergroup"},
			Document:  &Document{FileID: "file_1", FileName: "report.pdf"},
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].FileName != "report.pdf" {
		t.Fatalf("Attachments = %#v, want one document attachment", msg.Attachments)
	}
}

func TestUpdateToIncomingMessageIgnoresBotsAndPrivateChats(t *testing.T) {
	t.Parallel()

	_, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 12,
			Chat:      Chat{ID: 123, Type: "private"},
			From:      &User{IsBot: true},
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}

type fakeUpdateClient struct {
	mu       sync.Mutex
	batches  [][]Update
	calls    []int64
	blockGet bool
}

func (f *fakeUpdateClient) GetUpdates(ctx context.Context, offset int64, _ int) ([]Update, error) {
	f.mu.Lock()
	f.calls = append(f.calls, offset)
	blockGet := f.blockGet
	var batch []Update
	if len(f.batches) > 0 {
		batch = f.batches[0]
		if len(f.batches) > 1 {
			f.batches = f.batches[1:]
		}
	}
	f.mu.Unlock()

	if blockGet {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return batch, nil
}

func (f *fakeUpdateClient) allCalls() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.calls))
	copy(out, f.calls)
	return out
}

type blockingHandler struct {
	mu          sync.Mutex
	started     []string
	blockByText map[string]chan struct{}
}

func (h *blockingHandler) HandleMessage(_ context.Context, msg gateway.IncomingMessage) error {
	h.mu.Lock()
	h.started = append(h.started, msg.Text)
	block := h.blockByText[msg.Text]
	h.mu.Unlock()
	if block != nil {
		<-block
	}
	return nil
}

func (h *blockingHandler) allStarted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.started))
	copy(out, h.started)
	return out
}

type erroringHandler struct {
	mu        sync.Mutex
	started   []string
	errByText map[string]error
}

func (h *erroringHandler) HandleMessage(_ context.Context, msg gateway.IncomingMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = append(h.started, msg.Text)
	return h.errByText[msg.Text]
}

func (h *erroringHandler) allStarted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.started))
	copy(out, h.started)
	return out
}

func TestReceiverProcessesBatchByChatSerialOrder(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	handler := &blockingHandler{
		blockByText: map[string]chan struct{}{
			"chat1-first": block,
		},
	}
	receiver := NewReceiver(&fakeUpdateClient{}, handler, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- receiver.processBatch(ctx, []Update{
			{UpdateID: 1, Message: &Message{MessageID: 1, Chat: Chat{ID: -1, Type: "group"}, Text: "chat1-first"}},
			{UpdateID: 2, Message: &Message{MessageID: 2, Chat: Chat{ID: -2, Type: "group"}, Text: "chat2-only"}},
			{UpdateID: 3, Message: &Message{MessageID: 3, Chat: Chat{ID: -1, Type: "group"}, Text: "chat1-second"}},
		})
	}()

	waitForReceiver(t, 300*time.Millisecond, func() bool {
		started := handler.allStarted()
		return len(started) >= 2
	})

	started := handler.allStarted()
	if !containsReceiverText(started, "chat1-first") || !containsReceiverText(started, "chat2-only") {
		t.Fatalf("started = %#v, want first batch to include chat1-first and chat2-only", started)
	}
	for _, text := range started {
		if text == "chat1-second" {
			t.Fatalf("started = %#v, want chat1-second blocked behind chat1-first", started)
		}
	}

	close(block)
	waitForReceiver(t, 300*time.Millisecond, func() bool {
		started := handler.allStarted()
		return len(started) == 3 && started[2] == "chat1-second"
	})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("processBatch() error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("processBatch() did not finish after releasing block")
	}
}

func TestReceiverAbortsLongPollOnShutdown(t *testing.T) {
	t.Parallel()

	client := &fakeUpdateClient{blockGet: true}
	receiver := NewReceiver(client, &blockingHandler{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- receiver.Start(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Start() did not abort long poll on shutdown")
	}
}

func TestReceiverAdvancesOffsetAfterBatchCompletes(t *testing.T) {
	t.Parallel()

	client := &fakeUpdateClient{
		batches: [][]Update{
			{
				{UpdateID: 7, Message: &Message{MessageID: 1, Chat: Chat{ID: -1, Type: "group"}, Text: "slow"}},
				{UpdateID: 8, Message: &Message{MessageID: 2, Chat: Chat{ID: -2, Type: "group"}, Text: "fast"}},
			},
			{},
		},
	}
	block := make(chan struct{})
	handler := &blockingHandler{
		blockByText: map[string]chan struct{}{
			"slow": block,
		},
	}
	receiver := NewReceiver(client, handler, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- receiver.Start(ctx)
	}()

	time.Sleep(40 * time.Millisecond)
	if got := client.allCalls(); len(got) != 1 || got[0] != 0 {
		t.Fatalf("calls before batch completes = %#v, want only initial offset 0", got)
	}

	close(block)
	waitForReceiver(t, 300*time.Millisecond, func() bool {
		got := client.allCalls()
		return len(got) >= 2
	})

	if got := client.allCalls(); len(got) < 2 || got[1] != 9 {
		t.Fatalf("calls after batch completes = %#v, want next offset 9", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Start() did not stop after cancel")
	}
}

func TestReceiverContinuesAfterHandlerError(t *testing.T) {
	t.Parallel()

	handler := &erroringHandler{
		errByText: map[string]error{
			"bad": errors.New("boom"),
		},
	}
	receiver := NewReceiver(&fakeUpdateClient{}, handler, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := receiver.processBatch(ctx, []Update{
		{UpdateID: 4, Message: &Message{MessageID: 1, Chat: Chat{ID: -1, Type: "group"}, Text: "bad"}},
		{UpdateID: 5, Message: &Message{MessageID: 2, Chat: Chat{ID: -1, Type: "group"}, Text: "good"}},
	})
	if err != nil {
		t.Fatalf("processBatch() error = %v, want handler error logged and swallowed", err)
	}
	if got, want := receiver.offset, int64(6); got != want {
		t.Fatalf("offset = %d, want %d after batch", got, want)
	}
	if got := handler.allStarted(); len(got) != 2 || got[0] != "bad" || got[1] != "good" {
		t.Fatalf("started = %#v, want both messages handled despite first error", got)
	}
}

func waitForReceiver(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for receiver condition")
}

func containsReceiverText(texts []string, want string) bool {
	for _, text := range texts {
		if text == want {
			return true
		}
	}
	return false
}
