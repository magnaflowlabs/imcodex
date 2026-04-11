package lark

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func TestClientListChatMessagesSinceSkipsDecodeErrors(t *testing.T) {
	t.Parallel()

	client := &Client{
		listMessages: func(context.Context, *larkim.ListMessageReq, ...larkcore.RequestOptionFunc) (*larkim.ListMessageResp, error) {
			hasMore := false
			return &larkim.ListMessageResp{
				Data: &larkim.ListMessageRespData{
					HasMore: &hasMore,
					Items: []*larkim.Message{
						{
							MessageId: stringPtr("om_bad"),
							ChatId:    stringPtr("oc_1"),
							MsgType:   stringPtr("text"),
							Sender:    &larkim.Sender{SenderType: stringPtr("user")},
							Body:      &larkim.MessageBody{Content: stringPtr("{")},
						},
						{
							MessageId:  stringPtr("om_good"),
							ChatId:     stringPtr("oc_1"),
							MsgType:    stringPtr("text"),
							CreateTime: stringPtr("123456"),
							Sender:     &larkim.Sender{SenderType: stringPtr("user")},
							Body:       &larkim.MessageBody{Content: stringPtr(`{"text":"hello"}`)},
						},
					},
				},
			}, nil
		},
	}

	listed, err := client.ListChatMessagesSince(context.Background(), "oc_1", 0)
	if err != nil {
		t.Fatalf("ListChatMessagesSince() error = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len(listed) = %d, want 1", len(listed))
	}
	if got := listed[0].Message.MessageID; got != "om_good" {
		t.Fatalf("listed[0].Message.MessageID = %q, want om_good", got)
	}
}

func TestClientTenantAccessTokenRefreshesOnceConcurrently(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	tokenCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/open-apis/auth/v3/tenant_access_token/internal" {
			t.Fatalf("path = %q, want tenant token endpoint", r.URL.Path)
		}
		mu.Lock()
		tokenCalls++
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"expire":7200,"tenant_access_token":"tenant_token"}`)
	}))
	defer server.Close()

	client := NewClient("cli_app", "cli_secret", server.URL)
	client.httpClient = server.Client()

	const workers = 4
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := client.tenantAccessToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if token != "tenant_token" {
				errs <- io.ErrUnexpectedEOF
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("tenantAccessToken() error = %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 1 {
		t.Fatalf("tokenCalls = %d, want 1", tokenCalls)
	}
}
