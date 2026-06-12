package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/tusharbhardwaj/toolyard/internal/audit"
	"github.com/tusharbhardwaj/toolyard/internal/settings"
)

// ChatTelegram is the telegram-channel subset the API needs. Implemented by
// *internal/chatnotify/telegram.Service; declared here so the api package
// doesn't import telegram directly.
type ChatTelegram interface {
	Configure(ctx context.Context, token string) (string, error)
	GeneratePairing(ctx context.Context) (code, deepLink string, err error)
	Unpair(ctx context.Context) error
	Test(ctx context.Context) error
}

// chatRoutes registers the chat-notification endpoints.
func (s *Server) chatRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/chat/status", s.chatStatus)
	mux.HandleFunc("/v1/chat/telegram/", s.chatTelegramDispatch)
}

// chatStatus reports the configured/paired state of every chat channel. Reads
// from settings, so it works even when the telegram service isn't wired.
func (s *Server) chatStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	tg := map[string]any{
		"enabled":         s.settings.GetBool(settings.TelegramEnabled),
		"configured":      s.settings.GetString(settings.TelegramBotToken, "") != "",
		"paired":          s.settings.GetString(settings.TelegramChatID, "") != "",
		"bot_username":    s.settings.GetString(settings.TelegramBotUsername, ""),
		"include_details": s.settings.GetBoolDefault(settings.ChatIncludeDetails, true),
		"wired":           s.chatTelegram != nil,
	}
	writeJSON(w, http.StatusOK, map[string]any{"telegram": tg})
}

func (s *Server) chatTelegramDispatch(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireUser(r); err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if s.chatTelegram == nil {
		writeError(w, http.StatusServiceUnavailable, "telegram channel is not wired")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	action := strings.TrimPrefix(r.URL.Path, "/v1/chat/telegram/")
	switch action {
	case "configure":
		var body struct {
			Token string `json:"token"`
		}
		if err := decode(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		username, err := s.chatTelegram.Configure(r.Context(), body.Token)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.auditChat(r, "chat.telegram.configure", username)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "bot_username": username})
	case "pair":
		code, link, err := s.chatTelegram.GeneratePairing(r.Context())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"code": code, "deep_link": link})
	case "unpair":
		if err := s.chatTelegram.Unpair(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.auditChat(r, "chat.telegram.unpair", "")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case "test":
		if err := s.chatTelegram.Test(r.Context()); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) auditChat(r *http.Request, eventType, summary string) {
	if s.audit == nil {
		return
	}
	_ = s.audit.Write(r.Context(), audit.Event{EventType: eventType, ResultSummary: summary})
}
