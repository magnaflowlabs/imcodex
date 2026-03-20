package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	larksdk "github.com/larksuite/oapi-sdk-go/v3"
)

type Client struct {
	baseURL    string
	appID      string
	appSecret  string
	httpClient *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

func NewClient(appID string, appSecret string, baseURL string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = larksdk.LarkBaseUrl
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		appID:      appID,
		appSecret:  appSecret,
		httpClient: &http.Client{Timeout: 20 * time.Second},
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
	if c.token != "" && time.Now().Before(c.tokenExpiry) {
		token := c.token
		c.mu.Unlock()
		return token, nil
	}
	c.mu.Unlock()

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

	c.mu.Lock()
	c.token = out.TenantAccessToken
	c.tokenExpiry = time.Now().Add(time.Duration(expireSeconds) * time.Second)
	c.mu.Unlock()
	return out.TenantAccessToken, nil
}

func marshalTextContent(text string) string {
	data, _ := json.Marshal(map[string]string{"text": text})
	return string(data)
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
