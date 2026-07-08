package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Canonical UUIDs used across the suite (each exactly 36 chars, matches uuidRe).
const (
	uA = "11111111-1111-1111-1111-111111111111"
	uB = "22222222-2222-2222-2222-222222222222"
	uC = "33333333-3333-3333-3333-333333333333"
)

// setHome points HOME at a fresh temp dir (darwin UserHomeDir reads $HOME) and
// returns it. Because it mutates process env it forbids t.Parallel().
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// writeSession creates ~/.claude/projects/<projDir>/<id>.jsonl with the given
// content, then (when mtime is non-zero) stamps its mtime for deterministic
// ordering. Returns the transcript path.
func writeSession(t *testing.T, home, projDir, id, content string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", projDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtimes %s: %v", p, err)
		}
	}
	return p
}

var baseTime = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func TestDiscoverAbsentProjectsDir(t *testing.T) {
	setHome(t) // HOME exists but ~/.claude/projects does not.
	got, err := Discover()
	if err != nil {
		t.Fatalf("Discover: unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("Discover: want nil slice when projects dir absent, got %#v", got)
	}
}

func TestDiscoverFiltersEntries(t *testing.T) {
	home := setHome(t)
	proj := "-Users-me-proj"

	// The one valid transcript.
	writeSession(t, home, proj, uA, `{"cwd":"/Users/me/proj"}`+"\n", baseTime)

	// Noise that must be skipped.
	dir := filepath.Join(home, ".claude", "projects", proj)
	if err := os.WriteFile(filepath.Join(dir, "notauuid.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, uB+".txt"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plain file directly under projects/ (not a directory) must be skipped.
	if err := os.WriteFile(filepath.Join(home, ".claude", "projects", "loose.jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Discover: want exactly 1 session, got %d: %#v", len(got), got)
	}
	if got[0].ID != uA {
		t.Errorf("Discover: ID = %q, want %q", got[0].ID, uA)
	}
	if got[0].ProjectDir != proj {
		t.Errorf("Discover: ProjectDir = %q, want %q", got[0].ProjectDir, proj)
	}
	wantPath := filepath.Join(dir, uA+".jsonl")
	if got[0].TranscriptPath != wantPath {
		t.Errorf("Discover: TranscriptPath = %q, want %q", got[0].TranscriptPath, wantPath)
	}
}

func TestDiscoverSortsMostRecentFirst(t *testing.T) {
	home := setHome(t)
	// Sessions across two projects with distinct mtimes.
	writeSession(t, home, "-p-old", uA, `{"cwd":"/p/old"}`, baseTime)
	writeSession(t, home, "-p-mid", uB, `{"cwd":"/p/mid"}`, baseTime.Add(time.Hour))
	writeSession(t, home, "-p-new", uC, `{"cwd":"/p/new"}`, baseTime.Add(2*time.Hour))

	got, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{uC, uB, uA}
	if len(got) != len(want) {
		t.Fatalf("Discover: got %d sessions, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("Discover[%d].ID = %q, want %q", i, got[i].ID, id)
		}
	}
	// LastActive must reflect the file mtime.
	if !got[0].LastActive.Equal(baseTime.Add(2 * time.Hour)) {
		t.Errorf("LastActive = %v, want %v", got[0].LastActive, baseTime.Add(2*time.Hour))
	}
}

func TestDiscoverProjectNameFromCwd(t *testing.T) {
	home := setHome(t)
	// Encoded dir is lossy; cwd on the first record is authoritative.
	writeSession(t, home, "-Users-me-my-app", uA,
		`{"cwd":"/Users/me/my-app","type":"assistant"}`+"\n", baseTime)

	got, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	if got[0].ProjectName != "/Users/me/my-app" {
		t.Errorf("ProjectName = %q, want %q (from cwd)", got[0].ProjectName, "/Users/me/my-app")
	}
}

func TestDiscoverProjectNameFallsBackToDecodedDir(t *testing.T) {
	home := setHome(t)
	cases := []struct {
		name    string
		content string
	}{
		{"no cwd field", `{"type":"assistant"}` + "\n"},
		{"empty cwd", `{"cwd":"","type":"assistant"}` + "\n"},
		{"empty file", ""},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh HOME per subtest keeps each in isolation.
			h := setHome(t)
			proj := "-Users-me-proj"
			writeSession(t, h, proj, uA, tc.content, baseTime.Add(time.Duration(i)*time.Minute))
			got, err := Discover()
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("want 1 session, got %d", len(got))
			}
			want := decodeProjectDir(proj) // "/Users/me/proj"
			if got[0].ProjectName != want {
				t.Errorf("ProjectName = %q, want decoded %q", got[0].ProjectName, want)
			}
		})
	}
	_ = home
}

func TestDecodeProjectDir(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"-Users-me-proj", "/Users/me/proj"},
		{"-tmp", "/tmp"},
		{"relative-path", "relative/path"}, // no leading dash: only inner dashes become slashes
		{"-a-b-c", "/a/b/c"},
		{"-", "/"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := decodeProjectDir(tc.in); got != tc.want {
			t.Errorf("decodeProjectDir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsUUID(t *testing.T) {
	valid := []string{
		uA, uB,
		"abcdef01-2345-6789-abcd-ef0123456789",
		"ABCDEF01-2345-6789-ABCD-EF0123456789",
	}
	for _, s := range valid {
		if !isUUID(s) {
			t.Errorf("isUUID(%q) = false, want true", s)
		}
	}
	invalid := []string{
		"",
		"not-a-uuid",
		"11111111-1111-1111-1111-11111111111",   // 11 in last group (too short)
		"11111111-1111-1111-1111-1111111111111", // too long
		"g1111111-1111-1111-1111-111111111111",  // non-hex
		"11111111_1111_1111_1111_111111111111",  // wrong separators
		" 11111111-1111-1111-1111-111111111111", // leading space
		"11111111-1111-1111-1111-111111111111 ", // trailing space
	}
	for _, s := range invalid {
		if isUUID(s) {
			t.Errorf("isUUID(%q) = true, want false", s)
		}
	}
}

func TestMostRecent(t *testing.T) {
	t.Run("newest wins", func(t *testing.T) {
		home := setHome(t)
		writeSession(t, home, "-p-old", uA, `{"cwd":"/p/old"}`, baseTime)
		writeSession(t, home, "-p-new", uB, `{"cwd":"/p/new"}`, baseTime.Add(time.Hour))

		s, ok, err := MostRecent()
		if err != nil {
			t.Fatalf("MostRecent: %v", err)
		}
		if !ok {
			t.Fatal("MostRecent: ok = false, want true")
		}
		if s.ID != uB {
			t.Errorf("MostRecent: ID = %q, want %q (newest)", s.ID, uB)
		}
	})

	t.Run("none exist", func(t *testing.T) {
		setHome(t) // no projects dir
		s, ok, err := MostRecent()
		if err != nil {
			t.Fatalf("MostRecent: %v", err)
		}
		if ok {
			t.Errorf("MostRecent: ok = true, want false; got %#v", s)
		}
	})
}

func TestFindTranscript(t *testing.T) {
	home := setHome(t)
	// Two sessions sharing the leading block "11111111"; uA is OLDER, uPrefixNewer
	// is NEWER. This lets us prove exact-id lookup ignores recency while a bare
	// prefix lookup honours it.
	uPrefixNewer := "11111111-9999-9999-9999-999999999999"
	oldPath := writeSession(t, home, "-p-old", uA, `{"cwd":"/p/old"}`, baseTime)
	newPath := writeSession(t, home, "-p-new", uPrefixNewer, `{"cwd":"/p/new"}`, baseTime.Add(time.Hour))
	// An unrelated session to ensure it is never returned.
	writeSession(t, home, "-p-other", uC, `{"cwd":"/p/other"}`, baseTime.Add(2*time.Hour))

	t.Run("exact id wins over a more-recent prefix sibling", func(t *testing.T) {
		// Query uA exactly. uPrefixNewer is newer and shares uA's "11111111"
		// prefix, yet the exact match must return uA's (older) transcript.
		path, ok, err := FindTranscript(uA)
		if err != nil {
			t.Fatalf("FindTranscript: %v", err)
		}
		if !ok {
			t.Fatal("FindTranscript(exact): ok = false, want true")
		}
		if path != oldPath {
			t.Errorf("FindTranscript(exact): path = %q, want %q (exact older session)", path, oldPath)
		}
	})

	t.Run("bare prefix returns most-recent-first", func(t *testing.T) {
		// "11111111" is a prefix of both uA and uPrefixNewer, exact of neither;
		// the newer transcript must win.
		path, ok, err := FindTranscript("11111111")
		if err != nil {
			t.Fatalf("FindTranscript: %v", err)
		}
		if !ok {
			t.Fatal("FindTranscript(prefix): ok = false, want true")
		}
		if path != newPath {
			t.Errorf("FindTranscript(prefix): path = %q, want %q (newer prefix match)", path, newPath)
		}
	})

	t.Run("unknown id => ok=false", func(t *testing.T) {
		path, ok, err := FindTranscript("deadbeef-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("FindTranscript: %v", err)
		}
		if ok {
			t.Errorf("FindTranscript(unknown): ok = true, want false (path=%q)", path)
		}
		if path != "" {
			t.Errorf("FindTranscript(unknown): path = %q, want empty", path)
		}
	})
}
