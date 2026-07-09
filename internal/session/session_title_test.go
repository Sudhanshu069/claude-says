package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// mkTranscript writes newline-joined JSONL lines to
// $HOME/.claude/projects/<project>/<id>.jsonl and returns the path.
func mkTranscript(t *testing.T, home, project, id string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTitle(t *testing.T) {
	home := t.TempDir()

	t.Run("returns the most recent ai-title", func(t *testing.T) {
		p := mkTranscript(t, home, "-a", "11111111-1111-1111-1111-111111111111",
			`{"type":"user","message":{"content":"first prompt"},"cwd":"/a"}`,
			`{"type":"ai-title","aiTitle":"Old title"}`,
			`{"type":"assistant","message":{"content":[]}}`,
			`{"type":"ai-title","aiTitle":"Current title"}`,
		)
		if got := ReadTitle(p); got != "Current title" {
			t.Fatalf("ReadTitle = %q, want %q", got, "Current title")
		}
	})

	t.Run("falls back to first user prompt, one line, skipping <command> envelopes", func(t *testing.T) {
		p := mkTranscript(t, home, "-b", "22222222-2222-2222-2222-222222222222",
			`{"type":"user","message":{"content":"<command-name>/foo</command-name>"}}`,
			`{"type":"user","message":{"content":"real first prompt\nsecond line ignored"}}`,
		)
		if got := ReadTitle(p); got != "real first prompt" {
			t.Fatalf("ReadTitle fallback = %q, want %q", got, "real first prompt")
		}
	})

	t.Run("handles array-form content", func(t *testing.T) {
		p := mkTranscript(t, home, "-c", "33333333-3333-3333-3333-333333333333",
			`{"type":"user","message":{"content":[{"type":"text","text":"array prompt"}]}}`,
		)
		if got := ReadTitle(p); got != "array prompt" {
			t.Fatalf("ReadTitle array = %q, want %q", got, "array prompt")
		}
	})

	t.Run("truncates a long prompt to 72 runes + ellipsis", func(t *testing.T) {
		long := strings.Repeat("x", 200)
		p := mkTranscript(t, home, "-d", "44444444-4444-4444-4444-444444444444",
			`{"type":"user","message":{"content":"`+long+`"}}`,
		)
		got := ReadTitle(p)
		if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) != 73 {
			t.Fatalf("truncated = %q (runes %d), want 72+ellipsis", got, utf8.RuneCountInString(got))
		}
	})

	t.Run("no title and no prompt -> empty", func(t *testing.T) {
		p := mkTranscript(t, home, "-e", "55555555-5555-5555-5555-555555555555",
			`{"type":"system","content":"boot"}`,
			`{"type":"assistant","message":{"content":[]}}`,
		)
		if got := ReadTitle(p); got != "" {
			t.Fatalf("ReadTitle = %q, want empty", got)
		}
	})

	t.Run("missing file -> empty, no panic", func(t *testing.T) {
		if got := ReadTitle(filepath.Join(home, "nope.jsonl")); got != "" {
			t.Fatalf("ReadTitle(missing) = %q, want empty", got)
		}
	})
}

func TestDiscoverWithTitles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mkTranscript(t, home, "-proj", "66666666-6666-6666-6666-666666666666",
		`{"type":"user","message":{"content":"hi"},"cwd":"/proj"}`,
		`{"type":"ai-title","aiTitle":"My Session Title"}`,
	)
	sessions, err := DiscoverWithTitles()
	if err != nil {
		t.Fatalf("DiscoverWithTitles: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Title != "My Session Title" {
		t.Fatalf("got %+v, want one session titled %q", sessions, "My Session Title")
	}
}
