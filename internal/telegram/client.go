package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/magnaflowlabs/imcodex/internal/gateway"
)

const defaultBaseURL = "https://api.telegram.org"

const (
	defaultRequestTimeout = 75 * time.Second
	longPollTimeoutBuffer = 15 * time.Second
	defaultThrottleEvery  = 40 * time.Millisecond
	defaultActionBackoff  = time.Minute
	redactedToken         = "[REDACTED]"
)

type Client struct {
	baseURL         string
	botToken        string
	httpClient      *http.Client
	throttleEvery   time.Duration
	actionBackoff   time.Duration
	throttleMu      sync.Mutex
	lastRequestAt   time.Time
	actionStateMu   sync.Mutex
	actionBackoffTo time.Time
}

type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message,omitempty"`
}

type Message struct {
	MessageID int64     `json:"message_id"`
	Chat      Chat      `json:"chat"`
	From      *User     `json:"from,omitempty"`
	Text      string    `json:"text,omitempty"`
	Caption   string    `json:"caption,omitempty"`
	Photo     []Photo   `json:"photo,omitempty"`
	Document  *Document `json:"document,omitempty"`
	Audio     *Document `json:"audio,omitempty"`
	Video     *Document `json:"video,omitempty"`
	Voice     *Document `json:"voice,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type User struct {
	IsBot bool `json:"is_bot"`
}

type Photo struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size,omitempty"`
}

type Document struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int64  `json:"file_size,omitempty"`
}

type File struct {
	FilePath string `json:"file_path"`
}

func NewClient(botToken string, baseURL string) *Client {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		botToken:      strings.TrimSpace(botToken),
		httpClient:    &http.Client{Timeout: defaultRequestTimeout},
		throttleEvery: defaultThrottleEvery,
		actionBackoff: defaultActionBackoff,
	}
}

func (c *Client) SendTextToChat(ctx context.Context, groupID string, text string) error {
	_, err := c.SendTextToChatWithID(ctx, groupID, text)
	return err
}

func (c *Client) SendTextToChatWithID(ctx context.Context, groupID string, text string) (gateway.SentMessage, error) {
	var result struct {
		MessageID int64 `json:"message_id"`
	}
	err := c.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  strings.TrimSpace(groupID),
		"text":                     text,
		"disable_web_page_preview": true,
	}, &result)
	if err != nil {
		return gateway.SentMessage{}, err
	}
	return gateway.SentMessage{MessageID: strconv.FormatInt(result.MessageID, 10)}, nil
}

func (c *Client) EditTextInChat(ctx context.Context, groupID string, messageID string, text string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil {
		return fmt.Errorf("telegram message id is invalid: %w", err)
	}
	return c.call(ctx, "editMessageText", map[string]any{
		"chat_id":                  strings.TrimSpace(groupID),
		"message_id":               id,
		"text":                     text,
		"disable_web_page_preview": true,
	}, nil)
}

func (c *Client) DeleteMessageInChat(ctx context.Context, groupID string, messageID string) error {
	id, err := strconv.ParseInt(strings.TrimSpace(messageID), 10, 64)
	if err != nil {
		return fmt.Errorf("telegram message id is invalid: %w", err)
	}
	return c.call(ctx, "deleteMessage", map[string]any{
		"chat_id":    strings.TrimSpace(groupID),
		"message_id": id,
	}, nil)
}

func (c *Client) SendChatAction(ctx context.Context, groupID string, action string) error {
	if c == nil {
		return nil
	}
	if c.chatActionSuppressed(time.Now()) {
		return nil
	}
	err := c.call(ctx, "sendChatAction", map[string]any{
		"chat_id": strings.TrimSpace(groupID),
		"action":  strings.TrimSpace(action),
	}, nil)
	if err == nil {
		c.clearChatActionBackoff()
		return nil
	}
	if isTelegramUnauthorized(err) {
		c.setChatActionBackoff(time.Now())
	}
	return err
}

func (c *Client) DownloadMessageResource(ctx context.Context, _ string, _ string, resourceKey string) (gateway.DownloadedResource, error) {
	resourceKey = strings.TrimSpace(resourceKey)
	if resourceKey == "" {
		return gateway.DownloadedResource{}, fmt.Errorf("telegram file id is empty")
	}

	var file File
	if err := c.call(ctx, "getFile", map[string]any{"file_id": resourceKey}, &file); err != nil {
		return gateway.DownloadedResource{}, err
	}
	if strings.TrimSpace(file.FilePath) == "" {
		return gateway.DownloadedResource{}, fmt.Errorf("telegram getFile returned empty file_path")
	}

	fileURL := c.baseURL + "/file/bot" + c.botToken + "/" + url.PathEscape(strings.TrimLeft(file.FilePath, "/"))
	fileURL = strings.ReplaceAll(fileURL, "%2F", "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("build telegram file request: %s", c.redact(err.Error()))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("download telegram file: %s", c.redact(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return gateway.DownloadedResource{}, fmt.Errorf("telegram file download failed: http=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return gateway.DownloadedResource{}, fmt.Errorf("read telegram file: %w", err)
	}

	return gateway.DownloadedResource{
		Data:        data,
		FileName:    path.Base(strings.TrimSpace(file.FilePath)),
		ContentType: resp.Header.Get("Content-Type"),
	}, nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	if timeoutSeconds <= 0 {
		timeoutSeconds = 30
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second+longPollTimeoutBuffer)
	defer cancel()
	var updates []Update
	err := c.call(callCtx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeoutSeconds,
		"allowed_updates": []string{"message"},
	}, &updates)
	if err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	if strings.TrimSpace(c.botToken) == "" {
		return fmt.Errorf("telegram bot token is empty")
	}
	if err := c.waitForThrottle(ctx); err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal telegram request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/bot"+c.botToken+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build telegram request: %s", c.redact(err.Error()))
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call telegram api: %s", c.redact(err.Error()))
	}
	defer resp.Body.Close()

	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode telegram response: %w", err)
	}
	if resp.StatusCode >= 300 || !envelope.OK {
		errText := fmt.Sprintf("telegram api failed: http=%d code=%d desc=%s", resp.StatusCode, envelope.ErrorCode, envelope.Description)
		if envelope.Parameters.RetryAfter > 0 {
			errText += " retry_after=" + strconv.Itoa(envelope.Parameters.RetryAfter)
		}
		return errors.New(errText)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode telegram result: %w", err)
	}
	return nil
}

func (c *Client) redact(text string) string {
	token := strings.TrimSpace(c.botToken)
	if token == "" {
		return text
	}
	text = strings.ReplaceAll(text, "/file/bot"+token+"/", "/file/bot"+redactedToken+"/")
	text = strings.ReplaceAll(text, "/bot"+token+"/", "/bot"+redactedToken+"/")
	return strings.ReplaceAll(text, token, redactedToken)
}

func (c *Client) waitForThrottle(ctx context.Context) error {
	if c == nil || c.throttleEvery <= 0 {
		return nil
	}
	c.throttleMu.Lock()

	now := time.Now()
	next := c.lastRequestAt.Add(c.throttleEvery)
	if c.lastRequestAt.IsZero() || !next.After(now) {
		c.lastRequestAt = now
		c.throttleMu.Unlock()
		return nil
	}
	wait := next.Sub(now)
	c.lastRequestAt = next
	c.throttleMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) chatActionSuppressed(now time.Time) bool {
	if c == nil {
		return true
	}
	c.actionStateMu.Lock()
	defer c.actionStateMu.Unlock()
	return !c.actionBackoffTo.IsZero() && c.actionBackoffTo.After(now)
}

func (c *Client) setChatActionBackoff(now time.Time) {
	if c == nil || c.actionBackoff <= 0 {
		return
	}
	c.actionStateMu.Lock()
	defer c.actionStateMu.Unlock()
	c.actionBackoffTo = now.Add(c.actionBackoff)
}

func (c *Client) clearChatActionBackoff() {
	if c == nil {
		return
	}
	c.actionStateMu.Lock()
	defer c.actionStateMu.Unlock()
	c.actionBackoffTo = time.Time{}
}

func isTelegramUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "code=401") || strings.Contains(text, "http=401")
}
