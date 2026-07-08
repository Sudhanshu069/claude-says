// Package daemon is the orchestrator. It owns a single select loop that wires
// (watcher|ipc) -> textproc -> synth -> queue -> player, and is the ONLY place
// ItemIDs are stamped, so epoch bumps (session switch) and seq assignment can
// never interleave. Every session/reset gets a new epoch (generation counter);
// the queue drops any synth result whose epoch != current, which is the
// structural fix for Node's cross-session audio bleed. Per-sentence failures
// are isolated and every network call is deadline-bounded.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Sudhanshu069/claude-code-speak/internal/audio"
	"github.com/Sudhanshu069/claude-code-speak/internal/config"
	"github.com/Sudhanshu069/claude-code-speak/internal/ipc"
	"github.com/Sudhanshu069/claude-code-speak/internal/narrator"
	"github.com/Sudhanshu069/claude-code-speak/internal/session"
	"github.com/Sudhanshu069/claude-code-speak/internal/textproc"
	"github.com/Sudhanshu069/claude-code-speak/internal/transcript"
	"github.com/Sudhanshu069/claude-code-speak/internal/tts"
	"github.com/Sudhanshu069/claude-code-speak/internal/tui"
)

// Default timing knobs.
const (
	DefaultSynthTimeout = 15 * time.Second
	DefaultFlushDelay   = 1500 * time.Millisecond
)

// quitDrainTimeout bounds the graceful drain triggered by a TUI quit control
// (mirrors Node stop()'s 10s timeoutMs). Explicit Stop(timeout) callers pass
// their own bound.
const quitDrainTimeout = 10 * time.Second

// maxIPCText caps text accepted from a hook/IPC message so a rogue local client
// can't push a huge payload into the pipeline (and on to paid cloud TTS/LLM
// APIs). Mirrors Node's MAX_IPC_TEXT.
const maxIPCText = 100 * 1024

// eventBuf sizes the TUI events channel. It must be buffered so UI slowness can
// never stall playback; emits are non-blocking and drop when full.
const eventBuf = 256

// Options configures a Daemon. Zero-valued durations fall back to the defaults.
type Options struct {
	Provider       string
	SessionID      string
	TranscriptPath string
	NarratorOn     bool
	SynthTimeout   time.Duration // default DefaultSynthTimeout; deadline on every synth
	FlushDelay     time.Duration // default DefaultFlushDelay
}

// stopReq asks Run to run the graceful drain and signals completion by closing
// done. Sent by Stop over the unbuffered stopCh so drain runs in Run's goroutine
// (the single owner of epoch/seq stamping and the flush).
type stopReq struct {
	timeout time.Duration
	done    chan struct{}
}

// Daemon wires the whole pipeline together. All ItemID stamping happens in Run.
type Daemon struct {
	cfg          config.Config
	processor    *textproc.Processor
	queue        *audio.Queue
	player       *audio.AfplayPlayer
	provider     tts.Provider
	narrator     narrator.Narrator
	epoch        uint64 // stamped only by the main loop
	synthTimeout time.Duration
	flushDelay   time.Duration
	synthWG      sync.WaitGroup
	stopping     bool

	// Channels wired inside Run. events carries daemon -> TUI notifications;
	// ctrl carries TUI -> daemon controls (also fed by the exported control
	// methods). stopCh/runDone coordinate a single graceful shutdown.
	events   chan tui.Event
	ctrl     chan tui.Control
	stopCh   chan stopReq
	runDone  chan struct{}
	stopOnce sync.Once

	// Loop-owned state (touched only from Run's goroutine).
	paused         bool
	watcher        *transcript.Watcher
	watcherCancel  context.CancelFunc
	watcherEvents  <-chan transcript.Event
	sessionID      string
	transcriptPath string

	// Initial text-source selection (from Options).
	initialSessionID      string
	initialTranscriptPath string
}

// New builds a Daemon from cfg and opts, constructing the provider, optional
// narrator, player, queue, and processor.
func New(cfg config.Config, opts Options) (*Daemon, error) {
	if opts.Provider != "" {
		cfg.Provider = opts.Provider
	}
	narratorOn := cfg.Narrator.Enabled || opts.NarratorOn

	synthTimeout := opts.SynthTimeout
	if synthTimeout == 0 {
		synthTimeout = DefaultSynthTimeout
	}
	flushDelay := opts.FlushDelay
	if flushDelay == 0 {
		if cfg.TextProcessor.FlushDelay > 0 {
			flushDelay = time.Duration(cfg.TextProcessor.FlushDelay) * time.Millisecond
		} else {
			flushDelay = DefaultFlushDelay
		}
	}

	provider, err := tts.New(&cfg)
	if err != nil {
		return nil, err
	}

	var narr narrator.Narrator
	if narratorOn {
		narr, err = narrator.New(&cfg)
		if err != nil {
			return nil, err
		}
	}

	player, err := audio.NewPlayer()
	if err != nil {
		return nil, err
	}

	queue := audio.NewQueue(player, eventBuf)

	proc := textproc.New(textproc.Options{
		MinChunkLen: cfg.TextProcessor.MinChunkLength,
		MaxChunkLen: cfg.TextProcessor.MaxChunkLength,
	})

	return &Daemon{
		cfg:                   cfg,
		processor:             proc,
		queue:                 queue,
		player:                player,
		provider:              provider,
		narrator:              narr,
		synthTimeout:          synthTimeout,
		flushDelay:            flushDelay,
		initialSessionID:      opts.SessionID,
		initialTranscriptPath: opts.TranscriptPath,
		events:                make(chan tui.Event, eventBuf),
		ctrl:                  make(chan tui.Control, 16),
		stopCh:                make(chan stopReq),
		runDone:               make(chan struct{}),
	}, nil
}

// Events is the read side the TUI consumes (buffered; closed when Run returns).
func (d *Daemon) Events() <-chan tui.Event { return d.events }

// Controls is the send side the TUI writes controls to. The exported
// Pause/Resume/SwitchSession methods feed the same channel.
func (d *Daemon) Controls() chan<- tui.Control { return d.ctrl }

// Run owns the single select loop that wires watcher|ipc -> processor -> synth
// -> queue -> player. It is the only place ItemIDs are stamped, so epoch bumps
// and seq assignment never interleave. It returns when ctx is done or after a
// graceful drain (Stop / TUI quit).
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer close(d.runDone)
	defer close(d.events)

	// Queue drain goroutine (sole caller of player.Play).
	go d.queue.Run(ctx)

	// IPC/hook fallback server. Feeding from it is gated on "no active watcher".
	var ipcMessages <-chan ipc.Message
	if sockPath, err := config.SocketPath(); err != nil {
		slog.Warn("ipc disabled: socket path unavailable", "err", err)
	} else if srv, err := ipc.NewServer(sockPath); err != nil {
		slog.Warn("ipc disabled: could not bind socket", "err", err)
	} else {
		go func() { _ = srv.Serve(ctx) }()
		ipcMessages = srv.Messages()
	}

	// Choose the initial text source (mirrors Node start()).
	d.selectInitialSource(ctx)

	// Flush timer: armed while the processor holds pending buffered text, so a
	// trailing sentence with no terminal punctuation is still spoken. The timer
	// lives here (not in textproc) so seq stays race-free.
	var flushTimer *time.Timer
	var flushC <-chan time.Time
	armFlush := func() {
		if flushTimer == nil {
			flushTimer = time.NewTimer(d.flushDelay)
		} else {
			if !flushTimer.Stop() {
				select {
				case <-flushTimer.C:
				default:
				}
			}
			flushTimer.Reset(d.flushDelay)
		}
		flushC = flushTimer.C
	}
	disarmFlush := func() {
		if flushTimer != nil && !flushTimer.Stop() {
			select {
			case <-flushTimer.C:
			default:
			}
		}
		flushC = nil
	}

	feed := func(text string) {
		if d.stopping || text == "" {
			return
		}
		for _, s := range d.processor.Feed(text) {
			d.enqueueSentence(ctx, s)
		}
		if d.processor.HasPending() {
			armFlush()
		} else {
			disarmFlush()
		}
	}

	doSwitch := func(id string) {
		if d.stopping {
			return
		}
		// Bump the epoch BEFORE clearing so any old-epoch synth result still in
		// flight is dropped by the queue on arrival (fixes cross-session bleed).
		d.epoch++
		d.queue.Switch(d.epoch)
		d.processor.Reset()
		disarmFlush()
		d.stopWatcher()

		if id != "" {
			if path, ok, err := session.FindTranscript(id); err != nil {
				slog.Error("session lookup failed", "id", id, "err", err)
				d.sessionID = id
				d.emitStatus("Session lookup failed; listening via hooks")
			} else if ok {
				d.startWatcher(ctx, path, id)
			} else {
				d.sessionID = id
				d.emitStatus(fmt.Sprintf("No transcript for %s; listening via hooks", shortID(id)))
			}
		} else {
			// No id: tear the watcher down entirely so the hook/IPC fallback
			// (gated on watcher==nil) actually re-enables.
			d.sessionID = ""
			d.transcriptPath = ""
			d.emitStatus("Listening to all sessions (hooks only)")
		}
		d.emit(tui.Event{Kind: tui.EventSessionSwitched, Epoch: d.epoch, Session: d.sessionID, Time: time.Now()})
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case req := <-d.stopCh:
			d.drain(ctx, req.timeout)
			close(req.done)
			return nil

		case c := <-d.ctrl:
			switch c.Kind {
			case tui.ControlPause:
				d.queue.Pause()
				d.paused = true
				d.emitStatus("Paused")
			case tui.ControlResume:
				d.queue.Resume()
				d.paused = false
				d.emitStatus("Resumed")
			case tui.ControlSwitch:
				doSwitch(c.SessionID)
			case tui.ControlQuit:
				d.drain(ctx, quitDrainTimeout)
				return nil
			}

		case ev, ok := <-d.watcherEvents:
			if !ok {
				// The active watcher stopped (only happens on our cancel);
				// fall back to hook/IPC until the next switch.
				d.watcherEvents = nil
				d.watcher = nil
				continue
			}
			feed(ev.Text)

		case msg := <-ipcMessages:
			if d.stopping || d.watcher != nil || msg.Type != "text" {
				continue
			}
			feed(clip(msg.Text))

		case qe := <-d.queue.Events():
			d.forwardQueueEvent(qe)

		case <-flushC:
			flushC = nil
			for _, s := range d.processor.Flush() {
				d.enqueueSentence(ctx, s)
			}
		}
	}
}

// selectInitialSource picks the initial text source: an explicit transcript
// path, an explicit session id (falling back to hooks if unknown), else the
// most recently active session (else hooks). Mirrors Node start().
func (d *Daemon) selectInitialSource(ctx context.Context) {
	switch {
	case d.initialTranscriptPath != "":
		d.startWatcher(ctx, d.initialTranscriptPath, d.initialSessionID)
	case d.initialSessionID != "":
		if path, ok, err := session.FindTranscript(d.initialSessionID); err == nil && ok {
			d.startWatcher(ctx, path, d.initialSessionID)
		} else {
			d.sessionID = d.initialSessionID
			d.emitStatus(fmt.Sprintf("No transcript for %s; listening via hooks", shortID(d.initialSessionID)))
		}
	default:
		if s, ok, err := session.MostRecent(); err == nil && ok {
			slog.Info("auto-detected session", "id", shortID(s.ID), "project", s.ProjectName)
			d.startWatcher(ctx, s.TranscriptPath, s.ID)
		} else {
			d.emitStatus("No sessions found; listening via hooks")
		}
	}
}

// enqueueSentence stamps the ItemID (the ONLY place, alongside epoch bumps),
// reserves the slot in seq order, notifies the TUI, and launches the per-item
// synth goroutine. Called only from Run's goroutine.
func (d *Daemon) enqueueSentence(ctx context.Context, s textproc.Sentence) {
	id := audio.ItemID{Epoch: d.epoch, Seq: s.Seq}
	d.queue.Reserve(id)
	d.emit(tui.Event{
		Kind:    tui.EventText,
		Epoch:   d.epoch,
		Seq:     int(s.Seq),
		Text:    s.Text,
		Session: d.sessionID,
		Time:    time.Now(),
	})
	d.synthWG.Add(1)
	go d.synth(ctx, id, s.Text)
}

// startWatcher tears down any existing watcher and starts a new one for path
// under a child context so the next switch/shutdown can cancel it. Called only
// from Run's goroutine.
func (d *Daemon) startWatcher(parent context.Context, path, id string) {
	d.stopWatcher()
	wctx, cancel := context.WithCancel(parent)
	w := transcript.New(path)
	d.watcher = w
	d.watcherCancel = cancel
	d.watcherEvents = w.Events()
	d.sessionID = id
	d.transcriptPath = path
	go func() { _ = w.Run(wctx) }()
	slog.Info("watching transcript", "path", path)
	d.emit(tui.Event{Kind: tui.EventWatching, Epoch: d.epoch, Session: id, Time: time.Now()})
}

// stopWatcher cancels the active watcher (if any) and clears the fallback gate.
// Called only from Run's goroutine.
func (d *Daemon) stopWatcher() {
	if d.watcherCancel != nil {
		d.watcherCancel()
		d.watcherCancel = nil
	}
	d.watcher = nil
	d.watcherEvents = nil
}

// forwardQueueEvent maps a queue lifecycle event to a TUI event. Called only
// from Run's goroutine.
func (d *Daemon) forwardQueueEvent(qe audio.Event) {
	switch qe.Kind {
	case audio.EventPlaying:
		d.emit(tui.Event{Kind: tui.EventPlaying, Epoch: d.epoch, Seq: int(qe.Seq), Queue: qe.QueueSize, Time: time.Now()})
	case audio.EventDrained:
		d.emit(tui.Event{Kind: tui.EventDrained, Epoch: d.epoch, Queue: qe.QueueSize, Time: time.Now()})
	case audio.EventError:
		// A hard failure (afplay error, or a synth error surfaced by the queue).
		// Log it (headless has no TUI) in ADDITION to the UI event. Mirrors
		// Node's 'Audio error #seq' via logger.error.
		slog.Error("audio error", "seq", qe.Seq, "err", qe.Err)
		d.emit(tui.Event{Kind: tui.EventError, Epoch: d.epoch, Seq: int(qe.Seq), Err: qe.Err, Time: time.Now()})
	}
}

// SwitchSession bumps the epoch, resets the processor, and re-points the watcher
// (or tears it down to re-enable the hook/IPC fallback). Safe from any
// goroutine; the work runs in Run's loop.
func (d *Daemon) SwitchSession(id string) {
	d.sendCtrl(tui.Control{Kind: tui.ControlSwitch, SessionID: id})
}

// Pause pauses playback.
func (d *Daemon) Pause() { d.sendCtrl(tui.Control{Kind: tui.ControlPause}) }

// Resume resumes playback.
func (d *Daemon) Resume() { d.sendCtrl(tui.Control{Kind: tui.ControlResume}) }

// sendCtrl hands a control to Run, abandoning it if Run has already returned so
// a late caller can never block forever.
func (d *Daemon) sendCtrl(c tui.Control) {
	select {
	case d.ctrl <- c:
	case <-d.runDone:
	}
}

// Stop flushes the final sentence, Quiesces the queue (bounded by timeout,
// skipped when paused), then cancels. It is idempotent: a second call (or one
// racing the TUI quit) returns once the single drain has completed.
func (d *Daemon) Stop(timeout time.Duration) {
	d.stopOnce.Do(func() {
		req := stopReq{timeout: timeout, done: make(chan struct{})}
		select {
		case d.stopCh <- req:
			<-req.done
		case <-d.runDone:
			// Run already exited (e.g. TUI quit or parent ctx cancel); nothing
			// to drain.
		}
	})
}

// drain performs the graceful shutdown: stop accepting new text, flush the
// final buffered sentence into the pipeline, and wait (bounded by timeout) for
// the queue to finish playing. A paused queue never advances, so draining it
// would just spin to the deadline — a paused quit exits immediately. Called
// only from Run's goroutine, so all final reserves precede the Quiesce wait.
func (d *Daemon) drain(ctx context.Context, timeout time.Duration) {
	d.stopping = true
	d.stopWatcher()

	for _, s := range d.processor.Flush() {
		d.enqueueSentence(ctx, s)
	}

	if !d.paused {
		qctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		if err := d.queue.Quiesce(qctx); err != nil {
			slog.Warn("drain timed out", "err", err)
		}
	}
	slog.Info("stopped")
}

// synth runs per-sentence in its own goroutine and GUARANTEES exactly one
// queue.Deliver — even on provider error, context deadline, or recovered panic
// — so the ordered drain never waits forever on a reserved slot. It selects on
// ctx.Done() when delivering so a late synth goroutine can't leak after Run
// returns (once ctx is cancelled the queue is tearing down and nobody is
// waiting on the slot).
func (d *Daemon) synth(ctx context.Context, id audio.ItemID, text string) {
	defer d.synthWG.Done()

	result := audio.SynthResult{ID: id}
	func() {
		defer func() {
			if r := recover(); r != nil {
				// A provider panic degrades exactly this one sentence (#11).
				result.Audio = nil
				result.Format = ""
				result.Err = fmt.Errorf("synth panic: %v", r)
			}
		}()

		final := text
		if d.narrator != nil {
			// Narrate is TOTAL: it returns the input verbatim on any failure,
			// so a narrator outage never drops a sentence. When the narrator can
			// report a degradation, log it (warn) — the sentence is still spoken
			// with the original text. Mirrors Node's narrator-fallback warn.
			if dg, ok := d.narrator.(narrator.Degrader); ok {
				var nerr error
				final, nerr = dg.NarrateOrErr(ctx, text)
				if nerr != nil {
					slog.Warn("narrator degraded; speaking original text", "err", nerr)
				}
			} else {
				final = d.narrator.Narrate(ctx, text)
			}
		}

		sctx, cancel := context.WithTimeout(ctx, d.synthTimeout)
		defer cancel()
		audioBytes, format, err := d.provider.Synthesize(sctx, final)
		if err != nil {
			// Hard synth failure for this one sentence; log it (headless has no
			// TUI) then deliver an Err result so the ordered drain skips the slot.
			slog.Error("synth failed", "epoch", id.Epoch, "seq", id.Seq, "err", err)
			result.Err = err
			return
		}
		result.Audio = audioBytes
		result.Format = format
	}()

	select {
	case <-ctx.Done():
		// Run has returned and the queue is shutting down; delivering now would
		// block on a dead consumer. Dropping is safe — the slot has no waiter.
	default:
		d.queue.Deliver(result)
	}
}

// emit sends a TUI event without ever blocking playback (drops when the buffer
// is full). Called only from Run's goroutine, before d.events is closed.
func (d *Daemon) emit(ev tui.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	select {
	case d.events <- ev:
	default:
	}
}

// emitStatus is a convenience for a human-readable status line.
func (d *Daemon) emitStatus(text string) {
	slog.Info(text)
	d.emit(tui.Event{Kind: tui.EventStatus, Epoch: d.epoch, Text: text, Session: d.sessionID, Time: time.Now()})
}

// clip bounds untrusted IPC text to maxIPCText bytes.
func clip(s string) string {
	if len(s) > maxIPCText {
		return s[:maxIPCText]
	}
	return s
}

// shortID renders a session id's leading 8 chars for logs/status, matching the
// Node slice(0, 8) convention.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
