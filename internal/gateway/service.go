package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/magnaflowlabs/imcodex/internal/tmuxctl"
)

const (
	maxMessageRunes        = 3000
	queueSize              = 64
	recentMessageIDLimit   = 256
	maxDetachedSendRetries = 3
	defaultFlushIdleTicks  = 8
	defaultWorkingAfter    = time.Second
	defaultChatActionEvery = 4 * time.Second
	defaultBusyFlushAfter  = 5 * time.Second
	defaultCompleteAfter   = 0 * time.Millisecond
	defaultEditRolloverAt  = 2800
	workingStatusText      = "[working] Codex is processing."
)

type IncomingMessage struct {
	MessageID   string
	GroupID     string
	Text        string
	Attachments []IncomingAttachment
}

type IncomingAttachment struct {
	ResourceType string
	ResourceKey  string
	FileName     string
}

type DownloadedResource struct {
	Data        []byte
	FileName    string
	ContentType string
}

type Options struct {
	GroupID               string
	CWD                   string
	SessionName           string
	InterruptOnNewMessage bool
}

type Messenger interface {
	SendTextToChat(ctx context.Context, groupID string, text string) error
}

type SentMessage struct {
	MessageID string
}

type EditableMessenger interface {
	SendTextToChatWithID(ctx context.Context, groupID string, text string) (SentMessage, error)
	EditTextInChat(ctx context.Context, groupID string, messageID string, text string) error
}

type ActionMessenger interface {
	SendChatAction(ctx context.Context, groupID string, action string) error
}

type Console interface {
	EnsureSession(ctx context.Context, spec tmuxctl.SessionSpec) (bool, error)
	SendText(ctx context.Context, session string, text string) error
	Capture(ctx context.Context, session string, history int) (string, error)
	Interrupt(ctx context.Context, session string) error
	ForceInterrupt(ctx context.Context, session string) error
}

type ResourceFetcher interface {
	DownloadMessageResource(ctx context.Context, messageID string, resourceType string, resourceKey string) (DownloadedResource, error)
}

type Service struct {
	ctx                 context.Context
	opts                Options
	messenger           Messenger
	console             Console
	resources           ResourceFetcher
	logger              *slog.Logger
	pollEvery           time.Duration
	startWait           time.Duration
	history             int
	flushIdleTicks      int
	workingAfter        time.Duration
	chatActionEvery     time.Duration
	busyFlushAfter      time.Duration
	completeAfter       time.Duration
	editRolloverAt      int
	interruptForceAfter time.Duration

	mu      sync.Mutex
	runtime *groupRuntime

	recentMessageIDs []string
	recentMessageSet map[string]struct{}
}

type activeRequest struct {
	messageID string
	input     string
}

type groupRuntime struct {
	opts                 Options
	session              string
	queue                chan IncomingMessage
	pending              []IncomingMessage
	active               *activeRequest
	outputArmed          bool
	promptEchoTail       string
	promptEchoPending    bool
	statusSlotEmptySince time.Time
	lastProgressAt       time.Time
	sessionReady         bool
	lastText             string
	baseText             string
	lastBusy             bool
	idleTicks            int
	outputBuffer         string
	outputBufferedAt     time.Time
	outputText           string
	outputMessages       []trackedMessage
	detachedOutputs      []string
	detachedBackoffUntil time.Time
	detachedRetryCount   int
	editBackoffUntil     time.Time
	busySince            time.Time
	workingSent          bool
	lastActionAt         time.Time
	interruptSentAt      time.Time
	forceInterruptSent   bool
}

type trackedMessage struct {
	messageID string
	text      string
}

func NewService(ctx context.Context, opts Options, messenger Messenger, console Console, resources ResourceFetcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.SessionName == "" {
		opts.SessionName = DefaultSessionName(opts.CWD)
	}
	return &Service{
		ctx:                 ctx,
		opts:                opts,
		messenger:           messenger,
		console:             console,
		resources:           resources,
		logger:              logger,
		pollEvery:           500 * time.Millisecond,
		startWait:           4 * time.Second,
		history:             2000,
		flushIdleTicks:      defaultFlushIdleTicks,
		workingAfter:        defaultWorkingAfter,
		chatActionEvery:     defaultChatActionEvery,
		busyFlushAfter:      defaultBusyFlushAfter,
		completeAfter:       defaultCompleteAfter,
		editRolloverAt:      defaultEditRolloverAt,
		interruptForceAfter: time.Second,
	}
}

func (s *Service) HandleMessage(ctx context.Context, msg IncomingMessage) error {
	if strings.TrimSpace(msg.Text) == "" && len(msg.Attachments) == 0 {
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
	case rt.queue <- msg:
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
		queue:   make(chan IncomingMessage, queueSize),
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
		case msg := <-rt.queue:
			wasReady := rt.sessionReady
			s.enqueuePending(rt, msg)
			if err := s.ensureSession(rt); err != nil {
				s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("Failed to start Codex session: %v", err))
				continue
			}
			if !wasReady {
				s.keepLatestPending(rt)
			}
			s.flushDetachedOutputs(rt)
			if s.opts.InterruptOnNewMessage && rt.lastBusy && len(rt.pending) > 0 {
				s.requestInterrupt(rt)
			}
			if !rt.lastBusy && len(rt.pending) > 0 {
				s.dispatchNext(rt)
			}
		case <-ticker.C:
			s.flushDetachedOutputs(rt)
			if !rt.sessionReady {
				if len(rt.pending) == 0 && !rt.hasBufferedOutput() {
					continue
				}
				if err := s.ensureSession(rt); err != nil {
					s.logger.Warn("retry ensure session failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
					continue
				}
				s.keepLatestPending(rt)
				if !rt.lastBusy && rt.hasBufferedOutput() {
					s.flushOutputBuffer(rt)
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
	recoveringOutput := rt.outputArmed ||
		rt.hasBufferedOutput() ||
		strings.TrimSpace(rt.outputText) != "" ||
		len(rt.outputMessages) > 0 ||
		len(rt.detachedOutputs) > 0 ||
		rt.promptEchoPending ||
		!rt.detachedBackoffUntil.IsZero() ||
		!rt.editBackoffUntil.IsZero()

	_, err := s.console.EnsureSession(s.ctx, tmuxctl.SessionSpec{
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
	if !recoveringOutput {
		rt.clearOutputBuffer()
		rt.outputText = ""
		rt.outputMessages = nil
		rt.promptEchoTail = ""
		rt.promptEchoPending = false
		rt.statusSlotEmptySince = time.Time{}
		rt.lastProgressAt = time.Time{}
		rt.detachedBackoffUntil = time.Time{}
		rt.detachedRetryCount = 0
		rt.editBackoffUntil = time.Time{}
	}
	rt.outputArmed = recoveringOutput
	rt.busySince = time.Time{}
	rt.workingSent = false
	rt.lastActionAt = time.Time{}
	rt.interruptSentAt = time.Time{}
	rt.forceInterruptSent = false
	rt.sessionReady = true

	return nil
}

func (s *Service) dispatchNext(rt *groupRuntime) {
	if len(rt.pending) == 0 {
		return
	}
	s.finalizeOutputBeforeDispatch(rt)
	s.flushOutputBuffer(rt)
	if rt.hasBufferedOutput() {
		s.detachBufferedOutput(rt)
	}

	msg := rt.pending[0]
	rt.pending = rt.pending[1:]
	text, err := s.materializeInput(msg)
	if err != nil {
		s.logger.Error("prepare message for codex failed", "group_id", rt.opts.GroupID, "message_id", msg.MessageID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("Failed to prepare request for Codex: %v", err))
		if !rt.lastBusy && len(rt.pending) > 0 {
			s.dispatchNext(rt)
		}
		return
	}

	if err := s.dispatchPrepared(rt, &activeRequest{
		messageID: msg.MessageID,
		input:     text,
	}); err != nil {
		s.logger.Error("send text to codex failed", "group_id", rt.opts.GroupID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("Failed to send to Codex: %v", err))
		rt.pending = nil
		s.dropQueued(rt)
		rt.sessionReady = false
		rt.baseText = ""
		rt.lastBusy = false
		rt.idleTicks = 0
		rt.lastText = ""
		rt.outputArmed = false
		rt.promptEchoTail = ""
		rt.promptEchoPending = false
		rt.statusSlotEmptySince = time.Time{}
		rt.lastProgressAt = time.Time{}
		rt.clearOutputBuffer()
		rt.outputText = ""
		rt.outputMessages = nil
		rt.detachedOutputs = nil
		rt.detachedBackoffUntil = time.Time{}
		rt.detachedRetryCount = 0
		rt.editBackoffUntil = time.Time{}
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.lastActionAt = time.Time{}
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
		rt.active = nil
		return
	}
}

func (s *Service) finalizeOutputBeforeDispatch(rt *groupRuntime) {
	if rt == nil || !rt.sessionReady || strings.TrimSpace(rt.session) == "" {
		return
	}
	// Only run boundary finalization when there is existing reply state to reconcile.
	// This avoids consuming fresh run snapshots before the very first dispatch.
	if !rt.hasBufferedOutput() && strings.TrimSpace(rt.outputText) == "" && len(rt.outputMessages) == 0 {
		return
	}
	snapshot, err := s.console.Capture(s.ctx, rt.session, s.history)
	if err != nil {
		s.logger.Warn("capture finalize output failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
		return
	}
	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	rt.lastText = currFullText
	if tmuxctl.IsBusy(snapshot) {
		return
	}
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	currText = strings.Trim(currText, "\n")
	if strings.TrimSpace(currText) == "" {
		return
	}
	knownText := mergeBufferedOutput(rt.outputText, rt.outputBuffer)
	delta, reset := tmuxctl.DiffText(knownText, currText)
	if reset {
		s.resetBufferedOutput(rt, currText)
		return
	}
	rt.appendOutputBuffer(delta, time.Now())
}

func (s *Service) dispatchPrepared(rt *groupRuntime, req *activeRequest) error {
	if req == nil {
		return errors.New("active request is nil")
	}
	baseline := s.refreshDispatchBaseline(rt)
	s.prepareOutputForDispatch(rt)
	if err := s.console.SendText(s.ctx, rt.session, req.input); err != nil {
		return err
	}
	rt.baseText = baseline
	rt.lastBusy = true
	rt.idleTicks = 0
	rt.outputArmed = true
	rt.promptEchoTail = normalizePromptEchoTail(req.input)
	rt.promptEchoPending = rt.promptEchoTail != ""
	rt.statusSlotEmptySince = time.Time{}
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.editBackoffUntil = time.Time{}
	rt.busySince = time.Now()
	rt.lastProgressAt = rt.busySince
	rt.workingSent = false
	rt.lastActionAt = time.Time{}
	rt.interruptSentAt = time.Time{}
	rt.forceInterruptSent = false
	rt.active = req
	return nil
}

func (s *Service) refreshDispatchBaseline(rt *groupRuntime) string {
	if rt == nil || strings.TrimSpace(rt.session) == "" {
		return ""
	}
	if strings.TrimSpace(rt.lastText) == "" {
		return rt.lastText
	}
	snapshot, err := s.console.Capture(s.ctx, rt.session, s.history)
	if err != nil {
		s.logger.Warn("capture dispatch baseline failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
		return rt.lastText
	}
	rt.lastText = tmuxctl.NormalizeSnapshot(snapshot)
	return rt.lastText
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
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.lastActionAt = time.Time{}
		rt.statusSlotEmptySince = time.Time{}
		rt.lastProgressAt = time.Time{}
		rt.active = nil
		return
	}

	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	prevText := tmuxctl.SliceAfter(rt.baseText, rt.lastText)
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	now := time.Now()
	if rt.promptEchoPending {
		prevText, _ = suppressPromptEchoPrefix(prevText, rt.promptEchoTail)
		var consumed bool
		currText, consumed = suppressPromptEchoPrefix(currText, rt.promptEchoTail)
		if consumed || strings.TrimSpace(currText) != "" {
			rt.promptEchoPending = false
		}
	}
	delta, reset := tmuxctl.DiffText(prevText, currText)
	progressed := strings.TrimSpace(delta) != "" || reset
	if progressed {
		rt.lastProgressAt = now
	}

	slotText, slotKnown := tmuxctl.InputStatusSlot(snapshot)
	if slotKnown && strings.TrimSpace(slotText) == "" {
		if rt.statusSlotEmptySince.IsZero() {
			rt.statusSlotEmptySince = now
		}
	} else {
		rt.statusSlotEmptySince = time.Time{}
	}

	busy := tmuxctl.IsBusy(snapshot)
	if rt.active != nil && rt.lastBusy && !busy {
		if !s.isCompletionConfirmed(rt, now, slotKnown) {
			busy = true
		}
	}
	becameIdle := rt.lastBusy && !busy

	if busy {
		rt.idleTicks = 0
		if rt.outputText == "" {
			s.sendChatAction(rt)
		}
		if s.opts.InterruptOnNewMessage && len(rt.pending) > 0 && rt.interruptSentAt.IsZero() {
			s.requestInterrupt(rt)
		}
		if s.opts.InterruptOnNewMessage && !rt.interruptSentAt.IsZero() && !rt.forceInterruptSent && time.Since(rt.interruptSentAt) >= s.interruptForceAfter {
			if err := s.console.ForceInterrupt(s.ctx, rt.session); err != nil {
				s.logger.Warn("force interrupt codex failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
			} else {
				rt.forceInterruptSent = true
			}
		}
		if !rt.workingSent && !rt.busySince.IsZero() && time.Since(rt.busySince) >= s.workingAfter {
			s.sendWorkingStatus(rt)
			rt.workingSent = true
		}
	} else {
		rt.idleTicks++
	}

	if rt.outputArmed {
		rt.appendOutputBuffer(delta, now)
		if reset {
			s.resetBufferedOutput(rt, currText)
		}
		if shouldFlushOnSize(rt, s.editRolloverAt, maxMessageRunes) {
			s.flushOutputBuffer(rt)
		} else if becameIdle && strings.TrimSpace(rt.outputBuffer) != "" && s.editableMessenger() != nil {
			s.flushOutputBuffer(rt)
		} else if shouldFlush(rt.outputBuffer, rt.outputBufferedAt, busy, rt.idleTicks, s.flushIdleTicks, s.busyFlushAfter, now) {
			s.flushOutputBuffer(rt)
		}
	}

	rt.lastText = currFullText
	rt.lastBusy = busy
	if !busy {
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.lastActionAt = time.Time{}
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
		rt.active = nil
	}

	if !busy && len(rt.pending) > 0 {
		s.dispatchNext(rt)
	}
}

func shouldFlush(buffer string, bufferedAt time.Time, busy bool, idleTicks int, flushIdleTicks int, busyFlushAfter time.Duration, now time.Time) bool {
	if strings.TrimSpace(buffer) == "" {
		return false
	}
	if busy {
		return !bufferedAt.IsZero() && busyFlushAfter > 0 && now.Sub(bufferedAt) >= busyFlushAfter
	}
	if flushIdleTicks <= 0 {
		flushIdleTicks = 1
	}
	return idleTicks >= flushIdleTicks
}

func shouldFlushOnSize(rt *groupRuntime, softLimit int, hardLimit int) bool {
	if rt == nil || strings.TrimSpace(rt.outputBuffer) == "" {
		return false
	}
	limit := softLimit
	if limit <= 0 || (hardLimit > 0 && limit > hardLimit) {
		limit = hardLimit
	}
	if limit <= 0 {
		limit = maxMessageRunes
	}
	candidate := strings.Trim(rt.outputText+rt.outputBuffer, "\n")
	if strings.TrimSpace(candidate) == "" {
		return false
	}
	return utf8.RuneCountInString(candidate) >= limit
}

func (s *Service) isCompletionConfirmed(rt *groupRuntime, now time.Time, slotKnown bool) bool {
	if rt == nil {
		return true
	}
	if !slotKnown {
		// Fallback to legacy marker-only behavior when prompt/status slot is not visible.
		return true
	}
	wait := s.completeAfter
	if wait <= 0 {
		wait = s.pollEvery
	}
	if wait <= 0 {
		wait = 500 * time.Millisecond
	}
	if rt.statusSlotEmptySince.IsZero() {
		return false
	}
	if now.Sub(rt.statusSlotEmptySince) < wait {
		return false
	}
	if rt.lastProgressAt.IsZero() {
		return true
	}
	return now.Sub(rt.lastProgressAt) >= wait
}

func normalizePromptEchoTail(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(input, "\n")
	if len(lines) <= 1 {
		return ""
	}
	tail := normalizePromptEchoLines(lines[1:])
	if len(tail) == 0 {
		return ""
	}
	return strings.Join(tail, "\n")
}

func normalizePromptEchoLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if len(out) == 0 || prevBlank {
				continue
			}
			out = append(out, "")
			prevBlank = true
			continue
		}
		out = append(out, line)
		prevBlank = false
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func suppressPromptEchoPrefix(text string, echoTail string) (string, bool) {
	text = strings.TrimLeft(text, "\n")
	echoTail = strings.Trim(echoTail, "\n")
	if text == "" || echoTail == "" {
		return text, false
	}
	if strings.HasPrefix(text, echoTail) {
		return strings.TrimLeft(text[len(echoTail):], "\n"), true
	}
	return text, false
}

func (s *Service) resetBufferedOutput(rt *groupRuntime, currText string) {
	if rt == nil {
		return
	}
	currText = strings.Trim(currText, "\n")
	if strings.TrimSpace(currText) == "" {
		rt.outputText = ""
		if !rt.hasBufferedOutput() {
			rt.clearOutputBuffer()
		}
		return
	}

	delta, reset := tmuxctl.DiffText(rt.outputText, currText)
	if reset {
		rt.outputText = ""
		// Keep any already-buffered unsent body and merge the reset snapshot
		// so transient pane resets do not drop tail output.
		rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, currText), time.Now())
		return
	}
	rt.replaceOutputBuffer(delta, time.Now())
}

func (s *Service) enqueuePending(rt *groupRuntime, msg IncomingMessage) {
	if !rt.sessionReady {
		rt.pending = []IncomingMessage{msg}
		return
	}
	if s.opts.InterruptOnNewMessage && rt.lastBusy {
		rt.pending = []IncomingMessage{msg}
		return
	}
	rt.pending = append(rt.pending, msg)
}

func (s *Service) requestInterrupt(rt *groupRuntime) {
	if !rt.sessionReady || !rt.lastBusy || !rt.interruptSentAt.IsZero() {
		return
	}
	if err := s.console.Interrupt(s.ctx, rt.session); err != nil {
		s.logger.Warn("interrupt codex failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
		return
	}
	rt.interruptSentAt = time.Now()
	rt.forceInterruptSent = false
}

func (rt *groupRuntime) activeMessageID() string {
	if rt == nil || rt.active == nil {
		return ""
	}
	return rt.active.messageID
}

func (s *Service) keepLatestPending(rt *groupRuntime) {
	var latest IncomingMessage
	if len(rt.pending) > 0 {
		latest = rt.pending[len(rt.pending)-1]
	}
	for {
		select {
		case msg := <-rt.queue:
			latest = msg
		default:
			if strings.TrimSpace(latest.Text) == "" && len(latest.Attachments) == 0 {
				rt.pending = nil
			} else {
				rt.pending = []IncomingMessage{latest}
			}
			return
		}
	}
}

func (s *Service) dropQueued(rt *groupRuntime) {
	for {
		select {
		case <-rt.queue:
		default:
			return
		}
	}
}

func (s *Service) materializeInput(msg IncomingMessage) (string, error) {
	parts := make([]string, 0, 1+len(msg.Attachments))
	if strings.TrimSpace(msg.Text) != "" {
		parts = append(parts, msg.Text)
	}
	if len(msg.Attachments) == 0 {
		return strings.Join(parts, "\n\n"), nil
	}
	if s.resources == nil {
		return "", errors.New("attachments are not supported")
	}

	inboxDir := filepath.Join(s.opts.CWD, ".imcodex", "inbox")
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return "", fmt.Errorf("create inbox directory: %w", err)
	}

	for i, attachment := range msg.Attachments {
		resource, err := s.resources.DownloadMessageResource(s.ctx, msg.MessageID, attachment.ResourceType, attachment.ResourceKey)
		if err != nil {
			return "", fmt.Errorf("download %s attachment: %w", attachmentKind(attachment), err)
		}
		path, err := saveAttachment(inboxDir, msg.MessageID, i, attachment, resource)
		if err != nil {
			return "", fmt.Errorf("save %s attachment: %w", attachmentKind(attachment), err)
		}
		parts = append(parts, fmt.Sprintf("User attached %s: %s. Inspect it.", attachmentDescriptor(attachment), path))
	}
	return strings.Join(parts, "\n\n"), nil
}

func saveAttachment(inboxDir string, messageID string, index int, attachment IncomingAttachment, resource DownloadedResource) (string, error) {
	name := firstNonEmpty(attachment.FileName, resource.FileName)
	name = sanitizeFileName(name)
	if name == "" {
		name = defaultAttachmentFileName(index, attachment, resource.ContentType)
	}

	base := fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), sanitizeName(messageID))
	if index > 0 {
		base = fmt.Sprintf("%s-%d", base, index+1)
	}
	path := filepath.Join(inboxDir, base+"-"+name)
	if err := os.WriteFile(path, resource.Data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func defaultAttachmentFileName(index int, attachment IncomingAttachment, contentType string) string {
	ext := extensionFromContentType(contentType)
	if ext == "" {
		switch attachment.ResourceType {
		case "image":
			ext = ".img"
		default:
			ext = ".bin"
		}
	}
	return fmt.Sprintf("%s-%02d%s", attachmentKind(attachment), index+1, ext)
}

func extensionFromContentType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	exts, _ := mime.ExtensionsByType(mediaType)
	if len(exts) == 0 {
		return ""
	}
	for _, preferred := range preferredExtensionsForMediaType(mediaType) {
		for _, ext := range exts {
			if ext == preferred {
				return ext
			}
		}
	}
	return exts[0]
}

func preferredExtensionsForMediaType(mediaType string) []string {
	switch mediaType {
	case "image/jpeg":
		return []string{".jpg", ".jpeg"}
	case "text/plain":
		return []string{".txt"}
	}
	return nil
}

func sanitizeFileName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	if name == "" || name == "." {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return ""
	}
	return out
}

func attachmentKind(attachment IncomingAttachment) string {
	if attachment.ResourceType == "image" {
		return "image"
	}
	return "file"
}

func attachmentDescriptor(attachment IncomingAttachment) string {
	if attachmentKind(attachment) == "image" {
		return "an image"
	}
	return "a file"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *Service) sendChunked(groupID string, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := s.sendChunkedStrict(groupID, text); err != nil {
		s.logger.Error("send chunked message failed", "group_id", groupID, "err", err)
	}
}

func (s *Service) sendChunkedStrict(groupID string, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		if err := s.messenger.SendTextToChat(context.Background(), groupID, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) editableMessenger() EditableMessenger {
	editable, _ := s.messenger.(EditableMessenger)
	return editable
}

func (s *Service) actionMessenger() ActionMessenger {
	actioner, _ := s.messenger.(ActionMessenger)
	return actioner
}

func (s *Service) prepareOutputForDispatch(rt *groupRuntime) {
	if rt == nil {
		return
	}
	rt.clearOutputBuffer()
	rt.outputText = ""
	if len(rt.outputMessages) == 1 && rt.outputMessages[0].text == workingStatusText {
		return
	}
	rt.outputMessages = nil
}

func (s *Service) detachBufferedOutput(rt *groupRuntime) {
	if rt == nil {
		return
	}
	candidate := mergeBufferedOutput(rt.outputText, rt.outputBuffer)
	unsent := candidate
	if strings.HasPrefix(candidate, rt.outputText) {
		unsent = candidate[len(rt.outputText):]
	}
	unsent = strings.Trim(unsent, "\n")
	if strings.TrimSpace(unsent) != "" {
		rt.enqueueDetachedOutput(unsent)
	}
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.outputMessages = nil
	rt.editBackoffUntil = time.Time{}
	rt.detachedRetryCount = 0
}

func (s *Service) sendWorkingStatus(rt *groupRuntime) {
	if rt == nil {
		return
	}
	editable := s.editableMessenger()
	if editable == nil {
		s.sendBestEffort(rt.opts.GroupID, workingStatusText)
		return
	}
	if len(rt.outputMessages) > 0 {
		return
	}
	msg, err := editable.SendTextToChatWithID(context.Background(), rt.opts.GroupID, workingStatusText)
	if err != nil {
		s.logger.Error("send working message failed", "group_id", rt.opts.GroupID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, workingStatusText)
		return
	}
	rt.outputMessages = []trackedMessage{{
		messageID: msg.MessageID,
		text:      workingStatusText,
	}}
}

func (s *Service) sendChatAction(rt *groupRuntime) {
	if rt == nil || s.chatActionEvery <= 0 {
		return
	}
	actioner := s.actionMessenger()
	if actioner == nil {
		return
	}
	now := time.Now()
	if !rt.lastActionAt.IsZero() && now.Sub(rt.lastActionAt) < s.chatActionEvery {
		return
	}
	if err := actioner.SendChatAction(context.Background(), rt.opts.GroupID, "typing"); err != nil {
		s.logger.Warn("send chat action failed", "group_id", rt.opts.GroupID, "err", err)
		return
	}
	rt.lastActionAt = now
}

func (s *Service) flushOutputBuffer(rt *groupRuntime) {
	if rt == nil {
		return
	}
	now := time.Now()
	if !rt.editBackoffUntil.IsZero() {
		if rt.editBackoffUntil.After(now) {
			return
		}
		rt.editBackoffUntil = time.Time{}
	}
	raw := rt.outputBuffer
	bufferedAt := rt.outputBufferedAt
	rt.clearOutputBuffer()
	if strings.TrimSpace(raw) == "" {
		return
	}
	editable := s.editableMessenger()
	if editable == nil {
		text := strings.Trim(raw, "\n")
		if strings.TrimSpace(text) == "" {
			return
		}
		s.sendChunked(rt.opts.GroupID, text)
		return
	}
	candidateText := mergeBufferedOutput(rt.outputText, raw)
	desiredText := strings.Trim(candidateText, "\n")
	if strings.TrimSpace(desiredText) == "" {
		rt.outputText = candidateText
		return
	}
	if err := s.syncEditableOutput(rt, editable, desiredText); err != nil {
		if isMessageNotModifiedError(err) {
			rt.outputText = candidateText
			return
		}
		if shouldResetEditableThread(err) {
			rt.outputMessages = nil
			if retryErr := s.syncEditableOutput(rt, editable, desiredText); retryErr == nil {
				rt.outputText = candidateText
				return
			} else {
				err = retryErr
			}
		}
		rt.outputBuffer = raw + rt.outputBuffer
		if rt.outputBufferedAt.IsZero() {
			rt.outputBufferedAt = bufferedAt
		}
		if retryAfter := retryAfterFromRateLimitError(err); retryAfter > 0 {
			rt.editBackoffUntil = time.Now().Add(retryAfter)
			s.logger.Warn(
				"sync editable output rate-limited",
				"group_id",
				rt.opts.GroupID,
				"retry_after",
				retryAfter.String(),
				"retry_at",
				rt.editBackoffUntil,
				"err",
				err,
			)
			return
		}
		s.logger.Error("sync editable output failed", "group_id", rt.opts.GroupID, "err", err)
		return
	}
	rt.outputText = candidateText
}

func isMessageNotModifiedError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" || !strings.Contains(text, "message is not modified") {
		return false
	}
	return strings.Contains(text, "code=400") || strings.Contains(text, "http=400")
}

func shouldResetEditableThread(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	if !strings.Contains(text, "code=400") && !strings.Contains(text, "http=400") {
		return false
	}
	return strings.Contains(text, "message to edit not found") ||
		strings.Contains(text, "message can't be edited") ||
		strings.Contains(text, "message can not be edited")
}

func mergeBufferedOutput(existing string, delta string) string {
	if delta == "" {
		return existing
	}
	if existing == "" {
		return delta
	}
	if strings.HasPrefix(delta, existing) {
		return delta
	}
	if strings.HasSuffix(existing, delta) {
		return existing
	}
	if overlap := suffixPrefixOverlap(existing, delta); usableMergeOverlap(delta, overlap) {
		return existing + delta[overlap:]
	}
	return existing + delta
}

func usableMergeOverlap(delta string, overlap int) bool {
	if overlap <= 0 || overlap > len(delta) {
		return false
	}
	if overlap >= 8 {
		return true
	}
	return strings.Contains(delta[:overlap], "\n")
}

func suffixPrefixOverlap(prev string, curr string) int {
	limit := len(prev)
	if len(curr) < limit {
		limit = len(curr)
	}
	for size := limit; size > 0; size-- {
		if prev[len(prev)-size:] == curr[:size] {
			return size
		}
	}
	return 0
}

func retryAfterFromRateLimitError(err error) time.Duration {
	if err == nil {
		return 0
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" || (!strings.Contains(text, "http=429") && !strings.Contains(text, "code=429")) {
		return 0
	}
	if i := strings.Index(text, "retry_after="); i >= 0 {
		raw := text[i+len("retry_after="):]
		n := parseLeadingInt(raw)
		if n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if i := strings.Index(text, "retry after "); i >= 0 {
		raw := text[i+len("retry after "):]
		n := parseLeadingInt(raw)
		if n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 2 * time.Second
}

func parseLeadingInt(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	end := 0
	for end < len(text) {
		ch := text[end]
		if ch < '0' || ch > '9' {
			break
		}
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(text[:end])
	if err != nil {
		return 0
	}
	return n
}

func (rt *groupRuntime) appendOutputBuffer(delta string, now time.Time) {
	if rt == nil || delta == "" {
		return
	}
	if rt.outputBuffer == "" && rt.outputBufferedAt.IsZero() {
		rt.outputBufferedAt = now
	}
	rt.outputBuffer += delta
}

func (rt *groupRuntime) replaceOutputBuffer(text string, now time.Time) {
	if rt == nil {
		return
	}
	if text == "" {
		rt.clearOutputBuffer()
		return
	}
	rt.outputBuffer = text
	if rt.outputBufferedAt.IsZero() {
		rt.outputBufferedAt = now
	}
}

func (rt *groupRuntime) clearOutputBuffer() {
	if rt == nil {
		return
	}
	rt.outputBuffer = ""
	rt.outputBufferedAt = time.Time{}
}

func (rt *groupRuntime) hasBufferedOutput() bool {
	if rt == nil {
		return false
	}
	return strings.TrimSpace(rt.outputBuffer) != ""
}

func (rt *groupRuntime) enqueueDetachedOutput(text string) {
	if rt == nil {
		return
	}
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		chunk = strings.Trim(chunk, "\n")
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		rt.detachedOutputs = append(rt.detachedOutputs, chunk)
	}
}

func (s *Service) flushDetachedOutputs(rt *groupRuntime) {
	if rt == nil || len(rt.detachedOutputs) == 0 {
		return
	}
	now := time.Now()
	if !rt.detachedBackoffUntil.IsZero() {
		if rt.detachedBackoffUntil.After(now) {
			return
		}
		rt.detachedBackoffUntil = time.Time{}
	}

	for len(rt.detachedOutputs) > 0 {
		text := rt.detachedOutputs[0]
		if err := s.messenger.SendTextToChat(context.Background(), rt.opts.GroupID, text); err != nil {
			retryAfter := retryAfterFromRateLimitError(err)
			if retryAfter > 0 {
				rt.detachedBackoffUntil = time.Now().Add(retryAfter)
				rt.detachedRetryCount = 0
				s.logger.Warn(
					"flush detached output rate-limited",
					"group_id",
					rt.opts.GroupID,
					"retry_after",
					retryAfter.String(),
					"retry_at",
					rt.detachedBackoffUntil,
					"err",
					err,
				)
				return
			}
			rt.detachedRetryCount++
			if rt.detachedRetryCount >= maxDetachedSendRetries {
				s.logger.Error(
					"drop detached output chunk after retries",
					"group_id",
					rt.opts.GroupID,
					"attempts",
					rt.detachedRetryCount,
					"err",
					err,
				)
				s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("Failed to deliver buffered output chunk after %d attempts: %v", rt.detachedRetryCount, err))
				rt.detachedOutputs = rt.detachedOutputs[1:]
				rt.detachedBackoffUntil = time.Time{}
				rt.detachedRetryCount = 0
				continue
			}
			retryAfter = time.Duration(rt.detachedRetryCount) * time.Second
			rt.detachedBackoffUntil = time.Now().Add(retryAfter)
			s.logger.Warn(
				"flush detached output failed",
				"group_id",
				rt.opts.GroupID,
				"attempt",
				rt.detachedRetryCount,
				"retry_after",
				retryAfter.String(),
				"retry_at",
				rt.detachedBackoffUntil,
				"err",
				err,
			)
			return
		}
		rt.detachedOutputs = rt.detachedOutputs[1:]
		rt.detachedRetryCount = 0
	}
}

func (s *Service) syncEditableOutput(rt *groupRuntime, editable EditableMessenger, desiredText string) error {
	segments := splitForEditMessages(desiredText, s.editRolloverAt, maxMessageRunes)
	if len(segments) == 0 {
		return nil
	}

	next := make([]trackedMessage, len(rt.outputMessages))
	copy(next, rt.outputMessages)
	if len(next) > len(segments) {
		next = next[:len(segments)]
	}

	for i, segment := range segments {
		if i < len(next) {
			if next[i].text == segment {
				continue
			}
			if err := editable.EditTextInChat(context.Background(), rt.opts.GroupID, next[i].messageID, segment); err != nil {
				rt.outputMessages = next
				return err
			}
			next[i].text = segment
			continue
		}

		msg, err := editable.SendTextToChatWithID(context.Background(), rt.opts.GroupID, segment)
		if err != nil {
			rt.outputMessages = next
			return err
		}
		next = append(next, trackedMessage{
			messageID: msg.MessageID,
			text:      segment,
		})
	}

	rt.outputMessages = next
	return nil
}

func splitForEditMessages(text string, softLimit int, hardLimit int) []string {
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if hardLimit <= 0 {
		hardLimit = maxMessageRunes
	}
	if softLimit <= 0 || softLimit > hardLimit {
		softLimit = hardLimit
	}

	runes := []rune(text)
	var segments []string
	for len(runes) > 0 {
		limit := softLimit
		if len(runes) <= limit {
			segments = append(segments, string(runes))
			break
		}
		end := findSplitIndex(runes, limit)
		if end <= 0 || end > hardLimit {
			end = limit
		}
		segments = append(segments, string(runes[:end]))
		runes = runes[end:]
	}
	return segments
}

func findSplitIndex(runes []rune, limit int) int {
	if limit <= 0 || len(runes) <= limit {
		return len(runes)
	}
	for i := limit - 1; i >= 0; i-- {
		if runes[i] == '\n' {
			return i + 1
		}
	}
	for i := limit - 1; i >= 0; i-- {
		if runes[i] == ' ' || runes[i] == '\t' {
			return i + 1
		}
	}
	return limit
}

func (s *Service) sendBestEffort(groupID string, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if err := s.messenger.SendTextToChat(context.Background(), groupID, text); err != nil {
		s.logger.Error("send chat message failed", "group_id", groupID, "err", err)
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
