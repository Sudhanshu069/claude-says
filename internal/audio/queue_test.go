package audio

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

// --- Frozen fake player -----------------------------------------------------

// fakePlayer records the first byte of every fully-played buffer, in play
// order. It never touches afplay/audio. When release != nil, Play blocks until
// a value is received (or ctx is cancelled), letting tests observe EventPlaying,
// then release or cancel a specific playback deterministically. An interrupted
// (ctx-cancelled) Play records nothing — matching pause/switch semantics.
type fakePlayer struct {
	mu      sync.Mutex
	order   []byte        // first byte of each fully-played buffer, in play order
	release chan struct{} // nil => Play returns immediately; non-nil => Play blocks
	fail    error         // if non-nil, returned after (optional) release
}

func (f *fakePlayer) Play(ctx context.Context, audio []byte, format string) error {
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return ctx.Err() // interrupted attempt records nothing
		}
	}
	if f.fail != nil {
		return f.fail
	}
	f.mu.Lock()
	if len(audio) > 0 {
		f.order = append(f.order, audio[0])
	}
	f.mu.Unlock()
	return nil
}

func (f *fakePlayer) snapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]byte, len(f.order))
	copy(out, f.order)
	return out
}

// gatedPlayer models a playback that finishes SUCCESSFULLY the instant before a
// cancel lands: Play deliberately IGNORES ctx and returns nil once released,
// recording the audio's first byte. This is the exact scenario the PLAYBACK-side
// epoch fence guards — an old-epoch afplay that completes right after a Switch.
// fakePlayer cannot model it because it returns ctx.Err() on cancel, so both the
// fence branch and the cancel branch behave identically and the fence looks
// tested when it is not.
type gatedPlayer struct {
	mu      sync.Mutex
	order   []byte
	release chan struct{}
}

func (g *gatedPlayer) Play(_ context.Context, audio []byte, _ string) error {
	<-g.release // intentionally not selecting on ctx: this playback "already finished"
	g.mu.Lock()
	if len(audio) > 0 {
		g.order = append(g.order, audio[0])
	}
	g.mu.Unlock()
	return nil
}

func (g *gatedPlayer) snapshot() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]byte, len(g.order))
	copy(out, g.order)
	return out
}

// --- Event collector --------------------------------------------------------

// collector drains the queue's (droppable) Events channel into a slice for
// inspection and provides pulse-driven waits so tests never busy-sleep. It is
// used only for synchronization and event assertions; fakePlayer.order remains
// the source of truth for playback ordering.
type collector struct {
	mu     sync.Mutex
	events []Event
	notify chan struct{}
}

func newCollector(ch <-chan Event, stop <-chan struct{}) *collector {
	c := &collector{notify: make(chan struct{}, 1)}
	go func() {
		for {
			select {
			case e := <-ch:
				c.mu.Lock()
				c.events = append(c.events, e)
				c.mu.Unlock()
				select {
				case c.notify <- struct{}{}:
				default:
				}
			case <-stop:
				return
			}
		}
	}()
	return c
}

func (c *collector) count(k EventKind) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func (c *collector) has(k EventKind, seq uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.Kind == k && e.Seq == seq {
			return true
		}
	}
	return false
}

// waitFor blocks until at least n events of kind k have been observed, or fails
// the test on timeout. Pulse-driven: no polling sleeps.
func (c *collector) waitFor(t *testing.T, k EventKind, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if c.count(k) >= n {
			return
		}
		select {
		case <-c.notify:
		case <-deadline:
			t.Fatalf("timed out waiting for %d events of kind %d (have %d)", n, k, c.count(k))
		}
	}
}

// --- Harness ----------------------------------------------------------------

type harness struct {
	q    *Queue
	fake *fakePlayer
	col  *collector
}

func newHarness(t *testing.T, fake *fakePlayer) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	q := NewQueue(fake, 64)
	go q.Run(ctx)
	stop := make(chan struct{})
	col := newCollector(q.Events(), stop)
	t.Cleanup(func() {
		cancel()
		close(stop)
	})
	return &harness{q: q, fake: fake, col: col}
}

func quiesce(t *testing.T, q *Queue, d time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return q.Quiesce(ctx)
}

// deliver a success result with the seq encoded in Audio[0].
func deliverAudio(q *Queue, epoch, seq uint64) {
	q.Deliver(SynthResult{ID: ItemID{Epoch: epoch, Seq: seq}, Audio: []byte{byte(seq)}, Format: "wav"})
}

func deliverByte(q *Queue, epoch, seq uint64, b byte) {
	q.Deliver(SynthResult{ID: ItemID{Epoch: epoch, Seq: seq}, Audio: []byte{b}, Format: "wav"})
}

func waitClosed(t *testing.T, ch <-chan struct{}, d time.Duration, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// --- Tests ------------------------------------------------------------------

// (1) Out-of-order Deliver still plays in strict seq order within an epoch.
func TestInOrderPlaybackDespiteOutOfOrderDeliver(t *testing.T) {
	h := newHarness(t, &fakePlayer{})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 2})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 3})

	// Delivered 3, 1, 2 — order must not matter.
	deliverAudio(h.q, 0, 3)
	deliverAudio(h.q, 0, 1)
	deliverAudio(h.q, 0, 2)

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("play order = %v, want [1 2 3]", got)
	}
}

// (5) A synth error mid-stream is skipped; surrounding sentences still play in
// order and an EventError for the skipped seq is observed. No deadlock.
func TestSkipFailedSynthMidStream(t *testing.T) {
	h := newHarness(t, &fakePlayer{})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 2})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 3})

	deliverAudio(h.q, 0, 1)
	h.q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 2}, Err: errors.New("synth failed")})
	deliverAudio(h.q, 0, 3)

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1, 3}) {
		t.Fatalf("play order = %v, want [1 3]", got)
	}
	h.col.waitFor(t, EventError, 1, time.Second)
	if !h.col.has(EventError, 2) {
		t.Fatalf("expected EventError for seq 2, events=%v", h.col.events)
	}
}

// (5) A player.Play error (not a ctx cancel) is surfaced as EventError, the seq
// is dropped, and the drain advances — a failing player never deadlocks.
func TestPlayerErrorSkipsAndAdvances(t *testing.T) {
	h := newHarness(t, &fakePlayer{fail: errors.New("afplay boom")})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 2})
	deliverAudio(h.q, 0, 1)
	deliverAudio(h.q, 0, 2)

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	if got := h.fake.snapshot(); len(got) != 0 {
		t.Fatalf("failing player recorded playback %v, want none", got)
	}
	h.col.waitFor(t, EventError, 2, time.Second)
	if !h.col.has(EventError, 1) || !h.col.has(EventError, 2) {
		t.Fatalf("expected EventError for seq 1 and 2, events=%v", h.col.events)
	}
}

// (2) Epoch fence on Deliver: a stale-epoch result is dropped and never fills
// the slot; the current-epoch result plays. Extends the smoke test with real
// playback (audit #1 no-bleed at the delivery boundary).
func TestEpochFenceOnDeliver(t *testing.T) {
	h := newHarness(t, &fakePlayer{})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})

	// Stale-epoch result would otherwise fill seq 1 forever.
	deliverByte(h.q, 99, 1, 9)
	// Correct-epoch result fills and plays.
	deliverAudio(h.q, 0, 1)

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1}) {
		t.Fatalf("play order = %v, want [1] (stale byte 9 must not play)", got)
	}
}

// (audit #1 / #2) Switch bumps the epoch mid-playback: the in-flight old-epoch
// playback is cancelled, and a stale old-epoch Deliver COLLIDING with the reused
// seq 1 is dropped by the Deliver fence rather than filling the new session's
// slot; nextSeq resets and only the new-epoch item plays.
//
// The stale result uses the SAME seq the new session reuses (seq 1). The old
// version used seq 5, which sits above the post-switch nextSeq=1 frontier and can
// never play regardless of the fence — so its no-bleed assertion was inert
// (disabling the Deliver fence still passed). This version fails if the fence is
// removed: the stale byte 99 would fill the reused slot and play instead of 2.
func TestSwitchBumpsEpochNoBleedNoDeadlock(t *testing.T) {
	fake := &fakePlayer{release: make(chan struct{})}
	h := newHarness(t, fake)

	// Old epoch: reserve, deliver, playback starts and blocks on release.
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	deliverByte(h.q, 0, 1, 1)
	h.col.waitFor(t, EventPlaying, 1, 3*time.Second)

	// Session switch while a playback is in flight (races clear vs in-flight
	// play — audit #2 must not hang/deadlock). Cancels the old play.
	h.q.Switch(1)

	// New epoch reuses seq 1. Reserve it FIRST so a slot exists, then a stale
	// old-epoch result for that SAME seq must be dropped by the Deliver fence
	// (without the fence it would fill the new slot with byte 99 and play it).
	h.q.Reserve(ItemID{Epoch: 1, Seq: 1})
	deliverByte(h.q, 0, 1, 99) // stale epoch 0 — must be fenced out
	deliverByte(h.q, 1, 1, 2)  // real epoch 1 — must be the one that plays
	h.col.waitFor(t, EventPlaying, 2, 3*time.Second)

	// The old playback goroutine was cancelled by Switch and, being past its
	// select, will not consume this release; only the new playback receives it.
	fake.release <- struct{}{}

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle after switch: %v", err)
	}
	if got := fake.snapshot(); !reflect.DeepEqual(got, []byte{2}) {
		t.Fatalf("play order = %v, want [2] (old byte 1 and stale byte 99 must not play)", got)
	}
}

// (audit #1, playback side) The dangerous race the PLAYBACK-side epoch fence
// exists for: an old-epoch playback that COMPLETES SUCCESSFULLY (err == nil) just
// after a Switch. The fence (queue.go: `case pr.epoch != q.epoch`) must drop that
// result; without it the default branch deletes the NEW epoch's seq-1 slot and
// advances nextSeq past it, so the new sentence would never play.
//
// This is the mutation the review flagged as previously undetectable: replacing
// the fence with `case false` left every other test green. Here, disabling it
// makes the new sentence never start, so waitFor(EventPlaying, 2) times out.
func TestPlaybackEpochFenceDropsOldSuccessAfterSwitch(t *testing.T) {
	g := &gatedPlayer{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	q := NewQueue(g, 64)
	go q.Run(ctx)
	stop := make(chan struct{})
	col := newCollector(q.Events(), stop)
	t.Cleanup(func() { cancel(); close(stop) })

	// Old epoch 0: reserve, deliver, playback starts and blocks on the gate.
	q.Reserve(ItemID{Epoch: 0, Seq: 1})
	deliverByte(q, 0, 1, 111)
	col.waitFor(t, EventPlaying, 1, 3*time.Second)

	// Switch to epoch 1 while the old playback is in flight. This cancels the old
	// play's ctx, but gatedPlayer models an afplay that already succeeded: it
	// returns nil (not Canceled) once released.
	q.Switch(1)

	// New epoch reuses seq 1: reserved + ready, but cannot start until the old
	// playback's result is processed (one playback at a time).
	q.Reserve(ItemID{Epoch: 1, Seq: 1})
	deliverByte(q, 1, 1, 222)

	// Release the OLD playback: it completes SUCCESSFULLY with epoch 0 AFTER the
	// switch. The old audio "played" (records 111), but the playback-side fence
	// must NOT let that success delete/advance the new epoch's slot.
	g.release <- struct{}{}

	col.waitFor(t, EventPlaying, 2, 3*time.Second) // the new sentence must start
	g.release <- struct{}{}                        // let it finish

	qctx, qcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer qcancel()
	if err := q.Quiesce(qctx); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	// [111 222]: the old playback completed, then the NEW sentence still played.
	// Without the fence the new slot is deleted on the old success => [111] only.
	if got := g.snapshot(); !reflect.DeepEqual(got, []byte{111, 222}) {
		t.Fatalf("play order = %v, want [111 222]: the fence must let the new sentence play after an old-epoch success", got)
	}
}

// (audit #5) Pause cancels the in-flight playback but keeps the sentence ready;
// Resume replays that same sentence from the start. The sentence is never
// stranded, and it is recorded exactly once (the cancelled attempt records
// nothing). Two EventPlaying are emitted for the one sentence.
func TestPauseKeepsSentenceResumeReplays(t *testing.T) {
	fake := &fakePlayer{release: make(chan struct{})}
	h := newHarness(t, fake)

	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	deliverByte(h.q, 0, 1, 7)
	h.col.waitFor(t, EventPlaying, 1, 3*time.Second) // first attempt started

	h.q.Pause()  // cancels the in-flight attempt; slot stays ready
	h.q.Resume() // replays the same seq from the start

	h.col.waitFor(t, EventPlaying, 2, 3*time.Second) // replay started
	fake.release <- struct{}{}                       // let the replay finish

	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce did not reach idle: %v", err)
	}
	if got := fake.snapshot(); !reflect.DeepEqual(got, []byte{7}) {
		t.Fatalf("play order = %v, want [7] (recorded exactly once)", got)
	}
	if n := h.col.count(EventPlaying); n != 2 {
		t.Fatalf("EventPlaying count = %d, want 2 (one attempt + one replay)", n)
	}
}

// (6) Quiesce returns nil once idle, and returns ctx.Err() when a reserved slot
// never delivers before the timeout — without deadlocking the drain. A later
// delivery of the missing slot still plays.
func TestQuiesceIdleAndTimeout(t *testing.T) {
	h := newHarness(t, &fakePlayer{})

	// Already idle: returns nil promptly.
	if err := quiesce(t, h.q, time.Second); err != nil {
		t.Fatalf("quiesce on idle queue = %v, want nil", err)
	}

	// Reserved but never delivered: quiesce must time out, not hang forever.
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	if err := quiesce(t, h.q, 150*time.Millisecond); err == nil {
		t.Fatal("quiesce with an undelivered reservation returned nil, want ctx error")
	}

	// The drain is not wedged: delivering the missing slot lets it play.
	deliverAudio(h.q, 0, 1)
	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce after late delivery = %v, want nil", err)
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1}) {
		t.Fatalf("play order = %v, want [1]", got)
	}
}

// (5) A never-arriving middle sentence does not strand earlier playback; the
// gap blocks only later sentences until it is filled.
func TestMissingMiddleDoesNotStrandEarlier(t *testing.T) {
	h := newHarness(t, &fakePlayer{})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 2})
	h.q.Reserve(ItemID{Epoch: 0, Seq: 3})

	deliverAudio(h.q, 0, 1) // plays immediately
	deliverAudio(h.q, 0, 3) // waits behind the seq-2 gap

	// seq 2 missing: quiesce times out but seq 1 already played.
	if err := quiesce(t, h.q, 200*time.Millisecond); err == nil {
		t.Fatal("quiesce returned nil despite a missing sentence, want ctx error")
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1}) {
		t.Fatalf("play order before gap fill = %v, want [1]", got)
	}

	// Fill the gap: seq 2 then seq 3 drain in order.
	deliverAudio(h.q, 0, 2)
	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("quiesce after gap fill = %v, want nil", err)
	}
	if got := h.fake.snapshot(); !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("play order = %v, want [1 2 3]", got)
	}
}

// EventDrained is emitted exactly once per busy->idle transition.
func TestEventDrainedOncePerTransition(t *testing.T) {
	h := newHarness(t, &fakePlayer{})

	// Transition 1: reserve (busy) then skip via Err (idle).
	h.q.Reserve(ItemID{Epoch: 0, Seq: 1})
	h.q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 1}, Err: errors.New("skip")})
	h.col.waitFor(t, EventDrained, 1, 3*time.Second)
	if n := h.col.count(EventDrained); n != 1 {
		t.Fatalf("EventDrained after first transition = %d, want 1", n)
	}

	// Transition 2 (nextSeq advanced to 2 after the skip).
	h.q.Reserve(ItemID{Epoch: 0, Seq: 2})
	h.q.Deliver(SynthResult{ID: ItemID{Epoch: 0, Seq: 2}, Err: errors.New("skip")})
	h.col.waitFor(t, EventDrained, 2, 3*time.Second)
	if n := h.col.count(EventDrained); n != 2 {
		t.Fatalf("EventDrained after second transition = %d, want 2", n)
	}
}

// (6) Quiesce drains the final sentence within bound before returning, then the
// queue stops cleanly (Run goroutine exits, no leak).
func TestQuiesceDrainsFinalSentenceThenStops(t *testing.T) {
	fake := &fakePlayer{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	q := NewQueue(fake, 64)
	go q.Run(ctx)
	stop := make(chan struct{})
	col := newCollector(q.Events(), stop)
	defer close(stop)

	q.Reserve(ItemID{Epoch: 0, Seq: 1})
	deliverByte(q, 0, 1, 1)
	col.waitFor(t, EventPlaying, 1, 3*time.Second)

	// Quiesce must block until the in-flight final sentence completes.
	qdone := make(chan error, 1)
	go func() {
		qctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		defer c()
		qdone <- q.Quiesce(qctx)
	}()

	select {
	case err := <-qdone:
		t.Fatalf("Quiesce returned before the final sentence drained: %v", err)
	case <-time.After(100 * time.Millisecond):
		// still blocked, as expected
	}

	fake.release <- struct{}{} // let the final sentence finish
	if err := <-qdone; err != nil {
		t.Fatalf("Quiesce = %v, want nil after final drain", err)
	}
	if got := fake.snapshot(); !reflect.DeepEqual(got, []byte{1}) {
		t.Fatalf("final sentence not fully played: order = %v, want [1]", got)
	}

	// Clean shutdown: Run exits and closes q.done, no goroutine leak.
	cancel()
	waitClosed(t, q.done, 3*time.Second, "Run goroutine did not exit after ctx cancel")
}

// Every public mutator is total after Run exits — no send blocks once the queue
// has stopped (kept as a race-run assertion; audit safety net).
func TestSendersTotalAfterShutdown(t *testing.T) {
	q := NewQueue(&fakePlayer{}, 64)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	cancel()
	waitClosed(t, q.done, 3*time.Second, "Run goroutine did not exit")

	done := make(chan struct{})
	go func() {
		q.Reserve(ItemID{Epoch: 1, Seq: 5})
		deliverAudio(q, 1, 5)
		q.Pause()
		q.Resume()
		q.Switch(2)
		if err := q.Quiesce(context.Background()); err != nil {
			t.Errorf("Quiesce after shutdown = %v, want nil", err)
		}
		close(done)
	}()
	waitClosed(t, done, 3*time.Second, "a mutator blocked after Run exited")
}

// (audit #2) Concurrent Switch/Pause/Resume racing reserves and an occasionally
// blocking player must never deadlock; after a final switch the queue still
// drains a fresh item. Exercised under -race for data races.
func TestConcurrentSwitchPauseResumeNoDeadlock(t *testing.T) {
	fake := &fakePlayer{}
	h := newHarness(t, fake)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for e := uint64(1); e <= 20; e++ {
			h.q.Switch(e)
			h.q.Reserve(ItemID{Epoch: e, Seq: 1})
			deliverByte(h.q, e, 1, byte(e))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			h.q.Pause()
			h.q.Resume()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 40; i++ {
			h.q.Reserve(ItemID{Epoch: 7, Seq: uint64(i)})
		}
	}()
	wg.Wait()

	// Settle to a known epoch and prove the queue still drains.
	h.q.Resume()
	h.q.Switch(1000)
	h.q.Reserve(ItemID{Epoch: 1000, Seq: 1})
	deliverByte(h.q, 1000, 1, 42)
	if err := quiesce(t, h.q, 3*time.Second); err != nil {
		t.Fatalf("queue wedged after concurrent switch/pause storm: %v", err)
	}
}
