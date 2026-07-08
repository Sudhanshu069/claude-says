package tts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sudhanshu069/claude-code-speak/internal/config"
)

// MacOSProvider synthesizes speech via the macOS `say` command. Zero config,
// no API keys, lowest latency. Returns AIFF.
type MacOSProvider struct {
	voice  string
	rate   int
	tmpDir string // 0700 audio temp dir
}

// newMacOS builds a MacOSProvider from cfg.Macos.
func newMacOS(cfg *config.Config) (Provider, error) {
	voice := cfg.Macos.Voice
	if voice == "" {
		voice = "Samantha"
	}
	rate := cfg.Macos.Rate
	if rate == 0 {
		rate = 200
	}
	// Owner-only temp dir for the briefly-lived .aiff renders. Fall back to the
	// system temp dir if the dedicated dir can't be created.
	tmpDir := filepath.Join(os.TempDir(), "claude-says-audio")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		tmpDir = os.TempDir()
	}
	return &MacOSProvider{voice: voice, rate: rate, tmpDir: tmpDir}, nil
}

// sayArgs builds the exact argument vector passed to `say` (after the command
// name), as a pure function so the CWE-88 end-of-options guard, voice, and rate
// can be asserted in tests WITHOUT executing say. The layout is:
//
//	-v <voice> -r <rate> -o <outFile> -- <text>
//
// The literal "--" end-of-options marker MUST stay immediately before text so
// attacker-influenced text beginning with a dash is spoken literally, never
// parsed as a `say` flag (e.g. -f/path reads a file into audio, -o/path
// clobbers).
func sayArgs(voice string, rate int, outFile, text string) []string {
	return []string{
		"-v", voice,
		"-r", strconv.Itoa(rate),
		"-o", outFile,
		"--",
		text,
	}
}

// randToken returns an unpredictable hex token for temp-file names.
func randToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Extremely unlikely; a constant name still works, just less unique.
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

// Synthesize runs:  say -v <voice> -r <rate> -o <tmp>.aiff -- <text>
// The literal "--" end-of-options guard neutralizes dash-leading text (CWE-88).
// It uses exec.CommandContext (no shell), writes/reads a 0600 temp file, and
// removes it in a defer even on partial/cancelled render.
func (p *MacOSProvider) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	// Unpredictable name: avoids collisions between concurrent synth calls and
	// stops other local processes guessing/reading the rendered audio.
	outFile := filepath.Join(p.tmpDir, "claude-says-"+randToken()+".aiff")

	// Always remove the temp file — even if `say` failed partway and left a
	// partial/zero-byte .aiff behind (e.g. killed mid-render on shutdown).
	defer func() { _ = os.Remove(outFile) }()

	cmd := exec.CommandContext(ctx, "say", sayArgs(p.voice, p.rate, outFile, text)...)
	if err := cmd.Run(); err != nil {
		return nil, FormatAIFF, fmt.Errorf("say: %w", err)
	}

	// Restrict to owner before reading; the file briefly holds spoken content.
	_ = os.Chmod(outFile, 0o600)

	audio, err := os.ReadFile(outFile)
	if err != nil {
		return nil, FormatAIFF, fmt.Errorf("read rendered audio: %w", err)
	}
	return audio, FormatAIFF, nil
}

// Validate synthesizes a short test phrase.
func (p *MacOSProvider) Validate(ctx context.Context) error {
	_, _, err := p.Synthesize(ctx, "test")
	return err
}

// Voices parses `say -v ?` (implements VoiceLister). Each line looks like:
//
//	Samantha            en_US    # Hello, my name is Samantha.
//
// The language token is the last whitespace field before the "#" comment; the
// name is everything before it (voice names may contain spaces/parentheses).
func (p *MacOSProvider) Voices(ctx context.Context) ([]Voice, error) {
	out, err := exec.CommandContext(ctx, "say", "-v", "?").Output()
	if err != nil {
		return nil, fmt.Errorf("say -v ?: %w", err)
	}
	var voices []Voice
	for _, line := range strings.Split(string(out), "\n") {
		if hash := strings.IndexByte(line, '#'); hash >= 0 {
			line = line[:hash]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		lang := fields[len(fields)-1]
		name := strings.Join(fields[:len(fields)-1], " ")
		voices = append(voices, Voice{
			ID:       name,
			Name:     name,
			Language: strings.ReplaceAll(lang, "_", "-"),
		})
	}
	return voices, nil
}
