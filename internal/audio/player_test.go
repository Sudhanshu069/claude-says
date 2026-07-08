package audio

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
)

func TestExtForFormat(t *testing.T) {
	cases := map[string]string{
		"aiff": ".aiff",
		"mp3":  ".mp3",
		"wav":  ".wav",
		"":     ".wav", // empty -> default
		"flac": ".wav", // unknown -> default
		"AIFF": ".wav", // case-sensitive: not "aiff"
	}
	for in, want := range cases {
		if got := extForFormat(in); got != want {
			t.Errorf("extForFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRandomTokenHexAndDistinct(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		tok := randomToken()
		if !re.MatchString(tok) {
			t.Fatalf("randomToken() = %q, want 32 lowercase hex chars", tok)
		}
		if seen[tok] {
			t.Fatalf("randomToken() produced a duplicate within 200 draws: %q", tok)
		}
		seen[tok] = true
	}
}

func TestNewPlayerCreates0700Dir(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()) // keep os.TempDir() inside the sandbox
	p, err := NewPlayer()
	if err != nil {
		t.Fatalf("NewPlayer() error: %v", err)
	}
	info, err := os.Stat(p.dir)
	if err != nil {
		t.Fatalf("stat player dir: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("player path %q is not a dir", p.dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("player dir perm = %#o, want 0700", perm)
	}
}

// The real AfplayPlayer honors the Player ctx-cancel contract that the epoch
// queue's pause/switch paths depend on: a cancelled ctx makes Play return an
// error satisfying errors.Is(err, context.Canceled) WITHOUT producing audio
// (exec.CommandContext returns before afplay is started), and the temp file is
// cleaned up on the killed-render path.
func TestAfplayPlayerCancelledContextReturnsCanceledAndCleansUp(t *testing.T) {
	dir := t.TempDir()
	p := &AfplayPlayer{dir: dir}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: afplay must never actually run

	err := p.Play(ctx, []byte{0, 0}, "wav")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Play with cancelled ctx = %v, want errors.Is(err, context.Canceled)", err)
	}

	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatalf("readdir %q: %v", dir, rerr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temp file(s) not cleaned up after a killed render: %v", names)
	}
}
