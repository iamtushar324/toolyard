package inbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestElevenLabsVoiceRequestAndSettings(t *testing.T) {
	key, voice, model := "test-key", "", ""
	var got map[string]string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/text-to-speech/"+voiceSetting(func() string { return voice }, DefaultElevenLabsVoice) ||
			r.URL.Query().Get("output_format") != "mp3_44100_128" || r.Header.Get("xi-api-key") != key {
			t.Errorf("unexpected synthesis request")
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-test-audio"))
	}))
	defer ts.Close()
	v := NewElevenLabsVoice(func() string { return key }, func() string { return voice }, func() string { return model })
	v.baseURL = ts.URL
	for _, updated := range []bool{false, true} {
		if updated {
			key, voice, model = "replacement-key", "custom_voice", "custom-model"
		}
		audio, ct, err := v.Speak(context.Background(), "Read this exact script.")
		if err != nil || string(audio) != "ID3-test-audio" || ct != "audio/mpeg" || got["text"] != "Read this exact script." || got["model_id"] != voiceSetting(func() string { return model }, DefaultElevenLabsModel) {
			t.Fatalf("unexpected audio or request: %v %s %v", err, ct, got)
		}
	}
}

func TestElevenLabsFailuresAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		ct, body string
	}{
		{"bad-key", 401, "application/json", "private-script secret-key"},
		{"quota", 429, "application/json", "private-script secret-key"},
		{"outage", 503, "text/html", "private-script secret-key"},
		{"wrong-type", 200, "text/html", "private-script secret-key"},
		{"empty", 200, "audio/mpeg", ""},
		{"oversized", 200, "audio/mpeg", strings.Repeat("a", maxVoiceBytes+1)},
		{"redirect", 302, "text/html", "private-script secret-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				w.Header().Set("Location", "http://127.0.0.1:1/secret")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			v := NewElevenLabsVoice(func() string { return "secret-key" }, nil, nil)
			v.baseURL = ts.URL
			b, _, err := v.Speak(context.Background(), "private-script")
			if err == nil || len(b) != 0 || strings.Contains(err.Error(), "private-script") || strings.Contains(err.Error(), "secret-key") {
				t.Fatalf("unsafe failure: %v", err)
			}
		})
	}
	for _, tc := range []struct{ key, id, script string }{{"", "", "script"}, {"key", "../other", "script"}, {"key\ninvalid", "", "script"}, {"key", "", ""}} {
		v := NewElevenLabsVoice(func() string { return tc.key }, func() string { return tc.id }, nil)
		if _, _, err := v.Speak(context.Background(), tc.script); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	v := NewElevenLabsVoice(func() string { return "key" }, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := v.Speak(ctx, "script"); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestVoiceSelectionDoesNotFallbackToAnotherProvider(t *testing.T) {
	provider := "gemini"
	gemini := &fakeVoice{}
	v := &VoiceSelector{Provider: func() string { return provider }, Gemini: gemini, ElevenLabs: NewElevenLabsVoice(nil, nil, nil)}
	if !v.Available() {
		t.Fatal("Gemini missing")
	}
	provider = "elevenlabs"
	if v.Available() {
		t.Fatal("missing key reported available")
	}
	if _, _, err := v.Speak(context.Background(), "private script"); err == nil || gemini.called != 0 {
		t.Fatal("unselected provider received script")
	}
	provider = "unknown"
	if v.Available() {
		t.Fatal("unknown provider available")
	}
}

func TestElevenLabsRecordingAndAvailability(t *testing.T) {
	e := newEnv(t)
	snaps, err := NewSnapshotter(nil, filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-audio"))
	}))
	defer ts.Close()
	key := ""
	v := NewElevenLabsVoice(func() string { return key }, nil, nil)
	v.baseURL = ts.URL
	e.svc.opts.Voice = &VoiceSelector{Provider: func() string { return "elevenlabs" }, ElevenLabs: v}
	e.svc.opts.Blobs, e.svc.opts.VoiceEnabled = snaps, func() bool { return true }
	if e.svc.Info(context.Background()).VoiceAvailable {
		t.Fatal("available without key")
	}
	r := submitDeploy(t, e, "ag_1")
	if r.Audio.Blob != "" {
		t.Fatal("recorded without key")
	}
	key = "test-key"
	if !e.svc.Info(context.Background()).VoiceAvailable {
		t.Fatal("saved key not picked up")
	}
	r = submitDeploy(t, e, "ag_1")
	if r.Audio.Blob == "" || r.Audio.ContentType != "audio/mpeg" {
		t.Fatalf("MP3 not saved: %+v", r.Audio)
	}
	f, _, err := snaps.Open(context.Background(), r.Audio.Blob)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestElevenV4UsesDialogueAPI(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model  string `json:"model_id"`
			Inputs []struct {
				Text  string `json:"text"`
				Voice string `json:"voice_id"`
			} `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/v1/text-to-dialogue" || r.URL.Query().Get("output_format") != "mp3_44100_128" || body.Model != "eleven_v4" || len(body.Inputs) != 1 || body.Inputs[0].Text != "Your inbox update is ready." || body.Inputs[0].Voice != "custom_voice" {
			t.Errorf("wrong v4 request: %+v", body)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3-v4"))
	}))
	defer ts.Close()
	model := "eleven_v4"
	v := NewElevenLabsVoice(func() string { return "test-key" }, func() string { return "custom_voice" }, func() string { return model })
	v.baseURL = ts.URL
	b, ct, err := v.Speak(context.Background(), "Your inbox update is ready.")
	if err != nil || string(b) != "ID3-v4" || ct != "audio/mpeg" || calls != 1 {
		t.Fatalf("v4 synthesis: %v", err)
	}
	if _, _, err := v.Speak(context.Background(), strings.Repeat("a", 2001)); err == nil || calls != 1 {
		t.Fatal("oversized v4 input sent")
	}
	model = "eleven_v4_turbo"
	if _, _, err := v.Speak(context.Background(), "script"); err == nil || calls != 1 {
		t.Fatal("realtime model sent to batch API")
	}
}
