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
	maxMessageRunes            = 3000
	queueSize                  = 64
	recentMessageIDLimit       = 256
	defaultFlushIdleTicks      = 24
	defaultIdleConfirmTicks    = 3
	defaultWorkingAfter        = time.Second
	defaultChatActionEvery     = 4 * time.Second
	defaultBusyFlushAfter      = 5 * time.Second
	defaultOutputWatchdog      = 8 * time.Second
	defaultDetachedWatchdog    = 15 * time.Second
	defaultWatchdogDetachAfter = 30 * time.Second
	defaultEditRolloverAt      = 2800
	defaultEditableSyncEvery   = 5 * time.Second
	defaultDetachedSendEvery   = time.Second
	defaultDeliveryTimeout     = 20 * time.Second
	defaultSilentBusyGrace     = 20 * time.Minute
	workingStatusText          = "[working] Codex is processing."
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
	VisibleCWD            string
	SessionName           string
	LaunchCommand         string
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

type DeleteMessenger interface {
	DeleteMessageInChat(ctx context.Context, groupID string, messageID string) error
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
	ctx                   context.Context
	opts                  Options
	messenger             Messenger
	console               Console
	resources             ResourceFetcher
	logger                *slog.Logger
	pollEvery             time.Duration
	startWait             time.Duration
	history               int
	flushIdleTicks        int
	idleConfirmTicks      int
	workingAfter          time.Duration
	chatActionEvery       time.Duration
	busyFlushAfter        time.Duration
	outputWatchdogAfter   time.Duration
	detachedWatchdogAfter time.Duration
	watchdogDetachAfter   time.Duration
	editRolloverAt        int
	editableSyncEvery     time.Duration
	detachedSendEvery     time.Duration
	deliveryTimeout       time.Duration
	silentBusyGrace       time.Duration
	interruptForceAfter   time.Duration

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
	sessionReady         bool
	lastText             string
	baseText             string
	lastBusy             bool
	idleTicks            int
	outputBuffer         string
	outputBufferedAt     time.Time
	outputBufferedTicks  int
	outputText           string
	outputMessages       []trackedMessage
	statusMessage        trackedMessage
	detachedOutputs      []detachedOutput
	outputBackoffUntil   time.Time
	detachedBackoffUntil time.Time
	detachedRetryCount   int
	editBackoffUntil     time.Time
	editRateLimitCount   int
	lastEditableSyncAt   time.Time
	nextDetachedSendAt   time.Time
	deferBodyUntilIdle   bool
	forcePlainOutput     bool
	busySince            time.Time
	workingSent          bool
	workingBackoffUntil  time.Time
	lastActionAt         time.Time
	interruptSentAt      time.Time
	forceInterruptSent   bool
	runID                uint64
	nextRunID            uint64
	runCursorCommitted   map[uint64]int
}

type trackedMessage struct {
	messageID string
	text      string
}

type detachedOutput struct {
	runID      uint64
	cursor     int
	text       string
	enqueuedAt time.Time
}

func NewService(ctx context.Context, opts Options, messenger Messenger, console Console, resources ResourceFetcher, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.SessionName == "" {
		opts.SessionName = DefaultSessionName(opts.CWD)
	}
	if strings.TrimSpace(opts.VisibleCWD) == "" {
		opts.VisibleCWD = opts.CWD
	}
	return &Service{
		ctx:                   ctx,
		opts:                  opts,
		messenger:             messenger,
		console:               console,
		resources:             resources,
		logger:                logger,
		pollEvery:             500 * time.Millisecond,
		startWait:             4 * time.Second,
		history:               2000,
		flushIdleTicks:        defaultFlushIdleTicks,
		idleConfirmTicks:      defaultIdleConfirmTicks,
		workingAfter:          defaultWorkingAfter,
		chatActionEvery:       defaultChatActionEvery,
		busyFlushAfter:        defaultBusyFlushAfter,
		outputWatchdogAfter:   defaultOutputWatchdog,
		detachedWatchdogAfter: defaultDetachedWatchdog,
		watchdogDetachAfter:   defaultWatchdogDetachAfter,
		editRolloverAt:        defaultEditRolloverAt,
		editableSyncEvery:     defaultEditableSyncEvery,
		detachedSendEvery:     defaultDetachedSendEvery,
		deliveryTimeout:       defaultDeliveryTimeout,
		silentBusyGrace:       defaultSilentBusyGrace,
		interruptForceAfter:   time.Second,
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
		s.sendBestEffort(s.opts.GroupID, "This chat queue is full. Please try again shortly.")
		return nil
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
			if !wasReady && s.opts.InterruptOnNewMessage {
				s.keepLatestPending(rt)
			}
			s.flushDetachedOutputs(rt)
			if s.opts.InterruptOnNewMessage && rt.runInFlight() && len(rt.pending) > 0 {
				s.requestInterrupt(rt)
			}
			if !rt.runInFlight() && len(rt.pending) > 0 {
				s.dispatchNext(rt)
			}
		case <-ticker.C:
			s.flushDetachedOutputs(rt)
			if !rt.sessionReady {
				if !rt.hasRecoverableOutputState() && len(rt.pending) == 0 {
					continue
				}
				if err := s.ensureSession(rt); err != nil {
					s.logger.Warn("retry ensure session failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
					continue
				}
				if s.opts.InterruptOnNewMessage {
					s.keepLatestPending(rt)
				}
				if !rt.runInFlight() && rt.hasBufferedOutput() {
					s.flushOutputBuffer(rt)
				}
				if !rt.runInFlight() && len(rt.pending) > 0 {
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
		rt.statusMessage.messageID != "" ||
		len(rt.detachedOutputs) > 0 ||
		rt.promptEchoPending ||
		!rt.outputBackoffUntil.IsZero() ||
		!rt.detachedBackoffUntil.IsZero() ||
		!rt.editBackoffUntil.IsZero()

	_, err := s.console.EnsureSession(s.ctx, tmuxctl.SessionSpec{
		SessionName:                 rt.session,
		CWD:                         rt.opts.CWD,
		GroupID:                     rt.opts.GroupID,
		LaunchCommand:               rt.opts.LaunchCommand,
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

	captured := tmuxctl.NormalizeSnapshot(snapshot)
	if !recoveringOutput {
		rt.lastText = captured
		rt.baseText = ""
	}
	rt.lastBusy = tmuxctl.IsBusy(snapshot)
	if recoveringOutput && rt.active != nil {
		currText := strings.Trim(tmuxctl.SliceAfter(rt.baseText, captured), "\n")
		if !rt.lastBusy && !s.shouldHoldBusyForSilentRun(rt, currText, time.Now()) {
			rt.active = nil
			rt.busySince = time.Time{}
			rt.workingSent = false
			rt.workingBackoffUntil = time.Time{}
		}
	}
	rt.idleTicks = 0
	if !recoveringOutput {
		rt.clearOutputBuffer()
		rt.outputText = ""
		rt.outputMessages = nil
		rt.statusMessage = trackedMessage{}
		rt.promptEchoTail = ""
		rt.promptEchoPending = false
		rt.outputBackoffUntil = time.Time{}
		rt.detachedBackoffUntil = time.Time{}
		rt.detachedRetryCount = 0
		rt.editBackoffUntil = time.Time{}
		rt.editRateLimitCount = 0
		rt.deferBodyUntilIdle = false
		rt.forcePlainOutput = false
		rt.workingBackoffUntil = time.Time{}
	}
	rt.outputArmed = recoveringOutput
	if rt.active == nil {
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.workingBackoffUntil = time.Time{}
	} else if rt.busySince.IsZero() && rt.lastBusy {
		rt.busySince = time.Now()
	}
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
	if !s.finalizeOutputBeforeDispatch(rt) {
		return
	}
	s.flushOutputBufferForced(rt)
	if rt.hasBufferedOutput() {
		s.detachBufferedOutput(rt)
	}
	if len(rt.pending) == 0 {
		return
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
		rt.clearOutputBuffer()
		rt.outputText = ""
		rt.outputMessages = nil
		rt.statusMessage = trackedMessage{}
		rt.detachedOutputs = nil
		rt.outputBackoffUntil = time.Time{}
		rt.detachedBackoffUntil = time.Time{}
		rt.detachedRetryCount = 0
		rt.editBackoffUntil = time.Time{}
		rt.editRateLimitCount = 0
		rt.deferBodyUntilIdle = false
		rt.forcePlainOutput = false
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.lastActionAt = time.Time{}
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
		rt.active = nil
		return
	}
}

func (s *Service) finalizeOutputBeforeDispatch(rt *groupRuntime) bool {
	if rt == nil || !rt.sessionReady || strings.TrimSpace(rt.session) == "" {
		return true
	}
	// Run boundary finalization whenever output is armed. This catches late tail
	// output that arrived after the last poll but before the next dispatch.
	// Keep a fallback for explicit buffered/synced state even if outputArmed is false.
	if !rt.outputArmed && !rt.hasBufferedOutput() && strings.TrimSpace(rt.outputText) == "" && len(rt.outputMessages) == 0 {
		return true
	}
	snapshot, err := s.console.Capture(s.ctx, rt.session, s.history)
	if err != nil {
		s.logger.Warn("capture finalize output failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
		return false
	}
	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	rt.lastText = currFullText
	if tmuxctl.IsBusy(snapshot) {
		rt.lastBusy = true
		rt.idleTicks = 0
		return false
	}
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	currText = strings.Trim(currText, "\n")
	// If a run is still in-flight but boundary capture currently shows no tail,
	// defer dispatch and let poll confirm idle over multiple ticks.
	if strings.TrimSpace(currText) == "" && rt.active != nil {
		return false
	}
	if strings.TrimSpace(currText) == "" {
		return true
	}
	knownText := mergeBufferedOutput(rt.outputText, rt.outputBuffer)
	delta, reset := tmuxctl.DiffText(knownText, currText)
	if reset {
		s.resetBufferedOutput(rt, currText)
		return true
	}
	rt.appendOutputBuffer(delta, time.Now())
	return true
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
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.editBackoffUntil = time.Time{}
	rt.editRateLimitCount = 0
	rt.lastEditableSyncAt = time.Time{}
	rt.deferBodyUntilIdle = false
	rt.forcePlainOutput = false
	rt.busySince = time.Now()
	rt.workingSent = false
	rt.workingBackoffUntil = time.Time{}
	rt.lastActionAt = time.Time{}
	rt.interruptSentAt = time.Time{}
	rt.forceInterruptSent = false
	rt.active = req
	rt.nextRunID++
	rt.runID = rt.nextRunID
	rt.commitCursor(rt.runID, 0)
	rt.pruneCommittedCursors(64)
	s.logger.Info(
		"codex run started",
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"message_id", req.messageID,
		"cursor", rt.runCursor(rt.runID),
	)
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
		rt.idleTicks = 0
		if !rt.hasRecoverableOutputState() {
			rt.baseText = ""
			rt.lastText = ""
		}
		rt.lastActionAt = time.Time{}
		if !rt.runInFlight() {
			rt.lastBusy = false
			rt.busySince = time.Time{}
			rt.workingSent = false
			rt.active = nil
		}
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
	busyRaw := tmuxctl.IsBusy(snapshot)
	if !busyRaw && s.shouldHoldBusyForSilentRun(rt, currText, now) {
		busyRaw = true
	}
	if busyRaw {
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
			rt.workingSent = s.sendWorkingStatus(rt)
		}
	} else {
		rt.idleTicks++
	}
	idleConfirmTicks := s.idleConfirmTicks
	if idleConfirmTicks <= 0 {
		idleConfirmTicks = 1
	}
	busy := busyRaw || (rt.active != nil && rt.idleTicks < idleConfirmTicks)
	becameIdle := rt.lastBusy && !busy

	deferSnapshotCommit := false
	if rt.outputArmed {
		if s.shouldDeferBodyFlush(rt, now) {
			// During editable backoff, keep reconciling against the last
			// successfully published body so a later plain-send fallback can
			// forward only the unsent tail instead of replaying the whole body.
			s.reconcileDeferredOutput(rt, currText, now)
		} else {
			if reset {
				if !s.resetBufferedOutput(rt, currText) {
					// Keep previous snapshot baseline so deferred reset can be
					// reconciled on the next poll instead of silently advancing.
					deferSnapshotCommit = true
				}
			} else {
				rt.appendOutputBuffer(delta, now)
			}
		}
		if rt.hasBufferedOutput() {
			rt.outputBufferedTicks++
		} else {
			rt.outputBufferedTicks = 0
		}
		if shouldFlushOnSize(rt, s.editRolloverAt, maxMessageRunes) {
			s.flushOutputBuffer(rt)
		} else if becameIdle && strings.TrimSpace(rt.outputBuffer) != "" && s.editableMessenger() != nil {
			s.flushOutputBuffer(rt)
		} else if shouldFlush(rt.outputBuffer, rt.outputBufferedAt, rt.outputBufferedTicks, s.flushIdleTicks, s.busyFlushAfter, now) {
			s.flushOutputBuffer(rt)
		}
		s.applyOutputWatchdog(rt, now)
	}

	if !deferSnapshotCommit {
		rt.lastText = currFullText
	}
	busyChanged := rt.lastBusy != busy
	rt.lastBusy = busy
	if !busy {
		if !rt.hasPendingOutputDelivery() {
			s.clearWorkingStatus(rt)
			rt.workingSent = false
		}
		rt.busySince = time.Time{}
		rt.workingBackoffUntil = time.Time{}
		rt.lastActionAt = time.Time{}
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
		rt.active = nil
	}
	if busyChanged {
		s.logger.Info(
			"codex run state changed",
			"group_id", rt.opts.GroupID,
			"run_id", rt.runID,
			"busy", busy,
			"idle_ticks", rt.idleTicks,
			"cursor", rt.runCursor(rt.runID),
			"buffer_len", utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
			"detached_len", len(rt.detachedOutputs),
		)
	}

	if !busy && len(rt.pending) > 0 {
		s.dispatchNext(rt)
	}
}

func (s *Service) applyOutputWatchdog(rt *groupRuntime, now time.Time) {
	if rt == nil || !rt.hasBufferedOutput() || s.outputWatchdogAfter <= 0 || rt.outputBufferedAt.IsZero() {
		return
	}
	age := now.Sub(rt.outputBufferedAt)
	if age < s.outputWatchdogAfter {
		return
	}
	s.logger.Warn(
		"output watchdog forcing drain",
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"cursor", rt.runCursor(rt.runID),
		"age", age.String(),
		"buffer_len", utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
		"detached_len", len(rt.detachedOutputs),
	)
	s.flushOutputBuffer(rt)
	editable := s.editableMessenger()
	if editable != nil {
		// Keep one body-delivery strategy for editable messengers. Watchdogs may
		// nudge retries, but should not convert an editable body into detached
		// plain messages while the run is still active or backing off.
		return
	}
	if rt.hasBufferedOutput() {
		s.detachBufferedOutput(rt)
	}
	s.flushDetachedOutputs(rt)
}

func shouldFlush(buffer string, bufferedAt time.Time, bufferedTicks int, flushIdleTicks int, flushAfter time.Duration, now time.Time) bool {
	if strings.TrimSpace(buffer) == "" {
		return false
	}
	if flushIdleTicks <= 0 {
		flushIdleTicks = 1
	}
	if bufferedTicks >= flushIdleTicks {
		return true
	}
	if bufferedAt.IsZero() {
		return false
	}
	if flushAfter <= 0 {
		flushAfter = 500 * time.Millisecond
	}
	return now.Sub(bufferedAt) >= flushAfter
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

func (s *Service) shouldHoldBusyForSilentRun(rt *groupRuntime, currText string, now time.Time) bool {
	if rt == nil || rt.active == nil || s.silentBusyGrace <= 0 || rt.busySince.IsZero() {
		return false
	}
	if !rt.interruptSentAt.IsZero() {
		return false
	}
	if now.Sub(rt.busySince) >= s.silentBusyGrace {
		return false
	}
	return !hasVisibleRunOutput(rt, currText)
}

func hasVisibleRunOutput(rt *groupRuntime, currText string) bool {
	if rt == nil {
		return false
	}
	if strings.TrimSpace(currText) != "" ||
		rt.hasBufferedOutput() ||
		strings.TrimSpace(rt.outputText) != "" ||
		len(rt.detachedOutputs) > 0 {
		return true
	}
	for _, msg := range rt.outputMessages {
		if strings.TrimSpace(msg.text) != "" && msg.text != workingStatusText {
			return true
		}
	}
	return false
}

func (s *Service) shouldDeferBodyFlush(rt *groupRuntime, now time.Time) bool {
	if rt == nil || rt.forcePlainOutput || !rt.deferBodyUntilIdle {
		return false
	}
	if !rt.editBackoffUntil.IsZero() && rt.editBackoffUntil.After(now) {
		return true
	}
	if !rt.outputBackoffUntil.IsZero() && rt.outputBackoffUntil.After(now) {
		return true
	}
	return false
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

func (s *Service) resetBufferedOutput(rt *groupRuntime, currText string) bool {
	if rt == nil {
		return true
	}
	currText = strings.Trim(currText, "\n")
	if strings.TrimSpace(currText) == "" {
		// While a run is still in-flight, an empty capture can be transient
		// (pane refresh/window jitter). Keep baseline and retry next poll
		// instead of resetting and replaying old body as "new" output.
		if rt.active != nil {
			return false
		}
		rt.outputText = ""
		if !rt.hasBufferedOutput() {
			rt.clearOutputBuffer()
		}
		return true
	}

	// When nothing has been synced yet, prefer reconciling against the unsent
	// buffer directly so reset handling can distinguish "revised snapshot"
	// from "truncated window" without duplicating content.
	if strings.TrimSpace(rt.outputText) == "" && strings.TrimSpace(rt.outputBuffer) != "" {
		deltaBuf, resetBuf := tmuxctl.DiffText(rt.outputBuffer, currText)
		if resetBuf {
			// With no committed baseline yet, a reset means pane content was
			// rewritten. Keep only the latest snapshot to avoid replaying
			// transient body fragments that Codex has already replaced.
			rt.replaceOutputBuffer(currText, time.Now())
			return true
		}
		if strings.TrimSpace(deltaBuf) == "" {
			return true
		}
		rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, deltaBuf), time.Now())
		return true
	}

	delta, reset := tmuxctl.DiffText(rt.outputText, currText)
	if reset {
		// If the pane snapshot temporarily shrinks while the run is still
		// active, keep the already-synced baseline and wait for a stable
		// snapshot instead of rewriting to a shorter body.
		if rt.active != nil && utf8.RuneCountInString(currText) < utf8.RuneCountInString(rt.outputText) {
			return false
		}
		rt.outputText = ""
		// Keep any already-buffered unsent body and merge the reset snapshot
		// so transient pane resets do not drop tail output.
		rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, currText), time.Now())
		return true
	}
	if strings.TrimSpace(delta) == "" {
		return true
	}
	rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, delta), time.Now())
	return true
}

func (s *Service) enqueuePending(rt *groupRuntime, msg IncomingMessage) {
	if !rt.sessionReady {
		if s.opts.InterruptOnNewMessage {
			rt.pending = []IncomingMessage{msg}
		} else {
			rt.pending = append(rt.pending, msg)
		}
		return
	}
	if s.opts.InterruptOnNewMessage && rt.runInFlight() {
		rt.pending = []IncomingMessage{msg}
		return
	}
	rt.pending = append(rt.pending, msg)
}

func (s *Service) requestInterrupt(rt *groupRuntime) {
	if !rt.sessionReady || !rt.runInFlight() || !rt.interruptSentAt.IsZero() {
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

func (rt *groupRuntime) runCursor(runID uint64) int {
	if rt == nil || runID == 0 || rt.runCursorCommitted == nil {
		return 0
	}
	return rt.runCursorCommitted[runID]
}

func (rt *groupRuntime) nextCursor(runID uint64) int {
	return rt.runCursor(runID) + 1
}

func (rt *groupRuntime) commitCursor(runID uint64, cursor int) {
	if rt == nil || runID == 0 || cursor < 0 {
		return
	}
	if rt.runCursorCommitted == nil {
		rt.runCursorCommitted = make(map[uint64]int)
	}
	if cursor <= rt.runCursorCommitted[runID] {
		return
	}
	rt.runCursorCommitted[runID] = cursor
}

func (rt *groupRuntime) pruneCommittedCursors(keep int) {
	if rt == nil || keep <= 0 || rt.runCursorCommitted == nil {
		return
	}
	if len(rt.runCursorCommitted) <= keep {
		return
	}
	minRunID := uint64(1)
	if rt.nextRunID > uint64(keep) {
		minRunID = rt.nextRunID - uint64(keep) + 1
	}
	for runID := range rt.runCursorCommitted {
		if runID < minRunID {
			delete(rt.runCursorCommitted, runID)
		}
	}
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
		parts = append(parts, fmt.Sprintf("User attached %s: %s. Inspect it.", attachmentDescriptor(attachment), s.visiblePath(path)))
	}
	return strings.Join(parts, "\n\n"), nil
}

func (s *Service) visiblePath(path string) string {
	path = strings.TrimSpace(path)
	cwd := strings.TrimSpace(s.opts.CWD)
	visibleCWD := strings.TrimSpace(firstNonEmpty(s.opts.VisibleCWD, s.opts.CWD))
	if path == "" || cwd == "" || visibleCWD == "" {
		return path
	}
	path = filepath.Clean(path)
	cwd = filepath.Clean(cwd)
	visibleCWD = filepath.Clean(visibleCWD)

	rel, err := filepath.Rel(cwd, path)
	if err != nil {
		return path
	}
	if rel == "." {
		return visibleCWD
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return path
	}
	return filepath.Clean(filepath.Join(visibleCWD, rel))
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

func (s *Service) deliveryContext() (context.Context, context.CancelFunc) {
	base := context.Background()
	if s != nil && s.ctx != nil {
		base = s.ctx
	}
	if s == nil || s.deliveryTimeout <= 0 {
		return context.WithCancel(base)
	}
	return context.WithTimeout(base, s.deliveryTimeout)
}

func (s *Service) sendTextToChat(groupID string, text string) error {
	if s == nil {
		return errors.New("service is nil")
	}
	if s.messenger == nil {
		return errors.New("messenger is nil")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return s.messenger.SendTextToChat(ctx, groupID, text)
}

func (s *Service) sendTextToChatWithID(editable EditableMessenger, groupID string, text string) (SentMessage, error) {
	if editable == nil {
		return SentMessage{}, errors.New("editable messenger is nil")
	}
	if strings.TrimSpace(text) == "" {
		return SentMessage{}, nil
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return editable.SendTextToChatWithID(ctx, groupID, text)
}

func (s *Service) editTextInChat(editable EditableMessenger, groupID string, messageID string, text string) error {
	if editable == nil {
		return errors.New("editable messenger is nil")
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return editable.EditTextInChat(ctx, groupID, messageID, text)
}

func (s *Service) deleteMessageInChat(deleter DeleteMessenger, groupID string, messageID string) error {
	if deleter == nil {
		return errors.New("delete messenger is nil")
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return deleter.DeleteMessageInChat(ctx, groupID, messageID)
}

func (s *Service) sendChatActionToChat(actioner ActionMessenger, groupID string, action string) error {
	if actioner == nil {
		return errors.New("action messenger is nil")
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return actioner.SendChatAction(ctx, groupID, action)
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
		if err := s.sendTextToChat(groupID, chunk); err != nil {
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
	s.clearWorkingStatus(rt)
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.editRateLimitCount = 0
	rt.lastEditableSyncAt = time.Time{}
	rt.forcePlainOutput = false
	rt.outputMessages = nil
	rt.workingBackoffUntil = time.Time{}
}

func (s *Service) detachBufferedOutput(rt *groupRuntime) {
	if rt == nil {
		return
	}
	runID := rt.runID
	if runID == 0 {
		runID = rt.nextRunID
	}
	candidate := mergeBufferedOutput(rt.outputText, rt.outputBuffer)
	unsent := candidate
	if strings.HasPrefix(candidate, rt.outputText) {
		unsent = candidate[len(rt.outputText):]
	}
	unsent = strings.Trim(unsent, "\n")
	if strings.TrimSpace(unsent) != "" {
		rt.enqueueDetachedOutput(runID, unsent)
	}
	rt.clearOutputBuffer()
	// Keep delivery baseline aligned with detached handoff.
	rt.outputText = candidate
	rt.outputMessages = nil
	rt.editBackoffUntil = time.Time{}
	rt.detachedRetryCount = 0
	rt.lastEditableSyncAt = time.Time{}
}

func (s *Service) sendWorkingStatus(rt *groupRuntime) bool {
	if rt == nil {
		return false
	}
	now := time.Now()
	if rt.outputBackoffActive(now) || rt.workingBackoffActive(now) {
		return false
	}
	editable := s.editableMessenger()
	if editable == nil {
		return false
	}
	if rt.statusMessage.messageID != "" {
		return true
	}
	msg, err := s.sendTextToChatWithID(editable, rt.opts.GroupID, workingStatusText)
	if err != nil {
		s.applyAuxBackoff(rt, err, 2*time.Second)
		s.logger.Warn("send working message failed", "group_id", rt.opts.GroupID, "err", err)
		return false
	}
	rt.statusMessage = trackedMessage{
		messageID: msg.MessageID,
		text:      workingStatusText,
	}
	rt.workingBackoffUntil = time.Time{}
	return true
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
	if rt.outputBackoffActive(now) {
		return
	}
	if !rt.lastActionAt.IsZero() && now.Sub(rt.lastActionAt) < s.chatActionEvery {
		return
	}
	rt.lastActionAt = now
	if err := s.sendChatActionToChat(actioner, rt.opts.GroupID, "typing"); err != nil {
		if retryAfter := retryAfterFromRateLimitError(err); retryAfter > 0 {
			rt.applyOutputBackoff(retryAfter)
		}
		s.logger.Warn("send chat action failed", "group_id", rt.opts.GroupID, "err", err)
		return
	}
}

func (s *Service) flushOutputBuffer(rt *groupRuntime) {
	s.flushOutputBufferMode(rt, false)
}

func (s *Service) flushOutputBufferForced(rt *groupRuntime) {
	s.flushOutputBufferMode(rt, true)
}

func (s *Service) flushOutputBufferMode(rt *groupRuntime, forceEditable bool) {
	if rt == nil {
		return
	}
	now := time.Now()
	editable := s.editableMessenger()
	if rt.forcePlainOutput {
		editable = nil
	}
	if editable != nil && s.shouldDeferBodyFlush(rt, now) {
		return
	}
	if rt.outputBackoffActive(now) {
		return
	}
	if editable != nil && !rt.editBackoffUntil.IsZero() {
		if rt.editBackoffUntil.After(now) {
			return
		}
		rt.editBackoffUntil = time.Time{}
	}
	if editable != nil && len(rt.detachedOutputs) == 0 && !s.canSyncEditableOutput(rt, now, forceEditable) {
		return
	}
	raw := rt.outputBuffer
	bufferedAt := rt.outputBufferedAt
	rt.clearOutputBuffer()
	if strings.TrimSpace(raw) == "" {
		return
	}
	runID := rt.runID
	if runID == 0 {
		runID = rt.nextRunID
	}
	if len(rt.detachedOutputs) > 0 {
		candidateText := mergeBufferedOutput(rt.outputText, raw)
		text := strings.Trim(raw, "\n")
		if strings.TrimSpace(text) == "" {
			return
		}
		rt.enqueueDetachedOutput(runID, text)
		// Advance observed delivery baseline once accepted into detached queue.
		// This avoids replaying the same chunk on pane reset jitter.
		rt.outputText = candidateText
		s.logOutputStateDebug(rt, "flush redirected to detached queue while backlog exists")
		return
	}
	if editable == nil {
		candidateText := mergeBufferedOutput(rt.outputText, raw)
		text := strings.Trim(raw, "\n")
		if strings.TrimSpace(text) == "" {
			rt.outputText = candidateText
			return
		}
		rt.enqueueDetachedOutput(runID, text)
		rt.outputText = candidateText
		rt.editRateLimitCount = 0
		s.logOutputStateDebug(rt, "flush plain output redirected to detached queue")
		s.flushDetachedOutputs(rt)
		return
	}
	candidateText := mergeBufferedOutput(rt.outputText, raw)
	desiredText := strings.Trim(candidateText, "\n")
	if strings.TrimSpace(desiredText) == "" {
		rt.outputText = candidateText
		return
	}
	if err := s.syncEditableOutput(rt, editable, desiredText, !forceEditable); err != nil {
		if isMessageNotModifiedError(err) {
			rt.outputText = candidateText
			return
		}
		if shouldResetEditableThread(err) {
			rt.outputMessages = nil
			if retryErr := s.syncEditableOutput(rt, editable, desiredText, false); retryErr == nil {
				rt.outputText = candidateText
				rt.lastEditableSyncAt = now
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
			rt.editRateLimitCount++
			rt.editBackoffUntil = time.Now().Add(retryAfter)
			rt.applyOutputBackoff(retryAfter)
			rt.deferBodyUntilIdle = true
			s.logger.Warn(
				"sync editable output rate-limited",
				"group_id",
				rt.opts.GroupID,
				"run_id",
				runID,
				"cursor",
				rt.runCursor(runID),
				"retry_after",
				retryAfter.String(),
				"retry_at",
				rt.outputBackoffUntil,
				"err",
				err,
			)
			return
		}
		s.logger.Error(
			"sync editable output failed",
			"group_id",
			rt.opts.GroupID,
			"run_id",
			runID,
			"cursor",
			rt.runCursor(runID),
			"err",
			err,
		)
		return
	}
	rt.outputText = candidateText
	rt.editRateLimitCount = 0
	rt.lastEditableSyncAt = now
	rt.deferBodyUntilIdle = false
	rt.forcePlainOutput = false
	s.logOutputStateDebug(rt, "flush editable output committed")
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
	if overlap := suffixPrefixOverlap(existing, delta); usableMergeOverlap(existing, delta, overlap) {
		return existing + delta[overlap:]
	}
	return existing + delta
}

func usableMergeOverlap(existing string, delta string, overlap int) bool {
	if overlap < 8 || overlap > len(delta) || overlap > len(existing) {
		return false
	}
	prevStart := len(existing) - overlap
	prevBoundary := prevStart == 0 || existing[prevStart-1] == '\n'
	currBoundary := overlap == len(delta) || delta[overlap] == '\n'
	return prevBoundary && currBoundary
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
	rt.outputBufferedTicks = 0
}

func (rt *groupRuntime) hasBufferedOutput() bool {
	if rt == nil {
		return false
	}
	return strings.TrimSpace(rt.outputBuffer) != ""
}

func (rt *groupRuntime) hasRecoverableOutputState() bool {
	if rt == nil {
		return false
	}
	return rt.outputArmed ||
		rt.hasBufferedOutput() ||
		strings.TrimSpace(rt.outputText) != "" ||
		len(rt.outputMessages) > 0 ||
		rt.statusMessage.messageID != "" ||
		len(rt.detachedOutputs) > 0 ||
		rt.promptEchoPending ||
		!rt.outputBackoffUntil.IsZero() ||
		!rt.detachedBackoffUntil.IsZero() ||
		!rt.editBackoffUntil.IsZero()
}

func (rt *groupRuntime) runInFlight() bool {
	if rt == nil {
		return false
	}
	return rt.lastBusy || rt.active != nil
}

func (rt *groupRuntime) outputBackoffActive(now time.Time) bool {
	if rt == nil || rt.outputBackoffUntil.IsZero() {
		return false
	}
	if rt.outputBackoffUntil.After(now) {
		return true
	}
	rt.outputBackoffUntil = time.Time{}
	return false
}

func (rt *groupRuntime) applyOutputBackoff(retryAfter time.Duration) {
	if rt == nil || retryAfter <= 0 {
		return
	}
	retryAt := time.Now().Add(retryAfter)
	if retryAt.After(rt.outputBackoffUntil) {
		rt.outputBackoffUntil = retryAt
	}
}

func (rt *groupRuntime) workingBackoffActive(now time.Time) bool {
	if rt == nil || rt.workingBackoffUntil.IsZero() {
		return false
	}
	if rt.workingBackoffUntil.After(now) {
		return true
	}
	rt.workingBackoffUntil = time.Time{}
	return false
}

func (rt *groupRuntime) applyWorkingBackoff(retryAfter time.Duration) {
	if rt == nil {
		return
	}
	if retryAfter <= 0 {
		retryAfter = 2 * time.Second
	}
	retryAt := time.Now().Add(retryAfter)
	if retryAt.After(rt.workingBackoffUntil) {
		rt.workingBackoffUntil = retryAt
	}
}

func (rt *groupRuntime) publishedOutputText() string {
	if rt == nil {
		return ""
	}
	published := strings.Trim(rt.outputText, "\n")
	editable := strings.Trim(joinTrackedMessages(rt.outputMessages), "\n")
	switch {
	case published == "":
		return editable
	case editable == "":
		return published
	case strings.HasPrefix(published, editable):
		return published
	case strings.HasPrefix(editable, published):
		return editable
	default:
		return published
	}
}

func (rt *groupRuntime) hasPendingOutputDelivery() bool {
	if rt == nil {
		return false
	}
	return rt.hasBufferedOutput() ||
		len(rt.detachedOutputs) > 0 ||
		!rt.outputBackoffUntil.IsZero() ||
		!rt.detachedBackoffUntil.IsZero() ||
		!rt.editBackoffUntil.IsZero() ||
		!rt.workingBackoffUntil.IsZero()
}

func joinTrackedMessages(messages []trackedMessage) string {
	var b strings.Builder
	for _, msg := range messages {
		text := strings.Trim(msg.text, "\n")
		if strings.TrimSpace(text) == "" {
			continue
		}
		b.WriteString(text)
	}
	return b.String()
}

func (s *Service) reconcileDeferredOutput(rt *groupRuntime, currText string, now time.Time) {
	if rt == nil {
		return
	}
	currTrimmed := strings.Trim(currText, "\n")
	if strings.TrimSpace(currTrimmed) == "" {
		return
	}
	published := rt.publishedOutputText()
	if published == "" {
		rt.replaceOutputBuffer(currTrimmed, now)
		return
	}
	if strings.HasPrefix(currTrimmed, published) {
		rt.outputText = published
		rt.replaceOutputBuffer(currTrimmed[len(published):], now)
		return
	}
	delta, reset := tmuxctl.DiffText(published, currTrimmed)
	if !reset {
		rt.outputText = published
		rt.replaceOutputBuffer(delta, now)
		return
	}
	if rt.active != nil && utf8.RuneCountInString(currTrimmed) < utf8.RuneCountInString(published) {
		return
	}
	rt.outputText = ""
	rt.replaceOutputBuffer(currTrimmed, now)
}

func (rt *groupRuntime) enqueueDetachedOutput(runID uint64, text string) {
	if rt == nil {
		return
	}
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	cursor := rt.runCursor(runID)
	for i := len(rt.detachedOutputs) - 1; i >= 0; i-- {
		item := rt.detachedOutputs[i]
		if item.runID == runID && item.cursor > cursor {
			cursor = item.cursor
		}
	}
	for _, chunk := range splitByRunes(text, maxMessageRunes) {
		if chunk == "" {
			continue
		}
		cursor++
		rt.detachedOutputs = append(rt.detachedOutputs, detachedOutput{
			runID:      runID,
			cursor:     cursor,
			text:       chunk,
			enqueuedAt: time.Now(),
		})
	}
}

func (s *Service) flushDetachedOutputs(rt *groupRuntime) {
	if rt == nil || len(rt.detachedOutputs) == 0 {
		if rt != nil && !rt.runInFlight() && !rt.hasPendingOutputDelivery() {
			s.clearWorkingStatus(rt)
			rt.workingSent = false
		}
		return
	}
	now := time.Now()
	if rt.outputBackoffActive(now) {
		return
	}
	if s.detachedWatchdogAfter > 0 && !rt.detachedOutputs[0].enqueuedAt.IsZero() && now.Sub(rt.detachedOutputs[0].enqueuedAt) >= s.detachedWatchdogAfter {
		s.logger.Warn(
			"detached output watchdog forcing retry",
			"group_id", rt.opts.GroupID,
			"run_id", rt.detachedOutputs[0].runID,
			"cursor", rt.detachedOutputs[0].cursor,
			"age", now.Sub(rt.detachedOutputs[0].enqueuedAt).String(),
		)
	}
	if !rt.detachedBackoffUntil.IsZero() {
		if rt.detachedBackoffUntil.After(now) {
			return
		}
		rt.detachedBackoffUntil = time.Time{}
	}
	if !rt.nextDetachedSendAt.IsZero() && rt.nextDetachedSendAt.After(now) {
		return
	}

	item := rt.detachedOutputs[0]
	if err := s.sendTextToChat(rt.opts.GroupID, item.text); err != nil {
		retryAfter := retryAfterFromRateLimitError(err)
		if retryAfter > 0 {
			rt.detachedBackoffUntil = time.Now().Add(retryAfter)
			rt.applyOutputBackoff(retryAfter)
			rt.detachedRetryCount = 0
			s.logger.Warn(
				"flush detached output rate-limited",
				"group_id",
				rt.opts.GroupID,
				"run_id",
				item.runID,
				"cursor",
				item.cursor,
				"retry_after",
				retryAfter.String(),
				"retry_at",
				rt.outputBackoffUntil,
				"err",
				err,
			)
			return
		}
		rt.detachedRetryCount++
		retryAfter = time.Duration(rt.detachedRetryCount) * time.Second
		if retryAfter > 30*time.Second {
			retryAfter = 30 * time.Second
		}
		rt.detachedBackoffUntil = time.Now().Add(retryAfter)
		s.logger.Warn(
			"flush detached output failed",
			"group_id",
			rt.opts.GroupID,
			"run_id",
			item.runID,
			"cursor",
			item.cursor,
			"attempt",
			rt.detachedRetryCount,
			"retry_after",
			retryAfter.String(),
			"retry_at",
			rt.detachedBackoffUntil,
			"queue_len",
			len(rt.detachedOutputs),
			"err",
			err,
		)
		return
	}
	rt.detachedOutputs = rt.detachedOutputs[1:]
	rt.commitCursor(item.runID, item.cursor)
	rt.detachedRetryCount = 0
	if s.detachedSendEvery > 0 {
		rt.nextDetachedSendAt = time.Now().Add(s.detachedSendEvery)
	} else {
		rt.nextDetachedSendAt = time.Time{}
	}
	s.logOutputStateDebug(rt, "detached output committed")
	if !rt.runInFlight() && !rt.hasPendingOutputDelivery() {
		s.clearWorkingStatus(rt)
		rt.workingSent = false
	}
}

func (s *Service) syncEditableOutput(rt *groupRuntime, editable EditableMessenger, desiredText string, freezeCompleted bool) error {
	segments := splitForEditMessages(desiredText, s.editRolloverAt, maxMessageRunes)
	if len(segments) == 0 {
		return nil
	}

	next := make([]trackedMessage, len(rt.outputMessages))
	copy(next, rt.outputMessages)
	if len(next) > len(segments) {
		stale := s.pruneStaleEditableMessages(rt, editable, next[len(segments):])
		next = append(next[:len(segments)], stale...)
	}
	existingCount := len(next)
	if existingCount > len(segments) {
		existingCount = len(segments)
	}

	for i, segment := range segments {
		if i < len(next) {
			if next[i].text == segment {
				continue
			}
			if freezeCompleted && i < existingCount-1 {
				continue
			}
			if err := s.editTextInChat(editable, rt.opts.GroupID, next[i].messageID, segment); err != nil {
				rt.outputMessages = next
				return err
			}
			next[i].text = segment
			continue
		}

		msg, err := s.sendTextToChatWithID(editable, rt.opts.GroupID, segment)
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
	for i, msg := range rt.outputMessages {
		if strings.TrimSpace(msg.messageID) == "" {
			continue
		}
		rt.commitCursor(rt.runID, i+1)
	}
	return nil
}

func (s *Service) canSyncEditableOutput(rt *groupRuntime, now time.Time, force bool) bool {
	if rt == nil || force || s.editableSyncEvery <= 0 {
		return true
	}
	if rt.lastEditableSyncAt.IsZero() {
		return true
	}
	return now.Sub(rt.lastEditableSyncAt) >= s.editableSyncEvery
}

func (s *Service) shouldForceWatchdogDetach(rt *groupRuntime, age time.Duration, now time.Time) bool {
	if rt == nil {
		return false
	}
	if s.watchdogDetachAfter > 0 && age >= s.watchdogDetachAfter {
		return true
	}
	// Extremely large buffered bodies should not stay stuck in editable mode
	// even if the run is still active; move them to the detached queue and let
	// the shared output backoff govern actual delivery.
	return utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")) >= maxMessageRunes*8 &&
		now.Sub(rt.outputBufferedAt) >= s.outputWatchdogAfter*2
}

func (s *Service) clearWorkingStatus(rt *groupRuntime) {
	if rt == nil || rt.statusMessage.messageID == "" {
		return
	}
	if rt.outputBackoffActive(time.Now()) || rt.workingBackoffActive(time.Now()) {
		return
	}
	editable := s.editableMessenger()
	if editable == nil {
		rt.statusMessage = trackedMessage{}
		return
	}
	messageID := strings.TrimSpace(rt.statusMessage.messageID)
	if messageID == "" {
		rt.statusMessage = trackedMessage{}
		return
	}
	if deleter, ok := editable.(DeleteMessenger); ok {
		if err := s.deleteMessageInChat(deleter, rt.opts.GroupID, messageID); err == nil {
			rt.workingBackoffUntil = time.Time{}
			rt.statusMessage = trackedMessage{}
			return
		} else {
			if s.applyAuxBackoff(rt, err, 2*time.Second) {
				return
			}
			s.logger.Warn("delete working message failed", "group_id", rt.opts.GroupID, "message_id", messageID, "err", err)
		}
	}
	if err := s.editTextInChat(editable, rt.opts.GroupID, messageID, "…"); err != nil {
		s.applyAuxBackoff(rt, err, 2*time.Second)
		s.logger.Warn("neutralize working message failed", "group_id", rt.opts.GroupID, "message_id", messageID, "err", err)
		return
	}
	rt.workingBackoffUntil = time.Time{}
	rt.statusMessage = trackedMessage{}
}

func (s *Service) pruneStaleEditableMessages(rt *groupRuntime, editable EditableMessenger, stale []trackedMessage) []trackedMessage {
	if rt == nil || editable == nil || len(stale) == 0 {
		return stale
	}
	if rt.outputBackoffActive(time.Now()) {
		out := make([]trackedMessage, len(stale))
		copy(out, stale)
		return out
	}
	deleter, _ := editable.(DeleteMessenger)
	var kept []trackedMessage
	for _, item := range stale {
		messageID := strings.TrimSpace(item.messageID)
		if messageID == "" {
			continue
		}
		if deleter != nil {
			if err := s.deleteMessageInChat(deleter, rt.opts.GroupID, messageID); err == nil {
				continue
			} else if s.applyAuxBackoff(rt, err, 2*time.Second) {
				kept = append(kept, item)
				continue
			}
		}
		// Fallback when delete is unavailable/denied: neutralize stale segments
		// so old long chunks do not remain visible as repeated bodies.
		if err := s.editTextInChat(editable, rt.opts.GroupID, messageID, "…"); err != nil {
			s.applyAuxBackoff(rt, err, 2*time.Second)
			s.logger.Warn(
				"prune stale editable message failed",
				"group_id", rt.opts.GroupID,
				"message_id", messageID,
				"err", err,
			)
			kept = append(kept, item)
		}
	}
	return kept
}

func (s *Service) applyAuxBackoff(rt *groupRuntime, err error, fallback time.Duration) bool {
	if rt == nil || err == nil {
		return false
	}
	if retryAfter := retryAfterFromRateLimitError(err); retryAfter > 0 {
		rt.applyOutputBackoff(retryAfter)
		rt.applyWorkingBackoff(retryAfter)
		return true
	}
	rt.applyWorkingBackoff(fallback)
	return false
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
	if err := s.sendTextToChat(groupID, text); err != nil {
		s.logger.Error("send chat message failed", "group_id", groupID, "err", err)
	}
}

func (s *Service) logOutputStateDebug(rt *groupRuntime, message string) {
	if rt == nil {
		return
	}
	s.logger.Debug(
		message,
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"busy", rt.lastBusy,
		"idle_ticks", rt.idleTicks,
		"cursor", rt.runCursor(rt.runID),
		"buffer_len", utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
		"detached_len", len(rt.detachedOutputs),
	)
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
