package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// isolate points HOME at a fresh temp dir so ConfigDir()/ConfigFile()/
// SocketPath() resolve inside it. darwin's os.UserHomeDir() honours $HOME.
// t.Setenv forbids t.Parallel() in these tests.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestDefaultConfig(t *testing.T) {
	got := DefaultConfig()
	want := Config{
		Provider: "macos",
		Macos:    MacosConfig{Voice: "Samantha", Rate: 200},
		Google: GoogleConfig{
			Voice:           "en-US-Neural2-D",
			LanguageCode:    "en-US",
			AudioEncoding:   "LINEAR16",
			SampleRateHertz: 24000,
		},
		ElevenLabs: ElevenLabsConfig{
			VoiceID: "21m00Tcm4TlvDq8ikWAM",
			ModelID: "eleven_turbo_v2_5",
		},
		Playback: PlaybackConfig{Method: "afplay"},
		TextProcessor: TextProcessorConfig{
			MinChunkLength: 10,
			MaxChunkLength: 500,
			FlushDelay:     1500,
		},
		Narrator: NarratorConfig{
			Enabled:  false,
			Provider: "gemini",
			Gemini:   GeminiConfig{Model: "gemini-2.5-flash"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefaultConfig() mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestPathsResolveUnderHome(t *testing.T) {
	home := isolate(t)

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if want := filepath.Join(home, ".claude-says"); dir != want {
		t.Errorf("ConfigDir = %q, want %q", dir, want)
	}

	file, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	if want := filepath.Join(home, ".claude-says", "config.json"); file != want {
		t.Errorf("ConfigFile = %q, want %q", file, want)
	}

	sock, err := SocketPath()
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	if want := filepath.Join(home, ".claude-says", "claude-says.sock"); sock != want {
		t.Errorf("SocketPath = %q, want %q", sock, want)
	}
}

func TestLoadNoFileReturnsDefaults(t *testing.T) {
	isolate(t)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error %v", err)
	}
	if !reflect.DeepEqual(got, DefaultConfig()) {
		t.Fatalf("Load() with no file = %+v, want DefaultConfig()", got)
	}
}

// seed writes raw JSON to the isolated config.json, creating the dir.
func seed(t *testing.T, raw string) {
	t.Helper()
	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	if err := os.WriteFile(file, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestLoadOverlaysPartialJSON(t *testing.T) {
	isolate(t)
	// Only provider and macos.rate are present; every absent field — including
	// the sibling macos.voice — must stay at its default (deep-merge parity).
	seed(t, `{"provider":"google","macos":{"rate":150}}`)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got.Provider != "google" {
		t.Errorf("Provider = %q, want %q", got.Provider, "google")
	}
	if got.Macos.Rate != 150 {
		t.Errorf("Macos.Rate = %d, want 150", got.Macos.Rate)
	}
	if got.Macos.Voice != "Samantha" {
		t.Errorf("Macos.Voice = %q, want default %q", got.Macos.Voice, "Samantha")
	}
	// A wholly-absent nested block stays fully default.
	if got.Google != (DefaultConfig().Google) {
		t.Errorf("Google = %+v, want default %+v", got.Google, DefaultConfig().Google)
	}
	if got.TextProcessor != DefaultConfig().TextProcessor {
		t.Errorf("TextProcessor = %+v, want default", got.TextProcessor)
	}
}

func TestLoadMalformedJSONReturnsDefaults(t *testing.T) {
	isolate(t)
	seed(t, `{not valid json`)

	got, err := Load()
	if err != nil {
		t.Fatalf("Load with malformed JSON returned error %v, want nil (Node try/catch parity)", err)
	}
	if !reflect.DeepEqual(got, DefaultConfig()) {
		t.Fatalf("Load with malformed JSON = %+v, want DefaultConfig()", got)
	}
}

func TestSaveRoundTripsAndPerms(t *testing.T) {
	isolate(t)

	cfg := DefaultConfig()
	cfg.Provider = "elevenlabs"
	cfg.ElevenLabs.VoiceID = "custom-voice"
	cfg.Narrator.Enabled = true

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	file, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}

	// Owner-only 0600.
	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config.json mode = %o, want 600", perm)
	}

	// Atomic: no leftover temp file beside the target.
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("leftover %s.tmp exists (err=%v), want gone", file, err)
	}
	dir, _ := ConfigDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("unexpected temp artifact %q in config dir", e.Name())
		}
	}

	// Round-trips equal.
	got, err := Load()
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("round-trip mismatch\n got: %+v\nwant: %+v", got, cfg)
	}

	// Sanity: the on-disk bytes are valid JSON with camelCase tags.
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if _, ok := probe["provider"]; !ok {
		t.Errorf("saved JSON missing camelCase key %q", "provider")
	}
}

func TestSaveTightensLooseFile(t *testing.T) {
	isolate(t)

	dir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file, err := ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}

	// Pre-existing world-readable file.
	if err := os.WriteFile(file, []byte(`{"provider":"macos"}`), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	if info, _ := os.Stat(file); info.Mode().Perm() != 0o644 {
		t.Fatalf("precondition: seed file not 0644 (%o)", info.Mode().Perm())
	}

	if err := DefaultConfig().Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(file)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("after Save mode = %o, want 600 (tightened)", perm)
	}
}
