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
	"github.com/magnaflowlabs/imcodex/internal/xutil"
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
}

type queuedUpdate struct {
	updateID int64
	msg      gateway.IncomingMessage
	done     chan error
}

type batchResult struct {
	updateID int64
	done     chan error
}

func NewReceiver(client UpdateClient, handler MessageHandler, logger *slog.Logger) *Receiver {
	if logger == nil {
		logger = slog.Default()
	}
	return &Receiver{
		client:  client,
		handler: handler,
		logger:  logger,
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

	grouped := make(map[string][]queuedUpdate)
	ordered := make([]batchResult, 0, len(updates))
	keys := make([]string, 0, len(updates))
	for _, update := range updates {
		result := batchResult{
			updateID: update.UpdateID,
			done:     make(chan error, 1),
		}
		ordered = append(ordered, result)
		msg, ok, err := updateToIncomingMessage(update)
		if err != nil {
			r.logger.Warn("telegram update decode failed", "update_id", update.UpdateID, "err", err)
			result.done <- nil
			close(result.done)
			continue
		}
		if !ok {
			result.done <- nil
			close(result.done)
			continue
		}
		key := msg.GroupID
		if _, exists := grouped[key]; !exists {
			keys = append(keys, key)
		}
		grouped[key] = append(grouped[key], queuedUpdate{
			updateID: update.UpdateID,
			msg:      msg,
			done:     result.done,
		})
	}

	for _, key := range keys {
		items := grouped[key]
		go func(batch []queuedUpdate) {
			for _, item := range batch {
				err := r.handleMessage(ctx, item.updateID, item.msg)
				item.done <- err
				close(item.done)
				if err != nil {
					return
				}
			}
		}(items)
	}

	for _, item := range ordered {
		select {
		case err := <-item.done:
			if err != nil {
				return err
			}
			if item.updateID >= r.offset {
				r.offset = item.updateID + 1
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-item.done:
			if err != nil {
				return err
			}
			if item.updateID >= r.offset {
				r.offset = item.updateID + 1
			}
		}
	}
	return nil
}

func (r *Receiver) handleMessage(ctx context.Context, updateID int64, msg gateway.IncomingMessage) error {
	if err := r.handler.HandleMessage(ctx, msg); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.logger.Warn("telegram update handle failed", "update_id", updateID, "message_id", msg.MessageID, "group_id", msg.GroupID, "err", err)
	}
	return nil
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
		Text:      xutil.FirstNonEmpty(msg.Text, msg.Caption),
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
