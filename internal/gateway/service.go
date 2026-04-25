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
	"github.com/magnaflowlabs/imcodex/internal/xutil"
)

const (
	maxMessageRunes           = 3000
	maxDetachedMessageRunes   = 4096
	queueSize                 = 64
	recentMessageIDLimit      = 256
	defaultPollEvery          = 100 * time.Millisecond
	defaultFlushIdleTicks     = 24
	defaultIdleConfirmTicks   = 3
	defaultWorkingAfter       = time.Second
	defaultChatActionEvery    = 4 * time.Second
	defaultBusyFlushAfter     = time.Second
	defaultOutputWatchdog     = 8 * time.Second
	defaultDetachedWatchdog   = 15 * time.Second
	defaultEditRolloverAt     = 2800
	defaultEditableSyncEvery  = time.Second
	defaultDetachedSendEvery  = time.Second
	detachedCatchUpSendEvery  = time.Second
	defaultDeliveryTimeout    = 20 * time.Second
	defaultSilentBusyGrace    = 20 * time.Minute
	defaultPromptConfirmWait  = 500 * time.Millisecond
	defaultPromptConfirmEvery = 50 * time.Millisecond
	outputWatchdogLogEvery    = 5 * time.Second
	outputTraceLogEvery       = 5 * time.Second
	maxOutputDeliveryRunes    = maxMessageRunes * 8
	maxDetachedQueueItems     = 8
	maxDetachedQueueRunes     = maxDetachedMessageRunes * maxDetachedQueueItems
	workingStatusText         = "[working] Codex is processing."
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
	ResetSession(ctx context.Context, spec tmuxctl.SessionSpec) (bool, error)
	SendText(ctx context.Context, session string, text string) error
	Capture(ctx context.Context, session string, history int) (string, error)
	Interrupt(ctx context.Context, session string) error
	ForceInterrupt(ctx context.Context, session string) error
}

type SessionChecker interface {
	SessionExists(ctx context.Context, session string) (bool, error)
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
	editRolloverAt        int
	editableSyncEvery     time.Duration
	detachedSendEvery     time.Duration
	deliveryTimeout       time.Duration
	silentBusyGrace       time.Duration
	interruptForceAfter   time.Duration
	promptConfirmWait     time.Duration
	promptConfirmEvery    time.Duration

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
	opts                   Options
	session                string
	queue                  chan IncomingMessage
	deliveryDone           chan deliveryCompletion
	pending                []IncomingMessage
	active                 *activeRequest
	outputArmed            bool
	promptEchoTail         string
	promptEchoPending      bool
	runBusySeen            bool
	runPromptObserved      bool
	preBusyMutedText       string
	preBusyMutedChanges    int
	preBusyMutedStable     int
	sessionReady           bool
	lastText               string
	baseText               string
	lastBusy               bool
	idleTicks              int
	outputBuffer           string
	outputBufferedAt       time.Time
	outputBufferedTicks    int
	lastOutputWatchdogAt   time.Time
	lastDetachedWatchdogAt time.Time
	outputTraceAtByKey     map[string]time.Time
	outputText             string
	outputMessages         []trackedMessage
	statusMessage          trackedMessage
	detachedOutputs        []detachedOutput
	detachedBaselineByRun  map[uint64]string
	outputBackoffUntil     time.Time
	detachedBackoffUntil   time.Time
	detachedRetryCount     int
	editBackoffUntil       time.Time
	editRateLimitCount     int
	outputDroppedRunID     uint64
	outputDropReason       string
	lastEditableSyncAt     time.Time
	nextDetachedSendAt     time.Time
	deferBodyUntilIdle     bool
	monitorExistingSession bool
	busySince              time.Time
	workingSent            bool
	workingBackoffUntil    time.Time
	lastActionAt           time.Time
	interruptSentAt        time.Time
	forceInterruptSent     bool
	deliveryInFlight       bool
	outputGeneration       uint64
	runID                  uint64
	nextRunID              uint64
	runCursorCommitted     map[uint64]int
}

type deliveryCompletion struct {
	apply func()
}

type deliveryResult struct {
	message  SentMessage
	messages []trackedMessage
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
		pollEvery:             defaultPollEvery,
		startWait:             4 * time.Second,
		history:               2000,
		flushIdleTicks:        defaultFlushIdleTicks,
		idleConfirmTicks:      defaultIdleConfirmTicks,
		workingAfter:          defaultWorkingAfter,
		chatActionEvery:       defaultChatActionEvery,
		busyFlushAfter:        defaultBusyFlushAfter,
		outputWatchdogAfter:   defaultOutputWatchdog,
		detachedWatchdogAfter: defaultDetachedWatchdog,
		editRolloverAt:        defaultEditRolloverAt,
		editableSyncEvery:     defaultEditableSyncEvery,
		detachedSendEvery:     defaultDetachedSendEvery,
		deliveryTimeout:       defaultDeliveryTimeout,
		silentBusyGrace:       defaultSilentBusyGrace,
		interruptForceAfter:   time.Second,
		promptConfirmWait:     defaultPromptConfirmWait,
		promptConfirmEvery:    defaultPromptConfirmEvery,
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

	if s.recentMessageSet == nil {
		s.recentMessageSet = make(map[string]struct{}, recentMessageIDLimit)
	}
	if _, exists := s.recentMessageSet[messageID]; exists {
		return true
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
	return s.ensureRuntimeWithMonitoring(false)
}

func (s *Service) startMonitoringExistingSession() *groupRuntime {
	return s.ensureRuntimeWithMonitoring(true)
}

func (s *Service) ensureRuntimeWithMonitoring(monitorExisting bool) *groupRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runtime != nil {
		return s.runtime
	}

	rt := &groupRuntime{
		opts:                   s.opts,
		session:                s.opts.SessionName,
		queue:                  make(chan IncomingMessage, queueSize),
		deliveryDone:           make(chan deliveryCompletion, 8),
		monitorExistingSession: monitorExisting,
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
		case completion := <-rt.deliveryDone:
			rt.deliveryInFlight = false
			if completion.apply != nil {
				completion.apply()
			}
			s.advanceAfterDelivery(rt)
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
				if !rt.monitorExistingSession && !rt.hasRecoverableOutputState() && len(rt.pending) == 0 {
					continue
				}
				if rt.monitorExistingSession && len(rt.pending) == 0 && !rt.hasRecoverableOutputState() {
					exists, err := s.sessionExists(rt.session)
					if err != nil {
						s.logger.Warn("check existing session failed", "group_id", rt.opts.GroupID, "session", rt.session, "err", err)
						continue
					}
					if !exists {
						rt.monitorExistingSession = false
						continue
					}
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

func (s *Service) sessionExists(session string) (bool, error) {
	checker, ok := s.console.(SessionChecker)
	if !ok {
		return false, nil
	}
	return checker.SessionExists(s.ctx, session)
}

func (s *Service) advanceAfterDelivery(rt *groupRuntime) {
	if rt == nil {
		return
	}
	s.flushDetachedOutputs(rt)
	if rt.hasBufferedOutput() {
		s.flushOutputBuffer(rt)
	}
	if !rt.runInFlight() && len(rt.pending) > 0 {
		s.dispatchNext(rt)
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
	monitorExistingOutput := rt.monitorExistingSession &&
		len(rt.pending) == 0 &&
		rt.active == nil &&
		!recoveringOutput

	_, err := s.console.EnsureSession(s.ctx, s.sessionSpec(rt))
	if err != nil {
		return err
	}

	captureHistory := s.history
	if monitorExistingOutput {
		captureHistory = tmuxctl.CaptureRecoveryHistory
	}
	snapshot, err := s.console.Capture(s.ctx, rt.session, captureHistory)
	if err != nil {
		return err
	}

	captured := tmuxctl.NormalizeSnapshot(snapshot)
	busy := tmuxctl.IsBusy(snapshot)
	if !recoveringOutput {
		rt.lastText = captured
		rt.baseText = ""
	}
	if monitorExistingOutput {
		rt.baseText = captured
	}
	rt.lastBusy = busy
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
		rt.detachedOutputs = nil
		rt.detachedBaselineByRun = nil
		rt.promptEchoTail = ""
		rt.promptEchoPending = false
		rt.runBusySeen = false
		rt.runPromptObserved = false
		rt.clearPreBusyMutedState()
		rt.outputBackoffUntil = time.Time{}
		rt.detachedBackoffUntil = time.Time{}
		rt.detachedRetryCount = 0
		rt.editBackoffUntil = time.Time{}
		rt.editRateLimitCount = 0
		rt.clearOutputDropState()
		rt.deferBodyUntilIdle = false
		rt.workingBackoffUntil = time.Time{}
	}
	rt.outputArmed = recoveringOutput || monitorExistingOutput
	rt.monitorExistingSession = false
	if rt.active == nil {
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.workingBackoffUntil = time.Time{}
		rt.runBusySeen = false
		rt.runPromptObserved = false
		rt.clearPreBusyMutedState()
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
	for len(rt.pending) > 0 {
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
			if rt.lastBusy {
				return
			}
			continue
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
			rt.runBusySeen = false
			rt.runPromptObserved = false
			rt.clearPreBusyMutedState()
			rt.clearOutputBuffer()
			rt.outputText = ""
			rt.outputMessages = nil
			rt.statusMessage = trackedMessage{}
			rt.detachedOutputs = nil
			rt.detachedBaselineByRun = nil
			rt.outputBackoffUntil = time.Time{}
			rt.detachedBackoffUntil = time.Time{}
			rt.detachedRetryCount = 0
			rt.editBackoffUntil = time.Time{}
			rt.editRateLimitCount = 0
			rt.clearOutputDropState()
			rt.deferBodyUntilIdle = false
			rt.busySince = time.Time{}
			rt.workingSent = false
			rt.lastActionAt = time.Time{}
			rt.interruptSentAt = time.Time{}
			rt.forceInterruptSent = false
			rt.active = nil
			return
		}
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
	if rt.isDroppingCurrentRunOutput() {
		s.dropOutputForRun(rt, rt.currentRunID(), "finalize skipped dropped run output", time.Now())
		return true
	}
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
	return s.dispatchPreparedAttempt(rt, req, false)
}

func (s *Service) dispatchPreparedAttempt(rt *groupRuntime, req *activeRequest, retried bool) error {
	if req == nil {
		return errors.New("active request is nil")
	}
	baseline := s.refreshDispatchBaseline(rt)
	s.prepareOutputForDispatch(rt)
	if err := s.console.SendText(s.ctx, rt.session, req.input); err != nil {
		return err
	}
	if strings.TrimSpace(baseline) != "" {
		accepted, foreignPrompt, err := s.confirmPromptAccepted(rt, req.input)
		if err != nil {
			return err
		}
		if !accepted && foreignPrompt != "" {
			if retried {
				return fmt.Errorf("codex session kept previous prompt %q after reset", foreignPrompt)
			}
			s.logger.Warn(
				"codex prompt not accepted; resetting session",
				"group_id", rt.opts.GroupID,
				"session", rt.session,
				"foreign_prompt", foreignPrompt,
			)
			if _, err := s.console.ResetSession(s.ctx, s.sessionSpec(rt)); err != nil {
				return fmt.Errorf("reset codex session: %w", err)
			}
			rt.sessionReady = true
			rt.baseText = ""
			rt.lastText = ""
			rt.lastBusy = false
			rt.idleTicks = 0
			rt.outputArmed = false
			rt.promptEchoTail = ""
			rt.promptEchoPending = false
			rt.runBusySeen = false
			rt.runPromptObserved = false
			rt.clearPreBusyMutedState()
			rt.clearOutputBuffer()
			rt.outputText = ""
			rt.outputMessages = nil
			rt.statusMessage = trackedMessage{}
			rt.detachedOutputs = nil
			rt.detachedBaselineByRun = nil
			rt.outputBackoffUntil = time.Time{}
			rt.detachedBackoffUntil = time.Time{}
			rt.detachedRetryCount = 0
			rt.editBackoffUntil = time.Time{}
			rt.editRateLimitCount = 0
			rt.clearOutputDropState()
			rt.deferBodyUntilIdle = false
			rt.busySince = time.Time{}
			rt.workingSent = false
			rt.workingBackoffUntil = time.Time{}
			rt.lastActionAt = time.Time{}
			rt.interruptSentAt = time.Time{}
			rt.forceInterruptSent = false
			return s.dispatchPreparedAttempt(rt, req, true)
		}
		if accepted {
			rt.runPromptObserved = true
		}
	}
	rt.baseText = baseline
	rt.lastBusy = true
	rt.idleTicks = 0
	rt.outputArmed = true
	rt.promptEchoTail = normalizePromptEchoTail(req.input)
	rt.promptEchoPending = rt.promptEchoTail != ""
	rt.runBusySeen = false
	rt.runPromptObserved = false
	rt.clearPreBusyMutedState()
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.editBackoffUntil = time.Time{}
	rt.editRateLimitCount = 0
	rt.clearOutputDropState()
	rt.lastEditableSyncAt = time.Time{}
	rt.deferBodyUntilIdle = false
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
	rt.pruneRunState(64)
	s.logger.Info(
		"codex run started",
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"message_id", req.messageID,
		"cursor", rt.runCursor(rt.runID),
	)
	return nil
}

func (s *Service) sessionSpec(rt *groupRuntime) tmuxctl.SessionSpec {
	if rt == nil {
		return tmuxctl.SessionSpec{}
	}
	return tmuxctl.SessionSpec{
		SessionName:                 rt.session,
		CWD:                         rt.opts.CWD,
		GroupID:                     rt.opts.GroupID,
		LaunchCommand:               rt.opts.LaunchCommand,
		StartupWait:                 s.startWait,
		AutoPressEnterOnTrustPrompt: false,
	}
}

func (s *Service) confirmPromptAccepted(rt *groupRuntime, input string) (bool, string, error) {
	if rt == nil {
		return false, "", errors.New("group runtime is nil")
	}
	firstLine := promptFirstLine(input)
	if firstLine == "" {
		return true, "", nil
	}
	wait := s.promptConfirmWait
	if wait <= 0 {
		return true, "", nil
	}
	every := s.promptConfirmEvery
	if every <= 0 {
		every = defaultPromptConfirmEvery
	}
	history := s.history
	if history <= 0 || history > 200 {
		history = 200
	}
	if every > wait {
		every = wait
	}
	deadline := time.Now().Add(wait)
	foreignPrompt := ""
	for {
		sleepFor := every
		if remaining := time.Until(deadline); sleepFor > remaining {
			sleepFor = remaining
		}
		if sleepFor > 0 {
			select {
			case <-s.ctx.Done():
				return false, "", s.ctx.Err()
			case <-time.After(sleepFor):
			}
		}
		snapshot, err := s.console.Capture(s.ctx, rt.session, history)
		if err != nil {
			return false, "", fmt.Errorf("confirm prompt acceptance: %w", err)
		}
		if snapshotContainsPromptEcho(snapshot, input) {
			return true, "", nil
		}
		if candidate, foreign := latestPromptBody(snapshot); foreign && !promptBodiesMatch(candidate, firstLine) {
			foreignPrompt = candidate
		} else {
			return true, "", nil
		}
		if time.Now().After(deadline) || time.Now().Equal(deadline) {
			break
		}
	}
	if foreignPrompt != "" {
		return false, foreignPrompt, nil
	}
	return true, "", nil
}

func (s *Service) refreshDispatchBaseline(rt *groupRuntime) string {
	if rt == nil || strings.TrimSpace(rt.session) == "" {
		return ""
	}
	// Use CaptureFullHistory so the dispatch baseline covers the entire tmux
	// scrollback buffer. Subsequent poll() calls capture only s.history lines,
	// which will always be a suffix of this full baseline. SliceAfter can
	// therefore always find the overlap and return only genuinely new output,
	// even when the scrollback buffer exceeds s.history lines.
	snapshot, err := s.console.Capture(s.ctx, rt.session, tmuxctl.CaptureFullHistory)
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
			rt.runBusySeen = false
			rt.runPromptObserved = false
			rt.clearPreBusyMutedState()
			rt.active = nil
		}
		return
	}

	now := time.Now()
	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	busyRaw := tmuxctl.IsBusy(snapshot)
	prevText := tmuxctl.SliceAfter(rt.baseText, rt.lastText)
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	snapshotPromptObserved := false
	prevPromptBlockObserved := false
	currPromptBlockObserved := false
	adoptAnchoredSnapshotBaseline := false
	if rt.active != nil {
		var consumed bool
		prevText, consumed = suppressPromptEchoBlock(prevText, rt.active.input, rt.promptEchoTail)
		if consumed {
			prevPromptBlockObserved = true
			rt.runPromptObserved = true
		}
		currText, consumed = suppressPromptEchoBlock(currText, rt.active.input, rt.promptEchoTail)
		if consumed {
			currPromptBlockObserved = true
			rt.runPromptObserved = true
			rt.promptEchoPending = false
		}
	}
	if rt.active != nil && snapshotContainsPromptEcho(snapshot, rt.active.input) {
		snapshotPromptObserved = true
		rt.runPromptObserved = true
		if strings.TrimSpace(prevText) == "" &&
			strings.TrimSpace(currText) != "" {
			visibleRunOutput := mergeBufferedOutput(rt.publishedOutputText(), rt.outputBuffer)
			if anchoredText, anchored := anchoredRunOutputDelta(snapshot, rt.active.input, rt.promptEchoTail, visibleRunOutput); anchored {
				currText = anchoredText
				currPromptBlockObserved = true
				rt.promptEchoPending = false
			}
		}
	}
	if rt.outputArmed &&
		rt.active != nil &&
		strings.TrimSpace(prevText) == "" &&
		(strings.TrimSpace(currText) != "" || hasVisibleRunOutput(rt, "")) &&
		(currPromptBlockObserved || snapshotPromptObserved) {
		adoptAnchoredSnapshotBaseline = true
	}
	if rt.promptEchoPending {
		prevText, _ = suppressPromptEchoPrefix(prevText, rt.promptEchoTail)
		var consumed bool
		currText, consumed = suppressPromptEchoPrefix(currText, rt.promptEchoTail)
		if consumed {
			rt.runPromptObserved = true
		}
		if consumed || strings.TrimSpace(currText) != "" {
			rt.promptEchoPending = false
		}
	}
	unanchoredInitialBusyOutput := rt.outputArmed &&
		rt.active != nil &&
		busyRaw &&
		strings.TrimSpace(rt.baseText) != "" &&
		!rt.runBusySeen &&
		!rt.runPromptObserved &&
		strings.TrimSpace(prevText) == "" &&
		strings.TrimSpace(currText) != "" &&
		!hasVisibleRunOutput(rt, "")
	suspiciousNearBaselineReplay := rt.outputArmed &&
		rt.active != nil &&
		strings.TrimSpace(prevText) == "" &&
		strings.TrimSpace(currText) != "" &&
		!hasVisibleRunOutput(rt, "") &&
		suspiciousInitialWindowReplay(rt, currFullText, currText)
	recoveredWindowDelta := ""
	if suspiciousNearBaselineReplay {
		recoveredWindowDelta = recoverWindowRewriteDelta(rt.lastText, currFullText)
	}
	if unanchoredInitialBusyOutput || (suspiciousNearBaselineReplay && strings.TrimSpace(recoveredWindowDelta) == "") {
		s.logInitialReplayDiagnostic(rt, snapshot, snapshotPromptObserved, prevPromptBlockObserved, currPromptBlockObserved, currFullText, prevText, currText)
		rt.baseText = currFullText
		rt.lastText = currFullText
		if suspiciousNearBaselineReplay {
			rt.runBusySeen = false
			busyRaw = false
		}
		prevText = ""
		s.logger.Warn(
			"dropping unanchored initial busy output",
			"group_id", rt.opts.GroupID,
			"run_id", rt.runID,
			"curr_len", utf8.RuneCountInString(strings.Trim(currText, "\n")),
		)
		currText = ""
	}
	if suspiciousNearBaselineReplay && strings.TrimSpace(recoveredWindowDelta) != "" {
		s.logInitialReplayDiagnostic(rt, snapshot, snapshotPromptObserved, prevPromptBlockObserved, currPromptBlockObserved, currFullText, prevText, currText)
		rt.baseText = currFullText
		prevText = ""
		currText = recoveredWindowDelta
	}
	idleConfirmTicks := s.idleConfirmTicks
	if idleConfirmTicks <= 0 {
		idleConfirmTicks = 1
	}
	delta, reset := tmuxctl.DiffText(prevText, currText)
	if rt.outputArmed && !reset && strings.TrimSpace(delta) == "" {
		knownOutput := mergeBufferedOutput(rt.publishedOutputText(), rt.outputBuffer)
		if tail, ok := unsyncedVisibleTail(knownOutput, currText); ok {
			delta = tail
		}
	}
	if rt.outputArmed && rt.active != nil && !rt.runBusySeen {
		if busyRaw {
			rt.runBusySeen = true
			rt.clearPreBusyMutedState()
		} else if (reset || strings.TrimSpace(currText) != "") && strings.TrimSpace(rt.baseText) != "" {
			if rt.runPromptObserved {
				rt.notePreBusyMutedText(currText)
			} else {
				rt.clearPreBusyMutedState()
			}
			rt.baseText = currFullText
			rt.lastText = currFullText
			delta = ""
			reset = false
		} else if strings.TrimSpace(rt.preBusyMutedText) != "" {
			rt.preBusyMutedStable++
		}
	}
	if adoptAnchoredSnapshotBaseline {
		rt.baseText = currFullText
		rt.lastText = currFullText
	}
	heldSilentBusy := false
	if !busyRaw &&
		!(rt.active != nil && !rt.runBusySeen && strings.TrimSpace(rt.preBusyMutedText) != "") &&
		s.shouldHoldBusyForSilentRun(rt, currText, now) {
		busyRaw = true
		heldSilentBusy = true
	}
	if rt.outputArmed && (strings.TrimSpace(delta) != "" || reset || heldSilentBusy) {
		if rt.active != nil &&
			strings.TrimSpace(prevText) == "" &&
			strings.TrimSpace(currText) != "" &&
			utf8.RuneCountInString(strings.Trim(currText, "\n")) >= 4000 {
			s.logInitialReplayDiagnostic(rt, snapshot, snapshotPromptObserved, prevPromptBlockObserved, currPromptBlockObserved, currFullText, prevText, currText)
		}
		s.logOutputTrace(
			rt,
			"poll observed output",
			now,
			"prev_len",
			utf8.RuneCountInString(strings.Trim(prevText, "\n")),
			"curr_len",
			utf8.RuneCountInString(strings.Trim(currText, "\n")),
			"delta_len",
			utf8.RuneCountInString(strings.Trim(delta, "\n")),
			"reset",
			reset,
			"busy_raw",
			busyRaw,
			"held_silent_busy",
			heldSilentBusy,
		)
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
	preBusyHolding := rt.holdingPreBusyMutedOutput(idleConfirmTicks)
	busy := busyRaw || preBusyHolding || (rt.active != nil && rt.idleTicks < idleConfirmTicks)
	becameIdle := rt.lastBusy && !busy

	deferSnapshotCommit := false
	if rt.outputArmed {
		if rt.isDroppingCurrentRunOutput() {
			s.dropOutputForRun(rt, rt.currentRunID(), "poll skipped dropped run output", now)
		} else if s.shouldDeferBodyFlush(rt, now) {
			// During output backoff, discard the current run body instead of
			// accumulating a backlog that could later amplify Telegram sends.
			s.dropOutputForRun(rt, rt.currentRunID(), "poll observed output during editable backoff", now)
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
			if outputDeliveryTooLarge(mergeBufferedOutput(rt.outputText, rt.outputBuffer)) {
				s.dropOutputForRun(rt, rt.currentRunID(), "captured output exceeds safety cap", now)
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
		s.promoteStablePreBusyMutedOutput(rt, now)
		rt.runBusySeen = false
		rt.runPromptObserved = false
		rt.clearPreBusyMutedState()
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
	if rt.lastOutputWatchdogAt.IsZero() || now.Sub(rt.lastOutputWatchdogAt) >= outputWatchdogLogEvery {
		s.logger.Warn(
			"output watchdog forcing drain",
			"group_id", rt.opts.GroupID,
			"run_id", rt.runID,
			"cursor", rt.runCursor(rt.runID),
			"age", age.String(),
			"buffer_len", utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
			"detached_len", len(rt.detachedOutputs),
		)
		rt.lastOutputWatchdogAt = now
	}
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
	if rt.runBusySeen {
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

func (s *Service) promoteStablePreBusyMutedOutput(rt *groupRuntime, now time.Time) {
	if rt == nil {
		return
	}
	candidate := strings.Trim(rt.preBusyMutedText, "\n")
	if candidate == "" {
		return
	}
	if rt.preBusyMutedChanges == 0 {
		return
	}
	if rt.preBusyMutedChanges > 1 {
		s.logger.Warn("pre-busy muted output has multiple changes; promoting final text",
			"group_id", rt.opts.GroupID,
			"changes", rt.preBusyMutedChanges,
			"text_len", len(candidate),
		)
	}
	if rt.isDroppingCurrentRunOutput() ||
		rt.hasBufferedOutput() ||
		strings.TrimSpace(rt.outputText) != "" ||
		len(rt.outputMessages) > 0 ||
		len(rt.detachedOutputs) > 0 {
		return
	}
	rt.replaceOutputBuffer(candidate, now)
}

func (s *Service) shouldDeferBodyFlush(rt *groupRuntime, now time.Time) bool {
	if rt == nil || !rt.deferBodyUntilIdle {
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

func suppressPromptEchoBlock(text string, input string, echoTail string) (string, bool) {
	text = strings.TrimLeft(text, "\n")
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.TrimSpace(input)
	if text == "" || input == "" {
		return text, false
	}

	lines := strings.Split(text, "\n")
	firstLine := strings.TrimSpace(strings.Split(input, "\n")[0])
	if firstLine == "" {
		return text, false
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if !strings.HasPrefix(line, "›") && !strings.HasPrefix(line, ">") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "›"), ">"))
		if !promptBodiesMatch(body, firstLine) {
			continue
		}
		end := promptEchoBlockEnd(lines, i, echoTail)
		return strings.TrimLeft(strings.Join(lines[end+1:], "\n"), "\n"), true
	}
	return text, false
}

func normalizedSnapshotAfterPromptEcho(snapshot string, input string, echoTail string) (string, bool) {
	snapshot = strings.ReplaceAll(snapshot, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.TrimSpace(input)
	if snapshot == "" || input == "" {
		return "", false
	}

	lines := strings.Split(snapshot, "\n")
	firstLine := promptFirstLine(input)
	if firstLine == "" {
		return "", false
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if !strings.HasPrefix(line, "›") && !strings.HasPrefix(line, ">") {
			continue
		}
		body := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "›"), ">"))
		if !promptBodiesMatch(body, firstLine) {
			continue
		}
		end := promptEchoBlockEnd(lines, i, echoTail)
		return tmuxctl.NormalizeSnapshot(strings.Join(lines[end+1:], "\n")), true
	}
	return "", false
}

func anchoredRunOutputDelta(snapshot string, input string, echoTail string, visibleOutput string) (string, bool) {
	anchoredText, anchored := normalizedSnapshotAfterPromptEcho(snapshot, input, echoTail)
	if !anchored {
		return "", false
	}
	anchoredText = strings.Trim(anchoredText, "\n")
	visibleOutput = strings.Trim(visibleOutput, "\n")
	if strings.TrimSpace(visibleOutput) == "" {
		return anchoredText, true
	}
	if anchoredText == visibleOutput {
		return "", true
	}
	if strings.HasPrefix(anchoredText, visibleOutput) {
		return strings.TrimLeft(anchoredText[len(visibleOutput):], "\n"), true
	}
	delta, reset := tmuxctl.DiffText(visibleOutput, anchoredText)
	if !reset {
		return strings.Trim(delta, "\n"), true
	}
	// Treat unmatched anchored rewrites as baseline churn: keep the current
	// visible body and re-anchor the snapshot without forwarding a replay.
	return "", true
}

func promptEchoBlockEnd(lines []string, start int, echoTail string) int {
	if start < 0 || start >= len(lines) {
		return start
	}
	end := start
	tail := strings.ReplaceAll(echoTail, "\r\n", "\n")
	tail = strings.Trim(tail, "\n")
	if tail != "" {
		tailLines := normalizePromptEchoLines(strings.Split(tail, "\n"))
		idx := start + 1
		matched := true
		for _, line := range tailLines {
			if idx >= len(lines) || strings.TrimRight(lines[idx], " \t\r") != line {
				matched = false
				break
			}
			end = idx
			idx++
		}
		if matched {
			return end
		}
		end = start
	}

	for idx := start + 1; idx < len(lines); idx++ {
		trimmed := strings.TrimSpace(strings.TrimRight(lines[idx], " \t\r"))
		if trimmed == "" || looksLikePromptOrBusyChrome(trimmed) || looksLikeAssistantOutputLine(trimmed) {
			return end
		}
		end = idx
	}
	return end
}

func looksLikePromptOrBusyChrome(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "›") || strings.HasPrefix(line, ">") {
		return true
	}
	return strings.Contains(strings.ToLower(line), "esc to interrupt")
}

func looksLikeAssistantOutputLine(line string) bool {
	line = strings.TrimLeft(line, " \t")
	return strings.HasPrefix(line, "•") ||
		strings.HasPrefix(line, "◦") ||
		strings.HasPrefix(line, "●") ||
		strings.HasPrefix(line, "○")
}

func snapshotContainsPromptEcho(snapshot string, input string) bool {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.TrimSpace(input)
	if input == "" {
		return false
	}
	firstLine := strings.TrimSpace(strings.Split(input, "\n")[0])
	if firstLine == "" {
		return false
	}
	snapshot = strings.ReplaceAll(snapshot, "\r\n", "\n")
	lines := strings.Split(snapshot, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "›") {
			body := strings.TrimSpace(strings.TrimPrefix(line, "›"))
			if promptBodiesMatch(body, firstLine) {
				return true
			}
			continue
		}
		if strings.HasPrefix(line, ">") {
			body := strings.TrimSpace(strings.TrimPrefix(line, ">"))
			if promptBodiesMatch(body, firstLine) {
				return true
			}
			continue
		}
	}
	return false
}

func latestPromptBody(snapshot string) (string, bool) {
	snapshot = strings.ReplaceAll(snapshot, "\r\n", "\n")
	lines := strings.Split(snapshot, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimRight(lines[i], " \t\r"))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "›") {
			return strings.TrimSpace(strings.TrimPrefix(line, "›")), true
		}
		if strings.HasPrefix(line, ">") {
			return strings.TrimSpace(strings.TrimPrefix(line, ">")), true
		}
	}
	return "", false
}

func promptFirstLine(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(input, "\n")[0])
}

func promptBodiesMatch(body string, firstLine string) bool {
	body = normalizePromptBody(body)
	firstLine = normalizePromptBody(firstLine)
	if body == "" || firstLine == "" {
		return false
	}
	return body == firstLine
}

func normalizePromptBody(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return strings.Join(strings.Fields(text), " ")
}

func (s *Service) resetBufferedOutput(rt *groupRuntime, currText string) bool {
	if rt == nil {
		return true
	}
	now := time.Now()
	currText = strings.Trim(currText, "\n")
	if strings.TrimSpace(currText) == "" {
		// While a run is still in-flight, an empty capture can be transient
		// (pane refresh/window jitter). Keep baseline and retry next poll
		// instead of resetting and replaying old body as "new" output.
		if rt.active != nil {
			return false
		}
		rt.outputText = ""
		rt.outputMessages = nil
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
			bufferText := strings.Trim(rt.outputBuffer, "\n")
			if rt.active != nil {
				currLen := utf8.RuneCountInString(currText)
				bufferLen := utf8.RuneCountInString(bufferText)
				if currLen < bufferLen && strings.HasPrefix(bufferText, currText) {
					if len(rt.detachedOutputs) > 0 {
						rt.outputText = bufferText
						rt.clearOutputBuffer()
						return true
					}
					return false
				}
				if currLen <= bufferLen && len(rt.detachedOutputs) > 0 {
					rt.outputText = bufferText
					rt.clearOutputBuffer()
					return true
				}
				if rt.resetWindowAlreadyObserved(rt.outputBuffer, currText, now) {
					return true
				}
				if currLen <= bufferLen {
					rt.replaceOutputBuffer(currText, now)
					return true
				}
				if currLen == bufferLen && rt.shouldTreatEqualResetAsChurn(bufferLen) {
					return true
				}
			}
			if rt.resetWindowAlreadyObserved(rt.outputBuffer, currText, now) {
				return true
			}
			// With no committed baseline yet, a reset means pane content was
			// rewritten. Keep only the latest snapshot to avoid replaying
			// transient body fragments that Codex has already replaced.
			rt.replaceOutputBuffer(currText, now)
			return true
		}
		if strings.TrimSpace(deltaBuf) == "" {
			return true
		}
		rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, deltaBuf), now)
		return true
	}

	delta, reset := tmuxctl.DiffText(rt.outputText, currText)
	if reset {
		knownText := mergeBufferedOutput(rt.outputText, rt.outputBuffer)
		// If the pane snapshot temporarily shrinks while the run is still
		// active, keep the already-synced baseline and wait for a stable
		// snapshot instead of rewriting to a shorter body.
		if rt.active != nil {
			currLen := utf8.RuneCountInString(currText)
			trimmedKnownText := strings.Trim(knownText, "\n")
			knownLen := utf8.RuneCountInString(trimmedKnownText)
			if currLen < knownLen && strings.HasPrefix(trimmedKnownText, currText) {
				if len(rt.detachedOutputs) > 0 {
					rt.outputText = trimmedKnownText
					rt.clearOutputBuffer()
					return true
				}
				return false
			}
			if currLen <= knownLen && len(rt.detachedOutputs) > 0 {
				rt.outputText = trimmedKnownText
				rt.clearOutputBuffer()
				return true
			}
			if rt.resetWindowAlreadyObserved(knownText, currText, now) {
				return true
			}
			if currLen <= knownLen {
				rt.outputText = ""
				rt.replaceOutputBuffer(currText, now)
				return true
			}
			if currLen == knownLen && rt.shouldTreatEqualResetAsChurn(knownLen) {
				return true
			}
		}
		if rt.resetWindowAlreadyObserved(knownText, currText, now) {
			return true
		}
		rt.outputText = ""
		// Keep any already-buffered unsent body and merge the reset snapshot
		// so transient pane resets do not drop tail output.
		rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, currText), now)
		return true
	}
	if strings.TrimSpace(delta) == "" {
		return true
	}
	rt.replaceOutputBuffer(mergeBufferedOutput(rt.outputBuffer, delta), now)
	return true
}

func (rt *groupRuntime) resetWindowAlreadyObserved(knownText string, currText string, now time.Time) bool {
	if rt == nil {
		return false
	}
	knownText = strings.Trim(knownText, "\n")
	currText = strings.Trim(currText, "\n")
	if strings.TrimSpace(knownText) == "" || strings.TrimSpace(currText) == "" {
		return false
	}
	if currText == knownText || strings.Contains(knownText, currText) {
		return true
	}
	if idx := strings.Index(currText, knownText); idx >= 0 {
		rt.appendOutputBuffer(strings.TrimLeft(currText[idx+len(knownText):], "\n"), now)
		return true
	}
	if overlap := lineTailHeadOverlap(knownText, currText); overlap >= 3 {
		lines := strings.Split(currText, "\n")
		rt.appendOutputBuffer(strings.TrimLeft(strings.Join(lines[overlap:], "\n"), "\n"), now)
		return true
	}
	return false
}

func (rt *groupRuntime) shouldTreatEqualResetAsChurn(knownLen int) bool {
	if rt == nil || rt.active == nil {
		return false
	}
	return len(rt.detachedOutputs) > 0 || knownLen >= maxMessageRunes*8
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

func (rt *groupRuntime) pruneRunState(keep int) {
	if rt == nil {
		return
	}
	rt.pruneCommittedCursors(keep)
	rt.pruneDetachedBaselines(keep)
}

func (rt *groupRuntime) pruneDetachedBaselines(keep int) {
	if rt == nil || keep <= 0 || rt.detachedBaselineByRun == nil {
		return
	}
	if len(rt.detachedBaselineByRun) <= keep {
		return
	}
	minRunID := uint64(1)
	if rt.nextRunID > uint64(keep) {
		minRunID = rt.nextRunID - uint64(keep) + 1
	}
	for runID := range rt.detachedBaselineByRun {
		if runID < minRunID {
			delete(rt.detachedBaselineByRun, runID)
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
	visibleCWD := strings.TrimSpace(xutil.FirstNonEmpty(s.opts.VisibleCWD, s.opts.CWD))
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
	name := xutil.FirstNonEmpty(attachment.FileName, resource.FileName)
	name = sanitizeFileName(name)
	if name == "" {
		name = defaultAttachmentFileName(index, attachment, resource.ContentType)
	}

	base := fmt.Sprintf("%s-%s", time.Now().Format("20060102-150405"), tmuxctl.SanitizeName(messageID))
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

func sendTextToChatWithContext(ctx context.Context, messenger Messenger, groupID string, text string) error {
	if messenger == nil {
		return errors.New("messenger is nil")
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return messenger.SendTextToChat(ctx, groupID, text)
}

func sendTextToChatWithIDContext(ctx context.Context, editable EditableMessenger, groupID string, text string) (SentMessage, error) {
	if editable == nil {
		return SentMessage{}, errors.New("editable messenger is nil")
	}
	if strings.TrimSpace(text) == "" {
		return SentMessage{}, nil
	}
	return editable.SendTextToChatWithID(ctx, groupID, text)
}

func editTextInChatContext(ctx context.Context, editable EditableMessenger, groupID string, messageID string, text string) error {
	if editable == nil {
		return errors.New("editable messenger is nil")
	}
	return editable.EditTextInChat(ctx, groupID, messageID, text)
}

func deleteMessageInChatContext(ctx context.Context, deleter DeleteMessenger, groupID string, messageID string) error {
	if deleter == nil {
		return errors.New("delete messenger is nil")
	}
	return deleter.DeleteMessageInChat(ctx, groupID, messageID)
}

func sendChatActionToChatContext(ctx context.Context, actioner ActionMessenger, groupID string, action string) error {
	if actioner == nil {
		return errors.New("action messenger is nil")
	}
	return actioner.SendChatAction(ctx, groupID, action)
}

func (s *Service) sendTextToChat(groupID string, text string) error {
	if s == nil {
		return errors.New("service is nil")
	}
	if s.messenger == nil {
		return errors.New("messenger is nil")
	}
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return sendTextToChatWithContext(ctx, s.messenger, groupID, text)
}

func (s *Service) sendTextToChatWithID(editable EditableMessenger, groupID string, text string) (SentMessage, error) {
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return sendTextToChatWithIDContext(ctx, editable, groupID, text)
}

func (s *Service) editTextInChat(editable EditableMessenger, groupID string, messageID string, text string) error {
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return editTextInChatContext(ctx, editable, groupID, messageID, text)
}

func (s *Service) deleteMessageInChat(deleter DeleteMessenger, groupID string, messageID string) error {
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return deleteMessageInChatContext(ctx, deleter, groupID, messageID)
}

func (s *Service) sendChatActionToChat(actioner ActionMessenger, groupID string, action string) error {
	ctx, cancel := s.deliveryContext()
	defer cancel()
	return sendChatActionToChatContext(ctx, actioner, groupID, action)
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

func (s *Service) startDelivery(rt *groupRuntime, run func(context.Context) (deliveryResult, error), apply func(deliveryResult, error)) bool {
	if rt == nil || run == nil || apply == nil {
		return false
	}
	if rt.deliveryInFlight {
		return false
	}
	rt.deliveryInFlight = true
	done := rt.deliveryDone
	if done == nil {
		ctx, cancel := s.deliveryContext()
		defer cancel()
		result, err := run(ctx)
		rt.deliveryInFlight = false
		apply(result, err)
		return true
	}
	serviceCtx := context.Background()
	if s != nil && s.ctx != nil {
		serviceCtx = s.ctx
	}
	go func() {
		if s == nil {
			return
		}
		ctx, cancel := s.deliveryContext()
		defer cancel()
		result, err := run(ctx)
		completion := deliveryCompletion{
			apply: func() {
				apply(result, err)
			},
		}
		select {
		case done <- completion:
		case <-serviceCtx.Done():
		}
	}()
	return true
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
	rt.outputGeneration++
	s.clearWorkingStatus(rt)
	rt.clearOutputBuffer()
	rt.outputText = ""
	rt.editRateLimitCount = 0
	rt.clearOutputDropState()
	rt.lastEditableSyncAt = time.Time{}
	rt.outputMessages = nil
	rt.workingBackoffUntil = time.Time{}
}

func (s *Service) detachBufferedOutput(rt *groupRuntime) {
	if rt == nil {
		return
	}
	runID := rt.currentRunID()
	if rt.isDroppingCurrentRunOutput() {
		s.dropOutputForRun(rt, runID, "current run output already dropped", time.Now())
		return
	}
	baseline := mergeBufferedOutput(rt.outputText, rt.detachedBaseline(runID))
	candidate := mergeBufferedOutput(baseline, rt.outputBuffer)
	if outputDeliveryTooLarge(candidate) {
		s.dropOutputForRun(rt, runID, "detached handoff exceeds safety cap", time.Now())
		return
	}
	unsent := unsentOutputDelta(baseline, candidate)
	if strings.TrimSpace(unsent) != "" {
		if detachedQueueWouldExceedLimit(rt.detachedOutputs, unsent) {
			s.dropOutputForRun(rt, runID, "detached handoff queue exceeds safety cap", time.Now())
			return
		}
		rt.enqueueDetachedOutput(runID, unsent)
	}
	rt.clearOutputBuffer()
	// Keep delivery baseline aligned with detached handoff.
	rt.outputText = candidate
	rt.noteDetachedBaseline(runID, candidate)
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
	if rt.deliveryInFlight || rt.outputBackoffActive(now) || rt.workingBackoffActive(now) {
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
	if rt.deliveryInFlight || rt.outputBackoffActive(now) {
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
	runID := rt.currentRunID()
	if rt.isDroppingCurrentRunOutput() {
		s.dropOutputForRun(rt, runID, "current run output already dropped", now)
		return
	}
	if editable != nil && s.shouldDeferBodyFlush(rt, now) {
		s.dropOutputForRun(rt, runID, "editable output backoff active", now)
		return
	}
	if rt.outputBackoffActive(now) {
		s.dropOutputForRun(rt, runID, "shared output backoff active", now)
		return
	}
	if editable != nil && !rt.editBackoffUntil.IsZero() {
		if rt.editBackoffUntil.After(now) {
			s.dropOutputForRun(rt, runID, "editable retry window active", now)
			return
		}
		rt.editBackoffUntil = time.Time{}
	}
	if editable != nil && len(rt.detachedOutputs) == 0 && !s.canSyncEditableOutput(rt, now, forceEditable) {
		s.logOutputTrace(
			rt,
			"flush waiting for editable sync cadence",
			now,
			"buffer_len",
			utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
			"last_editable_sync_at",
			rt.lastEditableSyncAt,
			"sync_every",
			s.editableSyncEvery,
		)
		return
	}
	if rt.deliveryInFlight {
		s.logOutputTrace(
			rt,
			"flush waiting for in-flight delivery",
			now,
			"buffer_len",
			utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
		)
		return
	}
	raw := rt.outputBuffer
	bufferedAt := rt.outputBufferedAt
	rt.clearOutputBuffer()
	if strings.TrimSpace(raw) == "" {
		return
	}
	if len(rt.detachedOutputs) > 0 {
		baseline := mergeBufferedOutput(rt.outputText, rt.detachedBaseline(runID))
		candidateText := mergeBufferedOutput(baseline, raw)
		if outputDeliveryTooLarge(candidateText) {
			s.dropOutputForRun(rt, runID, "output body exceeds safety cap", now)
			return
		}
		text := unsentOutputDelta(baseline, candidateText)
		if strings.TrimSpace(text) == "" {
			rt.outputText = candidateText
			rt.noteDetachedBaseline(runID, candidateText)
			return
		}
		if detachedQueueWouldExceedLimit(rt.detachedOutputs, text) {
			s.dropOutputForRun(rt, runID, "detached output queue exceeds safety cap", now)
			return
		}
		rt.enqueueDetachedOutput(runID, text)
		// Advance observed delivery baseline once accepted into detached queue.
		// This avoids replaying the same chunk on pane reset jitter.
		rt.outputText = candidateText
		rt.noteDetachedBaseline(runID, candidateText)
		s.logOutputStateDebug(rt, "flush redirected to detached queue while backlog exists")
		return
	}
	if editable == nil {
		baseline := mergeBufferedOutput(rt.outputText, rt.detachedBaseline(runID))
		candidateText := mergeBufferedOutput(baseline, raw)
		if outputDeliveryTooLarge(candidateText) {
			s.dropOutputForRun(rt, runID, "plain output body exceeds safety cap", now)
			return
		}
		text := unsentOutputDelta(baseline, candidateText)
		if strings.TrimSpace(text) == "" {
			rt.outputText = candidateText
			rt.noteDetachedBaseline(runID, candidateText)
			return
		}
		if detachedQueueWouldExceedLimit(rt.detachedOutputs, text) {
			s.dropOutputForRun(rt, runID, "plain detached queue exceeds safety cap", now)
			return
		}
		rt.enqueueDetachedOutput(runID, text)
		rt.outputText = candidateText
		rt.noteDetachedBaseline(runID, candidateText)
		rt.editRateLimitCount = 0
		s.logOutputStateDebug(rt, "flush plain output redirected to detached queue")
		s.flushDetachedOutputs(rt)
		return
	}
	candidateText := mergeBufferedOutput(rt.outputText, raw)
	desiredText := strings.Trim(candidateText, "\n")
	if outputDeliveryTooLarge(desiredText) {
		s.dropOutputForRun(rt, runID, "editable output body exceeds safety cap", now)
		return
	}
	if strings.TrimSpace(desiredText) == "" {
		rt.outputText = candidateText
		return
	}
	outputGeneration := rt.outputGeneration
	existingMessages := make([]trackedMessage, len(rt.outputMessages))
	copy(existingMessages, rt.outputMessages)
	groupID := rt.opts.GroupID
	s.startDelivery(rt, func(ctx context.Context) (deliveryResult, error) {
		messages, err := s.syncEditableOutputSnapshot(ctx, editable, groupID, existingMessages, desiredText, !forceEditable)
		return deliveryResult{messages: messages}, err
	}, func(result deliveryResult, err error) {
		if outputGeneration != rt.outputGeneration {
			s.logger.Warn(
				"ignoring stale editable output completion",
				"group_id",
				rt.opts.GroupID,
				"run_id",
				runID,
				"stale_output_generation",
				outputGeneration,
				"current_output_generation",
				rt.outputGeneration,
			)
			return
		}
		if isMessageNotModifiedError(err) {
			rt.outputMessages = result.messages
			rt.outputText = candidateText
			return
		}
		if err != nil {
			rt.outputMessages = result.messages
			s.restoreOutputBufferPrefix(rt, raw, bufferedAt)
			if retryAfter := retryAfterFromRateLimitError(err); retryAfter > 0 {
				rt.editRateLimitCount++
				rt.applyOutputBackoff(retryAfter)
				s.logger.Warn(
					"sync editable output rate-limited; dropping current run output",
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
				s.dropOutputForRun(rt, runID, "editable output rate-limited", time.Now())
				return
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				s.logger.Warn(
					"sync editable output timed out; dropping current run output",
					"group_id",
					rt.opts.GroupID,
					"run_id",
					runID,
					"cursor",
					rt.runCursor(runID),
					"err",
					err,
				)
				s.dropOutputForRun(rt, runID, "editable output delivery timed out", time.Now())
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
		rt.outputMessages = result.messages
		rt.outputText = candidateText
		rt.editRateLimitCount = 0
		rt.lastEditableSyncAt = now
		rt.deferBodyUntilIdle = false
		rt.clearOutputDropState()
		s.logOutputTrace(
			rt,
			"editable output committed",
			now,
			"published_len",
			utf8.RuneCountInString(desiredText),
			"segments",
			len(splitForEditMessages(desiredText, s.editRolloverAt, maxMessageRunes)),
		)
		s.logOutputStateDebug(rt, "flush editable output committed")
	})
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
	if tail, ok := observedWindowTailDelta(existing, delta); ok {
		if tail == "" {
			return existing
		}
		return mergeBufferedOutput(existing, tail)
	}
	if strings.HasPrefix(delta, existing) {
		return delta
	}
	if strings.HasSuffix(existing, delta) {
		return existing
	}
	if overlap := tmuxctl.SuffixPrefixOverlap(existing, delta); usableMergeOverlap(existing, delta, overlap) {
		return existing + delta[overlap:]
	}
	return existing + delta
}

func unsentOutputDelta(baseline string, candidate string) string {
	baseline = strings.Trim(baseline, "\n")
	candidate = strings.Trim(candidate, "\n")
	if strings.TrimSpace(candidate) == "" || candidate == baseline {
		return ""
	}
	if baseline == "" {
		return candidate
	}
	if strings.HasPrefix(candidate, baseline) {
		return strings.Trim(candidate[len(baseline):], "\n")
	}
	if strings.Contains(baseline, candidate) {
		return ""
	}
	if overlap := tmuxctl.SuffixPrefixOverlap(baseline, candidate); usableMergeOverlap(baseline, candidate, overlap) {
		return strings.Trim(candidate[overlap:], "\n")
	}
	return candidate
}

func unsyncedVisibleTail(known string, visible string) (string, bool) {
	known = strings.Trim(known, "\n")
	visible = strings.Trim(visible, "\n")
	if strings.TrimSpace(known) == "" || strings.TrimSpace(visible) == "" {
		return "", false
	}
	if visible == known || strings.Contains(known, visible) {
		return "", false
	}
	if strings.HasPrefix(visible, known) {
		tail := strings.Trim(visible[len(known):], "\n")
		return tail, strings.TrimSpace(tail) != ""
	}
	if idx := strings.Index(visible, known); idx >= 0 {
		if idx == 0 || visible[idx-1] == '\n' {
			tail := strings.Trim(visible[idx+len(known):], "\n")
			return tail, strings.TrimSpace(tail) != ""
		}
	}
	if overlap := tmuxctl.SuffixPrefixOverlap(known, visible); usableMergeOverlap(known, visible, overlap) {
		tail := strings.Trim(visible[overlap:], "\n")
		return tail, strings.TrimSpace(tail) != ""
	}
	return "", false
}

func outputDeliveryTooLarge(text string) bool {
	return utf8.RuneCountInString(strings.Trim(text, "\n")) > maxOutputDeliveryRunes
}

func detachedQueueWouldExceedLimit(items []detachedOutput, text string) bool {
	queueLen, queueRunes := detachedQueueStats(items)
	textRunes := utf8.RuneCountInString(strings.Trim(text, "\n"))
	if textRunes == 0 {
		return false
	}
	chunks := (textRunes + maxDetachedMessageRunes - 1) / maxDetachedMessageRunes
	return queueLen+chunks > maxDetachedQueueItems || queueRunes+textRunes > maxDetachedQueueRunes
}

func detachedQueueExceedsLimit(items []detachedOutput) bool {
	queueLen, queueRunes := detachedQueueStats(items)
	return queueLen > maxDetachedQueueItems || queueRunes > maxDetachedQueueRunes
}

func detachedQueueStats(items []detachedOutput) (int, int) {
	runes := 0
	for _, item := range items {
		runes += utf8.RuneCountInString(strings.Trim(item.text, "\n"))
	}
	return len(items), runes
}

func observedWindowTailDelta(known string, snapshot string) (string, bool) {
	known = strings.Trim(known, "\n")
	snapshot = strings.Trim(snapshot, "\n")
	if strings.TrimSpace(known) == "" || strings.TrimSpace(snapshot) == "" {
		return "", false
	}
	if utf8.RuneCountInString(snapshot) < maxMessageRunes {
		return "", false
	}
	if strings.Contains(known, snapshot) {
		return "", true
	}

	lines := strings.Split(snapshot, "\n")
	for n := len(lines) - 1; n > 0; n-- {
		prefix := strings.Join(lines[:n], "\n")
		if utf8.RuneCountInString(prefix) < maxMessageRunes {
			break
		}
		if strings.Contains(known, prefix) {
			tail := strings.TrimRight(snapshot[len(prefix):], "\n")
			return tail, true
		}
	}
	return "", false
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
	if rt.isDroppingCurrentRunOutput() {
		rt.clearOutputBuffer()
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
	if rt.isDroppingCurrentRunOutput() {
		rt.clearOutputBuffer()
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
	rt.lastOutputWatchdogAt = time.Time{}
}

func (rt *groupRuntime) clearOutputDropState() {
	if rt == nil {
		return
	}
	rt.outputDroppedRunID = 0
	rt.outputDropReason = ""
}

func (rt *groupRuntime) currentRunID() uint64 {
	if rt == nil {
		return 0
	}
	if rt.runID != 0 {
		return rt.runID
	}
	if rt.nextRunID != 0 {
		return rt.nextRunID
	}
	if rt.outputDroppedRunID != 0 {
		return rt.outputDroppedRunID
	}
	return rt.nextRunID
}

func (rt *groupRuntime) isDroppingCurrentRunOutput() bool {
	if rt == nil || rt.outputDroppedRunID == 0 {
		return false
	}
	return rt.outputDroppedRunID == rt.currentRunID()
}

func (rt *groupRuntime) notePreBusyMutedText(text string) {
	if rt == nil {
		return
	}
	text = strings.Trim(text, "\n")
	if text == rt.preBusyMutedText {
		return
	}
	rt.preBusyMutedText = text
	rt.preBusyMutedChanges++
	rt.preBusyMutedStable = 0
}

func (rt *groupRuntime) clearPreBusyMutedState() {
	if rt == nil {
		return
	}
	rt.preBusyMutedText = ""
	rt.preBusyMutedChanges = 0
	rt.preBusyMutedStable = 0
}

func (rt *groupRuntime) holdingPreBusyMutedOutput(idleConfirmTicks int) bool {
	if rt == nil || rt.active == nil || rt.runBusySeen || strings.TrimSpace(rt.preBusyMutedText) == "" {
		return false
	}
	if idleConfirmTicks <= 0 {
		idleConfirmTicks = 1
	}
	return rt.preBusyMutedStable < idleConfirmTicks
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
		rt.deliveryInFlight ||
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

func (s *Service) dropOutputForRun(rt *groupRuntime, runID uint64, reason string, now time.Time) {
	if rt == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if runID == 0 {
		runID = rt.currentRunID()
	}
	if runID == 0 {
		runID = 1
	}
	firstDrop := rt.outputDroppedRunID != runID
	rt.outputDroppedRunID = runID
	rt.outputDropReason = reason
	rt.clearOutputBuffer()
	rt.detachedOutputs = nil
	rt.detachedBackoffUntil = time.Time{}
	rt.detachedRetryCount = 0
	rt.nextDetachedSendAt = time.Time{}
	rt.deferBodyUntilIdle = false
	rt.editBackoffUntil = time.Time{}
	rt.editRateLimitCount = 0
	rt.lastDetachedWatchdogAt = time.Time{}
	if !firstDrop {
		s.logOutputTrace(rt, "output delivery remains dropped", now, "reason", reason)
		return
	}
	s.logger.Warn(
		"output delivery dropped for current run",
		"group_id", rt.opts.GroupID,
		"run_id", runID,
		"cursor", rt.runCursor(runID),
		"reason", reason,
		"output_backoff_until", rt.outputBackoffUntil,
	)
}

func (s *Service) dropDetachedBacklog(rt *groupRuntime, runID uint64, reason string, now time.Time) {
	if rt == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	queueLen, queueRunes := detachedQueueStats(rt.detachedOutputs)
	rt.detachedOutputs = nil
	rt.detachedBackoffUntil = time.Time{}
	rt.detachedRetryCount = 0
	rt.nextDetachedSendAt = time.Time{}
	rt.lastDetachedWatchdogAt = time.Time{}
	if runID != 0 && runID == rt.currentRunID() {
		s.dropOutputForRun(rt, runID, reason, now)
		return
	}
	s.logger.Warn(
		"detached output backlog dropped",
		"group_id", rt.opts.GroupID,
		"run_id", runID,
		"reason", reason,
		"queue_len", queueLen,
		"queue_runes", queueRunes,
		"output_backoff_until", rt.outputBackoffUntil,
	)
}

func (rt *groupRuntime) detachedBaseline(runID uint64) string {
	if rt == nil || runID == 0 || len(rt.detachedBaselineByRun) == 0 {
		return ""
	}
	return rt.detachedBaselineByRun[runID]
}

func (rt *groupRuntime) noteDetachedBaseline(runID uint64, text string) {
	if rt == nil || runID == 0 {
		return
	}
	text = strings.Trim(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	if rt.detachedBaselineByRun == nil {
		rt.detachedBaselineByRun = make(map[uint64]string)
	}
	current := rt.detachedBaselineByRun[runID]
	rt.detachedBaselineByRun[runID] = mergeBufferedOutput(current, text)
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
	now := time.Now()
	outputBackoff := rt.outputBackoffActive(now)
	if !rt.detachedBackoffUntil.IsZero() && !rt.detachedBackoffUntil.After(now) {
		rt.detachedBackoffUntil = time.Time{}
	}
	if !rt.editBackoffUntil.IsZero() && !rt.editBackoffUntil.After(now) {
		rt.editBackoffUntil = time.Time{}
	}
	workingBackoff := rt.workingBackoffActive(now)
	return rt.deliveryInFlight ||
		rt.hasBufferedOutput() ||
		len(rt.detachedOutputs) > 0 ||
		outputBackoff ||
		!rt.detachedBackoffUntil.IsZero() ||
		!rt.editBackoffUntil.IsZero() ||
		workingBackoff
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
	for _, chunk := range splitByRunes(text, maxDetachedMessageRunes) {
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

type detachedSendBatch struct {
	runID      uint64
	cursor     int
	text       string
	count      int
	enqueuedAt time.Time
}

func nextDetachedSendBatch(items []detachedOutput, limit int) detachedSendBatch {
	if len(items) == 0 {
		return detachedSendBatch{}
	}
	head := items[0]
	batch := detachedSendBatch{
		runID:      head.runID,
		cursor:     head.cursor,
		text:       head.text,
		count:      1,
		enqueuedAt: head.enqueuedAt,
	}
	if limit <= 0 {
		return batch
	}
	current := utf8.RuneCountInString(batch.text)
	if current >= limit {
		return batch
	}
	for i := 1; i < len(items); i++ {
		item := items[i]
		if item.runID != batch.runID || item.cursor != batch.cursor+1 {
			break
		}
		merged := batch.text + item.text
		if utf8.RuneCountInString(merged) > limit {
			break
		}
		batch.text = merged
		batch.cursor = item.cursor
		batch.count++
		current = utf8.RuneCountInString(batch.text)
		if current >= limit {
			break
		}
	}
	return batch
}

func (s *Service) detachedSendSpacing(rt *groupRuntime) time.Duration {
	if s.detachedSendEvery <= 0 {
		return 0
	}
	if rt == nil || rt.runInFlight() {
		return s.detachedSendEvery
	}
	if s.detachedSendEvery < detachedCatchUpSendEvery {
		return s.detachedSendEvery
	}
	return detachedCatchUpSendEvery
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
	if detachedQueueExceedsLimit(rt.detachedOutputs) {
		runID := rt.detachedOutputs[0].runID
		s.dropDetachedBacklog(rt, runID, "detached queue exceeds safety cap", now)
		return
	}
	if rt.outputBackoffActive(now) {
		runID := rt.detachedOutputs[0].runID
		s.dropDetachedBacklog(rt, runID, "detached flush blocked by shared output backoff", now)
		return
	}
	if s.detachedWatchdogAfter > 0 &&
		!rt.detachedOutputs[0].enqueuedAt.IsZero() &&
		now.Sub(rt.detachedOutputs[0].enqueuedAt) >= s.detachedWatchdogAfter &&
		(rt.lastDetachedWatchdogAt.IsZero() || now.Sub(rt.lastDetachedWatchdogAt) >= outputWatchdogLogEvery) {
		s.logger.Warn(
			"detached output watchdog forcing retry",
			"group_id", rt.opts.GroupID,
			"run_id", rt.detachedOutputs[0].runID,
			"cursor", rt.detachedOutputs[0].cursor,
			"age", now.Sub(rt.detachedOutputs[0].enqueuedAt).String(),
		)
		rt.lastDetachedWatchdogAt = now
	}
	if !rt.detachedBackoffUntil.IsZero() {
		if rt.detachedBackoffUntil.After(now) {
			s.logOutputTrace(
				rt,
				"detached flush waiting for retry window",
				now,
				"queue_len",
				len(rt.detachedOutputs),
				"detached_backoff_until",
				rt.detachedBackoffUntil,
			)
			return
		}
		rt.detachedBackoffUntil = time.Time{}
	}
	if !rt.nextDetachedSendAt.IsZero() && rt.nextDetachedSendAt.After(now) {
		s.logOutputTrace(
			rt,
			"detached flush waiting for per-chat spacing",
			now,
			"queue_len",
			len(rt.detachedOutputs),
			"next_detached_send_at",
			rt.nextDetachedSendAt,
		)
		return
	}
	if rt.deliveryInFlight {
		s.logOutputTrace(
			rt,
			"detached flush waiting for in-flight delivery",
			now,
			"queue_len",
			len(rt.detachedOutputs),
		)
		return
	}

	batch := nextDetachedSendBatch(rt.detachedOutputs, maxDetachedMessageRunes)
	groupID := rt.opts.GroupID
	s.startDelivery(rt, func(ctx context.Context) (deliveryResult, error) {
		return deliveryResult{}, sendTextToChatWithContext(ctx, s.messenger, groupID, batch.text)
	}, func(_ deliveryResult, err error) {
		if err != nil {
			retryAfter := retryAfterFromRateLimitError(err)
			if retryAfter > 0 {
				rt.applyOutputBackoff(retryAfter)
				s.logger.Warn(
					"flush detached output rate-limited; dropping backlog",
					"group_id",
					rt.opts.GroupID,
					"run_id",
					batch.runID,
					"cursor",
					batch.cursor,
					"retry_after",
					retryAfter.String(),
					"retry_at",
					rt.outputBackoffUntil,
					"err",
					err,
				)
				s.dropDetachedBacklog(rt, batch.runID, "detached output rate-limited", time.Now())
				return
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				s.logger.Warn(
					"flush detached output timed out; dropping backlog",
					"group_id",
					rt.opts.GroupID,
					"run_id",
					batch.runID,
					"cursor",
					batch.cursor,
					"err",
					err,
				)
				s.dropDetachedBacklog(rt, batch.runID, "detached output delivery timed out", time.Now())
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
				batch.runID,
				"cursor",
				batch.cursor,
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
		if len(rt.detachedOutputs) >= batch.count {
			rt.detachedOutputs = rt.detachedOutputs[batch.count:]
		} else {
			rt.detachedOutputs = nil
		}
		rt.commitCursor(batch.runID, batch.cursor)
		rt.detachedRetryCount = 0
		if spacing := s.detachedSendSpacing(rt); spacing > 0 {
			rt.nextDetachedSendAt = time.Now().Add(spacing)
		} else {
			rt.nextDetachedSendAt = time.Time{}
		}
		s.logOutputTrace(
			rt,
			"detached output committed",
			time.Now(),
			"item_len",
			utf8.RuneCountInString(batch.text),
			"batch_count",
			batch.count,
			"remaining_queue_len",
			len(rt.detachedOutputs),
			"next_detached_send_at",
			rt.nextDetachedSendAt,
		)
		s.logOutputStateDebug(rt, "detached output committed")
		if !rt.runInFlight() && !rt.hasPendingOutputDelivery() {
			s.clearWorkingStatus(rt)
			rt.workingSent = false
		}
	})
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

func (s *Service) syncEditableOutputSnapshot(ctx context.Context, editable EditableMessenger, groupID string, existing []trackedMessage, desiredText string, freezeCompleted bool) ([]trackedMessage, error) {
	next, err := syncEditableOutputState(ctx, editable, groupID, existing, desiredText, freezeCompleted, s.editRolloverAt)
	if shouldResetEditableThread(err) {
		// The first attempt may have created new messages that are now orphaned
		// (they exist in the IM platform but we're about to discard their IDs).
		// Prune any newly created messages before retrying with a clean slate.
		if orphans := orphanedMessages(existing, next); len(orphans) > 0 {
			s.logger.Warn("pruning orphaned messages before editable thread reset",
				"group_id", groupID,
				"orphan_count", len(orphans),
			)
			pruneStaleEditableMessagesState(ctx, editable, groupID, orphans)
		}
		next, err = syncEditableOutputState(ctx, editable, groupID, nil, desiredText, false, s.editRolloverAt)
	}
	return next, err
}

// orphanedMessages returns messages that are in next but not in existing —
// i.e. messages newly created during a failed syncEditableOutputState call.
func orphanedMessages(existing []trackedMessage, next []trackedMessage) []trackedMessage {
	known := make(map[string]struct{}, len(existing))
	for _, m := range existing {
		if m.messageID != "" {
			known[m.messageID] = struct{}{}
		}
	}
	var orphans []trackedMessage
	for _, m := range next {
		if m.messageID != "" {
			if _, ok := known[m.messageID]; !ok {
				orphans = append(orphans, m)
			}
		}
	}
	return orphans
}

func syncEditableOutputState(ctx context.Context, editable EditableMessenger, groupID string, existing []trackedMessage, desiredText string, freezeCompleted bool, rolloverAt int) ([]trackedMessage, error) {
	segments := splitForEditMessages(desiredText, rolloverAt, maxMessageRunes)
	if len(segments) == 0 {
		return nil, nil
	}

	next := make([]trackedMessage, len(existing))
	copy(next, existing)
	if len(next) > len(segments) {
		stale := pruneStaleEditableMessagesState(ctx, editable, groupID, next[len(segments):])
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
			if err := editTextInChatContext(ctx, editable, groupID, next[i].messageID, segment); err != nil {
				return next, err
			}
			next[i].text = segment
			continue
		}

		msg, err := sendTextToChatWithIDContext(ctx, editable, groupID, segment)
		if err != nil {
			return next, err
		}
		next = append(next, trackedMessage{
			messageID: msg.MessageID,
			text:      segment,
		})
	}
	return next, nil
}

func pruneStaleEditableMessagesState(ctx context.Context, editable EditableMessenger, groupID string, stale []trackedMessage) []trackedMessage {
	if editable == nil || len(stale) == 0 {
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
			if err := deleteMessageInChatContext(ctx, deleter, groupID, messageID); err == nil {
				continue
			}
		}
		if err := editTextInChatContext(ctx, editable, groupID, messageID, "…"); err != nil {
			kept = append(kept, item)
		}
	}
	return kept
}

func (s *Service) restoreOutputBufferPrefix(rt *groupRuntime, prefix string, bufferedAt time.Time) {
	if rt == nil || strings.TrimSpace(prefix) == "" {
		return
	}
	if rt.isDroppingCurrentRunOutput() {
		rt.clearOutputBuffer()
		return
	}
	rt.outputBuffer = prefix + rt.outputBuffer
	switch {
	case rt.outputBufferedAt.IsZero():
		rt.outputBufferedAt = bufferedAt
	case !bufferedAt.IsZero() && bufferedAt.Before(rt.outputBufferedAt):
		rt.outputBufferedAt = bufferedAt
	}
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

func (s *Service) clearWorkingStatus(rt *groupRuntime) {
	if rt == nil || rt.statusMessage.messageID == "" {
		return
	}
	if rt.deliveryInFlight || rt.outputBackoffActive(time.Now()) || rt.workingBackoffActive(time.Now()) {
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

func (s *Service) logOutputTrace(rt *groupRuntime, message string, now time.Time, attrs ...any) {
	if rt == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	if rt.outputTraceAtByKey == nil {
		rt.outputTraceAtByKey = make(map[string]time.Time)
	}
	if last := rt.outputTraceAtByKey[message]; !last.IsZero() && now.Sub(last) < outputTraceLogEvery {
		return
	}
	rt.outputTraceAtByKey[message] = now

	base := []any{
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"busy", rt.lastBusy,
		"idle_ticks", rt.idleTicks,
		"cursor", rt.runCursor(rt.runID),
		"buffer_len", utf8.RuneCountInString(strings.Trim(rt.outputBuffer, "\n")),
		"published_len", utf8.RuneCountInString(strings.Trim(rt.outputText, "\n")),
		"detached_len", len(rt.detachedOutputs),
	}
	base = append(base, attrs...)
	s.logger.Info(message, base...)
}

func (s *Service) logInitialReplayDiagnostic(rt *groupRuntime, snapshot string, snapshotPromptObserved bool, prevPromptBlockObserved bool, currPromptBlockObserved bool, currFullText string, prevText string, currText string) {
	if rt == nil {
		return
	}
	s.logger.Warn(
		"initial replay diagnostic",
		"group_id", rt.opts.GroupID,
		"run_id", rt.runID,
		"busy", rt.lastBusy,
		"run_busy_seen", rt.runBusySeen,
		"run_prompt_observed", rt.runPromptObserved,
		"prompt_echo_pending", rt.promptEchoPending,
		"snapshot_prompt_observed", snapshotPromptObserved,
		"prev_prompt_block_observed", prevPromptBlockObserved,
		"curr_prompt_block_observed", currPromptBlockObserved,
		"base_len", utf8.RuneCountInString(strings.Trim(rt.baseText, "\n")),
		"last_len", utf8.RuneCountInString(strings.Trim(rt.lastText, "\n")),
		"curr_full_len", utf8.RuneCountInString(strings.Trim(currFullText, "\n")),
		"prev_len", utf8.RuneCountInString(strings.Trim(prevText, "\n")),
		"curr_len", utf8.RuneCountInString(strings.Trim(currText, "\n")),
		"base_curr_overlap_lines", lineTailHeadOverlap(rt.baseText, currFullText),
		"last_curr_overlap_lines", lineTailHeadOverlap(rt.lastText, currFullText),
		"base_head", logTextHead(rt.baseText, 3),
		"base_tail", logTextTail(rt.baseText, 6),
		"last_head", logTextHead(rt.lastText, 3),
		"last_tail", logTextTail(rt.lastText, 6),
		"curr_head", logTextHead(currFullText, 3),
		"curr_tail", logTextTail(currFullText, 6),
		"snapshot_tail", logTextTail(snapshot, 10),
	)
}

func lineTailHeadOverlap(base string, curr string) int {
	base = strings.Trim(base, "\n")
	curr = strings.Trim(curr, "\n")
	if base == "" || curr == "" {
		return 0
	}
	baseLines := strings.Split(base, "\n")
	currLines := strings.Split(curr, "\n")
	maxOverlap := minInt(len(baseLines), len(currLines))
	for n := maxOverlap; n > 0; n-- {
		if equalStringSlices(baseLines[len(baseLines)-n:], currLines[:n]) {
			return n
		}
	}
	return 0
}

func suspiciousInitialWindowReplay(rt *groupRuntime, currFullText string, currText string) bool {
	if rt == nil {
		return false
	}
	baseText := strings.Trim(rt.baseText, "\n")
	currFullText = strings.Trim(currFullText, "\n")
	currText = strings.Trim(currText, "\n")
	if baseText == "" || currFullText == "" || currText == "" {
		return false
	}
	baseLen := utf8.RuneCountInString(baseText)
	currFullLen := utf8.RuneCountInString(currFullText)
	currLen := utf8.RuneCountInString(currText)
	if baseLen < 4000 || currLen < 4000 {
		return false
	}
	if lineTailHeadOverlap(baseText, currFullText) != 0 || lineTailHeadOverlap(rt.lastText, currFullText) != 0 {
		return false
	}
	nearDeltaLimit := maxInt(512, baseLen/100)
	return absInt(currFullLen-baseLen) <= nearDeltaLimit
}

func recoverWindowRewriteDelta(prev string, curr string) string {
	prev = strings.Trim(prev, "\n")
	curr = strings.Trim(curr, "\n")
	if prev == "" || curr == "" || prev == curr {
		return ""
	}
	prevLines := strings.Split(prev, "\n")
	currLines := strings.Split(curr, "\n")

	prefix := 0
	for prefix < len(prevLines) && prefix < len(currLines) && prevLines[prefix] == currLines[prefix] {
		prefix++
	}

	suffix := 0
	for suffix < len(prevLines)-prefix && suffix < len(currLines)-prefix {
		prevLine := prevLines[len(prevLines)-1-suffix]
		currLine := currLines[len(currLines)-1-suffix]
		if prevLine != currLine {
			break
		}
		suffix++
	}

	start := prefix
	end := len(currLines) - suffix
	if start >= end {
		return ""
	}
	delta := strings.Trim(strings.Join(currLines[start:end], "\n"), "\n")
	if delta == "" {
		return ""
	}
	if strings.Contains(prev, delta) {
		return ""
	}
	currLen := utf8.RuneCountInString(curr)
	deltaLen := utf8.RuneCountInString(delta)
	if deltaLen > maxInt(2048, currLen/8) {
		return ""
	}
	return delta
}

func equalStringSlices(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func logTextHead(text string, lines int) string {
	return logTextPreview(text, lines, true)
}

func logTextTail(text string, lines int) string {
	return logTextPreview(text, lines, false)
}

func logTextPreview(text string, lines int, head bool) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.Trim(text, "\n")
	if text == "" || lines <= 0 {
		return ""
	}
	parts := strings.Split(text, "\n")
	if len(parts) > lines {
		if head {
			parts = parts[:lines]
		} else {
			parts = parts[len(parts)-lines:]
		}
	}
	return strings.Join(parts, " | ")
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a int, b int) int {
	if a > b {
		return a
	}
	return b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
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
	return "imcodex-" + tmuxctl.SanitizeName(base)
}

func DefaultSessionNameForGroup(groupID string, cwd string) string {
	group := tmuxctl.SanitizeName(groupID)
	if group == "" || group == "session" {
		return DefaultSessionName(cwd)
	}
	return DefaultSessionName(cwd) + "-" + group
}
