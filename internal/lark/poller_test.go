package lark

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

func TestPollerPollGroupForwardsOnlyNewMessages(t *testing.T) {
	t.Parallel()

	handler := &recordingHandler{}
	lister := &fakeMessageLister{
		messages: []ListedMessage{
			{
				Message:         gateway.IncomingMessage{GroupID: "oc_1", MessageID: "om_new"},
				CreatedAtMillis: 2_000,
			},
		},
	}

	poller := NewPoller(lister, []string{"oc_1"}, handler, nil)
	poller.cursors["oc_1"] = pollCursor{
		createdAtMillis: 1_000,
		messageIDs:      map[string]struct{}{"om_old": {}},
	}

	if err := poller.pollGroup(context.Background(), "oc_1"); err != nil {
		t.Fatalf("pollGroup() error = %v", err)
	}
	if len(handler.messages) != 1 {
		t.Fatalf("handler.messages = %d, want 1", len(handler.messages))
	}
	if got := handler.messages[0].MessageID; got != "om_new" {
		t.Fatalf("handler.messages[0].MessageID = %q, want om_new", got)
	}
	if got := poller.cursors["oc_1"].createdAtMillis; got != 2_000 {
		t.Fatalf("cursor.createdAtMillis = %d, want 2000", got)
	}
}

type fakeMessageLister struct {
	messages []ListedMessage
}

func (f *fakeMessageLister) ListChatMessagesSince(context.Context, string, int64) ([]ListedMessage, error) {
	return f.messages, nil
}

type recordingHandler struct {
	mu       sync.Mutex
	messages []gateway.IncomingMessage
}

func (h *recordingHandler) HandleMessage(_ context.Context, msg gateway.IncomingMessage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, msg)
	return nil
}

func (h *recordingHandler) snapshot() []gateway.IncomingMessage {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]gateway.IncomingMessage, len(h.messages))
	copy(out, h.messages)
	return out
}

func TestPollerPollAllRunsGroupsInParallel(t *testing.T) {
	t.Parallel()

	block := make(chan struct{})
	handler := &recordingHandler{}
	lister := &groupedMessageLister{
		blockGroup: "oc_slow",
		block:      block,
		byGroup: map[string][]ListedMessage{
			"oc_fast": {{
				Message:         gateway.IncomingMessage{GroupID: "oc_fast", MessageID: "om_fast"},
				CreatedAtMillis: 2_000,
			}},
			"oc_slow": {{
				Message:         gateway.IncomingMessage{GroupID: "oc_slow", MessageID: "om_slow"},
				CreatedAtMillis: 2_000,
			}},
		},
	}

	poller := NewPoller(lister, []string{"oc_slow", "oc_fast"}, handler, nil)
	poller.cursors["oc_slow"] = pollCursor{createdAtMillis: 1_000, messageIDs: map[string]struct{}{}}
	poller.cursors["oc_fast"] = pollCursor{createdAtMillis: 1_000, messageIDs: map[string]struct{}{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		poller.pollAll(ctx)
		close(done)
	}()

	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		messages := handler.snapshot()
		if len(messages) == 1 && messages[0].MessageID == "om_fast" {
			close(block)
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(block)
	t.Fatal("fast group did not complete while slow group was blocked")
}

type groupedMessageLister struct {
	blockGroup string
	block      <-chan struct{}
	byGroup    map[string][]ListedMessage
}

func (g *groupedMessageLister) ListChatMessagesSince(_ context.Context, groupID string, _ int64) ([]ListedMessage, error) {
	if groupID == g.blockGroup && g.block != nil {
		<-g.block
	}
	return g.byGroup[groupID], nil
}
