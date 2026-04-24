package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
)

type routedMessage struct {
	groupID string
	text    string
}

type groupRecordingMessenger struct {
	mu       sync.Mutex
	messages []routedMessage
}

func (m *groupRecordingMessenger) SendTextToChat(_ context.Context, groupID string, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, routedMessage{groupID: groupID, text: text})
	return nil
}

func (m *groupRecordingMessenger) messagesFor(groupID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.messages {
		if msg.groupID == groupID {
			out = append(out, msg.text)
		}
	}
	return out
}

type sessionConsole struct {
	mu        sync.Mutex
	captures  map[string][]string
	sendTexts map[string][]string
}

func (c *sessionConsole) EnsureSession(context.Context, tmuxctl.SessionSpec) (bool, error) {
	return true, nil
}

func (c *sessionConsole) ResetSession(context.Context, tmuxctl.SessionSpec) (bool, error) {
	return true, nil
}

func (c *sessionConsole) SendText(_ context.Context, session string, text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sendTexts == nil {
		c.sendTexts = make(map[string][]string)
	}
	c.sendTexts[session] = append(c.sendTexts[session], text)
	return nil
}

func (c *sessionConsole) Capture(_ context.Context, session string, _ int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := c.captures[session]
	if len(seq) == 0 {
		return "", nil
	}
	out := seq[0]
	if len(seq) > 1 {
		c.captures[session] = seq[1:]
	}
	return out, nil
}

func (c *sessionConsole) Interrupt(context.Context, string) error {
	return nil
}

func (c *sessionConsole) ForceInterrupt(context.Context, string) error {
	return nil
}

func TestRouterRoutesKnownGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &fakeMessenger{}
	console := &fakeConsole{}
	router, err := NewRouter(ctx, []Options{
		{GroupID: "oc_1", CWD: "/srv/a"},
		{GroupID: "oc_2", CWD: "/srv/b"},
	}, messenger, console, nil, slog.Default())
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	if err := router.HandleMessage(context.Background(), IncomingMessage{
		GroupID: "oc_2",
		Text:    "hello",
	}); err != nil {
		t.Fatalf("HandleMessage() error = %v", err)
	}

	if router.GroupCount() != 2 {
		t.Fatalf("GroupCount() = %d, want 2", router.GroupCount())
	}
}

func TestRouterKeepsGroupBuffersIsolated(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	messenger := &groupRecordingMessenger{}
	console := &sessionConsole{
		captures: map[string][]string{
			"imcodex-a-oc-1": {"", "", "• alpha reply", "• alpha reply"},
			"imcodex-b-oc-2": {"", "", "• beta reply", "• beta reply"},
		},
	}
	router, err := NewRouter(ctx, []Options{
		{GroupID: "oc_1", CWD: "/srv/a", SessionName: "imcodex-a-oc-1"},
		{GroupID: "oc_2", CWD: "/srv/b", SessionName: "imcodex-b-oc-2"},
	}, messenger, console, nil, slog.Default())
	if err != nil {
		t.Fatalf("NewRouter() error = %v", err)
	}

	for _, service := range router.services {
		service.pollEvery = 5 * time.Millisecond
		service.startWait = 0
		service.history = 2000
		service.flushIdleTicks = 2
		service.workingAfter = time.Hour
	}

	if err := router.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "m1",
		GroupID:   "oc_1",
		Text:      "hello a",
	}); err != nil {
		t.Fatalf("HandleMessage(oc_1) error = %v", err)
	}
	if err := router.HandleMessage(context.Background(), IncomingMessage{
		MessageID: "m2",
		GroupID:   "oc_2",
		Text:      "hello b",
	}); err != nil {
		t.Fatalf("HandleMessage(oc_2) error = %v", err)
	}

	waitFor(t, 500*time.Millisecond, func() bool {
		a := strings.Join(messenger.messagesFor("oc_1"), "\n")
		b := strings.Join(messenger.messagesFor("oc_2"), "\n")
		return strings.Contains(a, "alpha reply") && strings.Contains(b, "beta reply")
	})

	a := strings.Join(messenger.messagesFor("oc_1"), "\n")
	b := strings.Join(messenger.messagesFor("oc_2"), "\n")
	if strings.Contains(a, "beta reply") {
		t.Fatalf("oc_1 messages = %q, want no oc_2 output", a)
	}
	if strings.Contains(b, "alpha reply") {
		t.Fatalf("oc_2 messages = %q, want no oc_1 output", b)
	}
}

func TestRouterRejectsDuplicateGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := NewRouter(ctx, []Options{
		{GroupID: "oc_1", CWD: "/srv/a"},
		{GroupID: "oc_1", CWD: "/srv/b"},
	}, &fakeMessenger{}, &fakeConsole{}, nil, slog.Default())
	if err == nil {
		t.Fatal("NewRouter() error = nil, want duplicate group error")
	}
}
