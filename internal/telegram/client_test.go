package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientSendTextToChat(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	defer server.Close()

	client := NewClient("123:abc", server.URL)
	if err := client.SendTextToChat(context.Background(), "-1001", "hello"); err != nil {
		t.Fatalf("SendTextToChat() error = %v", err)
	}
	if gotPath != "/bot123:abc/sendMessage" {
		t.Fatalf("path = %q, want sendMessage path", gotPath)
	}
	if gotBody["chat_id"] != "-1001" || gotBody["text"] != "hello" {
		t.Fatalf("body = %#v, want chat_id/text", gotBody)
	}
}

func TestClientDownloadMessageResource(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_path":"photos/test.jpg"}}`))
		case strings.Contains(r.URL.Path, "/file/bot123:abc/photos/test.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("jpeg-bytes"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient("123:abc", server.URL)
	resource, err := client.DownloadMessageResource(context.Background(), "", "image", "file_123")
	if err != nil {
		t.Fatalf("DownloadMessageResource() error = %v", err)
	}
	if got, want := string(resource.Data), "jpeg-bytes"; got != want {
		t.Fatalf("resource.Data = %q, want %q", got, want)
	}
	if got, want := resource.FileName, "test.jpg"; got != want {
		t.Fatalf("resource.FileName = %q, want %q", got, want)
	}
}

func TestNewClientTimeoutSupportsLongPolling(t *testing.T) {
	t.Parallel()

	client := NewClient("123:abc", "")
	minTimeout := time.Duration(pollTimeoutSeconds)*time.Second + longPollTimeoutBuffer
	if client.httpClient.Timeout < minTimeout {
		t.Fatalf("http timeout = %v, want >= %v", client.httpClient.Timeout, minTimeout)
	}
}

func TestClientRedactsTokenFromErrors(t *testing.T) {
	t.Parallel()

	client := NewClient("123:abc", "http://127.0.0.1:1")
	err := client.SendTextToChat(context.Background(), "-1001", "hello")
	if err == nil {
		t.Fatal("SendTextToChat() error = nil, want transport failure")
	}
	if strings.Contains(err.Error(), "123:abc") {
		t.Fatalf("error leaked bot token: %v", err)
	}
	if !strings.Contains(err.Error(), redactedToken) {
		t.Fatalf("error = %v, want redacted token marker", err)
	}
}
