package telegram

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
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
	client  UpdateClient
	handler MessageHandler
	logger  *slog.Logger
	offset  int64
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

	for {
		updates, err := r.client.GetUpdates(ctx, r.offset, pollTimeoutSeconds)
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

		for _, update := range updates {
			if update.UpdateID >= r.offset {
				r.offset = update.UpdateID + 1
			}
			msg, ok, err := updateToIncomingMessage(update)
			if err != nil {
				r.logger.Warn("telegram update decode failed", "update_id", update.UpdateID, "err", err)
				continue
			}
			if !ok {
				continue
			}
			if err := r.handler.HandleMessage(ctx, msg); err != nil {
				return err
			}
		}
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
		Text:      firstNonEmpty(strings.TrimSpace(msg.Text), strings.TrimSpace(msg.Caption)),
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
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
