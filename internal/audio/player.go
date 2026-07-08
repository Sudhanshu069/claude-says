// Package audio plays synthesized speech through macOS `afplay` and holds the
// epoch-fenced ordered queue that serializes playback. This file is the player:
// it writes audio to a 0600 temp file and runs afplay under a context so
// pause/switch/shutdown can cancel it. Mirrors Node src/player.js.
package audio

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
)

// AfplayPlayer runs afplay against temp files in an owner-only temp dir. It is
// the production implementation of the Player interface the Queue drives.
type AfplayPlayer struct {
	dir string // 0700 temp dir
}

// NewPlayer creates the 0700 temp dir and returns an *AfplayPlayer.
func NewPlayer() (*AfplayPlayer, error) {
	dir := filepath.Join(os.TempDir(), "claude-says-audio")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Tighten perms even if the dir already existed with looser bits: the temp
	// files hold the audio of the user's session.
	_ = os.Chmod(dir, 0o700)
	return &AfplayPlayer{dir: dir}, nil
}

// Play writes audio to a 0600 temp file (unpredictable name) and runs `afplay`
// under ctx. Cancelling ctx kills afplay; Play returns an error satisfying
// errors.Is(err, context.Canceled) so the queue treats it as an interruption,
// not a failure. The temp file is removed in a defer even on partial/killed
// render.
func (p *AfplayPlayer) Play(ctx context.Context, audio []byte, format string) error {
	// Unpredictable name (no other process can guess/enumerate it) with
	// owner-only perms. Random names also avoid same-millisecond collisions.
	tmp := filepath.Join(p.dir, "chunk-"+randomToken()+extForFormat(format))
	if err := os.WriteFile(tmp, audio, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)

	cmd := exec.CommandContext(ctx, "afplay", tmp)
	if err := cmd.Run(); err != nil {
		// A cancelled/expired ctx means pause/switch/shutdown killed afplay
		// intentionally. Surface it as ctx.Err() (context.Canceled or
		// DeadlineExceeded) so the queue treats it as an interruption.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

// randomToken returns 16 bytes of cryptographically-random hex.
func randomToken() string {
	var b [16]byte
	// crypto/rand.Read never returns a short read on success; on the vanishingly
	// rare error we fall back to a fixed token — the temp file is still 0600 and
	// removed promptly, so predictability of the name is the only cost.
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

// extForFormat is the single source of truth mapping a Provider format string
// to the afplay temp-file extension. Unknown formats default to ".wav".
func extForFormat(format string) string {
	switch format {
	case "aiff":
		return ".aiff"
	case "mp3":
		return ".mp3"
	case "wav":
		return ".wav"
	default:
		return ".wav"
	}
}
