package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

type Client struct {
	baseURL      string
	appID        string
	appSecret    string
	httpClient   *http.Client
	listMessages func(context.Context, *larkim.ListMessageReq, ...larkcore.RequestOptionFunc) (*larkim.ListMessageResp, error)

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewClient(appID string, appSecret string, baseURL string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = larksdk.LarkBaseUrl
	}
	sdkClient := larksdk.NewClient(
		strings.TrimSpace(appID),
		strings.TrimSpace(appSecret),
		larksdk.WithOpenBaseUrl(baseURL),
	)
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		appID:        strings.TrimSpace(appID),
		appSecret:    strings.TrimSpace(appSecret),
		httpClient:   &http.Client{Timeout: 20 * time.Second},
		listMessages: sdkClient.Im.V1.Message.List,
	}
}

func (c *Client) SendTextToChat(ctx context.Context, groupID string, text string) error {
	query := url.Values{}
	query.Set("receive_id_type", "chat_id")

	_, err := c.post(ctx, "/open-apis/im/v1/messages?"+query.Encode(), map[string]any{
		"receive_id": groupID,
		"msg_type":   "text",
		"content":    marshalTextContent(text),
	})
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) DownloadMessageResource(ctx context.Context, messageID string, resourceType string, resourceKey string) (gateway.DownloadedResource, error) {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return gateway.DownloadedResource{}, err
	}

	resourceType = strings.TrimSpace(resourceType)
	resourceKey = strings.TrimSpace(resourceKey)
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || resourceType == "" || resourceKey == "" {
		return gateway.DownloadedResource{}, fmt.Errorf("message resource identifiers are incomplete")
	}

	query := url.Values{}
	query.Set("type", resourceType)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fmt.Sprintf("%s/open-apis/im/v1/messages/%s/resources/%s?%s", c.baseURL, url.PathEscape(messageID), url.PathEscape(resourceKey), query.Encode()),
		nil,
	)
	if err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("build resource request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("call lark api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return gateway.DownloadedResource{}, fmt.Errorf("read resource body: %w", err)
		}
		return gateway.DownloadedResource{
			Data:        data,
			FileName:    fileNameFromDisposition(resp.Header.Get("Content-Disposition")),
			ContentType: resp.Header.Get("Content-Type"),
		}, nil
	}

	var out messageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("decode error response: %w", err)
	}
	return gateway.DownloadedResource{}, fmt.Errorf("lark api failed: http=%d code=%d msg=%s", resp.StatusCode, out.Code, out.Msg)
}

func (c *Client) ListChatMessagesSince(ctx context.Context, groupID string, startAtMillis int64) ([]ListedMessage, error) {
	groupID = strings.TrimSpace(groupID)
	if groupID == "" {
		return nil, fmt.Errorf("group id is empty")
	}

	startAtSeconds := startAtMillis/1000 - 1
	if startAtSeconds < 0 {
		startAtSeconds = 0
	}

	var (
		pageToken string
		out       []ListedMessage
	)
	for {
		builder := larkim.NewListMessageReqBuilder().
			ContainerIdType("chat").
			ContainerId(groupID).
			StartTime(fmt.Sprint(startAtSeconds)).
			SortType(larkim.SortTypeListMessageByCreateTimeAsc).
			PageSize(50)
		if pageToken != "" {
			builder.PageToken(pageToken)
		}

		resp, err := c.listMessages(ctx, builder.Build())
		if err != nil {
			return nil, fmt.Errorf("list messages: %w", err)
		}
		if resp == nil || resp.Data == nil {
			return out, nil
		}

		for _, item := range resp.Data.Items {
			msg, ok, err := listMessageToIncomingMessage(item)
			if err != nil {
				continue
			}
			if !ok {
				continue
			}
			out = append(out, ListedMessage{
				Message:         msg,
				CreatedAtMillis: parseMillis(stringValue(item.CreateTime)),
			})
		}

		if resp.Data.HasMore == nil || !*resp.Data.HasMore {
			return out, nil
		}
		pageToken = strings.TrimSpace(stringValue(resp.Data.PageToken))
		if pageToken == "" {
			return out, nil
		}
	}
}

func (c *Client) post(ctx context.Context, path string, payload any) (*messageAPIResponse, error) {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call lark api: %w", err)
	}
	defer resp.Body.Close()

	var out messageAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode >= 300 || out.Code != 0 {
		return nil, fmt.Errorf("lark api failed: http=%d code=%d msg=%s", resp.StatusCode, out.Code, out.Msg)
	}
	return &out, nil
}

func (c *Client) tenantAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		token := c.token
		return token, nil
	}

	body, err := json.Marshal(map[string]string{
		"app_id":     c.appID,
		"app_secret": c.appSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal token request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("request token: %w", err)
	}
	defer resp.Body.Close()

	var out tokenAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if resp.StatusCode >= 300 || out.Code != 0 {
		return "", fmt.Errorf("tenant access token failed: http=%d code=%d msg=%s", resp.StatusCode, out.Code, out.Msg)
	}

	expireSeconds := out.Expire - 60
	if expireSeconds < 60 {
		expireSeconds = out.Expire
	}

	c.token = out.TenantAccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(expireSeconds) * time.Second)
	return out.TenantAccessToken, nil
}

func marshalTextContent(text string) string {
	data, _ := json.Marshal(map[string]string{"text": text})
	return string(data)
}

func fileNameFromDisposition(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(params["filename"])
}

func parseMillis(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func messageIDOf(msg *larkim.Message) string {
	if msg == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(msg.MessageId))
}

type tokenAPIResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	Expire            int    `json:"expire"`
	TenantAccessToken string `json:"tenant_access_token"`
}

type messageAPIResponse struct {
	Code int      `json:"code"`
	Msg  string   `json:"msg"`
	Data struct{} `json:"data"`
}
