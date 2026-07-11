package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sudhanshu069/claude-says/internal/config"
	"github.com/Sudhanshu069/claude-says/internal/narrator"
	"github.com/Sudhanshu069/claude-says/internal/tts"
	"github.com/Sudhanshu069/claude-says/internal/tui"
)

// ---------------------------------------------------------------------------
// Fakes. None of these touch afplay/say/network/config side effects.
// ---------------------------------------------------------------------------

// fakeProvider records the texts it is asked to synthesize (in call order) and
// returns []byte(text) as the "audio" so a test can prove which text's audio
// reached the player. errFor lets a test fail one specific sentence's synth.
// When enter != nil the FIRST call announces itself on enter and blocks on
// release, letting a test park a synth result in flight while it bumps the
// epoch (session switch).
type fakeProvider struct {
	mu        sync.Mutex
	texts     []string
	format    string
	errFor    map[string]error
	panicFor  map[string]bool // texts that make Synthesize panic (tests #11 recover)
	firstDone bool
	enter     chan struct{}
	release   chan struct{}
}

func (f *fakeProvider) Synthesize(ctx context.Context, text string) ([]byte, string, error) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	first := !f.firstDone
	f.firstDone = true
	err := f.errFor[text]
	shouldPanic := f.panicFor[text]
	f.mu.Unlock()

	if first && f.enter != nil {
		f.enter <- struct{}{}
		<-f.release
	}
	if shouldPanic {
		panic("fake provider panic: " + text)
	}
	if err != nil {
		return nil, "", err
	}
	format := f.format
	if format == "" {
		format = tts.FormatAIFF
	}
	return []byte(text), format, nil
}

func (f *fakeProvider) Validate(ctx context.Context) error { return nil }

func (f *fakeProvider) askedTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.texts...)
}

// fakePlayer records playback order/args. When block != nil, Play announces
// itself on entered (if non-nil) then blocks until block is closed — used to
// hold a sentence "in flight" while a test exercises Stop's drain. It never
// execs afplay.
type fakePlayer struct {
	mu       sync.Mutex
	played   [][]byte
	formats  []string
	playedCh chan string   // each successful play publishes string(audio)
	entered  chan struct{} // signalled once when Play is entered
	block    chan struct{} // if non-nil, Play blocks until closed
}

func (p *fakePlayer) Play(ctx context.Context, audio []byte, format string) error {
	if p.entered != nil {
		select {
		case p.entered <- struct{}{}:
		default:
		}
	}
	if p.block != nil {
		select {
		case <-p.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p.mu.Lock()
	p.played = append(p.played, audio)
	p.formats = append(p.formats, format)
	p.mu.Unlock()
	if p.playedCh != nil {
		p.playedCh <- string(audio)
	}
	return nil
}

func (p *fakePlayer) playedTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.played))
	for i, b := range p.played {
		out[i] = string(b)
	}
	return out
}

// upperNarrator uppercases the text. It does NOT implement narrator.Degrader,
// so the daemon takes the plain Narrate path.
type upperNarrator struct{}

func (upperNarrator) Narrate(ctx context.Context, text string) string {
	return strings.ToUpper(text)
}
func (upperNarrator) Validate(ctx context.Context) error { return nil }

// degradeNarrator uppercases AND implements narrator.Degrader, reporting a
// non-nil error so the daemon logs a degrade warning but still speaks the
// returned (uppercased) string.
type degradeNarrator struct{ err error }

func (d degradeNarrator) Narrate(ctx context.Context, text string) string {
	return strings.ToUpper(text)
}
func (d degradeNarrator) Validate(ctx context.Context) error { return nil }
func (d degradeNarrator) NarrateOrErr(ctx context.Context, text string) (string, error) {
	return strings.ToUpper(text), d.err
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// lockedBuf is a race-safe io.Writer for capturing slog output from the
// daemon's concurrent synth goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs redirects the global slog logger to a buffer for the test.
func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	lb := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return lb
}

// isolateHome points HOME/TMPDIR at fresh temp dirs so session.MostRecent finds
// nothing (~/.claude/projects absent) => the daemon starts no watcher and stays
// idle until a test injects text. Nothing touches the real user dirs.
func isolateHome(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", home)
}

// feedText injects one text chunk into the daemon's pipeline via the inject seam
// (the successor to the removed IPC path): Run feeds it exactly like a watcher
// event. The inject channel is buffered, so a send before Run reaches its select
// simply queues — no start-up race.
func feedText(t *testing.T, d *Daemon, text string) {
	t.Helper()
	select {
	case d.inject <- text:
	case <-d.runDone:
		t.Fatalf("Run already returned; cannot inject %q", text)
	case <-time.After(3 * time.Second):
		t.Fatalf("inject of %q timed out (pipeline not draining)", text)
	}
}

// assistantLine builds one Claude-style transcript JSONL record (a single text
// content block) as the watcher expects it, newline-terminated.
func assistantLine(uuid, sessionID, text string) string {
	return fmt.Sprintf(
		`{"type":"assistant","uuid":%q,"sessionId":%q,"message":{"content":[{"type":"text","text":%q}]}}`+"\n",
		uuid, sessionID, text,
	)
}

// waitForEvent reads events (discarding non-matching ones) until pred matches or
// the deadline fires. Safe because the daemon's emit is non-blocking.
func waitForEvent(t *testing.T, events <-chan tui.Event, pred func(tui.Event) bool, d time.Duration) tui.Event {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("events channel closed before a matching event arrived")
			}
			if pred(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for event")
		}
	}
}

func waitPlayed(t *testing.T, ch <-chan string, d time.Duration) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		t.Fatal("timed out waiting for playback")
		return ""
	}
}

// startRun launches d.Run in a goroutine and returns a channel carrying its
// return value, plus a cancel func. The caller must drain runErr (via
// awaitRun) before the test ends.
func startRun(t *testing.T, d *Daemon) (context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- d.Run(ctx) }()
	return ctx, cancel, runErr
}

func awaitRun(t *testing.T, runErr <-chan error, d time.Duration) error {
	t.Helper()
	select {
	case err := <-runErr:
		return err
	case <-time.After(d):
		t.Fatal("Run did not return in time")
		return nil
	}
}

// newDaemon builds a Daemon with injected fakes and no initial session/transcript
// (so the IPC fallback is the active text source). narr may be nil.
func newDaemon(t *testing.T, opts Options, p *fakeProvider, pl *fakePlayer, narr narrator.Narrator) *Daemon {
	t.Helper()
	cfg := config.DefaultConfig()
	d, err := newWithDeps(cfg, opts, p, pl, narr)
	if err != nil {
		t.Fatalf("newWithDeps: %v", err)
	}
	return d
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// Req 1: a fed sentence is synthesized by the provider and played end-to-end,
// carrying the provider's audio bytes and format through to the player.
func TestPipeline_EndToEnd(t *testing.T) {
	isolateHome(t)

	prov := &fakeProvider{format: tts.FormatWAV}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	feedText(t, d, "Hello there listeners. ")

	got := waitPlayed(t, player.playedCh, 3*time.Second)
	const want = "Hello there listeners."
	if got != want {
		t.Fatalf("played audio = %q, want %q", got, want)
	}
	if texts := prov.askedTexts(); len(texts) != 1 || texts[0] != want {
		t.Fatalf("provider asked to synthesize %v, want [%q]", texts, want)
	}
	if fmts := player.formats; len(fmts) != 1 || fmts[0] != tts.FormatWAV {
		t.Fatalf("player format = %v, want [%q]", fmts, tts.FormatWAV)
	}
}

// Req 2: with a narrator on, the provider receives the NARRATED text, not raw.
func TestPipeline_NarratorRewritesText(t *testing.T) {
	t.Run("plain narrator", func(t *testing.T) {
		isolateHome(t)
		prov := &fakeProvider{}
		player := &fakePlayer{playedCh: make(chan string, 8)}
		d := newDaemon(t, Options{}, prov, player, upperNarrator{})

		_, cancel, runErr := startRun(t, d)
		defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

		feedText(t, d, "make this loud please. ")

		got := waitPlayed(t, player.playedCh, 3*time.Second)
		const want = "MAKE THIS LOUD PLEASE."
		if got != want {
			t.Fatalf("played audio = %q, want narrated %q", got, want)
		}
		if texts := prov.askedTexts(); len(texts) != 1 || texts[0] != want {
			t.Fatalf("provider got %v, want narrated [%q]", texts, want)
		}
	})

	t.Run("degrader logs warn but still speaks", func(t *testing.T) {
		isolateHome(t)
		logs := captureLogs(t)
		prov := &fakeProvider{}
		player := &fakePlayer{playedCh: make(chan string, 8)}
		d := newDaemon(t, Options{}, prov, player, degradeNarrator{err: errors.New("llm unreachable")})

		_, cancel, runErr := startRun(t, d)
		defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

		feedText(t, d, "speak this anyway. ")

		got := waitPlayed(t, player.playedCh, 3*time.Second)
		const want = "SPEAK THIS ANYWAY."
		if got != want {
			t.Fatalf("played audio = %q, want narrated %q", got, want)
		}
		if texts := prov.askedTexts(); len(texts) != 1 || texts[0] != want {
			t.Fatalf("provider got %v, want narrated [%q]", texts, want)
		}
		if !strings.Contains(logs.String(), "narrator degraded") {
			t.Fatalf("expected a narrator-degrade warning in logs, got:\n%s", logs.String())
		}
	})
}

// Req 3: SwitchSession bumps the epoch, so a stale old-session synth result is
// dropped by the queue — no cross-session audio bleed.
func TestPipeline_SwitchSessionDropsStaleResult(t *testing.T) {
	isolateHome(t)

	prov := &fakeProvider{
		enter:   make(chan struct{}),
		release: make(chan struct{}),
	}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	// Sentence 1 enters synth at epoch 0 and parks there.
	feedText(t, d, "First sentence alpha. ")
	select {
	case <-prov.enter:
	case <-time.After(3 * time.Second):
		t.Fatal("first synth never started")
	}

	// Switch sessions: epoch bumps to 1, queue.Switch(1) supersedes epoch 0.
	d.SwitchSession("")
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventSessionSwitched
	}, 3*time.Second)

	// Release the parked synth: its epoch-0 result must be dropped by the queue.
	close(prov.release)

	// Sentence 2 belongs to the new epoch and must be the one that plays.
	feedText(t, d, "Second sentence beta. ")
	got := waitPlayed(t, player.playedCh, 3*time.Second)
	const want = "Second sentence beta."
	if got != want {
		t.Fatalf("played %q, want new-session %q", got, want)
	}

	// The stale first-session audio must never have reached the player.
	if played := player.playedTexts(); len(played) != 1 || played[0] != want {
		t.Fatalf("player played %v, want exactly [%q] (stale result leaked)", played, want)
	}
	// Both sentences were, however, handed to the provider.
	texts := prov.askedTexts()
	if len(texts) != 2 || texts[0] != "First sentence alpha." || texts[1] != want {
		t.Fatalf("provider asked %v, want both sentences", texts)
	}
}

// Req #11: a provider PANIC is recovered — it degrades exactly that one
// sentence (surfaced as an error event) while the daemon keeps running and a
// later sentence still plays. Without synth's recover(), the panic in the synth
// goroutine would crash the whole process, so this test would hard-fail.
func TestPipeline_ProviderPanicIsRecovered(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)

	prov := &fakeProvider{
		panicFor: map[string]bool{"This one panics.": true},
	}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	feedText(t, d, "This one panics. ")
	feedText(t, d, "This one survives. ")

	// The panicking sentence surfaces as a (recovered) error event.
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventError
	}, 3*time.Second)

	got := waitPlayed(t, player.playedCh, 3*time.Second)
	const want = "This one survives."
	if got != want {
		t.Fatalf("played %q, want the surviving sentence %q after a provider panic", got, want)
	}
	if played := player.playedTexts(); len(played) != 1 || played[0] != want {
		t.Fatalf("player played %v, want exactly [%q] (panic must not drop later sentences)", played, want)
	}
	if !strings.Contains(logs.String(), "synth panic") {
		t.Fatalf("expected a recovered-panic entry in the queue error log, got:\n%s", logs.String())
	}
}

// Req 4: a provider Synthesize error is logged and skipped; the daemon keeps
// going and later sentences still play.
func TestPipeline_SynthErrorIsSkipped(t *testing.T) {
	isolateHome(t)
	logs := captureLogs(t)

	prov := &fakeProvider{
		errFor: map[string]error{"This fails now.": errors.New("boom")},
	}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	feedText(t, d, "This fails now. ")
	feedText(t, d, "This works fine. ")

	// The failing sentence surfaces as an error event, and the daemon survives.
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventError
	}, 3*time.Second)

	got := waitPlayed(t, player.playedCh, 3*time.Second)
	const want = "This works fine."
	if got != want {
		t.Fatalf("played %q, want the surviving sentence %q", got, want)
	}
	if played := player.playedTexts(); len(played) != 1 || played[0] != want {
		t.Fatalf("player played %v, want exactly [%q]", played, want)
	}
	if !strings.Contains(logs.String(), "synth failed") {
		t.Fatalf("expected a synth-failure log, got:\n%s", logs.String())
	}
}

// Drives the real transcript-watcher path (Options.TranscriptPath) rather than
// the IPC fallback: it exercises selectInitialSource's transcript branch,
// startWatcher, and Run's watcherEvents case end-to-end.
func TestPipeline_WatcherEndToEnd(t *testing.T) {
	isolateHome(t)

	path := filepath.Join(t.TempDir(), "t.jsonl")
	// Create the file empty so the watcher starts at offset 0.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create transcript: %v", err)
	}

	prov := &fakeProvider{format: tts.FormatAIFF}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{TranscriptPath: path}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	// Wait until the watcher is armed before appending (shrinks the EOF-offset
	// race; the same empty-file-then-append pattern the watcher's own suite uses).
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventWatching
	}, 3*time.Second)

	// The watcher records its start offset from a one-time os.Stat inside Run.
	// EventWatching is emitted the instant that goroutine is launched, NOT after
	// its Stat, so under load the Stat can lose the race to this append: the
	// watcher would then start at EOF *past* our line and never see it. Re-append
	// the SAME block (identical uuid) on a ticker until the first sentence plays.
	// This is dedup-safe: any copy written before the watcher's offset is never
	// parsed (so its uuid is never marked seen), and the first copy the watcher
	// does read emits the block and dedups every later copy — so exactly one block
	// is spoken no matter how many copies we wrote.
	line := assistantLine("u1", "sess-1", "The watcher path works. Second bit follows. ")
	appendCopy := func() {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		_, _ = f.WriteString(line)
		_ = f.Close()
	}

	stopAppend := make(chan struct{})
	appendDone := make(chan struct{})
	go func() {
		defer close(appendDone)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			appendCopy()
			select {
			case <-stopAppend:
				return
			case <-tick.C:
			}
		}
	}()
	// Always join the appender before t.TempDir is torn down, even if an assertion
	// below exits via t.Fatal. Registered after t.TempDir's own cleanup, so LIFO
	// ordering runs this (joining the writer) before the dir is removed.
	t.Cleanup(func() { close(stopAppend); <-appendDone })

	// Playback order is guaranteed (ordered queue); the first sentence in the
	// block plays first.
	got := waitPlayed(t, player.playedCh, 3*time.Second)
	const want = "The watcher path works."
	if got != want {
		t.Fatalf("played %q, want %q", got, want)
	}
	// The two sentences launch concurrent synth goroutines, so provider CALL
	// order is nondeterministic — assert membership, not position.
	if texts := prov.askedTexts(); !contains(texts, want) {
		t.Fatalf("provider asked %v, want it to include %q", texts, want)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// Pause/Resume flow through the exported methods and the Controls() channel,
// each surfacing a status event from Run's control handler.
func TestPipeline_PauseResume(t *testing.T) {
	isolateHome(t)

	prov := &fakeProvider{}
	player := &fakePlayer{}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	d.Pause()
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventStatus && e.Text == "Paused"
	}, 3*time.Second)

	// Resume via the raw Controls() channel to exercise that accessor too.
	d.Controls() <- tui.Control{Kind: tui.ControlResume}
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventStatus && e.Text == "Resumed"
	}, 3*time.Second)
}

// Mute makes the daemon discard sentences instead of playing them; Unmute
// resumes. Proves the ControlMute/ControlUnmute wiring end to end through the
// real processor + queue: the muted sentence never reaches the player.
func TestPipeline_MuteDiscardsUnmutePlays(t *testing.T) {
	isolateHome(t)

	prov := &fakeProvider{}
	player := &fakePlayer{playedCh: make(chan string, 8)}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer func() { cancel(); awaitRun(t, runErr, 3*time.Second) }()

	d.Controls() <- tui.Control{Kind: tui.ControlMute}
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventStatus && e.Text == "Muted"
	}, 3*time.Second)

	// Feed while muted: the queue discards it and goes idle. Gating on the drain
	// makes the discard deterministic before we unmute (avoids the synth race).
	feedText(t, d, "This must be silenced now. ")
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventDrained
	}, 3*time.Second)

	d.Controls() <- tui.Control{Kind: tui.ControlUnmute}
	waitForEvent(t, d.Events(), func(e tui.Event) bool {
		return e.Kind == tui.EventStatus && e.Text == "Unmuted"
	}, 3*time.Second)
	feedText(t, d, "This must be spoken now. ")

	if got := waitPlayed(t, player.playedCh, 3*time.Second); got != "This must be spoken now." {
		t.Fatalf("played %q, want only the post-unmute sentence", got)
	}
	for _, s := range player.playedTexts() {
		if s == "This must be silenced now." {
			t.Fatalf("muted sentence reached the player: %v", player.playedTexts())
		}
	}
}

// Req 5: Stop(timeout) drains the in-flight sentence, then returns; Run's
// goroutine exits gracefully (nil).
func TestPipeline_StopDrainsInFlight(t *testing.T) {
	isolateHome(t)

	prov := &fakeProvider{}
	block := make(chan struct{})
	player := &fakePlayer{
		entered: make(chan struct{}, 1),
		block:   block,
	}
	d := newDaemon(t, Options{}, prov, player, nil)

	_, cancel, runErr := startRun(t, d)
	defer cancel()

	feedText(t, d, "Draining in progress now. ")

	// Wait until the sentence is genuinely mid-playback (in flight).
	select {
	case <-player.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("playback never started")
	}

	stopReturned := make(chan struct{})
	go func() { d.Stop(2 * time.Second); close(stopReturned) }()

	// Stop must block on the in-flight (blocked) playback rather than returning.
	select {
	case <-stopReturned:
		t.Fatal("Stop returned before the in-flight playback drained")
	case <-time.After(50 * time.Millisecond):
	}

	// Let playback finish; the drain should now complete.
	close(block)

	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after playback drained")
	}

	// Run exited gracefully (nil), and the in-flight sentence was played.
	if err := awaitRun(t, runErr, 2*time.Second); err != nil {
		t.Fatalf("Run returned %v, want nil after graceful drain", err)
	}
	if played := player.playedTexts(); len(played) != 1 || played[0] != "Draining in progress now." {
		t.Fatalf("player played %v, want the drained sentence", played)
	}
}
