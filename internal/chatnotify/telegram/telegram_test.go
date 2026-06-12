package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tusharbhardwaj/toolyard/internal/sealbox"
)

// fakeSettings is an in-memory SettingsStore.
type fakeSettings struct {
	mu sync.Mutex
	m  map[string]any
}

func newFakeSettings() *fakeSettings { return &fakeSettings{m: map[string]any{}} }

func (f *fakeSettings) GetString(key, fallback string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.m[key].(string); ok {
		return v
	}
	return fallback
}
func (f *fakeSettings) GetBool(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := f.m[key].(bool)
	return b
}
func (f *fakeSettings) GetInt(key string, fallback int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch v := f.m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return fallback
}
func (f *fakeSettings) Set(ctx context.Context, key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}
func (f *fakeSettings) Reveal(ctx context.Context, key string) (string, error) {
	return f.GetString(key, ""), nil
}

func testCipher(t *testing.T) *sealbox.Cipher {
	t.Helper()
	key := make([]byte, sealbox.MasterKeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := sealbox.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// botServer is a fake Telegram Bot API. Handlers keyed by method name.
type botServer struct {
	*httptest.Server
	mu            sync.Mutex
	lastOffset    int64
	sentMessages  int
	editedMessage int
	answeredCBs   []string
}

func newBotServer(t *testing.T) *botServer {
	bs := &botServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		bs.mu.Lock()
		defer bs.mu.Unlock()
		switch method {
		case "getMe":
			writeResult(w, map[string]any{"id": 42, "is_bot": true, "username": "toolyard_bot"})
		case "deleteWebhook":
			writeResult(w, true)
		case "sendMessage":
			bs.sentMessages++
			writeResult(w, map[string]any{"message_id": 555, "chat": map[string]any{"id": 1000}})
		case "editMessageText":
			bs.editedMessage++
			writeResult(w, map[string]any{"message_id": 555, "chat": map[string]any{"id": 1000}})
		case "answerCallbackQuery":
			bs.answeredCBs = append(bs.answeredCBs, r.Form.Get("text"))
			writeResult(w, true)
		default:
			writeResult(w, []any{})
		}
	})
	bs.Server = httptest.NewServer(mux)
	t.Cleanup(bs.Close)
	return bs
}

func writeResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func newTestService(t *testing.T, decide func(ctx context.Context, id, action, decidedBy string) (string, bool, error)) (*Service, *fakeSettings, *botServer) {
	bs := newBotServer(t)
	set := newFakeSettings()
	svc := New(Options{
		Settings: set,
		Cipher:   testCipher(t),
		Decide:   decide,
		BaseURL:  bs.URL,
	})
	return svc, set, bs
}

func TestConfigureValidatesAndSeals(t *testing.T) {
	svc, set, _ := newTestService(t, nil)
	username, err := svc.Configure(context.Background(), "123:ABC")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if username != "toolyard_bot" {
		t.Fatalf("username = %q", username)
	}
	// Token stored sealed (not the plaintext).
	stored := set.GetString(KeyBotToken, "")
	if stored == "" || stored == "123:ABC" {
		t.Fatalf("token not sealed: %q", stored)
	}
	if !set.GetBool(KeyEnabled) {
		t.Fatal("enabled not set")
	}
}

func TestPairingRightAndWrongCode(t *testing.T) {
	svc, set, bs := newTestService(t, nil)
	if _, err := svc.Configure(context.Background(), "123:ABC"); err != nil {
		t.Fatal(err)
	}
	c, err := svc.client(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	code, link, err := svc.GeneratePairing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(link, "toolyard_bot") || !strings.Contains(link, code) {
		t.Fatalf("deep link wrong: %q", link)
	}

	// Wrong code → not paired.
	svc.handleUpdate(context.Background(), c, startUpdate(1, "/start WRONGCODE", 7, 1000))
	if set.GetString(KeyChatID, "") != "" {
		t.Fatal("paired with wrong code")
	}

	// Right code → paired with this chat + user.
	svc.handleUpdate(context.Background(), c, startUpdate(2, "/start "+code, 7, 1000))
	if set.GetString(KeyChatID, "") != "1000" || set.GetString(KeyUserID, "") != "7" {
		t.Fatalf("not paired: chat=%q user=%q", set.GetString(KeyChatID, ""), set.GetString(KeyUserID, ""))
	}
	if bs.sentMessages == 0 {
		t.Fatal("expected a confirmation message")
	}
}

func TestCallbackDecideAuthorized(t *testing.T) {
	var gotID, gotAction, gotBy string
	decide := func(ctx context.Context, id, action, decidedBy string) (string, bool, error) {
		gotID, gotAction, gotBy = id, action, decidedBy
		return "allowed", false, nil
	}
	svc, set, _ := newTestService(t, decide)
	_, _ = svc.Configure(context.Background(), "123:ABC")
	_ = set.Set(context.Background(), KeyChatID, "1000")
	_ = set.Set(context.Background(), KeyUserID, "7")
	c, _ := svc.client(context.Background())

	// Authorized callback from the paired user/chat.
	svc.handleUpdate(context.Background(), c, callbackUpdate(3, "d|allowed|ap_xyz", 7, 1000))
	if gotID != "ap_xyz" || gotAction != "allowed" || gotBy != "telegram:7" {
		t.Fatalf("decide args wrong: id=%q action=%q by=%q", gotID, gotAction, gotBy)
	}

	// Unauthorized callback from a different user → decide NOT called again.
	gotID = ""
	svc.handleUpdate(context.Background(), c, callbackUpdate(4, "d|denied|ap_other", 999, 1000))
	if gotID != "" {
		t.Fatalf("decide should not run for wrong user, got id=%q", gotID)
	}
}

func TestCallbackAlreadyDecidedToast(t *testing.T) {
	decide := func(ctx context.Context, id, action, decidedBy string) (string, bool, error) {
		return "", true, nil // notPending
	}
	svc, set, bs := newTestService(t, decide)
	_, _ = svc.Configure(context.Background(), "123:ABC")
	_ = set.Set(context.Background(), KeyChatID, "1000")
	_ = set.Set(context.Background(), KeyUserID, "7")
	c, _ := svc.client(context.Background())
	svc.handleUpdate(context.Background(), c, callbackUpdate(5, "d|allowed|ap_xyz", 7, 1000))
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if len(bs.answeredCBs) != 1 || !strings.Contains(strings.ToLower(bs.answeredCBs[0]), "already") {
		t.Fatalf("expected 'already decided' toast, got %v", bs.answeredCBs)
	}
}

func TestClient429Retry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": false, "error_code": 429,
				"parameters": map[string]any{"retry_after": 0},
			})
			return
		}
		writeResult(w, map[string]any{"id": 1, "username": "b"})
	}))
	defer srv.Close()
	c := NewClient("tok", nil, nil)
	c.SetBaseURL(srv.URL)
	if _, err := c.GetMe(context.Background()); err != nil {
		t.Fatalf("getMe after 429: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls (429 then ok), got %d", calls.Load())
	}
}

func TestPollerPersistsOffset(t *testing.T) {
	set := newFakeSettings()
	// One update then empties.
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if method != "getUpdates" {
			writeResult(w, true)
			return
		}
		if served.Add(1) == 1 {
			writeResult(w, []map[string]any{{"update_id": 5}})
			return
		}
		writeResult(w, []any{})
	}))
	defer srv.Close()
	_ = set.Set(context.Background(), KeyEnabled, true)
	// Seal a token so client() works.
	cipher := testCipher(t)
	sealed, _ := cipher.Seal([]byte("tok"), []byte(tokenAAD))
	_ = set.Set(context.Background(), KeyBotToken, sealed)

	svc := New(Options{Settings: set, Cipher: cipher, BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = svc.RunPoller(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if set.GetInt(KeyUpdateOffset, 0) == 6 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("offset not persisted as 6, got %d", set.GetInt(KeyUpdateOffset, 0))
}

// ---- update fixtures --------------------------------------------------------

func startUpdate(updateID int64, text string, fromID, chatID int64) Update {
	u := Update{UpdateID: updateID}
	u.Message = &struct {
		MessageID int64  `json:"message_id"`
		Text      string `json:"text"`
		From      User   `json:"from"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{}
	u.Message.MessageID = updateID
	u.Message.Text = text
	u.Message.From = User{ID: fromID}
	u.Message.Chat.ID = chatID
	return u
}

func callbackUpdate(updateID int64, data string, fromID, chatID int64) Update {
	u := Update{UpdateID: updateID}
	u.CallbackQuery = &struct {
		ID      string `json:"id"`
		Data    string `json:"data"`
		From    User   `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	}{ID: fmt.Sprintf("cb%d", updateID), Data: data, From: User{ID: fromID}}
	u.CallbackQuery.Message = &struct {
		MessageID int64 `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{}
	u.CallbackQuery.Message.MessageID = 555
	u.CallbackQuery.Message.Chat.ID = chatID
	return u
}
