// Package telegram is a minimal Telegram Bot API client + Channel
// implementation for toolyard's chat approval notifications. It uses outbound
// long-polling (getUpdates) so no public URL / inbound webhook is needed —
// the right fit for self-hosted / NAT deployments. Zero third-party deps.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const apiBase = "https://api.telegram.org"

// Client is a thin Bot API wrapper bound to a single bot token.
type Client struct {
	token string
	base  string
	http  *http.Client
	log   *slog.Logger
}

// NewClient builds a client. httpClient may be nil (a default is used). The
// timeout must exceed the long-poll timeout in getUpdates.
func NewClient(token string, httpClient *http.Client, log *slog.Logger) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 65 * time.Second}
	}
	return &Client{token: token, base: apiBase, http: httpClient, log: log}
}

// SetBaseURL overrides the API base (used by tests with httptest.Server).
func (c *Client) SetBaseURL(base string) { c.base = base }

// apiError models Telegram's error envelope, including 429 retry_after.
type apiError struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// call invokes a Bot API method and decodes result into out. On HTTP 429 it
// honors retry_after once and retries a single time.
func (c *Client) call(ctx context.Context, method string, params url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		endpoint := fmt.Sprintf("%s/bot%s/%s", c.base, c.token, method)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint,
			bytes.NewBufferString(params.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			var ae apiError
			_ = json.Unmarshal(body, &ae)
			wait := time.Duration(ae.Parameters.RetryAfter) * time.Second
			if wait <= 0 {
				wait = time.Second
			}
			if c.log != nil {
				c.log.Warn("telegram 429", "method", method, "retry_after", wait.String())
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			var ae apiError
			_ = json.Unmarshal(body, &ae)
			return fmt.Errorf("telegram %s: HTTP %d: %s", method, resp.StatusCode, ae.Description)
		}
		if out == nil {
			return nil
		}
		env := struct {
			OK     bool            `json:"ok"`
			Result json.RawMessage `json:"result"`
		}{}
		if err := json.Unmarshal(body, &env); err != nil {
			return fmt.Errorf("telegram %s: decode: %w", method, err)
		}
		if !env.OK {
			return fmt.Errorf("telegram %s: not ok: %s", method, string(body))
		}
		return json.Unmarshal(env.Result, out)
	}
	return fmt.Errorf("telegram %s: retried after 429 and gave up", method)
}

// User is the subset of Telegram's User we use.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// GetMe returns the bot's own identity — used to validate a token and learn
// the bot username for the deep link.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", url.Values{}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// DeleteWebhook clears any inbound webhook so getUpdates long-polling works.
// drop_pending_updates avoids replaying a backlog on first start.
func (c *Client) DeleteWebhook(ctx context.Context) error {
	v := url.Values{}
	v.Set("drop_pending_updates", "true")
	return c.call(ctx, "deleteWebhook", v, nil)
}

// SentMessage is the subset of a sent/edited message we persist.
type SentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

// InlineButton is one inline keyboard button.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
	URL          string `json:"url,omitempty"`
}

// SendMessage posts a message, optionally with a single row of inline buttons.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, buttons []InlineButton) (*SentMessage, error) {
	v := url.Values{}
	v.Set("chat_id", strconv.FormatInt(chatID, 10))
	v.Set("text", text)
	if len(buttons) > 0 {
		v.Set("reply_markup", inlineKeyboardJSON(buttons))
	}
	var m SentMessage
	if err := c.call(ctx, "sendMessage", v, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EditMessageText edits a message in place. Passing no buttons strips the
// keyboard (the decision is final).
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string, buttons []InlineButton) error {
	v := url.Values{}
	v.Set("chat_id", strconv.FormatInt(chatID, 10))
	v.Set("message_id", strconv.FormatInt(messageID, 10))
	v.Set("text", text)
	if len(buttons) > 0 {
		v.Set("reply_markup", inlineKeyboardJSON(buttons))
	}
	// editMessageText returns the edited message; we ignore the body.
	err := c.call(ctx, "editMessageText", v, nil)
	return err
}

// AnswerCallbackQuery acknowledges a button tap (shows the optional toast and
// stops the client's loading spinner).
func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackID, text string) error {
	v := url.Values{}
	v.Set("callback_query_id", callbackID)
	if text != "" {
		v.Set("text", text)
	}
	return c.call(ctx, "answerCallbackQuery", v, nil)
}

// Update is one long-poll update. Only the fields we act on are modeled.
type Update struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      User   `json:"from"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	CallbackQuery *struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		From    User   `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// GetUpdates long-polls for updates after offset with the given timeout
// (seconds). The HTTP client timeout must exceed timeoutSec.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]Update, error) {
	v := url.Values{}
	v.Set("offset", strconv.FormatInt(offset, 10))
	v.Set("timeout", strconv.Itoa(timeoutSec))
	v.Set("allowed_updates", `["message","callback_query"]`)
	var ups []Update
	if err := c.call(ctx, "getUpdates", v, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

func inlineKeyboardJSON(buttons []InlineButton) string {
	type kb struct {
		InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
	}
	b, _ := json.Marshal(kb{InlineKeyboard: [][]InlineButton{buttons}})
	return string(b)
}
