package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

const pollTimeoutSeconds = 8

type MessageHandler interface {
	HandleMessage(ctx context.Context, msg gateway.IncomingMessage) error
}

type UpdateClient interface {
	GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error)
}

type Receiver struct {
	client   UpdateClient
	handler  MessageHandler
	logger   *slog.Logger
	offset   int64
	mu       sync.Mutex
	pollStop context.CancelFunc
	workers  map[string]chan queuedUpdate
}

type queuedUpdate struct {
	update Update
	done   chan error
}

func NewReceiver(client UpdateClient, handler MessageHandler, logger *slog.Logger) *Receiver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Receiver{
		client:  client,
		handler: handler,
		logger:  logger,
		workers: make(map[string]chan queuedUpdate),
	}
}

func (r *Receiver) Start(ctx context.Context) error {
	if r == nil || r.client == nil || r.handler == nil {
		<-ctx.Done()
		return nil
	}

	go func() {
		<-ctx.Done()
		r.stopPoll()
	}()

	for {
		if ctx.Err() != nil {
			return nil
		}
		pollCtx, cancel := context.WithCancel(ctx)
		r.setPollStop(cancel)
		updates, err := r.client.GetUpdates(pollCtx, r.offset, pollTimeoutSeconds)
		r.clearPollStop(cancel)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.logger.Warn("telegram getUpdates failed", "err", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(3 * time.Second):
			}
			continue
		}

		if len(updates) == 0 {
			continue
		}
		if err := r.processBatch(ctx, updates); err != nil {
			return err
		}
	}
}

func (r *Receiver) processBatch(ctx context.Context, updates []Update) error {
	if len(updates) == 0 {
		return nil
	}

	done := make(chan error, len(updates))
	nextOffset := r.offset
	dispatched := 0
	for _, update := range updates {
		if update.UpdateID >= nextOffset {
			nextOffset = update.UpdateID + 1
		}
		if err := r.dispatchUpdate(ctx, update, done); err != nil {
			return err
		}
		dispatched++
	}

	for i := 0; i < dispatched; i++ {
		if err := <-done; err != nil {
			return err
		}
	}
	r.offset = nextOffset
	return nil
}

func (r *Receiver) dispatchUpdate(ctx context.Context, update Update, done chan error) error {
	key := updateWorkerKey(update)
	worker := r.workerForKey(ctx, key)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case worker <- queuedUpdate{update: update, done: done}:
		return nil
	}
}

func (r *Receiver) workerForKey(ctx context.Context, key string) chan queuedUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()

	if worker, ok := r.workers[key]; ok {
		return worker
	}

	worker := make(chan queuedUpdate, 32)
	r.workers[key] = worker
	go r.runWorker(ctx, key, worker)
	return worker
}

func (r *Receiver) runWorker(ctx context.Context, key string, worker chan queuedUpdate) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-worker:
			item.done <- r.handleUpdate(ctx, item.update)
		}
	}
}

func (r *Receiver) handleUpdate(ctx context.Context, update Update) error {
	msg, ok, err := updateToIncomingMessage(update)
	if err != nil {
		r.logger.Warn("telegram update decode failed", "update_id", update.UpdateID, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return r.handler.HandleMessage(ctx, msg)
}

func updateWorkerKey(update Update) string {
	if update.Message != nil {
		return strconv.FormatInt(update.Message.Chat.ID, 10)
	}
	return "update:" + strconv.FormatInt(update.UpdateID, 10)
}

func (r *Receiver) setPollStop(cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pollStop = cancel
}

func (r *Receiver) clearPollStop(cancel context.CancelFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pollStop = nil
}

func (r *Receiver) stopPoll() {
	r.mu.Lock()
	cancel := r.pollStop
	r.pollStop = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func updateToIncomingMessage(update Update) (gateway.IncomingMessage, bool, error) {
	msg := update.Message
	if msg == nil {
		return gateway.IncomingMessage{}, false, nil
	}
	if msg.From != nil && msg.From.IsBot {
		return gateway.IncomingMessage{}, false, nil
	}
	if !isSupportedChatType(msg.Chat.Type) {
		return gateway.IncomingMessage{}, false, nil
	}

	incoming := gateway.IncomingMessage{
		MessageID: strconv.FormatInt(msg.MessageID, 10),
		GroupID:   strconv.FormatInt(msg.Chat.ID, 10),
		Text:      firstNonEmpty(msg.Text, msg.Caption),
	}

	if photo := selectLargestPhoto(msg.Photo); photo != nil {
		incoming.Attachments = append(incoming.Attachments, gateway.IncomingAttachment{
			ResourceType: "image",
			ResourceKey:  strings.TrimSpace(photo.FileID),
		})
	}
	if attachment := documentAttachment(msg.Document); attachment != nil {
		incoming.Attachments = append(incoming.Attachments, *attachment)
	}
	if attachment := documentAttachment(msg.Audio); attachment != nil {
		incoming.Attachments = append(incoming.Attachments, *attachment)
	}
	if attachment := documentAttachment(msg.Video); attachment != nil {
		incoming.Attachments = append(incoming.Attachments, *attachment)
	}
	if attachment := documentAttachment(msg.Voice); attachment != nil {
		incoming.Attachments = append(incoming.Attachments, *attachment)
	}

	if strings.TrimSpace(incoming.Text) == "" && len(incoming.Attachments) == 0 {
		return gateway.IncomingMessage{}, false, nil
	}
	if strings.TrimSpace(incoming.GroupID) == "" {
		return gateway.IncomingMessage{}, false, fmt.Errorf("telegram chat id is empty")
	}
	return incoming, true, nil
}

func isSupportedChatType(chatType string) bool {
	switch strings.TrimSpace(chatType) {
	case "group", "supergroup":
		return true
	default:
		return false
	}
}

func selectLargestPhoto(photos []Photo) *Photo {
	if len(photos) == 0 {
		return nil
	}
	best := &photos[0]
	bestScore := int64(best.Width*best.Height) + best.FileSize
	for i := 1; i < len(photos); i++ {
		score := int64(photos[i].Width*photos[i].Height) + photos[i].FileSize
		if score >= bestScore {
			best = &photos[i]
			bestScore = score
		}
	}
	if strings.TrimSpace(best.FileID) == "" {
		return nil
	}
	return best
}

func documentAttachment(doc *Document) *gateway.IncomingAttachment {
	if doc == nil || strings.TrimSpace(doc.FileID) == "" {
		return nil
	}
	return &gateway.IncomingAttachment{
		ResourceType: "file",
		ResourceKey:  strings.TrimSpace(doc.FileID),
		FileName:     strings.TrimSpace(doc.FileName),
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
