package audio

import (
	"context"
	"errors"
)

// ItemID is the generation-fenced identity of a queued item. The Epoch is
// bumped on every session switch/reset so a stale in-flight TTS result can
// never fill a new session's slot (fixes Node cross-session audio bleed).
type ItemID struct {
	Epoch uint64
	Seq   uint64
}

// SynthResult is the terminal outcome of one synth (success, error, timeout, or
// recovered panic). Err != nil means "skip this seq".
type SynthResult struct {
	ID     ItemID
	Audio  []byte
	Format string
	Err    error
}

// EventKind classifies queue lifecycle events emitted to the TUI.
type EventKind int

const (
	EventQueued EventKind = iota
	EventPlaying
	EventPlayed
	EventDrained
	EventError
)

// Event is a queue lifecycle notification (non-blocking emit; dropped if full).
type Event struct {
	Kind      EventKind
	Seq       uint64
	QueueSize int
	Err       error
}

// slot is drain-goroutine-owned; ready==true is terminal (audio or err set).
type slot struct {
	id     ItemID
	audio  []byte
	format string
	err    error
	ready  bool
}

// cmdKind tags the union sent on the single cmds channel.
type cmdKind int

const (
	cmdReserve cmdKind = iota
	cmdResult
	cmdPause
	cmdResume
	cmdSwitch
	cmdQuiesce
)

// cmd is the tagged union consumed by Run's single drain goroutine.
type cmd struct {
	kind   cmdKind
	id     ItemID        // reserve
	result SynthResult   // result
	epoch  uint64        // switch
	done   chan struct{} // quiesce: closed when the queue goes idle
}

// playResult is the terminal outcome of one player.Play, carried back to Run
// from the single playback goroutine. epoch/seq identify which item played so a
// result that raced a session switch can be recognized and ignored.
type playResult struct {
	epoch uint64
	seq   uint64
	err   error
}

// Player renders one audio buffer to the speakers under a context. It is the
// single seam the Queue drives for playback: production wires in *AfplayPlayer
// (afplay), while tests inject a fake that records playback order without ever
// touching afplay/audio. Cancelling ctx must interrupt the render and return an
// error satisfying errors.Is(err, context.Canceled) so the queue treats it as
// an interruption (pause/switch), not a failure.
type Player interface {
	Play(ctx context.Context, audio []byte, format string) error
}

// Queue is the epoch-fenced, sequence-ordered audio queue. Every field below
// cmds/events is owned EXCLUSIVELY by Run's goroutine — no locks, no shared
// draining bool. Exactly one playback runs at a time.
//
// INVARIANT: Reserve and the cmdQuiesce marker must be sent from the SAME
// caller goroutine (the daemon main loop) on this SAME FIFO cmds channel, so
// all final reserves are enqueued before a quiesce marker. Moving Reserve to a
// different channel/goroutine breaks the final-sentence shutdown guarantee.
type Queue struct {
	player  Player
	cmds    chan cmd
	events  chan Event
	done    chan struct{} // closed by Run on exit; makes senders total
	epoch   uint64
	nextSeq uint64
	slots   map[uint64]*slot
	paused  bool
	waiters []chan struct{}
}

// NewQueue builds a Queue bound to player, with an events channel buffered to
// eventBuf. Run must be started to consume commands.
func NewQueue(player Player, eventBuf int) *Queue {
	return &Queue{
		player:  player,
		cmds:    make(chan cmd),
		events:  make(chan Event, eventBuf),
		done:    make(chan struct{}),
		nextSeq: 1,
		slots:   make(map[uint64]*slot),
	}
}

// Run is the single drain goroutine: sole owner of queue state and sole caller
// of player.Play (exactly one playback at a time). It returns when ctx is done.
func (q *Queue) Run(ctx context.Context) {
	// Playback state, owned solely by this goroutine. playDone is nil whenever
	// nothing is playing, so the select arm blocks (nil channel) until a
	// playback finishes.
	var (
		playing    bool
		playSeq    uint64
		playEpoch  uint64
		playCancel context.CancelFunc
		playDone   chan playResult
	)

	defer func() {
		if playCancel != nil {
			playCancel()
		}
		close(q.done)
		// Unblock any pending Quiesce callers; the queue is going away.
		for _, w := range q.waiters {
			close(w)
		}
		q.waiters = nil
	}()

	// announced tracks whether EventDrained has already been emitted for the
	// current idle period, so we announce a drain exactly once per busy->idle
	// transition. The queue starts idle with nothing to announce.
	announced := true

	emit := func(e Event) {
		select {
		case q.events <- e:
		default: // non-blocking: a slow TUI must never stall playback
		}
	}

	// startPlay launches the single playback goroutine for slot s at nextSeq.
	startPlay := func(s *slot) {
		playing = true
		playSeq = q.nextSeq
		playEpoch = q.epoch
		pctx, cancel := context.WithCancel(ctx)
		playCancel = cancel
		ch := make(chan playResult, 1) // buffered: goroutine never blocks/leaks
		playDone = ch
		emit(Event{Kind: EventPlaying, Seq: q.nextSeq, QueueSize: len(q.slots)})
		go func(ep, seq uint64, audio []byte, format string, pc context.Context) {
			ch <- playResult{epoch: ep, seq: seq, err: q.player.Play(pc, audio, format)}
		}(playEpoch, playSeq, s.audio, s.format, pctx)
	}

	// drain advances through ready slots in strict seq order, skipping failed
	// synths, until it hits a gap (missing/unready slot) or starts a playback.
	drain := func() {
		if q.paused || playing {
			return
		}
		for {
			s, ok := q.slots[q.nextSeq]
			if !ok || !s.ready {
				return // waiting for this seq's reserve/result
			}
			if s.err != nil {
				delete(q.slots, q.nextSeq)
				emit(Event{Kind: EventError, Seq: q.nextSeq, Err: s.err, QueueSize: len(q.slots)})
				q.nextSeq++
				continue
			}
			startPlay(s)
			return // exactly one playback at a time; wait for playDone
		}
	}

	// settle runs after every state change: closes quiesce waiters and announces
	// a drain when the queue is fully idle (no playback, no reserved/ready slots).
	settle := func() {
		if !playing && len(q.slots) == 0 {
			for _, w := range q.waiters {
				close(w)
			}
			q.waiters = nil
			if !announced {
				emit(Event{Kind: EventDrained})
				announced = true
			}
		} else {
			announced = false
		}
	}

	for {
		select {
		case <-ctx.Done():
			return

		case c := <-q.cmds:
			switch c.kind {
			case cmdReserve:
				// Reserves are stamped by the daemon with the current epoch; a
				// stale one (epoch mismatch) is ignored.
				if c.id.Epoch != q.epoch {
					break
				}
				if _, exists := q.slots[c.id.Seq]; !exists {
					q.slots[c.id.Seq] = &slot{id: c.id}
				}
				emit(Event{Kind: EventQueued, Seq: c.id.Seq, QueueSize: len(q.slots)})

			case cmdResult:
				r := c.result
				// Epoch fence: a result from a superseded session is dropped so it
				// can never fill the new session's slot (Node cross-session bleed).
				if r.ID.Epoch != q.epoch {
					break
				}
				s, ok := q.slots[r.ID.Seq]
				if !ok || s.ready {
					// Never reserved, already played/skipped, or a duplicate — drop.
					break
				}
				s.audio = r.Audio
				s.format = r.Format
				s.err = r.Err
				s.ready = true
				drain()

			case cmdPause:
				q.paused = true
				if playing {
					// Kill the in-flight afplay; playDone will report Canceled and
					// we keep the slot ready so Resume replays it from the start.
					playCancel()
				}

			case cmdResume:
				q.paused = false
				drain()

			case cmdSwitch:
				// New generation: drop every old-epoch slot and reset ordering.
				q.epoch = c.epoch
				q.slots = make(map[uint64]*slot)
				q.nextSeq = 1
				if playing {
					// Cancel the old-epoch playback; its playDone is recognized by
					// epoch and ignored on arrival.
					playCancel()
				}

			case cmdQuiesce:
				// If already idle, settle (below) closes this immediately;
				// otherwise it fires when the last reserved item finishes.
				q.waiters = append(q.waiters, c.done)
			}

		case pr := <-playDone:
			playing = false
			playDone = nil
			// Always release this playback's child-ctx registration before
			// dropping the cancel func. pause/switch may have already called it;
			// a second cancel is a safe no-op. Skipping it (as the old success
			// branch did) leaked one child-ctx per played sentence.
			if playCancel != nil {
				playCancel()
				playCancel = nil
			}

			switch {
			case pr.epoch != q.epoch:
				// Superseded by a session switch: the slot is already gone. Just
				// resume draining the current epoch.
				drain()

			case errors.Is(pr.err, context.Canceled), errors.Is(pr.err, context.DeadlineExceeded):
				// Interrupted by pause/switch (same epoch => pause). Keep the slot
				// ready and do not advance; Resume replays it from the start.
				if !q.paused {
					drain()
				}

			default:
				if pr.err != nil {
					emit(Event{Kind: EventError, Seq: pr.seq, Err: pr.err, QueueSize: len(q.slots)})
				}
				delete(q.slots, pr.seq)
				q.nextSeq = pr.seq + 1
				emit(Event{Kind: EventPlayed, Seq: pr.seq, QueueSize: len(q.slots)})
				drain()
			}
		}

		settle()
	}
}

// Events is the read side consumed by the TUI (non-blocking emit; drops if
// full).
func (q *Queue) Events() <-chan Event {
	return q.events
}

// send delivers a command to Run, or gives up if Run has already returned, so
// every public mutator is total (never blocks after shutdown).
func (q *Queue) send(c cmd) {
	select {
	case q.cmds <- c:
	case <-q.done:
	}
}

// Reserve announces an expected item, in seq order, before its synth starts.
func (q *Queue) Reserve(id ItemID) {
	q.send(cmd{kind: cmdReserve, id: id})
}

// Deliver hands a terminal synth result to the drain. A result whose Epoch !=
// the current epoch is dropped (fixes cross-session bleed).
func (q *Queue) Deliver(r SynthResult) {
	q.send(cmd{kind: cmdResult, result: r})
}

// Pause stops advancing and cancels any in-flight playback; the interrupted
// item stays ready so Resume replays it from the start.
func (q *Queue) Pause() {
	q.send(cmd{kind: cmdPause})
}

// Resume clears the paused flag and resumes draining.
func (q *Queue) Resume() {
	q.send(cmd{kind: cmdResume})
}

// Switch bumps the active epoch: clears slots, resets nextSeq to 1, and cancels
// any in-flight playback. Stale old-epoch results/playbacks are ignored on
// arrival.
func (q *Queue) Switch(epoch uint64) {
	q.send(cmd{kind: cmdSwitch, epoch: epoch})
}

// Quiesce blocks until every reserved item has played and the queue is idle, or
// ctx fires. Used by shutdown to play the final sentence without cutting it.
func (q *Queue) Quiesce(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case q.cmds <- cmd{kind: cmdQuiesce, done: done}:
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return nil // queue stopped: nothing left to wait for
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-q.done:
		return nil
	}
}
