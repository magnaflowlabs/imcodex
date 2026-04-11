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

type postElement struct {
	Tag      string `json:"tag"`
	Text     string `json:"text"`
	ImageKey string `json:"image_key"`
	FileKey  string `json:"file_key"`
	FileName string `json:"file_name"`
}

type postBody struct {
	Title   string          `json:"title"`
	Content [][]postElement `json:"content"`
}

func NewReceiver(appID string, appSecret string, baseURL string, verificationToken string, encryptKey string, handler MessageHandler) *Receiver {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = larksdk.LarkBaseUrl
	}

	dispatcher := newEventDispatcher(handler, verificationToken, encryptKey)

	client := larkws.NewClient(
		appID,
		appSecret,
		larkws.WithDomain(baseURL),
		larkws.WithEventHandler(dispatcher),
		larkws.WithLogLevel(larkcore.LogLevelError),
	)

	return &Receiver{start: client.Start}
}

func newEventDispatcher(handler MessageHandler, verificationToken string, encryptKey string) *larkdispatcher.EventDispatcher {
	return larkdispatcher.NewEventDispatcher(strings.TrimSpace(verificationToken), strings.TrimSpace(encryptKey)).
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
	senderType := ""
	if sender := event.Event.Sender; sender != nil {
		senderType = stringValue(sender.SenderType)
	}

	return incomingMessageFromFields(
		strings.TrimSpace(stringValue(msg.ChatId)),
		strings.TrimSpace(stringValue(msg.MessageId)),
		stringValue(msg.ChatType),
		senderType,
		stringValue(msg.MessageType),
		stringValue(msg.Content),
	)
}

func listMessageToIncomingMessage(msg *larkim.Message) (gateway.IncomingMessage, bool, error) {
	if msg == nil {
		return gateway.IncomingMessage{}, false, nil
	}

	body := ""
	if msg.Body != nil {
		body = stringValue(msg.Body.Content)
	}
	senderType := ""
	if msg.Sender != nil {
		senderType = stringValue(msg.Sender.SenderType)
	}

	return incomingMessageFromFields(
		strings.TrimSpace(stringValue(msg.ChatId)),
		strings.TrimSpace(stringValue(msg.MessageId)),
		"group",
		senderType,
		stringValue(msg.MsgType),
		body,
	)
}

func incomingMessageFromFields(groupID string, messageID string, chatType string, senderType string, messageType string, content string) (gateway.IncomingMessage, bool, error) {
	if strings.TrimSpace(chatType) != "group" {
		return gateway.IncomingMessage{}, false, nil
	}
	switch strings.TrimSpace(senderType) {
	case "app", "bot":
		return gateway.IncomingMessage{}, false, nil
	}
	if strings.TrimSpace(groupID) == "" {
		return gateway.IncomingMessage{}, false, nil
	}

	incoming := gateway.IncomingMessage{
		MessageID: strings.TrimSpace(messageID),
		GroupID:   strings.TrimSpace(groupID),
	}

	switch strings.TrimSpace(messageType) {
	case "text":
		text, err := decodeTextContent(content)
		if err != nil {
			return gateway.IncomingMessage{}, false, err
		}
		incoming.Text = text
	case "post":
		text, attachments, err := decodePostContent(content)
		if err != nil {
			return gateway.IncomingMessage{}, false, err
		}
		incoming.Text = text
		incoming.Attachments = attachments
	case "image":
		image, err := decodeImageContent(content)
		if err != nil {
			return gateway.IncomingMessage{}, false, err
		}
		incoming.Attachments = []gateway.IncomingAttachment{image}
	case "file", "audio", "media", "video":
		file, err := decodeFileContent(content)
		if err != nil {
			return gateway.IncomingMessage{}, false, err
		}
		incoming.Attachments = []gateway.IncomingAttachment{file}
	default:
		return gateway.IncomingMessage{}, false, nil
	}

	if strings.TrimSpace(incoming.Text) == "" && len(incoming.Attachments) == 0 {
		return gateway.IncomingMessage{}, false, nil
	}

	return incoming, true, nil
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

func decodeImageContent(content string) (gateway.IncomingAttachment, error) {
	var payload struct {
		ImageKey string `json:"image_key"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return gateway.IncomingAttachment{}, err
	}
	return gateway.IncomingAttachment{
		ResourceType: "image",
		ResourceKey:  strings.TrimSpace(payload.ImageKey),
	}, nil
}

func decodeFileContent(content string) (gateway.IncomingAttachment, error) {
	var payload struct {
		FileKey  string `json:"file_key"`
		FileName string `json:"file_name"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return gateway.IncomingAttachment{}, err
	}
	return gateway.IncomingAttachment{
		ResourceType: "file",
		ResourceKey:  strings.TrimSpace(payload.FileKey),
		FileName:     strings.TrimSpace(payload.FileName),
	}, nil
}

func decodePostContent(content string) (string, []gateway.IncomingAttachment, error) {
	var direct postBody
	if err := json.Unmarshal([]byte(content), &direct); err == nil && (len(direct.Content) > 0 || strings.TrimSpace(direct.Title) != "") {
		return flattenPostBody(direct), collectPostAttachments(direct), nil
	}

	var localized map[string]postBody
	if err := json.Unmarshal([]byte(content), &localized); err != nil {
		return "", nil, err
	}
	for _, body := range localized {
		if len(body.Content) > 0 || strings.TrimSpace(body.Title) != "" {
			return flattenPostBody(body), collectPostAttachments(body), nil
		}
	}
	return "", nil, nil
}

func flattenPostBody(body postBody) string {
	parts := make([]string, 0, len(body.Content)+1)
	if strings.TrimSpace(body.Title) != "" {
		parts = append(parts, strings.TrimSpace(body.Title))
	}
	for _, row := range body.Content {
		var rowText strings.Builder
		for _, item := range row {
			switch item.Tag {
			case "text", "a", "at":
				rowText.WriteString(item.Text)
			}
		}
		text := strings.TrimSpace(rowText.String())
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func collectPostAttachments(body postBody) []gateway.IncomingAttachment {
	var attachments []gateway.IncomingAttachment
	for _, row := range body.Content {
		for _, item := range row {
			switch item.Tag {
			case "img":
				if strings.TrimSpace(item.ImageKey) != "" {
					attachments = append(attachments, gateway.IncomingAttachment{
						ResourceType: "image",
						ResourceKey:  strings.TrimSpace(item.ImageKey),
					})
				}
			case "file":
				if strings.TrimSpace(item.FileKey) != "" {
					attachments = append(attachments, gateway.IncomingAttachment{
						ResourceType: "file",
						ResourceKey:  strings.TrimSpace(item.FileKey),
						FileName:     strings.TrimSpace(item.FileName),
					})
				}
			}
		}
	}
	return attachments
}

func stringValue(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
