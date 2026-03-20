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

	dispatcher := newEventDispatcher(fakeMessageHandler{})
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
