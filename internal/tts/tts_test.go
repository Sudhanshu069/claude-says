package tts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Sudhanshu069/claude-code-speak/internal/config"
)

// ---- frozen HTTP fakes (no network, no httptest server) -------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

// ---- macOS pure arg builder (never exec say) ------------------------------

func TestSayArgs_ExactVectorAndEndOfOptionsGuard(t *testing.T) {
	cases := []struct {
		name  string
		voice string
		rate  int
		out   string
		text  string
	}{
		{"plain", "Samantha", 200, "/tmp/a.aiff", "hello world"},
		{"dash-leading text (CWE-88)", "Alex", 175, "/tmp/b.aiff", "-f/etc/passwd"},
		{"double-dash text", "Alex", 175, "/tmp/b.aiff", "--o/tmp/evil"},
		{"empty text", "Samantha", 200, "/tmp/c.aiff", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sayArgs(tc.voice, tc.rate, tc.out, tc.text)
			want := []string{
				"-v", tc.voice,
				"-r", strconv.Itoa(tc.rate),
				"-o", tc.out,
				"--",
				tc.text,
			}
			if len(got) != len(want) {
				t.Fatalf("len(sayArgs)=%d args %v, want %d %v", len(got), got, len(want), want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("arg[%d]=%q, want %q (full=%v)", i, got[i], want[i], got)
				}
			}
			// The CWE-88 guard: "--" must be second-to-last and text must be the
			// final element, so dash-leading text is never parsed as a flag.
			if got[len(got)-2] != "--" {
				t.Fatalf("second-to-last arg=%q, want the %q end-of-options guard", got[len(got)-2], "--")
			}
			if got[len(got)-1] != tc.text {
				t.Fatalf("last arg=%q, want the text %q as the final (non-flag) element", got[len(got)-1], tc.text)
			}
		})
	}
}

// ---- macOS provider defaults / overrides (asserted via sayArgs) -----------

func TestNewMacOS_DefaultsAndOverrides(t *testing.T) {
	tests := []struct {
		name      string
		cfg       config.MacosConfig
		wantVoice string
		wantRate  int
	}{
		{"zero => Samantha/200", config.MacosConfig{}, "Samantha", 200},
		{"override voice only", config.MacosConfig{Voice: "Daniel"}, "Daniel", 200},
		{"override rate only", config.MacosConfig{Rate: 300}, "Samantha", 300},
		{"override both", config.MacosConfig{Voice: "Alex", Rate: 150}, "Alex", 150},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov, err := newMacOS(&config.Config{Macos: tt.cfg})
			if err != nil {
				t.Fatalf("newMacOS: %v", err)
			}
			p := prov.(*MacOSProvider)
			if p.voice != tt.wantVoice {
				t.Errorf("voice=%q, want %q", p.voice, tt.wantVoice)
			}
			if p.rate != tt.wantRate {
				t.Errorf("rate=%d, want %d", p.rate, tt.wantRate)
			}
			// Confirm the defaults flow through the arg vector (never exec say).
			args := sayArgs(p.voice, p.rate, "/tmp/x.aiff", "hi")
			if args[1] != tt.wantVoice {
				t.Errorf("sayArgs voice=%q, want %q", args[1], tt.wantVoice)
			}
			if args[3] != strconv.Itoa(tt.wantRate) {
				t.Errorf("sayArgs rate=%q, want %q", args[3], strconv.Itoa(tt.wantRate))
			}
		})
	}
}

func TestMacOS_SynthesizeFormatIsAIFF(t *testing.T) {
	// Force a guaranteed-fast failure of `say` by cancelling ctx, so the real
	// binary never renders audio, yet the returned format is still AIFF.
	prov, err := newMacOS(&config.Config{})
	if err != nil {
		t.Fatalf("newMacOS: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, format, synErr := prov.Synthesize(ctx, "hi")
	if synErr == nil {
		t.Fatalf("expected error from cancelled Synthesize")
	}
	if format != FormatAIFF {
		t.Fatalf("format=%q, want %q", format, FormatAIFF)
	}
	if FormatAIFF != "aiff" {
		t.Fatalf("FormatAIFF=%q, want %q", FormatAIFF, "aiff")
	}
}

// ---- registry -------------------------------------------------------------

func TestList_Order(t *testing.T) {
	got := List()
	want := []string{"google", "elevenlabs", "macos"}
	if len(got) != len(want) {
		t.Fatalf("List()=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List()[%d]=%q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestNew_ReturnsRightProviderAndErrors(t *testing.T) {
	// Set both API keys so google's ADC branch and elevenlabs stay offline.
	t.Setenv("GOOGLE_API_KEY", "k")
	t.Setenv("ELEVENLABS_API_KEY", "k")

	tests := []struct {
		name     string
		provider string
		assert   func(*testing.T, Provider)
	}{
		{"google", "google", func(t *testing.T, p Provider) {
			if _, ok := p.(*GoogleProvider); !ok {
				t.Fatalf("got %T, want *GoogleProvider", p)
			}
		}},
		{"elevenlabs", "elevenlabs", func(t *testing.T, p Provider) {
			if _, ok := p.(*ElevenLabsProvider); !ok {
				t.Fatalf("got %T, want *ElevenLabsProvider", p)
			}
		}},
		{"macos", "macos", func(t *testing.T, p Provider) {
			if _, ok := p.(*MacOSProvider); !ok {
				t.Fatalf("got %T, want *MacOSProvider", p)
			}
		}},
		{"empty defaults to macos", "", func(t *testing.T, p Provider) {
			if _, ok := p.(*MacOSProvider); !ok {
				t.Fatalf("got %T, want *MacOSProvider (default)", p)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(&config.Config{Provider: tt.provider})
			if err != nil {
				t.Fatalf("New(%q): %v", tt.provider, err)
			}
			tt.assert(t, p)
		})
	}
}

func TestNew_UnknownProvider(t *testing.T) {
	_, err := New(&config.Config{Provider: "nope"})
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("err=%v, want wrap of ErrUnknownProvider", err)
	}
}

func TestErrHTTPStatus_ErrorFormat(t *testing.T) {
	e := &ErrHTTPStatus{Provider: "google", Code: 503}
	if got := e.Error(); got != "google API error 503" {
		t.Fatalf("Error()=%q, want %q", got, "google API error 503")
	}
}

// ---- Google provider (white-box, injected RoundTripper) -------------------

func googleProvider(rt http.RoundTripper) *GoogleProvider {
	return &GoogleProvider{
		http:            &http.Client{Transport: rt},
		apiKey:          "k",
		languageCode:    "en-US",
		voice:           "en-US-Neural2-D",
		audioEncoding:   "LINEAR16",
		sampleRateHertz: 24000,
	}
}

func TestGoogle_SynthesizeSuccess(t *testing.T) {
	want := []byte("RIFFfakewavbytes")
	audioB64 := base64.StdEncoding.EncodeToString(want)

	var captured *http.Request
	var capturedBody []byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		if r.Body != nil {
			capturedBody, _ = io.ReadAll(r.Body)
		}
		return resp(200, `{"audioContent":"`+audioB64+`"}`), nil
	})

	p := googleProvider(rt)
	audio, format, err := p.Synthesize(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if format != FormatWAV {
		t.Fatalf("format=%q, want %q", format, FormatWAV)
	}
	if string(audio) != string(want) {
		t.Fatalf("audio=%q, want %q", audio, want)
	}

	if captured.URL.String() != googleSynthURL {
		t.Fatalf("URL=%q, want %q", captured.URL.String(), googleSynthURL)
	}
	if captured.Method != http.MethodPost {
		t.Fatalf("method=%q, want POST", captured.Method)
	}
	if got := captured.Header.Get("X-Goog-Api-Key"); got != "k" {
		t.Fatalf("X-Goog-Api-Key=%q, want %q", got, "k")
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}

	var body googleSynthReq
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("request body not valid JSON: %v (%s)", err, capturedBody)
	}
	if body.Input.Text != "hello" {
		t.Errorf("body input.text=%q, want %q", body.Input.Text, "hello")
	}
	if body.Voice.LanguageCode != "en-US" || body.Voice.Name != "en-US-Neural2-D" {
		t.Errorf("body voice=%+v, want en-US / en-US-Neural2-D", body.Voice)
	}
	if body.AudioConfig.AudioEncoding != "LINEAR16" || body.AudioConfig.SampleRateHertz != 24000 {
		t.Errorf("body audioConfig=%+v, want LINEAR16 / 24000", body.AudioConfig)
	}
}

func TestGoogle_Unconfigured_NoHTTPCall(t *testing.T) {
	called := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return resp(200, `{}`), nil
	})
	p := &GoogleProvider{http: &http.Client{Transport: rt}} // apiKey=="" tokenSource==nil
	_, _, err := p.Synthesize(context.Background(), "hi")
	if !errors.Is(err, errGoogleNotConfigured) {
		t.Fatalf("err=%v, want errGoogleNotConfigured", err)
	}
	if called {
		t.Fatal("unconfigured Synthesize must not issue an HTTP request")
	}
	if err := p.Validate(context.Background()); !errors.Is(err, errGoogleNotConfigured) {
		t.Fatalf("Validate err=%v, want errGoogleNotConfigured", err)
	}
}

func TestGoogle_NonSuccessStatus(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(503, `{"error":"unavailable"}`), nil
	})
	p := googleProvider(rt)
	_, _, err := p.Synthesize(context.Background(), "hi")
	var httpErr *ErrHTTPStatus
	if !errors.As(err, &httpErr) {
		t.Fatalf("err=%v (%T), want *ErrHTTPStatus", err, err)
	}
	if httpErr.Provider != "google" || httpErr.Code != 503 {
		t.Fatalf("ErrHTTPStatus=%+v, want {google 503}", httpErr)
	}
}

func TestGoogle_ValidateEmptyAudio(t *testing.T) {
	// 200 with empty audioContent => Synthesize returns 0 bytes => Validate errors.
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, `{"audioContent":""}`), nil
	})
	p := googleProvider(rt)
	err := p.Validate(context.Background())
	if err == nil {
		t.Fatal("expected empty-audio error from Validate")
	}
	if !strings.Contains(err.Error(), "empty audio") {
		t.Fatalf("err=%v, want empty-audio error", err)
	}
}

// ---- ElevenLabs provider (white-box, injected RoundTripper) ---------------

func elevenProvider(rt http.RoundTripper, voiceID string) *ElevenLabsProvider {
	return &ElevenLabsProvider{
		http:    &http.Client{Transport: rt},
		apiKey:  "k",
		voiceID: voiceID,
		modelID: "eleven_turbo_v2_5",
	}
}

func TestElevenLabs_SynthesizeConfiguredVoice(t *testing.T) {
	mp3 := []byte("ID3fakemp3bytes")
	var captured *http.Request
	var capturedBody []byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		captured = r
		if r.Body != nil {
			capturedBody, _ = io.ReadAll(r.Body)
		}
		return resp(200, string(mp3)), nil
	})

	p := elevenProvider(rt, "v")
	audio, format, err := p.Synthesize(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if format != FormatMP3 {
		t.Fatalf("format=%q, want %q", format, FormatMP3)
	}
	if string(audio) != string(mp3) {
		t.Fatalf("audio=%q, want %q", audio, mp3)
	}
	if captured.Method != http.MethodPost {
		t.Fatalf("method=%q, want POST", captured.Method)
	}
	if got := captured.URL.Path; got != "/v1/text-to-speech/v" {
		t.Fatalf("path=%q, want %q", got, "/v1/text-to-speech/v")
	}
	if got := captured.Header.Get("xi-api-key"); got != "k" {
		t.Fatalf("xi-api-key=%q, want %q", got, "k")
	}
	if got := captured.Header.Get("Accept"); got != "audio/mpeg" {
		t.Fatalf("Accept=%q, want audio/mpeg", got)
	}
	if got := captured.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", got)
	}

	var body elevenLabsSynthReq
	if err := json.Unmarshal(capturedBody, &body); err != nil {
		t.Fatalf("request body not valid JSON: %v (%s)", err, capturedBody)
	}
	if body.Text != "hi" {
		t.Errorf("body text=%q, want %q", body.Text, "hi")
	}
	if body.ModelID != "eleven_turbo_v2_5" {
		t.Errorf("body model_id=%q, want %q", body.ModelID, "eleven_turbo_v2_5")
	}
	if body.VoiceSettings.Stability != 0.5 || body.VoiceSettings.SimilarityBoost != 0.75 {
		t.Errorf("body voice_settings=%+v, want {0.5 0.75}", body.VoiceSettings)
	}
}

func TestElevenLabs_EmptyVoiceIDResolvesAndCaches(t *testing.T) {
	var getCount, postCount int
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/voices":
			getCount++
			return resp(200, `{"voices":[{"voice_id":"first-id","name":"First"},{"voice_id":"second","name":"Second"}]}`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/v1/text-to-speech/first-id":
			postCount++
			return resp(200, "mp3"), nil
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			return resp(404, ""), nil
		}
	})

	p := elevenProvider(rt, "") // empty voiceID => lookup+cache
	if _, _, err := p.Synthesize(context.Background(), "one"); err != nil {
		t.Fatalf("first Synthesize: %v", err)
	}
	if _, _, err := p.Synthesize(context.Background(), "two"); err != nil {
		t.Fatalf("second Synthesize: %v", err)
	}
	if getCount != 1 {
		t.Fatalf("GET /v1/voices called %d times, want exactly 1 (cached)", getCount)
	}
	if postCount != 2 {
		t.Fatalf("POST synth called %d times, want 2", postCount)
	}
	if p.cachedVoiceID != "first-id" {
		t.Fatalf("cachedVoiceID=%q, want %q", p.cachedVoiceID, "first-id")
	}
}

func TestElevenLabs_Unconfigured(t *testing.T) {
	called := false
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return resp(200, "mp3"), nil
	})
	p := &ElevenLabsProvider{http: &http.Client{Transport: rt}, modelID: "eleven_turbo_v2_5"}
	_, _, err := p.Synthesize(context.Background(), "hi")
	if !errors.Is(err, errElevenLabsNotConfigured) {
		t.Fatalf("err=%v, want errElevenLabsNotConfigured", err)
	}
	if called {
		t.Fatal("unconfigured Synthesize must not issue an HTTP request")
	}
	if err := p.Validate(context.Background()); !errors.Is(err, errElevenLabsNotConfigured) {
		t.Fatalf("Validate err=%v, want errElevenLabsNotConfigured", err)
	}
}

func TestElevenLabs_NonSuccessStatus(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(401, `{"detail":"bad key"}`), nil
	})
	p := elevenProvider(rt, "v")
	_, _, err := p.Synthesize(context.Background(), "hi")
	var httpErr *ErrHTTPStatus
	if !errors.As(err, &httpErr) {
		t.Fatalf("err=%v (%T), want *ErrHTTPStatus", err, err)
	}
	if httpErr.Provider != "elevenlabs" || httpErr.Code != 401 {
		t.Fatalf("ErrHTTPStatus=%+v, want {elevenlabs 401}", httpErr)
	}
}

func TestElevenLabs_VoicesMapsIDAndName(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/voices" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		return resp(200, `{"voices":[{"voice_id":"a","name":"Alpha"},{"voice_id":"b","name":"Beta"}]}`), nil
	})
	p := elevenProvider(rt, "v")
	voices, err := p.Voices(context.Background())
	if err != nil {
		t.Fatalf("Voices: %v", err)
	}
	if len(voices) != 2 {
		t.Fatalf("got %d voices, want 2", len(voices))
	}
	if voices[0].ID != "a" || voices[0].Name != "Alpha" {
		t.Errorf("voices[0]=%+v, want {a Alpha}", voices[0])
	}
	if voices[1].ID != "b" || voices[1].Name != "Beta" {
		t.Errorf("voices[1]=%+v, want {b Beta}", voices[1])
	}
}

// Format constants are the contract the audio player relies on.
func TestFormatConstants(t *testing.T) {
	if FormatAIFF != "aiff" || FormatWAV != "wav" || FormatMP3 != "mp3" {
		t.Fatalf("format consts = %q/%q/%q, want aiff/wav/mp3", FormatAIFF, FormatWAV, FormatMP3)
	}
}
