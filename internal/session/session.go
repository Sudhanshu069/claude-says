// Package session discovers Claude Code sessions from ~/.claude/projects,
// mirroring Node src/sessions.js. Each transcript is a JSONL file at
// ~/.claude/projects/<project-dir>/<session-id>.jsonl.
package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Info is an alias for Session; the TUI package refers to sessions as
// session.Info, so this keeps both frozen names in sync.
type Info = Session

// Session is one discovered Claude Code transcript.
type Session struct {
	ID             string    // transcript basename (a UUID)
	ProjectDir     string    // encoded dir name under ~/.claude/projects
	ProjectName    string    // real cwd if recoverable, else decoded ProjectDir
	TranscriptPath string    // absolute path to <id>.jsonl
	LastActive     time.Time // file mtime
	Title          string    // human label (most recent ai-title, else first prompt); populated by DiscoverWithTitles
}

// uuidRe matches a canonical UUID, mirroring the Node isUUID regex.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// cwdReadLimit bounds how much of a transcript readProjectCwd inspects.
const cwdReadLimit = 8192

// projectsDir returns ~/.claude/projects.
func projectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// Discover scans ~/.claude/projects for *.jsonl whose basename is a UUID,
// sorted most-recent-first. Returns (nil, nil) when the projects dir is absent.
func Discover() ([]Session, error) {
	root, err := projectsDir()
	if err != nil {
		return nil, err
	}

	projectDirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			// ~/.claude/projects/ doesn't exist yet.
			return nil, nil
		}
		return nil, err
	}

	var sessions []Session
	for _, pd := range projectDirs {
		if !pd.IsDir() {
			continue
		}
		projectDir := pd.Name()
		projectPath := filepath.Join(root, projectDir)

		entries, err := os.ReadDir(projectPath)
		if err != nil {
			// Skip inaccessible dirs.
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if !isUUID(id) {
				continue
			}

			entryPath := filepath.Join(projectPath, name)
			fi, err := os.Stat(entryPath)
			if err != nil {
				continue
			}

			projectName := readProjectCwd(entryPath)
			if projectName == "" {
				projectName = decodeProjectDir(projectDir)
			}

			sessions = append(sessions, Session{
				ID:             id,
				ProjectDir:     projectDir,
				ProjectName:    projectName,
				TranscriptPath: entryPath,
				LastActive:     fi.ModTime(),
			})
		}
	}

	// Sort by most recently active first.
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].LastActive.After(sessions[j].LastActive)
	})
	return sessions, nil
}

// MostRecent returns the newest session; ok=false when none exist.
//
// NOTE (global-vs-cwd gotcha, preserved from Node getMostRecentSession): this
// returns the single most-recently-active session across ALL projects, not the
// session for the current working directory. If you have an old shell in one
// project and are actively working in another, this still points at whichever
// transcript was touched last, regardless of where the daemon was started.
func MostRecent() (s Session, ok bool, err error) {
	sessions, err := Discover()
	if err != nil {
		return Session{}, false, err
	}
	if len(sessions) == 0 {
		return Session{}, false, nil
	}
	return sessions[0], true, nil
}

// FindTranscript resolves a full-or-prefix session id to its transcript path.
// It matches an exact id first, then a prefix (Node's startsWith). This is a
// deliberate tightening of Node's single-pass find, which could return a
// more-recent prefix match ahead of an exact match; here an exact hit always
// wins over any prefix hit.
func FindTranscript(id string) (path string, ok bool, err error) {
	sessions, err := Discover()
	if err != nil {
		return "", false, err
	}
	// Exact match first.
	for _, s := range sessions {
		if s.ID == id {
			return s.TranscriptPath, true, nil
		}
	}
	// Then prefix match (most-recent-first, since sessions is sorted).
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, id) {
			return s.TranscriptPath, true, nil
		}
	}
	return "", false, nil
}

// isUUID reports whether s is a canonical UUID (transcript basename shape).
func isUUID(s string) bool {
	return uuidRe.MatchString(s)
}

// readProjectCwd reads a bounded 8KB prefix of the transcript and returns the
// first record's cwd (accurate for dashed dir names). Empty string on failure.
func readProjectCwd(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	buf, err := io.ReadAll(io.LimitReader(f, cwdReadLimit))
	if err != nil || len(buf) == 0 {
		return ""
	}

	scanner := bufio.NewScanner(strings.NewReader(string(buf)))
	scanner.Buffer(make([]byte, 0, 64*1024), cwdReadLimit+1)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// Truncated last line in the window — ignore.
			continue
		}
		if rec.Cwd != "" {
			return rec.Cwd
		}
	}
	return ""
}

// decodeProjectDir is the lossy fallback that turns an encoded project dir name
// back into a path (dashes -> slashes). Prefer readProjectCwd.
//
// Mirrors Node: dir.replace(/^-/, '/').replace(/-/g, '/'). The leading dash and
// every other dash all become '/', which is lossy for directory names that
// themselves contain dashes.
func decodeProjectDir(dir string) string {
	if strings.HasPrefix(dir, "-") {
		dir = "/" + dir[1:]
	}
	return strings.ReplaceAll(dir, "-", "/")
}

// titleTailLimit bounds how much of a transcript's tail readAITitle scans.
const titleTailLimit = 128 * 1024

// DiscoverWithTitles is Discover plus a human Title per session (its most recent
// ai-title, else first prompt). Display surfaces (the switcher, `sessions`) use
// it; the hot paths (MostRecent/FindTranscript) stay on the cheaper Discover.
func DiscoverWithTitles() ([]Session, error) {
	sessions, err := Discover()
	if err != nil {
		return nil, err
	}
	for i := range sessions {
		sessions[i].Title = ReadTitle(sessions[i].TranscriptPath)
	}
	return sessions, nil
}

// ReadTitle returns a human label for a transcript: the most recent Claude Code
// "ai-title" record, else the first user prompt (one line), else "". It reads
// only the file's tail for the title and, on fallback, a bounded head — never
// the whole file.
func ReadTitle(transcriptPath string) string {
	if t := readAITitle(transcriptPath); t != "" {
		return t
	}
	return readFirstPrompt(transcriptPath)
}

// readAITitle scans the transcript's tail for the last "ai-title" record's title.
// Titles are regenerated through a session, so the last one is the current one.
func readAITitle(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	if start := fi.Size() - titleTailLimit; start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return ""
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		return ""
	}

	lines := bytes.Split(buf, []byte{'\n'})
	// Scan from the end; the first ai-title found is the most recent.
	for i := len(lines) - 1; i >= 0; i-- {
		if !bytes.Contains(lines[i], []byte(`"ai-title"`)) {
			continue
		}
		var rec struct {
			AITitle string `json:"aiTitle"`
		}
		if json.Unmarshal(lines[i], &rec) == nil && rec.AITitle != "" {
			return strings.TrimSpace(rec.AITitle)
		}
	}
	return ""
}

// readFirstPrompt returns the first user prompt in the transcript as a single
// trimmed line, skipping command/tool-injected content, or "".
func readFirstPrompt(transcriptPath string) string {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"type":"user"`)) {
			continue
		}
		var rec struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		text := oneLine(firstText(rec.Message.Content))
		// Skip slash-command / tool-result envelopes (e.g. "<command-name>...").
		if text == "" || strings.HasPrefix(text, "<") {
			continue
		}
		return truncate(text, 72)
	}
	return ""
}

// truncate shortens s to at most n runes, appending an ellipsis when it cuts.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

// firstText extracts human text from a user message's content, which is either a
// JSON string or an array of {type:"text",text:"..."} blocks.
func firstText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}

// oneLine trims s to its first non-empty line.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
