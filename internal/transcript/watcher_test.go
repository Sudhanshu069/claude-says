package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recvTimeout is a generous bound: the 200ms safety poll guarantees delivery
// regardless of fsnotify availability, so ~3s never flakes but still catches a
// genuine stall.
const recvTimeout = 3 * time.Second

// assistantLine builds one assistant JSONL record (newline-terminated) with the
// given uuid, sessionId, and one text block per texts entry.
func assistantLine(uuid, sessionID string, texts ...string) string {
	rec := map[string]any{
		"type":      "assistant",
		"uuid":      uuid,
		"sessionId": sessionID,
	}
	content := make([]map[string]any, 0, len(texts))
	for _, t := range texts {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	rec["message"] = map[string]any{"content": content}
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// append writes s to the end of path, creating it if absent.
func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("open for append: %v", err)
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		t.Fatalf("append write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("append close: %v", err)
	}
}

// recv waits for one Event on w.Events() up to recvTimeout, failing the test on
// timeout or on an unexpected channel close.
func recv(t *testing.T, w *Watcher) Event {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if !ok {
			t.Fatalf("Events channel closed while awaiting an event")
		}
		return ev
	case <-time.After(recvTimeout):
		t.Fatalf("timed out after %s awaiting an event", recvTimeout)
		return Event{}
	}
}

// expectNoEvent asserts that no Event arrives within d.
func expectNoEvent(t *testing.T, w *Watcher, d time.Duration) {
	t.Helper()
	select {
	case ev, ok := <-w.Events():
		if !ok {
			return // closed channel is not a spurious event
		}
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(d):
	}
}

// startWatcher creates a watcher on path, runs it, and returns it plus a stop
// func that cancels and waits for Run to return (asserting ctx.Err and that
// Events() is closed).
func startWatcher(t *testing.T, path string) (*Watcher, func()) {
	t.Helper()
	w := New(path)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	return w, func() {
		cancel()
		select {
		case err := <-done:
			if err != context.Canceled {
				t.Errorf("Run returned %v, want context.Canceled", err)
			}
		case <-time.After(recvTimeout):
			t.Errorf("Run did not return after cancel")
		}
	}
}

func TestAppendedAssistantLineEmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "") // create empty file so Run starts at offset 0
	w, stop := startWatcher(t, path)
	defer stop()

	appendTo(t, path, assistantLine("u1", "sess-1", "hello world"))

	ev := recv(t, w)
	if ev.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", ev.SessionID)
	}
	if ev.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", ev.Text)
	}
	if ev.Time.IsZero() {
		t.Errorf("Time is zero, want set")
	}
}

func TestStartsAtEOF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	// Pre-existing content must NOT be re-emitted.
	appendTo(t, path, assistantLine("pre1", "sess-1", "old one"))
	appendTo(t, path, assistantLine("pre2", "sess-1", "old two"))

	w, stop := startWatcher(t, path)
	defer stop()

	// Give the poll a couple of cycles: nothing should arrive for old lines.
	expectNoEvent(t, w, 500*time.Millisecond)

	// Only the newly appended line is emitted.
	appendTo(t, path, assistantLine("new1", "sess-1", "fresh"))
	ev := recv(t, w)
	if ev.Text != "fresh" {
		t.Errorf("Text = %q, want fresh", ev.Text)
	}
}

func TestDedupByUUID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w, stop := startWatcher(t, path)
	defer stop()

	line := assistantLine("dup", "sess-1", "once")
	appendTo(t, path, line)
	ev := recv(t, w)
	if ev.Text != "once" {
		t.Errorf("Text = %q, want once", ev.Text)
	}

	// Same uuid again => dropped.
	appendTo(t, path, line)
	expectNoEvent(t, w, 500*time.Millisecond)

	// A record with an empty uuid is always emitted (matches Node behavior).
	appendTo(t, path, assistantLine("", "sess-1", "nouuid-a"))
	appendTo(t, path, assistantLine("", "sess-1", "nouuid-b"))
	if ev := recv(t, w); ev.Text != "nouuid-a" {
		t.Errorf("Text = %q, want nouuid-a", ev.Text)
	}
	if ev := recv(t, w); ev.Text != "nouuid-b" {
		t.Errorf("Text = %q, want nouuid-b", ev.Text)
	}
}

func TestNonAssistantAndContentFiltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w, stop := startWatcher(t, path)
	defer stop()

	// Non-assistant record: skipped.
	userRec := `{"type":"user","uuid":"ur","sessionId":"sess-1","message":{"content":[{"type":"text","text":"user says"}]}}` + "\n"
	appendTo(t, path, userRec)

	// Assistant record whose only block is non-text: skipped.
	toolRec := `{"type":"assistant","uuid":"tr","sessionId":"sess-1","message":{"content":[{"type":"tool_use","text":"ignored"}]}}` + "\n"
	appendTo(t, path, toolRec)

	// Assistant record with multiple text blocks: one Event per block, in order.
	appendTo(t, path, assistantLine("multi", "sess-1", "block A", "block B"))

	if ev := recv(t, w); ev.Text != "block A" {
		t.Errorf("first Text = %q, want block A", ev.Text)
	}
	if ev := recv(t, w); ev.Text != "block B" {
		t.Errorf("second Text = %q, want block B", ev.Text)
	}
	// Nothing leaked from the skipped records.
	expectNoEvent(t, w, 300*time.Millisecond)
}

func TestMalformedLineSkipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w, stop := startWatcher(t, path)
	defer stop()

	// Truncated/garbage JSON must be skipped without stalling the loop.
	appendTo(t, path, "{not valid json at all\n")
	appendTo(t, path, `{"type":"assistant","uuid":"x","message":{"content":[{"type":"text","text":`+"\n")
	// A well-formed line after the garbage still gets through.
	appendTo(t, path, assistantLine("ok", "sess-1", "recovered"))

	ev := recv(t, w)
	if ev.Text != "recovered" {
		t.Errorf("Text = %q, want recovered", ev.Text)
	}
}

func TestEmptySessionDefaultsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w, stop := startWatcher(t, path)
	defer stop()

	appendTo(t, path, assistantLine("s0", "", "no session"))
	ev := recv(t, w)
	if ev.SessionID != "unknown" {
		t.Errorf("SessionID = %q, want unknown", ev.SessionID)
	}
}

func TestTruncateResetsOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, assistantLine("orig", "sess-1", "original content"))
	w, stop := startWatcher(t, path)
	defer stop()

	// Consume nothing yet (started at EOF). Now shrink the file below the
	// current offset by rewriting it smaller: the watcher resets to 0 and
	// re-emits from the top.
	if err := os.WriteFile(path, []byte(assistantLine("after", "sess-1", "short")), 0o600); err != nil {
		t.Fatalf("truncate rewrite: %v", err)
	}
	ev := recv(t, w)
	if ev.Text != "short" {
		t.Errorf("Text = %q, want short", ev.Text)
	}
}

func TestAbsentFileThenCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "later.jsonl")
	// Do NOT create the file; Run must start at offset 0 and the safety poll
	// picks it up once it appears.
	w, stop := startWatcher(t, path)
	defer stop()

	appendTo(t, path, assistantLine("born", "sess-1", "created late"))
	ev := recv(t, w)
	if ev.Text != "created late" {
		t.Errorf("Text = %q, want created late", ev.Text)
	}
}

func TestDeletionDoesNotPanicOrSpam(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w, stop := startWatcher(t, path)
	defer stop()

	// The first line is deliberately long so the recreated file below is
	// strictly smaller than the offset we advance to here.
	appendTo(t, path, assistantLine("a1", "sess-1", "first line, kept deliberately long"))
	if ev := recv(t, w); ev.Text != "first line, kept deliberately long" {
		t.Errorf("Text = %q, want the long first line", ev.Text)
	}

	// Remove the file: the watcher must neither panic nor spam events.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	expectNoEvent(t, w, 500*time.Millisecond)

	// Recreating it works again. The new file is shorter than the previous
	// offset, so the watcher's shrink/rotate guard resets to 0 and re-reads
	// from the top, re-emitting the fresh line.
	appendTo(t, path, assistantLine("a2", "sess-1", "reborn"))
	if ev := recv(t, w); ev.Text != "reborn" {
		t.Errorf("Text = %q, want reborn", ev.Text)
	}
}

func TestCancelClosesEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	appendTo(t, path, "")
	w := New(path)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(recvTimeout):
		t.Fatalf("Run did not return after cancel")
	}

	// Events() must be closed after Run returns.
	select {
	case _, ok := <-w.Events():
		if ok {
			t.Errorf("Events() delivered a value, want closed channel")
		}
	case <-time.After(recvTimeout):
		t.Fatalf("Events() not closed after Run returned")
	}
}
