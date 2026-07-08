package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sudhanshu069/claude-code-speak/internal/config"
	"github.com/Sudhanshu069/claude-code-speak/internal/session"
)

// newTestModel builds a Model wired to test channels. ctrl is buffered so the
// non-blocking send in Model.send lands in the buffer for assertion.
func newTestModel(t *testing.T, sessions []session.Info) (Model, chan Control) {
	t.Helper()
	cfg := config.Config{Provider: "macos"}
	events := make(chan Event)
	ctrl := make(chan Control, 8)
	return New(cfg, events, ctrl, sessions), ctrl
}

// step applies a message to the model and returns the concrete next Model.
func step(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	next, cmd := m.Update(msg)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want tui.Model", next)
	}
	return nm, cmd
}

// runes builds a KeyMsg for a printable key (e.g. "p", "q", "s").
func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// drainCtrl returns the next control if one is buffered, else ok=false.
func drainCtrl(ctrl chan Control) (Control, bool) {
	select {
	case c := <-ctrl:
		return c, true
	default:
		return Control{}, false
	}
}

// ready drives a WindowSizeMsg so the viewport is initialised (m.ready == true),
// which is required before appended lines reach the viewport and View renders
// the full chrome.
func ready(t *testing.T, m Model) Model {
	t.Helper()
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 80, Height: 24})
	if !m.ready {
		t.Fatal("model not ready after WindowSizeMsg")
	}
	return m
}

func TestNew_InitialState(t *testing.T) {
	cfg := config.Config{Provider: "elevenlabs"}
	cfg.Narrator.Enabled = true
	cfg.Narrator.Provider = "gemini"
	m := New(cfg, make(chan Event), make(chan Control), nil)

	if m.provider != "elevenlabs" {
		t.Errorf("provider = %q, want elevenlabs", m.provider)
	}
	if m.narrator != "gemini" {
		t.Errorf("narrator = %q, want gemini (enabled)", m.narrator)
	}
	if m.ready {
		t.Error("model should not be ready before first WindowSizeMsg")
	}
	// Narrator disabled leaves the narrator label empty.
	cfg2 := config.Config{Provider: "macos"}
	m2 := New(cfg2, make(chan Event), make(chan Control), nil)
	if m2.narrator != "" {
		t.Errorf("narrator = %q, want empty when disabled", m2.narrator)
	}
}

func TestUpdate_EventTextFoldsIntoLog(t *testing.T) {
	m, _ := newTestModel(t, nil)
	m = ready(t, m)

	m, cmd := step(t, m, Event{Kind: EventText, Text: "Hello world"})
	if cmd == nil {
		t.Error("Event should re-issue waitForEvent cmd, got nil")
	}
	if len(m.lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(m.lines))
	}
	if !strings.Contains(m.lines[0], "Hello world") {
		t.Errorf("log line %q missing spoken text", m.lines[0])
	}
	if !strings.Contains(m.View(), "Hello world") {
		t.Error("View() does not show folded spoken text")
	}
}

func TestUpdate_BlankTextNotLogged(t *testing.T) {
	m, _ := newTestModel(t, nil)
	m = ready(t, m)
	m, _ = step(t, m, Event{Kind: EventText, Text: "   \t "})
	if len(m.lines) != 0 {
		t.Errorf("blank text produced %d lines, want 0", len(m.lines))
	}
}

func TestApplyEvent_HeaderCounters(t *testing.T) {
	tests := []struct {
		name      string
		events    []Event
		wantQueue int
		wantEpoch uint64
	}{
		{
			name:      "playing sets queue and epoch",
			events:    []Event{{Kind: EventPlaying, Epoch: 1, Queue: 4, Text: "a"}},
			wantQueue: 4,
			wantEpoch: 1,
		},
		{
			name: "drained clears queue",
			events: []Event{
				{Kind: EventPlaying, Epoch: 1, Queue: 3},
				{Kind: EventDrained, Epoch: 1, Queue: 0},
			},
			wantQueue: 0,
			wantEpoch: 1,
		},
		{
			name: "text with positive queue updates counter",
			events: []Event{
				{Kind: EventText, Epoch: 2, Queue: 7, Text: "hi"},
			},
			wantQueue: 7,
			wantEpoch: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, nil)
			for _, ev := range tc.events {
				m.applyEvent(ev)
			}
			if m.queue != tc.wantQueue {
				t.Errorf("queue = %d, want %d", m.queue, tc.wantQueue)
			}
			if m.epoch != tc.wantEpoch {
				t.Errorf("epoch = %d, want %d", m.epoch, tc.wantEpoch)
			}
		})
	}
}

func TestApplyEvent_StaleEpochIgnored(t *testing.T) {
	m, _ := newTestModel(t, nil)

	// Establish current epoch 5 with a queued item.
	m.applyEvent(Event{Kind: EventPlaying, Epoch: 5, Queue: 2, Text: "current"})
	if m.epoch != 5 || len(m.lines) != 1 {
		t.Fatalf("setup: epoch=%d lines=%d", m.epoch, len(m.lines))
	}

	// A stale-epoch mirror event (epoch 3 < 5) must be disregarded entirely.
	m.applyEvent(Event{Kind: EventText, Epoch: 3, Queue: 99, Text: "stale"})
	if m.epoch != 5 {
		t.Errorf("stale event bumped epoch to %d, want 5", m.epoch)
	}
	if len(m.lines) != 1 {
		t.Errorf("stale event logged text: lines=%d, want 1", len(m.lines))
	}
	if m.queue != 2 {
		t.Errorf("stale event mutated queue to %d, want 2", m.queue)
	}

	// A newer epoch advances the model.
	m.applyEvent(Event{Kind: EventText, Epoch: 8, Text: "fresh"})
	if m.epoch != 8 {
		t.Errorf("epoch = %d, want 8 after newer event", m.epoch)
	}
	if len(m.lines) != 2 {
		t.Errorf("lines = %d, want 2 after fresh event", len(m.lines))
	}
}

// The stale-drop escape: an event with Epoch 0 is NEVER treated as stale, even
// when the model is on a later epoch. Pre-switch status/error lines (which carry
// epoch 0) must always render — otherwise early "listening…"/error messages
// would silently vanish once a session switch advanced m.epoch.
func TestApplyEvent_ZeroEpochAlwaysRenders(t *testing.T) {
	m, _ := newTestModel(t, nil)

	// Advance the model to epoch 5.
	m.applyEvent(Event{Kind: EventPlaying, Epoch: 5, Text: "current"})
	if m.epoch != 5 || len(m.lines) != 1 {
		t.Fatalf("setup: epoch=%d lines=%d", m.epoch, len(m.lines))
	}

	// An epoch-0 status event still renders and does not lower the epoch.
	m.applyEvent(Event{Kind: EventStatus, Epoch: 0, Text: "Listening via hooks"})
	if m.epoch != 5 {
		t.Errorf("zero-epoch event changed epoch to %d, want 5", m.epoch)
	}
	if len(m.lines) != 2 {
		t.Fatalf("zero-epoch status event was dropped: lines=%d, want 2", len(m.lines))
	}

	// An epoch-0 error event likewise renders.
	m.applyEvent(Event{Kind: EventError, Epoch: 0, Err: errors.New("early boom")})
	if len(m.lines) != 3 {
		t.Errorf("zero-epoch error event was dropped: lines=%d, want 3", len(m.lines))
	}
}

func TestApplyEvent_KindsLogging(t *testing.T) {
	tests := []struct {
		name       string
		ev         Event
		wantLines  int
		wantActive string
		contains   string
	}{
		{
			name:      "error uses Err when Text empty",
			ev:        Event{Kind: EventError, Err: errors.New("boom")},
			wantLines: 1,
			contains:  "boom",
		},
		{
			name:      "error prefers Text",
			ev:        Event{Kind: EventError, Text: "explicit", Err: errors.New("boom")},
			wantLines: 1,
			contains:  "explicit",
		},
		{
			name:       "watching sets active session",
			ev:         Event{Kind: EventWatching, Session: "sess-1", Text: "watching sess-1"},
			wantLines:  1,
			wantActive: "sess-1",
			contains:   "watching sess-1",
		},
		{
			name:       "session switched to named",
			ev:         Event{Kind: EventSessionSwitched, Session: "abc"},
			wantLines:  1,
			wantActive: "abc",
			contains:   "switched to abc",
		},
		{
			name:      "session switched to all",
			ev:        Event{Kind: EventSessionSwitched, Session: ""},
			wantLines: 1,
			contains:  "all sessions",
		},
		{
			name:      "status logs text",
			ev:        Event{Kind: EventStatus, Text: "ready"},
			wantLines: 1,
			contains:  "ready",
		},
		{
			name:      "drained logs nothing",
			ev:        Event{Kind: EventDrained},
			wantLines: 0,
		},
		{
			name:      "empty error logs nothing",
			ev:        Event{Kind: EventError},
			wantLines: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestModel(t, nil)
			m.applyEvent(tc.ev)
			if len(m.lines) != tc.wantLines {
				t.Fatalf("lines = %d, want %d", len(m.lines), tc.wantLines)
			}
			if tc.wantActive != "" && m.active != tc.wantActive {
				t.Errorf("active = %q, want %q", m.active, tc.wantActive)
			}
			if tc.contains != "" {
				if len(m.lines) == 0 || !strings.Contains(m.lines[0], tc.contains) {
					t.Errorf("lines %v missing %q", m.lines, tc.contains)
				}
			}
		})
	}
}

func TestAppendLine_RingBounded(t *testing.T) {
	m, _ := newTestModel(t, nil)
	for i := 0; i < maxLogLines+50; i++ {
		m.appendLine("line")
	}
	if len(m.lines) != maxLogLines {
		t.Errorf("lines = %d, want capped at %d", len(m.lines), maxLogLines)
	}
}

func TestHandleKey_PauseResumeSendsControls(t *testing.T) {
	m, ctrl := newTestModel(t, nil)

	// First "p" pauses.
	m, _ = step(t, m, runes("p"))
	if !m.paused {
		t.Error("model not paused after first p")
	}
	c, ok := drainCtrl(ctrl)
	if !ok || c.Kind != ControlPause {
		t.Fatalf("first p sent %+v ok=%v, want ControlPause", c, ok)
	}

	// Second "p" resumes.
	m, _ = step(t, m, runes("p"))
	if m.paused {
		t.Error("model still paused after second p")
	}
	c, ok = drainCtrl(ctrl)
	if !ok || c.Kind != ControlResume {
		t.Fatalf("second p sent %+v ok=%v, want ControlResume", c, ok)
	}
}

func TestHandleKey_SpaceTogglesPause(t *testing.T) {
	m, ctrl := newTestModel(t, nil)
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeySpace})
	if !m.paused {
		t.Error("space did not pause")
	}
	c, ok := drainCtrl(ctrl)
	if !ok || c.Kind != ControlPause {
		t.Fatalf("space sent %+v ok=%v, want ControlPause", c, ok)
	}
}

func TestHandleKey_QuitSendsControlAndQuits(t *testing.T) {
	for _, key := range []tea.KeyMsg{runes("q"), {Type: tea.KeyCtrlC}} {
		m, ctrl := newTestModel(t, nil)
		m, cmd := step(t, m, key)
		c, ok := drainCtrl(ctrl)
		if !ok || c.Kind != ControlQuit {
			t.Fatalf("%s sent %+v ok=%v, want ControlQuit", key, c, ok)
		}
		if cmd == nil {
			t.Fatalf("%s returned nil cmd, want tea.Quit", key)
		}
		if _, isQuit := cmd().(tea.QuitMsg); !isQuit {
			t.Errorf("%s cmd did not yield tea.QuitMsg", key)
		}
	}
}

func TestHandleKey_SwitchOpensPicker(t *testing.T) {
	// Point HOME at an empty temp dir so session.Discover touches no real data;
	// it returns an empty list and the picker still opens.
	t.Setenv("HOME", t.TempDir())

	sessions := []session.Info{{ID: "1111111122223333", ProjectName: "proj", LastActive: time.Now()}}
	m, _ := newTestModel(t, sessions)
	m = ready(t, m)

	if m.picking {
		t.Fatal("picker open before pressing s")
	}
	m, _ = step(t, m, runes("s"))
	if !m.picking {
		t.Error("picker not open after pressing s")
	}
}

func TestHandleKey_PickerEnterSelectsAndSwitches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sessions := []session.Info{{ID: "deadbeefcafef00d", ProjectName: "proj", LastActive: time.Now()}}
	m, ctrl := newTestModel(t, sessions)
	m = ready(t, m)

	// Open the picker; the first item is the "All sessions (hooks)" row with id "".
	m, _ = step(t, m, runes("s"))
	if !m.picking {
		t.Fatal("picker did not open")
	}

	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.picking {
		t.Error("picker still open after enter")
	}
	c, ok := drainCtrl(ctrl)
	if !ok || c.Kind != ControlSwitch {
		t.Fatalf("enter sent %+v ok=%v, want ControlSwitch", c, ok)
	}
	if c.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (all-sessions row)", c.SessionID)
	}
}

func TestHandleKey_PickerEscClosesWithoutSwitch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, ctrl := newTestModel(t, nil)
	m = ready(t, m)

	m, _ = step(t, m, runes("s"))
	if !m.picking {
		t.Fatal("picker did not open")
	}
	m, _ = step(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.picking {
		t.Error("esc did not close picker")
	}
	if _, ok := drainCtrl(ctrl); ok {
		t.Error("esc should not send a control")
	}
}

func TestSend_DropsWhenChannelFull(t *testing.T) {
	// A full ctrl channel must not block the UI loop; the control is dropped.
	cfg := config.Config{Provider: "macos"}
	ctrl := make(chan Control) // unbuffered, no reader
	m := New(cfg, make(chan Event), ctrl, nil)

	done := make(chan struct{})
	go func() {
		m2, _ := m.Update(runes("p"))
		_ = m2
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Update blocked on a full ctrl channel")
	}
}

func TestUpdate_EventsClosedQuits(t *testing.T) {
	m, _ := newTestModel(t, nil)
	_, cmd := step(t, m, eventsClosedMsg{})
	if cmd == nil {
		t.Fatal("eventsClosedMsg returned nil cmd, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("eventsClosedMsg did not yield tea.QuitMsg")
	}
}

func TestLayout_ResizeWhenReady(t *testing.T) {
	m, _ := newTestModel(t, nil)
	m = ready(t, m)
	// A second WindowSizeMsg exercises the already-ready resize branch.
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.vp.Width != 120 {
		t.Errorf("viewport width = %d, want 120 after resize", m.vp.Width)
	}
	// A zero-size message is a no-op (guarded).
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 0, Height: 0})
	if m.vp.Width != 120 {
		t.Errorf("viewport width = %d, zero resize should be ignored", m.vp.Width)
	}
}

func TestSend_NilChannelNoPanic(t *testing.T) {
	// A model with no ctrl channel must not panic when a control key is pressed.
	m := New(config.Config{Provider: "macos"}, make(chan Event), nil, nil)
	if _, cmd := m.Update(runes("q")); cmd == nil {
		t.Error("quit with nil ctrl should still return tea.Quit cmd")
	}
}

func TestView_ShowsNarrator(t *testing.T) {
	cfg := config.Config{Provider: "macos"}
	cfg.Narrator.Enabled = true
	cfg.Narrator.Provider = "gemini"
	m := New(cfg, make(chan Event), make(chan Control), nil)
	m = ready(t, m)
	if v := m.View(); !strings.Contains(v, "narrator=gemini") {
		t.Errorf("View missing narrator field: %q", v)
	}
}

func TestInit_ConsumesEvents(t *testing.T) {
	events := make(chan Event, 1)
	m := New(config.Config{Provider: "macos"}, events, make(chan Control), nil)
	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init returned nil cmd")
	}
	events <- Event{Kind: EventStatus, Text: "hi"}
	msg := cmd()
	ev, ok := msg.(Event)
	if !ok {
		t.Fatalf("Init cmd yielded %T, want Event", msg)
	}
	if ev.Text != "hi" {
		t.Errorf("Text = %q, want hi", ev.Text)
	}
}

func TestWaitForEvent_ClosedChannel(t *testing.T) {
	ch := make(chan Event)
	close(ch)
	if _, ok := waitForEvent(ch)().(eventsClosedMsg); !ok {
		t.Error("closed channel did not yield eventsClosedMsg")
	}
}

func TestUpdate_RoutesUnhandledMsg(t *testing.T) {
	// Non-key, non-event messages route to the active child (viewport or picker)
	// without panicking or changing the returned model type.
	t.Setenv("HOME", t.TempDir())
	m, _ := newTestModel(t, nil)
	m = ready(t, m)

	// Viewport path.
	m, _ = step(t, m, tea.MouseMsg{})
	// Picker path.
	m, _ = step(t, m, runes("s"))
	if !m.picking {
		t.Fatal("picker did not open")
	}
	if _, _ = step(t, m, tea.MouseMsg{}); m.picking != true {
		t.Error("picker closed unexpectedly on routed msg")
	}
}

func TestPickerItem_Interface(t *testing.T) {
	it := pickerItem{id: "x", title: "T", desc: "D"}
	if it.Title() != "T" || it.Description() != "D" {
		t.Errorf("Title/Description = %q/%q", it.Title(), it.Description())
	}
	if it.FilterValue() != "T D" {
		t.Errorf("FilterValue = %q, want %q", it.FilterValue(), "T D")
	}
}

func TestAgeString(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"zero", time.Time{}, ""},
		{"seconds", now.Add(-5 * time.Second), "5s ago"},
		{"minutes", now.Add(-3 * time.Minute), "3m ago"},
		{"hours", now.Add(-2 * time.Hour), "2h ago"},
		{"days", now.Add(-49 * time.Hour), "2d ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ageString(tc.t); got != tc.want {
				t.Errorf("ageString = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestView_AcrossStates(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Before ready: placeholder, never panics.
	m, _ := newTestModel(t, nil)
	if got := m.View(); !strings.Contains(got, "starting claude-says") {
		t.Errorf("pre-ready View = %q", got)
	}

	// Idle after layout.
	m = ready(t, m)
	idle := m.View()
	if idle == "" || !strings.Contains(idle, "claude-says") {
		t.Errorf("idle View missing header: %q", idle)
	}
	if !strings.Contains(idle, "provider=macos") {
		t.Errorf("idle View missing provider field: %q", idle)
	}

	// Playing: header reflects queue depth.
	m.applyEvent(Event{Kind: EventPlaying, Epoch: 1, Queue: 3, Text: "speaking"})
	if playing := m.View(); !strings.Contains(playing, "queue=3") {
		t.Errorf("playing View missing queue counter: %q", playing)
	}

	// Paused: header shows the paused badge.
	mp, _ := step(t, m, runes("p"))
	if paused := mp.View(); !strings.Contains(paused, "[PAUSED]") {
		t.Errorf("paused View missing badge: %q", paused)
	}

	// Picker open: footer switches to picker controls.
	ms, _ := step(t, m, runes("s"))
	if picker := ms.View(); !strings.Contains(picker, "enter select") {
		t.Errorf("picker View missing picker footer: %q", picker)
	}
}
