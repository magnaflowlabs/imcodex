package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientSendTextToChat(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	client := NewClient("123:abc", "https://example.invalid")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		return jsonResponse(r, `{"ok":true,"result":{"message_id":1}}`), nil
	})}
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

func TestClientSendTextToChatWithID(t *testing.T) {
	t.Parallel()

	client := NewClient("123:abc", "https://example.invalid")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResponse(r, `{"ok":true,"result":{"message_id":42}}`), nil
	})}
	msg, err := client.SendTextToChatWithID(context.Background(), "-1001", "hello")
	if err != nil {
		t.Fatalf("SendTextToChatWithID() error = %v", err)
	}
	if got, want := msg.MessageID, "42"; got != want {
		t.Fatalf("msg.MessageID = %q, want %q", got, want)
	}
}

func TestClientEditTextInChat(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody map[string]any
	client := NewClient("123:abc", "https://example.invalid")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("Decode() error = %v", err)
		}
		return jsonResponse(r, `{"ok":true,"result":{"message_id":42}}`), nil
	})}
	if err := client.EditTextInChat(context.Background(), "-1001", "42", "updated"); err != nil {
		t.Fatalf("EditTextInChat() error = %v", err)
	}
	if gotPath != "/bot123:abc/editMessageText" {
		t.Fatalf("path = %q, want editMessageText path", gotPath)
	}
	if gotBody["chat_id"] != "-1001" || gotBody["text"] != "updated" {
		t.Fatalf("body = %#v, want chat_id/text", gotBody)
	}
	if got, ok := gotBody["message_id"].(float64); !ok || int64(got) != 42 {
		t.Fatalf("body = %#v, want message_id=42", gotBody)
	}
}

func TestClientDownloadMessageResource(t *testing.T) {
	t.Parallel()

	client := NewClient("123:abc", "https://example.invalid")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			return jsonResponse(r, `{"ok":true,"result":{"file_path":"photos/test.jpg"}}`), nil
		case strings.Contains(r.URL.Path, "/file/bot123:abc/photos/test.jpg"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
				Body:       io.NopCloser(strings.NewReader("jpeg-bytes")),
				Request:    r,
			}, nil
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return nil, nil
		}
	})}
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func jsonResponse(r *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}
}
