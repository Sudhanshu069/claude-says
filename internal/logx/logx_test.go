package logx

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearLevelEnv ensures neither log-level env var leaks in from the
// surrounding environment, so Level()/InitTo see a known baseline. t.Setenv
// restores the prior value (and unset-ness) at the end of the test.
func clearLevelEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_SAYS_LOG", "")
	t.Setenv("LOG_LEVEL", "")
	os.Unsetenv("CLAUDE_SAYS_LOG")
	os.Unsetenv("LOG_LEVEL")
}

func TestInitTo_JSON(t *testing.T) {
	clearLevelEnv(t)

	var buf bytes.Buffer
	l := InitTo(&buf, false)
	if l == nil {
		t.Fatal("InitTo returned nil logger")
	}

	const msg = "hello json world"
	l.Info(msg)

	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("no output written")
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, line)
	}
	if got := rec[slog.MessageKey]; got != msg {
		t.Errorf("msg = %v, want %q", got, msg)
	}
	if got := rec[slog.LevelKey]; got != "INFO" {
		t.Errorf("level = %v, want INFO", got)
	}
}

func TestInitTo_TTYPretty(t *testing.T) {
	clearLevelEnv(t)

	var buf bytes.Buffer
	l := InitTo(&buf, true)
	if l == nil {
		t.Fatal("InitTo returned nil logger")
	}

	const msg = "pretty tty message"
	l.Info(msg)

	out := buf.String()
	if out == "" {
		t.Fatal("no output written")
	}
	// The pretty (tint) handler is not JSON; it should not parse as a JSON
	// object, and must contain the human-readable message text.
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err == nil {
		t.Errorf("tty output unexpectedly parsed as JSON: %q", out)
	}
	if !strings.Contains(out, msg) {
		t.Errorf("tty output %q does not contain message %q", out, msg)
	}
}

func TestInitTo_HonorsLevel(t *testing.T) {
	clearLevelEnv(t)
	t.Setenv("LOG_LEVEL", "warn")

	var buf bytes.Buffer
	l := InitTo(&buf, false)

	l.Info("filtered out")
	l.Warn("kept warn")

	sc := bufio.NewScanner(&buf)
	var msgs []string
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("bad json line %q: %v", sc.Text(), err)
		}
		msgs = append(msgs, rec[slog.MessageKey].(string))
	}
	if len(msgs) != 1 || msgs[0] != "kept warn" {
		t.Errorf("with LOG_LEVEL=warn got msgs %v, want only [kept warn]", msgs)
	}
}

func TestInit(t *testing.T) {
	// Init() targets os.Stderr (which is not a TTY under `go test`), so it
	// takes the JSON path. We only assert it wires up a usable logger without
	// panicking; its output goes to the real stderr.
	clearLevelEnv(t)
	if l := Init(); l == nil {
		t.Fatal("Init returned nil logger")
	}
	if slog.Default() == nil {
		t.Fatal("Init did not install a default logger")
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"trace", LevelTrace},
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"fatal", LevelFatal},
		{"silent", LevelSilent},
		// Unknown values fall back to info (the documented default).
		{"", slog.LevelInfo},
		{"verbose", slog.LevelInfo},
		{"nonsense", slog.LevelInfo},
		// parseLevel is case-sensitive: non-lowercase spellings are unknown
		// and therefore also fall back to info.
		{"DEBUG", slog.LevelInfo},
		{"Warn", slog.LevelInfo},
		{"ERROR", slog.LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := parseLevel(tt.in); got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestLevel_EnvPrecedence(t *testing.T) {
	t.Run("default is info", func(t *testing.T) {
		clearLevelEnv(t)
		if got := Level(); got != slog.LevelInfo {
			t.Errorf("Level() = %v, want info", got)
		}
	})

	t.Run("LOG_LEVEL is read", func(t *testing.T) {
		clearLevelEnv(t)
		t.Setenv("LOG_LEVEL", "error")
		if got := Level(); got != slog.LevelError {
			t.Errorf("Level() = %v, want error", got)
		}
	})

	t.Run("CLAUDE_SAYS_LOG takes precedence over LOG_LEVEL", func(t *testing.T) {
		clearLevelEnv(t)
		t.Setenv("CLAUDE_SAYS_LOG", "debug")
		t.Setenv("LOG_LEVEL", "error")
		if got := Level(); got != slog.LevelDebug {
			t.Errorf("Level() = %v, want debug", got)
		}
	})

	t.Run("empty CLAUDE_SAYS_LOG falls through to LOG_LEVEL", func(t *testing.T) {
		clearLevelEnv(t)
		t.Setenv("CLAUDE_SAYS_LOG", "")
		t.Setenv("LOG_LEVEL", "warn")
		if got := Level(); got != slog.LevelWarn {
			t.Errorf("Level() = %v, want warn", got)
		}
	})
}

func TestIsTTY_RegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-tty.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()

	if isTTY(f) {
		t.Errorf("isTTY(regular file) = true, want false")
	}
}

func TestIsTTY_ClosedFile(t *testing.T) {
	// Stat on a closed file errors; isTTY must treat that as not-a-TTY.
	path := filepath.Join(t.TempDir(), "closed.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()

	if isTTY(f) {
		t.Errorf("isTTY(closed file) = true, want false")
	}
}
