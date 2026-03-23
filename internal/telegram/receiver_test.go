package telegram

import "testing"

func TestUpdateToIncomingMessageAcceptsText(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		UpdateID: 1,
		Message: &Message{
			MessageID: 9,
			Chat:      Chat{ID: -100123, Type: "supergroup"},
			From:      &User{IsBot: false},
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.GroupID, "-100123"; got != want {
		t.Fatalf("GroupID = %q, want %q", got, want)
	}
	if got, want := msg.Text, "hello"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

func TestUpdateToIncomingMessageAcceptsPhotoWithCaption(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 10,
			Chat:      Chat{ID: -100123, Type: "group"},
			Caption:   "inspect this",
			Photo: []Photo{
				{FileID: "small", Width: 10, Height: 10},
				{FileID: "large", Width: 20, Height: 20},
			},
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if got, want := msg.Text, "inspect this"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].ResourceType != "image" || msg.Attachments[0].ResourceKey != "large" {
		t.Fatalf("Attachments = %#v, want largest photo", msg.Attachments)
	}
}

func TestUpdateToIncomingMessageAcceptsDocument(t *testing.T) {
	t.Parallel()

	msg, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 11,
			Chat:      Chat{ID: -100123, Type: "supergroup"},
			Document:  &Document{FileID: "file_1", FileName: "report.pdf"},
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].FileName != "report.pdf" {
		t.Fatalf("Attachments = %#v, want one document attachment", msg.Attachments)
	}
}

func TestUpdateToIncomingMessageIgnoresBotsAndPrivateChats(t *testing.T) {
	t.Parallel()

	_, ok, err := updateToIncomingMessage(Update{
		Message: &Message{
			MessageID: 12,
			Chat:      Chat{ID: 123, Type: "private"},
			From:      &User{IsBot: true},
			Text:      "hello",
		},
	})
	if err != nil {
		t.Fatalf("updateToIncomingMessage() error = %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false")
	}
}
