package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
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
	interruptForceAfter time.Duration

	mu      sync.Mutex
	runtime *groupRuntime

	recentMessageIDs []string
	recentMessageSet map[string]struct{}
}

type groupRuntime struct {
	opts               Options
	session            string
	queue              chan IncomingMessage
	pending            []IncomingMessage
	sessionReady       bool
	lastText           string
	baseText           string
	lastBusy           bool
	idleTicks          int
	outputBuffer       string
	busySince          time.Time
	workingSent        bool
	interruptSentAt    time.Time
	forceInterruptSent bool
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
				s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Failed to start session: %v", err))
				continue
			}
			if !wasReady {
				s.keepLatestPending(rt)
			}
			if s.opts.InterruptOnNewMessage && rt.lastBusy && len(rt.pending) > 0 {
				s.requestInterrupt(rt)
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
				s.keepLatestPending(rt)
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
	rt.busySince = time.Time{}
	rt.workingSent = false
	rt.interruptSentAt = time.Time{}
	rt.forceInterruptSent = false
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

	msg := rt.pending[0]
	rt.pending = rt.pending[1:]
	text, err := s.materializeInput(msg)
	if err != nil {
		s.logger.Error("prepare message for codex failed", "group_id", rt.opts.GroupID, "message_id", msg.MessageID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Failed to prepare message for Codex: %v", err))
		if !rt.lastBusy && len(rt.pending) > 0 {
			s.dispatchNext(rt)
		}
		return
	}

	if err := s.console.SendText(s.ctx, rt.session, text); err != nil {
		s.logger.Error("send text to codex failed", "group_id", rt.opts.GroupID, "err", err)
		s.sendBestEffort(rt.opts.GroupID, fmt.Sprintf("[imcodex] Failed to send to Codex: %v", err))
		rt.pending = nil
		s.dropQueued(rt)
		rt.sessionReady = false
		rt.baseText = ""
		rt.lastBusy = false
		rt.idleTicks = 0
		rt.lastText = ""
		rt.outputBuffer = ""
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
		return
	}

	rt.baseText = rt.lastText
	rt.lastBusy = true
	rt.idleTicks = 0
	rt.outputBuffer = ""
	rt.busySince = time.Now()
	rt.workingSent = false
	rt.interruptSentAt = time.Time{}
	rt.forceInterruptSent = false
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
		rt.busySince = time.Time{}
		rt.workingSent = false
		return
	}

	currFullText := tmuxctl.NormalizeSnapshot(snapshot)
	prevText := tmuxctl.SliceAfter(rt.baseText, rt.lastText)
	currText := tmuxctl.SliceAfter(rt.baseText, currFullText)
	busy := tmuxctl.IsBusy(snapshot)
	delta, reset := tmuxctl.DiffText(prevText, currText)

	if busy {
		rt.idleTicks = 0
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
		if !rt.workingSent && !rt.busySince.IsZero() && time.Since(rt.busySince) >= 3*time.Second {
			s.sendBestEffort(rt.opts.GroupID, "[working] Codex is processing.")
			rt.workingSent = true
		}
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
	if !busy {
		rt.busySince = time.Time{}
		rt.workingSent = false
		rt.interruptSentAt = time.Time{}
		rt.forceInterruptSent = false
	}

	if !busy && len(rt.pending) > 0 {
		s.dispatchNext(rt)
	}
}

func shouldFlush(buffer string, busy bool, idleTicks int) bool {
	if strings.TrimSpace(buffer) == "" {
		return false
	}
	return !busy && idleTicks >= 2
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
