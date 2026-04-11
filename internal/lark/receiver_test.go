package lark

import (
	"context"
	"testing"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

func TestEventToIncomingMessageAcceptsGroupText(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_1"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("text"),
				Content:     stringPtr(`{"text":" /new "}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if msg.MessageID != "om_1" || msg.GroupID != "oc_1" || msg.Text != " /new " {
		t.Fatalf("msg = %#v, want group=text pair", msg)
	}
}

func TestEventToIncomingMessagePreservesTrailingNewlines(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_2"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("text"),
				Content:     stringPtr(`{"text":"line1\n\n"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if msg.Text != "line1\n\n" {
		t.Fatalf("msg.Text = %q, want preserved trailing newlines", msg.Text)
	}
}

func TestEventToIncomingMessageAcceptsGroupImage(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_img"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("image"),
				Content:     stringPtr(`{"image_key":"img_v3_123"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "image" || msg.Attachments[0].ResourceKey != "img_v3_123" {
		t.Fatalf("msg = %#v, want image attachment", msg)
	}
}

func TestEventToIncomingMessageAcceptsGroupFile(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_file"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("file"),
				Content:     stringPtr(`{"file_key":"file_v3_123","file_name":"report.pdf"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "file" || msg.Attachments[0].ResourceKey != "file_v3_123" || msg.Attachments[0].FileName != "report.pdf" {
		t.Fatalf("msg = %#v, want file attachment", msg)
	}
}

func TestEventToIncomingMessageAcceptsGroupPostWithTextAndImage(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_post"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("post"),
				Content:     stringPtr(`{"content":[[{"tag":"img","image_key":"img_v3_post"}],[{"tag":"text","text":"你能看到我发的图片吧？"}]],"title":""}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "你能看到我发的图片吧？"; got != want {
		t.Fatalf("msg.Text = %q, want %q", got, want)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "image" || msg.Attachments[0].ResourceKey != "img_v3_post" {
		t.Fatalf("msg = %#v, want post image attachment", msg)
	}
}

func TestEventToIncomingMessageAcceptsGroupPostWithTitleOnly(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_post_title"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("post"),
				Content:     stringPtr(`{"title":"title only","content":[]}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "title only"; got != want {
		t.Fatalf("msg.Text = %q, want %q", got, want)
	}
}

func TestListMessageToIncomingMessageAcceptsGroupPostWithTextAndImage(t *testing.T) {
	t.Parallel()

	msg, ok, err := listMessageToIncomingMessage(&larkim.Message{
		MessageId: stringPtr("om_post"),
		ChatId:    stringPtr("oc_1"),
		MsgType:   stringPtr("post"),
		Sender:    &larkim.Sender{SenderType: stringPtr("user")},
		Body: &larkim.MessageBody{
			Content: stringPtr(`{"content":[[{"tag":"img","image_key":"img_v3_post"}],[{"tag":"text","text":"你能看到我发的图片吧？"}]],"title":""}`),
		},
	})
	if err != nil {
		t.Fatalf("listMessageToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "你能看到我发的图片吧？"; got != want {
		t.Fatalf("msg.Text = %q, want %q", got, want)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "image" || msg.Attachments[0].ResourceKey != "img_v3_post" {
		t.Fatalf("msg = %#v, want post image attachment", msg)
	}
}

func TestEventToIncomingMessageTreatsAudioAsFileAttachment(t *testing.T) {
	t.Parallel()

	msg, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Sender: &larkim.EventSender{SenderType: stringPtr("user")},
			Message: &larkim.EventMessage{
				MessageId:   stringPtr("om_audio"),
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("audio"),
				Content:     stringPtr(`{"file_key":"file_v3_audio","file_name":"voice.m4a"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "file" || msg.Attachments[0].ResourceKey != "file_v3_audio" || msg.Attachments[0].FileName != "voice.m4a" {
		t.Fatalf("msg = %#v, want file-like attachment for audio", msg)
	}
}

func TestEventToIncomingMessageIgnoresNonGroup(t *testing.T) {
	t.Parallel()

	_, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("p2p"),
				MessageType: stringPtr("text"),
				Content:     stringPtr(`{"text":"hello"}`),
			},
		},
	})
	if err != nil {
		t.Fatalf("eventToIncomingMessage() error = %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}

func TestEventToIncomingMessageRejectsBadTextPayload(t *testing.T) {
	t.Parallel()

	_, ok, err := eventToIncomingMessage(&larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				ChatId:      stringPtr("oc_1"),
				ChatType:    stringPtr("group"),
				MessageType: stringPtr("text"),
				Content:     stringPtr("{"),
			},
		},
	})
	if err == nil {
		t.Fatal("eventToIncomingMessage() error = nil, want decode error")
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}

func TestEventDispatcherIgnoresBotP2PEntered(t *testing.T) {
	t.Parallel()

	dispatcher := newEventDispatcher(fakeMessageHandler{}, "", "")
	_, err := dispatcher.Do(context.Background(), []byte(`{
		"schema":"2.0",
		"header":{"event_type":"im.chat.access_event.bot_p2p_chat_entered_v1"},
		"event":{}
	}`))
	if err != nil {
		t.Fatalf("dispatcher.Do() error = %v, want nil", err)
	}
}

type fakeMessageHandler struct{}

func (fakeMessageHandler) HandleMessage(context.Context, gateway.IncomingMessage) error {
	return nil
}

func stringPtr(v string) *string {
	return &v
}
