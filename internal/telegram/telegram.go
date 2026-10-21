// Package telegram is a thin client for the few Bot API methods the gateway
// uses. One Bot value per token; the gateway may hold several.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Bot struct {
	token   string
	baseURL string
	http    *http.Client
}

func New(token string) *Bot {
	return &Bot{
		token:   token,
		baseURL: "https://api.telegram.org",
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Error is a refusal from Telegram, as opposed to a network failure. Code is
// Telegram's own error_code (400 for a bad chat, 403 when the bot was
// blocked, 429 when flooding), so callers can pass it on meaningfully.
type Error struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *Error) Error() string {
	return fmt.Sprintf("telegram %s refused (%d): %s", e.Method, e.Code, e.Description)
}

// Send posts a text message and returns its id.
func (b *Bot) Send(ctx context.Context, chat int64, text, parseMode string, silent bool) (int64, error) {
	payload := map[string]any{
		"chat_id":              chat,
		"text":                 text,
		"link_preview_options": map[string]any{"is_disabled": true},
	}
	if parseMode != "" {
		payload["parse_mode"] = parseMode
	}
	if silent {
		payload["disable_notification"] = true
	}
	var out struct {
		MessageID int64 `json:"message_id"`
	}
	if err := b.call(ctx, "sendMessage", payload, &out); err != nil {
		return 0, err
	}
	return out.MessageID, nil
}

// SetWebhook points Telegram at url. allowed may be nil for Telegram's default
// set of update kinds.
func (b *Bot) SetWebhook(ctx context.Context, url, secret string, allowed []string) error {
	payload := map[string]any{
		"url":          url,
		"secret_token": secret,
	}
	if allowed != nil {
		payload["allowed_updates"] = allowed
	}
	return b.call(ctx, "setWebhook", payload, nil)
}

func (b *Bot) DeleteWebhook(ctx context.Context) error {
	return b.call(ctx, "deleteWebhook", map[string]any{}, nil)
}

// WebhookInfo is returned as Telegram sends it, for printing.
func (b *Bot) WebhookInfo(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := b.call(ctx, "getWebhookInfo", map[string]any{}, &out)
	return out, err
}

// Me returns the bot's username: the cheapest proof the token works.
func (b *Bot) Me(ctx context.Context) (string, error) {
	var out struct {
		Username string `json:"username"`
	}
	err := b.call(ctx, "getMe", map[string]any{}, &out)
	return out.Username, err
}

func (b *Bot) call(ctx context.Context, method string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/bot%s/%s", b.baseURL, b.token, method), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.http.Do(req)
	if err != nil {
		// The URL carries the token; never let it into an error message.
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env struct {
		OK          bool            `json:"ok"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("telegram %s: bad response (HTTP %d)", method, resp.StatusCode)
	}
	if !env.OK {
		return &Error{Method: method, Code: env.ErrorCode, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}
