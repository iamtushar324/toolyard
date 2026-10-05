package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/chatnotify"
	"github.com/tusharbhardwaj/toolyard/internal/logx"
	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// Settings keys owned by the telegram channel.
const (
	KeyEnabled        = "telegram_enabled"
	KeyBotToken       = "telegram_bot_token" // sealed base64
	KeyBotUsername    = "telegram_bot_username"
	KeyChatID         = "telegram_chat_id"
	KeyUserID         = "telegram_user_id"
	KeyUpdateOffset   = "telegram_update_offset"
	KeyIncludeDetails = "chat_include_details"
)

const (
	channelName   = "telegram"
	pairingTTL    = 10 * time.Minute
	tokenAAD      = "toolyard.chat.v1|telegram|bot_token"
	longPollTimeS = 50
)

// SettingsStore is the subset of *settings.Service the channel needs. Declared
// as an interface so tests can supply a fake.
type SettingsStore interface {
	GetString(key, fallback string) string
	GetBool(key string) bool
	GetInt(key string, fallback int) int
	Set(ctx context.Context, key string, value any) error
	Reveal(ctx context.Context, key string) (string, error)
}

// Service implements chatnotify.Channel over the Telegram Bot API.
type Service struct {
	settings  SettingsStore
	cipher    *sealbox.Cipher
	publicURL string
	log       interface {
		Warn(msg string, args ...any)
		Info(msg string, args ...any)
	}

	// test hooks
	httpClient *http.Client
	baseURL    string

	mu       sync.Mutex
	pairings map[string]time.Time // code -> expiry
}

// Options configure a telegram Service.
type Options struct {
	Settings SettingsStore
	Cipher   *sealbox.Cipher
	// PublicURL is the configured Toolyard origin for Inbox review links.
	PublicURL string
	// Decide is retained for source compatibility. Telegram never invokes
	// it; human permission decisions must be submitted in Inbox.
	Decide func(ctx context.Context, id, action, decidedBy string) (status string, notPending bool, err error)
	// HTTPClient / BaseURL are test seams; nil/empty use production defaults.
	HTTPClient *http.Client
	BaseURL    string
}

// New builds a telegram Service.
func New(opts Options) *Service {
	return &Service{
		settings:   opts.Settings,
		cipher:     opts.Cipher,
		publicURL:  opts.PublicURL,
		log:        logx.For("telegram"),
		httpClient: opts.HTTPClient,
		baseURL:    opts.BaseURL,
		pairings:   map[string]time.Time{},
	}
}

var _ chatnotify.Channel = (*Service)(nil)

// Name implements chatnotify.Channel.
func (s *Service) Name() string { return channelName }

// Ready reports whether the channel is enabled, has a token, and is paired.
func (s *Service) Ready() bool {
	if !s.settings.GetBool(KeyEnabled) {
		return false
	}
	if s.settings.GetString(KeyBotToken, "") == "" {
		return false
	}
	return s.settings.GetString(KeyChatID, "") != ""
}

// client builds a Bot API client from the stored (sealed) token.
func (s *Service) client(ctx context.Context) (*Client, error) {
	sealed, err := s.settings.Reveal(ctx, KeyBotToken)
	if err != nil {
		return nil, err
	}
	if sealed == "" {
		return nil, errors.New("telegram: no bot token configured")
	}
	plain, err := s.cipher.Open(sealed, []byte(tokenAAD))
	if err != nil {
		return nil, fmt.Errorf("telegram: decrypt bot token: %w", err)
	}
	c := NewClient(string(plain), s.httpClient, logx.For("telegram"))
	if s.baseURL != "" {
		c.SetBaseURL(s.baseURL)
	}
	return c, nil
}

// Configure validates a bot token via getMe, seals it, and persists it +
// the bot username. Enables the channel. Returns the bot username.
func (s *Service) Configure(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("token required")
	}
	c := NewClient(token, s.httpClient, logx.For("telegram"))
	if s.baseURL != "" {
		c.SetBaseURL(s.baseURL)
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		return "", fmt.Errorf("validate token: %w", err)
	}
	_ = c.DeleteWebhook(ctx) // best-effort; long-polling needs no webhook
	sealed, err := s.cipher.Seal([]byte(token), []byte(tokenAAD))
	if err != nil {
		return "", err
	}
	if err := s.settings.Set(ctx, KeyBotToken, sealed); err != nil {
		return "", err
	}
	_ = s.settings.Set(ctx, KeyBotUsername, me.Username)
	_ = s.settings.Set(ctx, KeyEnabled, true)
	return me.Username, nil
}

// GeneratePairing mints a one-time pairing code (10-min TTL) and returns the
// t.me deep link the operator opens to pair their chat.
func (s *Service) GeneratePairing(ctx context.Context) (code, deepLink string, err error) {
	username := s.settings.GetString(KeyBotUsername, "")
	if username == "" {
		return "", "", errors.New("configure a bot token first")
	}
	code, err = randCode(9)
	if err != nil {
		return "", "", err
	}
	s.mu.Lock()
	// Garbage-collect expired codes opportunistically.
	now := time.Now()
	for k, exp := range s.pairings {
		if now.After(exp) {
			delete(s.pairings, k)
		}
	}
	s.pairings[code] = now.Add(pairingTTL)
	s.mu.Unlock()
	return code, fmt.Sprintf("https://t.me/%s?start=%s", username, code), nil
}

// consumePairing checks a code is valid + unexpired and removes it.
func (s *Service) consumePairing(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.pairings[code]
	if !ok || time.Now().After(exp) {
		delete(s.pairings, code)
		return false
	}
	delete(s.pairings, code)
	return true
}

// Unpair forgets the paired chat/user (the token stays so re-pairing is quick).
func (s *Service) Unpair(ctx context.Context) error {
	_ = s.settings.Set(ctx, KeyChatID, "")
	_ = s.settings.Set(ctx, KeyUserID, "")
	return nil
}

// Test sends a confirmation message to the paired chat.
func (s *Service) Test(ctx context.Context) error {
	chatID, err := s.pairedChatID()
	if err != nil {
		return err
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	_, err = c.SendMessage(ctx, chatID, "✅ toolyard is connected. Approval requests will appear here.", nil)
	return err
}

func (s *Service) pairedChatID() (int64, error) {
	raw := s.settings.GetString(KeyChatID, "")
	if raw == "" {
		return 0, errors.New("telegram not paired")
	}
	return strconv.ParseInt(raw, 10, 64)
}

// ---- chatnotify.Channel send/update ----------------------------------------

// SendApproval implements chatnotify.Channel.
func (s *Service) SendApproval(ctx context.Context, approvalID, text string) (chatnotify.MessageRef, error) {
	chatID, err := s.pairedChatID()
	if err != nil {
		return chatnotify.MessageRef{}, err
	}
	c, err := s.client(ctx)
	if err != nil {
		return chatnotify.MessageRef{}, err
	}
	m, err := c.SendMessage(ctx, chatID, text+"\n\nReview and submit the decision in Toolyard Inbox.", inboxButtons(s.publicURL, approvalID))
	if err != nil {
		return chatnotify.MessageRef{}, err
	}
	return chatnotify.MessageRef{
		ChatID:    strconv.FormatInt(m.Chat.ID, 10),
		MessageID: strconv.FormatInt(m.MessageID, 10),
	}, nil
}

// UpdateApproval implements chatnotify.Channel: edit text + strip buttons.
func (s *Service) UpdateApproval(ctx context.Context, ref chatnotify.MessageRef, text string) error {
	chatID, err := strconv.ParseInt(ref.ChatID, 10, 64)
	if err != nil {
		return err
	}
	msgID, err := strconv.ParseInt(ref.MessageID, 10, 64)
	if err != nil {
		return err
	}
	c, err := s.client(ctx)
	if err != nil {
		return err
	}
	return c.EditMessageText(ctx, chatID, msgID, text, nil)
}

// Notifications hand off to the only permission interface. Without a
// configured public origin the text still directs the user to Inbox.
func inboxButtons(publicURL, approvalID string) []InlineButton {
	u, err := url.Parse(publicURL)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/", "", "", ""
	return []InlineButton{{Text: "Open Inbox", URL: u.String() + "#inbox/" + url.PathEscape(approvalID)}}
}

func randCode(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
