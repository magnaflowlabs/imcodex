package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
)

const (
	maxMessageRunes      = 3000
	queueSize            = 64
	recentMessageIDLimit = 256
)

type IncomingMessage struct {
	MessageID string
	GroupID   string
	Text      string
}

type Options struct {
	GroupID     string
	CWD         string
	SessionName string
}

type Messenger interface {
	SendTextToChat(ctx context.Context, groupID string, text string) error
}

type Console interface {
	EnsureSession(ctx context.Context, spec tmuxctl.SessionSpec) (bool, error)
	SendText(ctx context.Context, session string, text string) error
	Capture(ctx context.Context, session string, history int) (string, error)
}

type Service struct {
	ctx       context.Context
	opts      Options
	messenger Messenger
	console   Console
	logger    *slog.Logger
	pollEvery time.Duration
	startWait time.Duration
	history   int

	mu      sync.Mutex
	runtime *groupRuntime

	recentMessageIDs []string
	recentMessageSet map[string]struct{}
}

type groupRuntime struct {
	opts         Options
	session      string
	queue        chan string
	pending      []string
	sessionReady bool
	lastText     string
	baseText     string
	lastBusy     bool
	idleTicks    int
	outputBuffer string
}

func NewService(ctx context.Context, opts Options, messenger Messenger, console Console, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.SessionName == "" {
		opts.SessionName = DefaultSessionName(opts.CWD)
	}
	return &Service{
		ctx:       ctx,
		opts:      opts,
		messenger: messenger,
		console:   console,
		logger:    logger,
		pollEvery: time.Second,
		startWait: 4 * time.Second,
		history:   2000,
	}
}

func (s *Service) HandleMessage(ctx context.Context, msg IncomingMessage) error {
	if strings.TrimSpace(msg.Text) == "" {
		return nil
	}
	if msg.GroupID != s.opts.GroupID {
		return nil
	}
	if s.markMessageSeen(msg.MessageID) {
		return nil
	}

	rt := s.ensureRuntime()
	select {
	case rt.queue <- msg.Text:
		return nil
	default:
		s.forgetMessage(msg.MessageID)
		return s.messenger.SendTextToChat(ctx, s.opts.GroupID, "This chat queue is full. Please try again shortly.")
	}
}

func (s *Service) markMessageSeen(messageID string) bool {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.recentMessageSet[messageID]; exists {
		return true
	}
	if s.recentMessageSet == nil {
		s.recentMessageSet = make(map[string]struct{}, recentMessageIDLimit)
	}

	s.recentMessageIDs = append(s.recentMessageIDs, messageID)
	s.recentMessageSet[messageID] = struct{}{}

	if len(s.recentMessageIDs) > recentMessageIDLimit {
		oldest := s.recentMessageIDs[0]
		s.recentMessageIDs = s.recentMessageIDs[1:]
		delete(s.recentMessageSet, oldest)
	}
	return false
}

func (s *Service) forgetMessage(messageID string) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.recentMessageSet[messageID]; !exists {
		return
	}
	delete(s.recentMessageSet, messageID)
	for i, seenID := range s.recentMessageIDs {
		if seenID == messageID {
			s.recentMessageIDs = append(s.recentMessageIDs[:i], s.recentMessageIDs[i+1:]...)
			break
		}
	}
}

func (s *Service) ensureRuntime() *groupRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runtime != nil {
		return s.runtime
	}

	rt := &groupRuntime{
		opts:    s.opts,
		session: s.opts.SessionName,
		queue:   make(chan string, queueSize),
	}
	s.runtime = rt
	go s.runGroup(rt)
	return rt
}

func (s *Service) runGroup(rt *groupRuntime) {
	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case text := <-rt.queue:
			rt.pending = append(rt.pending, text)
			if err := s.ensureSession(rt); err != nil {
				s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Failed to start session: %v", err))
				continue
			}
			if !rt.lastBusy && len(rt.pending) > 0 {
				s.dispatchNext(rt)
			}
		case <-ticker.C:
			if !rt.sessionReady {
				if len(rt.pending) == 0 {
					continue
				}
				if err := s.ensureSession(rt); err != nil {
					s.logger.Warn("retry ensure session failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
					continue
				}
				if !rt.lastBusy && len(rt.pending) > 0 {
					s.dispatchNext(rt)
				}
				continue
			}
			s.poll(rt)
		}
	}
}

func (s *Service) ensureSession(rt *groupRuntime) error {
	if rt.sessionReady {
		return nil
	}

	created, err := s.console.EnsureSession(s.ctx, tmuxctl.SessionSpec{
		SessionName:                 rt.session,
		CWD:                         rt.opts.CWD,
		StartupWait:                 s.startWait,
		AutoPressEnterOnTrustPrompt: true,
	})
	if err != nil {
		return err
	}

	snapshot, err := s.console.Capture(s.ctx, rt.session, s.history)
	if err != nil {
		return err
	}

	rt.lastText = tmuxctl.NormalizeSnapshot(snapshot)
	rt.baseText = ""
	rt.lastBusy = tmuxctl.IsBusy(snapshot)
	rt.idleTicks = 0
	rt.outputBuffer = ""
	rt.sessionReady = true

	if created {
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Connected to `%s`, tmux=`%s`.", filepath.Base(rt.opts.CWD), rt.session))
	}
	return nil
}

func (s *Service) dispatchNext(rt *groupRuntime) {
	if len(rt.pending) == 0 {
		return
	}

	text := rt.pending[0]
	rt.pending = rt.pending[1:]
	if err := s.console.SendText(s.ctx, rt.session, text); err != nil {
		s.logger.Error("send text to codex failed", "group_id", rt.opts.GroupID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Failed to send to Codex: %v", err))
		rt.pending = append([]string{text}, rt.pending...)
		rt.sessionReady = false
		rt.baseText = ""
		rt.lastBusy = false
		rt.idleTicks = 0
		rt.lastText = ""
		rt.outputBuffer = ""
		return
	}

	rt.baseText = rt.lastText
	rt.lastBusy = true
	rt.idleTicks = 0
	rt.outputBuffer = ""
	s.sendBestEffort(rt.opts.GroupID, "[working] Codex is processing.")
}

func (s *Service) poll(rt *groupRuntime) {
	snapshot, err := s.console.Capture(s.ctx, rt.session, s.history)
	if err != nil {
		s.logger.Warn("capture tmux pane failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
		rt.sessionReady = false
		rt.baseText = ""
		rt.lastBusy = false
		rt.idleTicks = 0
		rt.lastText = ""
		rt.outputBuffer = ""
		return
	}

	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	prevText := tmuxctl.SliceAfter(rt.baseText, rt.lastText)
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	busy := tmuxctl.IsBusy(snapshot)
	delta, reset := tmuxctl.DiffText(prevText, currText)

	if busy {
		rt.idleTicks = 0
	} else {
		rt.idleTicks++
	}

	if delta != "" {
		rt.outputBuffer += delta
	}
	if reset {
		rt.outputBuffer = ""
	}
	if shouldFlush(rt.outputBuffer, busy, rt.idleTicks) {
		s.sendChunked(rt.opts.GroupID, strings.Trim(rt.outputBuffer, "\n"))
		rt.outputBuffer = ""
	}

	rt.lastText = currFullText
	rt.lastBusy = busy

	if !busy && len(rt.pending) > 0 {
		s.dispatchNext(rt)
	}
}

func shouldFlush(buffer string, busy bool, idleTicks int) bool {
	if strings.TrimSpace(buffer) == "" {
		return false
	}
	if !busy && idleTicks >= 2 {
		return true
	}
	if strings.Contains(buffer, "\n") {
		return true
	}
	return utf8.RuneCountInString(buffer) >= 80
}

func (s *Service) sendChunked(groupID string, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		s.sendBestEffort(groupID, chunk)
	}
}

func (s *Service) sendBestEffort(groupID string, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := s.messenger.SendTextToChat(context.Background(), groupID, text); err != nil {
		s.logger.Error("send lark message failed", "group_id", groupID, "err", err)
	}
}

func splitByRunes(text string, limit int) []string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}

	var chunks []string
	var builder strings.Builder
	count := 0
	for _, r := range text {
		builder.WriteRune(r)
		count++
		if count >= limit {
			chunks = append(chunks, builder.String())
			builder.Reset()
			count = 0
		}
	}
	if builder.Len() > 0 {
		chunks = append(chunks, builder.String())
	}
	return chunks
}

func DefaultSessionName(cwd string) string {
	base := filepath.Base(strings.TrimRight(strings.TrimSpace(cwd), "/"))
	if base == "" || base == "." || base == "/" {
		base = "session"
	}
	return "imcodex-" + sanitizeName(base)
}

func DefaultSessionNameForGroup(groupID string, cwd string) string {
	group := sanitizeName(groupID)
	if group == "" || group == "session" {
		return DefaultSessionName(cwd)
	}
	return DefaultSessionName(cwd) + "-" + group
}

func sanitizeName(in string) string {
	var b strings.Builder
	for _, r := range in {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}
