package telegram

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// RunPoller long-polls getUpdates and handles two kinds of update:
//   - "/start <code>" messages → pairing (binds chat_id + user_id)
//   - legacy callback_query button taps → review-in-Inbox notice
//
// It persists the next update offset in settings so a restart doesn't replay
// already-processed updates. Shaped as a goroutines.Supervise fn (blocking,
// returns on ctx cancel). It self-throttles to avoid a hot loop when the
// channel isn't configured.
func (s *Service) RunPoller(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Only poll when a token is configured (pairing can happen before a
		// chat is bound, so we don't require Ready() here).
		if !s.settings.GetBool(KeyEnabled) || s.settings.GetString(KeyBotToken, "") == "" {
			if sleep(ctx, 5*time.Second) != nil {
				return ctx.Err()
			}
			continue
		}
		c, err := s.client(ctx)
		if err != nil {
			s.log.Warn("poller: build client", "err", err.Error())
			if sleep(ctx, 5*time.Second) != nil {
				return ctx.Err()
			}
			continue
		}
		offset := int64(s.settings.GetInt(KeyUpdateOffset, 0))
		updates, err := c.GetUpdates(ctx, offset, longPollTimeS)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("poller: getUpdates", "err", err.Error())
			if sleep(ctx, 3*time.Second) != nil {
				return ctx.Err()
			}
			continue
		}
		maxID := offset - 1
		for i := range updates {
			u := updates[i]
			if u.UpdateID > maxID {
				maxID = u.UpdateID
			}
			s.handleUpdate(ctx, c, u)
		}
		if maxID >= offset {
			_ = s.settings.Set(ctx, KeyUpdateOffset, maxID+1)
		}
	}
}

func (s *Service) handleUpdate(ctx context.Context, c *Client, u Update) {
	switch {
	case u.Message != nil && strings.HasPrefix(strings.TrimSpace(u.Message.Text), "/start"):
		s.handleStart(ctx, c, u)
	case u.CallbackQuery != nil:
		s.handleCallback(ctx, c, u)
	}
}

// handleStart processes "/start <code>": validates the pairing code, binds the
// chat + user, and confirms.
func (s *Service) handleStart(ctx context.Context, c *Client, u Update) {
	fields := strings.Fields(u.Message.Text)
	if len(fields) < 2 {
		_, _ = c.SendMessage(ctx, u.Message.Chat.ID,
			"Open the pairing link from toolyard's Settings → Chat Notifications to connect.", nil)
		return
	}
	code := fields[1]
	if !s.consumePairing(code) {
		_, _ = c.SendMessage(ctx, u.Message.Chat.ID,
			"That pairing link is invalid or expired. Generate a fresh one in toolyard.", nil)
		return
	}
	_ = s.settings.Set(ctx, KeyChatID, strconv.FormatInt(u.Message.Chat.ID, 10))
	_ = s.settings.Set(ctx, KeyUserID, strconv.FormatInt(u.Message.From.ID, 10))
	s.log.Info("paired", "chat_id", u.Message.Chat.ID, "user_id", u.Message.From.ID)
	_, _ = c.SendMessage(ctx, u.Message.Chat.ID,
		"✅ Paired. Request notifications will appear here. Review and submit decisions in Toolyard Inbox.", nil)
}

// handleCallback verifies a legacy tap came from the paired identity and
// directs the person to Inbox. Authorization is layered: updates arrive on our
// authenticated poll, (b) from.id + chat.id must match the paired values.
func (s *Service) handleCallback(ctx context.Context, c *Client, u Update) {
	cb := u.CallbackQuery
	_, _, ok := parseCallback(cb.Data)
	if !ok {
		_ = c.AnswerCallbackQuery(ctx, cb.ID, "Unrecognised action")
		return
	}
	// Identity check.
	wantUser := s.settings.GetString(KeyUserID, "")
	wantChat := s.settings.GetString(KeyChatID, "")
	gotUser := strconv.FormatInt(cb.From.ID, 10)
	gotChat := ""
	if cb.Message != nil {
		gotChat = strconv.FormatInt(cb.Message.Chat.ID, 10)
	}
	if wantUser == "" || gotUser != wantUser || (wantChat != "" && gotChat != wantChat) {
		_ = c.AnswerCallbackQuery(ctx, cb.ID, "Not authorised")
		return
	}
	_ = c.AnswerCallbackQuery(ctx, cb.ID, "Review and submit the decision in Toolyard Inbox.")
}

// parseCallback splits "d|<action>|<approval_id>".
func parseCallback(data string) (action, approvalID string, ok bool) {
	parts := strings.SplitN(data, "|", 3)
	if len(parts) != 3 || parts[0] != "d" {
		return "", "", false
	}
	if parts[1] != "allowed" && parts[1] != "denied" {
		return "", "", false
	}
	if parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
