package lark

import (
	"context"
	"encoding/json"
	"strings"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	larkdispatcher "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

const botP2PChatEnteredEvent = "im.chat.access_event.bot_p2p_chat_entered_v1"

type MessageHandler interface {
	HandleMessage(ctx context.Context, msg gateway.IncomingMessage) error
}

type Receiver struct {
	start func(context.Context) error
}

func NewReceiver(appID string, appSecret string, baseURL string, handler MessageHandler) *Receiver {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = larksdk.LarkBaseUrl
	}

	dispatcher := newEventDispatcher(handler)

	client := larkws.NewClient(
		appID,
		appSecret,
		larkws.WithDomain(baseURL),
		larkws.WithEventHandler(dispatcher),
		larkws.WithLogLevel(larkcore.LogLevelError),
	)

	return &Receiver{start: client.Start}
}

func newEventDispatcher(handler MessageHandler) *larkdispatcher.EventDispatcher {
	return larkdispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			msg, ok, err := eventToIncomingMessage(event)
			if err != nil || !ok {
				return err
			}
			return handler.HandleMessage(ctx, msg)
		}).
		OnCustomizedEvent(botP2PChatEnteredEvent, func(context.Context, *larkevent.EventReq) error {
			return nil
		})
}

func (r *Receiver) Start(ctx context.Context) error {
	return r.start(ctx)
}

func eventToIncomingMessage(event *larkim.P2MessageReceiveV1) (gateway.IncomingMessage, bool, error) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return gateway.IncomingMessage{}, false, nil
	}

	msg := event.Event.Message
	if stringValue(msg.ChatType) != "group" {
		return gateway.IncomingMessage{}, false, nil
	}
	if stringValue(msg.MessageType) != "text" {
		return gateway.IncomingMessage{}, false, nil
	}
	if sender := event.Event.Sender; sender != nil {
		switch stringValue(sender.SenderType) {
		case "app", "bot":
			return gateway.IncomingMessage{}, false, nil
		}
	}

	groupID := strings.TrimSpace(stringValue(msg.ChatId))
	if groupID == "" {
		return gateway.IncomingMessage{}, false, nil
	}

	text, err := decodeTextContent(stringValue(msg.Content))
	if err != nil {
		return gateway.IncomingMessage{}, false, err
	}
	if strings.TrimSpace(text) == "" {
		return gateway.IncomingMessage{}, false, nil
	}

	return gateway.IncomingMessage{
		MessageID: strings.TrimSpace(stringValue(msg.MessageId)),
		GroupID:   groupID,
		Text:      text,
	}, true, nil
}

func decodeTextContent(content string) (string, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return "", err
	}
	return payload.Text, nil
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
