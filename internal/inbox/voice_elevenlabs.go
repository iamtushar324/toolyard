package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultElevenLabsVoice = "JBFqnCBsd6RMkjVDRZzb" // George
	DefaultElevenLabsModel = "eleven_multilingual_v2"
	maxVoiceBytes          = 8 << 20
)

var voiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ElevenLabsVoice reads credentials and voice settings on each call. The key
// stays on the gateway; only the voice-note script is sent to ElevenLabs.
type ElevenLabsVoice struct {
	key, voice, model func() string
	client            *http.Client
	baseURL           string
}

func NewElevenLabsVoice(key, voice, model func() string) *ElevenLabsVoice {
	return &ElevenLabsVoice{key: key, voice: voice, model: model,
		baseURL: "https://api.elevenlabs.io",
		client:  &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func voiceSetting(get func() string, fallback string) string {
	if get != nil {
		if v := strings.TrimSpace(get()); v != "" {
			return v
		}
	}
	return fallback
}

func (v *ElevenLabsVoice) Available() bool { return voiceSetting(v.key, "") != "" }

func (v *ElevenLabsVoice) Speak(ctx context.Context, script string) ([]byte, string, error) {
	key := voiceSetting(v.key, "")
	if key == "" {
		return nil, "", errors.New("ElevenLabs API key is not configured")
	}
	if strings.ContainsAny(key, "\r\n") {
		return nil, "", errors.New("invalid ElevenLabs API key")
	}
	name := voiceSetting(v.voice, DefaultElevenLabsVoice)
	if !voiceIDPattern.MatchString(name) {
		return nil, "", errors.New("invalid ElevenLabs voice ID")
	}
	if strings.TrimSpace(script) == "" || len(script) > 5000 {
		return nil, "", errors.New("invalid voice-note script length")
	}
	model := voiceSetting(v.model, DefaultElevenLabsModel)
	endpoint := "/v1/text-to-speech/" + url.PathEscape(name)
	var payload any = map[string]string{"text": script, "model_id": model}
	switch model {
	case "eleven_v4":
		if len(script) > 2000 {
			return nil, "", errors.New("Eleven v4 inbox scripts must stay under 2000 bytes")
		}
		endpoint = "/v1/text-to-dialogue"
		payload = map[string]any{"model_id": model, "inputs": []map[string]string{{"text": script, "voice_id": name}}}
	case "eleven_v4_turbo":
		return nil, "", errors.New("Eleven v4 Turbo requires the realtime API; use eleven_v4 for inbox notes")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.baseURL+endpoint+"?output_format=mp3_44100_128", bytes.NewReader(body))
	if err != nil {
		return nil, "", errors.New("could not create ElevenLabs request")
	}
	req.Header.Set("xi-api-key", key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")
	resp, err := v.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", errors.New("ElevenLabs request failed")
	}
	defer resp.Body.Close()
	// Never include provider bodies: they may echo private scripts or credentials.
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("ElevenLabs returned HTTP %d", resp.StatusCode)
	}
	if !strings.EqualFold(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]), "audio/mpeg") {
		return nil, "", errors.New("ElevenLabs returned an unexpected audio type")
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, maxVoiceBytes+1))
	if err != nil {
		return nil, "", errors.New("could not read ElevenLabs audio")
	}
	if len(audio) == 0 || len(audio) > maxVoiceBytes {
		return nil, "", errors.New("ElevenLabs returned empty or oversized audio")
	}
	return audio, "audio/mpeg", nil
}

// VoiceSelector changes providers without a gateway restart. A missing provider
// never silently sends the script to a different cloud service.
type VoiceSelector struct {
	Provider   func() string
	Gemini     Voice
	ElevenLabs *ElevenLabsVoice
}

func (v *VoiceSelector) selected() Voice {
	switch voiceSetting(v.Provider, "gemini") {
	case "gemini":
		return v.Gemini
	case "elevenlabs":
		if v.ElevenLabs != nil && v.ElevenLabs.Available() {
			return v.ElevenLabs
		}
	}
	return nil
}

func (v *VoiceSelector) Available() bool { return v.selected() != nil }

func (v *VoiceSelector) Speak(ctx context.Context, script string) ([]byte, string, error) {
	if selected := v.selected(); selected != nil {
		return selected.Speak(ctx, script)
	}
	return nil, "", errors.New("selected voice provider is not configured")
}
