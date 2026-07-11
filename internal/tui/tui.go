// Package tui is the Bubble Tea front end. It replaces Node's raw-mode keypress
// controls with a status header, a scrolling spoken-log viewport, and a session
// picker. It consumes daemon events over a channel (via a waitForEvent tea.Cmd,
// no polling) and sends controls back over another channel — never shared
// mutable state. Events carry epoch+seq so the UI can disregard stale-epoch
// chatter after a session switch.
package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Sudhanshu069/claude-says/internal/config"
	"github.com/Sudhanshu069/claude-says/internal/session"
)

// EventKind classifies daemon -> UI notifications.
type EventKind int

const (
	EventText EventKind = iota // a sentence entering/at playback
	EventPlaying
	EventDrained
	EventWatching
	EventSessionSwitched
	EventStatus
	EventError
)

// Event is emitted by the daemon over a channel; it carries epoch+seq so the UI
// can disregard stale-epoch items after a session switch.
type Event struct {
	Kind    EventKind
	Epoch   uint64
	Seq     int
	Text    string
	Session string
	Queue   int
	Err     error
	Time    time.Time
}

// ControlKind is a UI -> daemon command.
type ControlKind int

const (
	ControlPause ControlKind = iota
	ControlResume
	ControlSwitch
	ControlQuit
	ControlSkip
	ControlMute
	ControlUnmute
)

// Control is a command from the UI to the daemon.
type Control struct {
	Kind      ControlKind
	SessionID string // for ControlSwitch; "" means no session (idle)
}

// maxLogLines bounds the spoken-log ring so a long-running session can't grow
// UI memory without limit.
const maxLogLines = 500

// Styles bundles the lipgloss styles used across the view.
type Styles struct {
	Header   lipgloss.Style
	Session  lipgloss.Style
	Paused   lipgloss.Style
	Speaking lipgloss.Style
	Line     lipgloss.Style
	ErrLine  lipgloss.Style
	Picker   lipgloss.Style
}

// NewStyles builds theme-aware (light/dark) styles. lipgloss AdaptiveColor picks
// the light or dark variant from the terminal background, so the same Styles
// value renders correctly in both.
func NewStyles() Styles {
	subtle := lipgloss.AdaptiveColor{Light: "244", Dark: "240"}
	fg := lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
	accent := lipgloss.AdaptiveColor{Light: "63", Dark: "111"}
	warn := lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	danger := lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	good := lipgloss.AdaptiveColor{Light: "28", Dark: "78"}

	return Styles{
		Header:   lipgloss.NewStyle().Bold(true).Foreground(accent),
		Session:  lipgloss.NewStyle().Foreground(subtle),
		Paused:   lipgloss.NewStyle().Bold(true).Foreground(warn),
		Speaking: lipgloss.NewStyle().Bold(true).Foreground(good),
		Line:     lipgloss.NewStyle().Foreground(fg),
		ErrLine:  lipgloss.NewStyle().Foreground(danger),
		Picker:   lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(subtle).Padding(0, 1),
	}
}

// pickerItem is one row in the session picker; id "" means no session (idle).
type pickerItem struct {
	id    string
	title string
	desc  string
}

func (i pickerItem) Title() string       { return i.title }
func (i pickerItem) Description() string { return i.desc }
func (i pickerItem) FilterValue() string { return i.title + " " + i.desc }

// Model is the Bubble Tea model.
type Model struct {
	events   <-chan Event
	ctrl     chan<- Control
	vp       viewport.Model
	picker   list.Model
	sessions []session.Info
	lines    []string
	picking  bool
	paused   bool
	muted    bool
	speaking bool
	provider string
	narrator string
	active   string
	epoch    uint64
	queue    int
	width    int
	height   int
	styles   Styles
	ready    bool
}

// New builds the initial model wired to the daemon channels.
func New(cfg config.Config, events <-chan Event, ctrl chan<- Control, sessions []session.Info) Model {
	narrator := ""
	if cfg.Narrator.Enabled {
		narrator = cfg.Narrator.Provider
	}
	m := Model{
		events:   events,
		ctrl:     ctrl,
		sessions: sessions,
		provider: cfg.Provider,
		narrator: narrator,
		styles:   NewStyles(),
	}
	m.picker = list.New(pickerItems(sessions), list.NewDefaultDelegate(), 0, 0)
	m.picker.Title = "Switch session"
	m.picker.SetShowHelp(true)
	m.picker.SetShowStatusBar(false)
	return m
}

// pickerItems builds the picker rows: a "no session" row first, then every
// discovered session most-recent-first.
func pickerItems(sessions []session.Info) []list.Item {
	items := make([]list.Item, 0, len(sessions)+1)
	items = append(items, pickerItem{
		id:    "",
		title: "No session (idle)",
		desc:  "Stop following — speak nothing until you pick a session",
	})
	for _, s := range sessions {
		short := s.ID
		if len(short) > 8 {
			short = short[:8]
		}
		proj := filepath.Base(s.ProjectName)
		// Primary label is the session's name (its ai-title, else first prompt);
		// fall back to the project when a transcript has neither.
		title := s.Title
		if title == "" {
			title = proj
		}
		items = append(items, pickerItem{
			id:    s.ID,
			title: title,
			desc:  proj + "  ·  " + ageString(s.LastActive) + "  ·  " + short,
		})
	}
	return items
}

// Init starts consuming daemon events.
func (m Model) Init() tea.Cmd {
	return waitForEvent(m.events)
}

// Update handles messages (events, keys, resize) and returns the next model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case eventsClosedMsg:
		// The daemon closed its event channel: it has shut down, so quit.
		return m, tea.Quit

	case Event:
		m.applyEvent(msg)
		// Keep consuming — no polling.
		return m, waitForEvent(m.events)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Route anything else to whichever child owns the screen.
	if m.picking {
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		return m, cmd
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// handleKey processes key input, dispatching to the picker when it is open.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picking {
		switch msg.String() {
		case "esc":
			m.picking = false
			return m, nil
		case "enter":
			if it, ok := m.picker.SelectedItem().(pickerItem); ok {
				m.active = it.id
				m.send(Control{Kind: ControlSwitch, SessionID: it.id})
			}
			m.picking = false
			return m, nil
		}
		var cmd tea.Cmd
		m.picker, cmd = m.picker.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "q", "ctrl+c":
		m.send(Control{Kind: ControlQuit})
		return m, tea.Quit
	case "p", " ":
		m.paused = !m.paused
		if m.paused {
			m.send(Control{Kind: ControlPause})
		} else {
			m.send(Control{Kind: ControlResume})
		}
		return m, nil
	case "m":
		m.muted = !m.muted
		if m.muted {
			m.send(Control{Kind: ControlMute})
		} else {
			m.send(Control{Kind: ControlUnmute})
		}
		return m, nil
	case "n", "right":
		m.send(Control{Kind: ControlSkip})
		return m, nil
	case "s":
		// Re-run discovery so the picker reflects sessions created/updated since
		// startup, with refreshed names and ages. On error we keep the last list.
		if fresh, err := session.DiscoverWithTitles(); err == nil {
			m.sessions = fresh
		}
		m.picking = true
		m.picker.SetItems(pickerItems(m.sessions))
		m.layout()
		return m, nil
	}

	// Otherwise let the viewport scroll (up/down/pgup/pgdn/home/end).
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// applyEvent folds one daemon event into the model state and the spoken log.
func (m *Model) applyEvent(ev Event) {
	// Disregard stale-epoch chatter after a session switch.
	if ev.Epoch != 0 && m.epoch != 0 && ev.Epoch < m.epoch {
		return
	}
	if ev.Epoch > m.epoch {
		m.epoch = ev.Epoch
	}
	if ev.Queue > 0 || ev.Kind == EventPlaying || ev.Kind == EventDrained {
		m.queue = ev.Queue
	}

	switch ev.Kind {
	case EventText, EventPlaying:
		if ev.Kind == EventPlaying {
			m.speaking = true
		}
		if t := strings.TrimSpace(ev.Text); t != "" {
			m.appendLine(m.styles.Line.Render(t))
		}
	case EventError:
		msg := ev.Text
		if msg == "" && ev.Err != nil {
			msg = ev.Err.Error()
		}
		if msg != "" {
			m.appendLine(m.styles.ErrLine.Render("! " + msg))
		}
	case EventWatching:
		m.active = ev.Session
		if ev.Text != "" {
			m.appendLine(m.styles.Session.Render("— " + ev.Text))
		}
	case EventSessionSwitched:
		m.active = ev.Session
		label := ev.Session
		if label == "" {
			label = "no session"
		}
		m.appendLine(m.styles.Session.Render("— switched to " + label))
	case EventStatus:
		if ev.Text != "" {
			m.appendLine(m.styles.Session.Render("— " + ev.Text))
		}
	case EventDrained:
		m.speaking = false
	}
}

// appendLine adds a rendered line to the bounded log ring and re-flows the
// viewport, keeping the newest content in view.
func (m *Model) appendLine(line string) {
	m.lines = append(m.lines, line)
	if len(m.lines) > maxLogLines {
		m.lines = append(m.lines[:0], m.lines[len(m.lines)-maxLogLines:]...)
	}
	if m.ready {
		m.vp.SetContent(strings.Join(m.lines, "\n"))
		m.vp.GotoBottom()
	}
}

// layout sizes the viewport and picker for the current terminal dimensions.
func (m *Model) layout() {
	if m.width <= 0 || m.height <= 0 {
		return
	}
	// Header (1) + spacer (1) + footer (1) = 3 rows of chrome.
	bodyH := m.height - 3
	if bodyH < 1 {
		bodyH = 1
	}
	if !m.ready {
		m.vp = viewport.New(m.width, bodyH)
		m.ready = true
		if len(m.lines) > 0 {
			m.vp.SetContent(strings.Join(m.lines, "\n"))
			m.vp.GotoBottom()
		}
	} else {
		m.vp.Width = m.width
		m.vp.Height = bodyH
	}
	m.picker.SetSize(m.width, bodyH)
}

// send delivers a control without ever blocking the UI loop; if the daemon-side
// reader is gone the control is dropped rather than freezing the render.
func (m Model) send(c Control) {
	if m.ctrl == nil {
		return
	}
	select {
	case m.ctrl <- c:
	default:
	}
}

// View renders the header, spoken-log viewport, and (when open) session picker.
func (m Model) View() string {
	if !m.ready {
		return "starting claude-says…"
	}

	var header strings.Builder
	header.WriteString(m.styles.Header.Render("claude-says"))
	header.WriteString("   ")
	header.WriteString(m.styles.Header.Render(m.activeSessionName()))
	header.WriteString("   ")
	header.WriteString(m.statusBadge())

	var body, footer string
	if m.picking {
		body = m.picker.View()
		footer = m.styles.Session.Render("↑/↓ move  enter select  esc cancel")
	} else {
		if len(m.lines) == 0 {
			// Nothing spoken yet: an idle hint beats a blank screen.
			hint := m.styles.Session.Render("  Listening — Claude Code's replies show up here as they're spoken.")
			pad := m.vp.Height - 1
			if pad < 0 {
				pad = 0
			}
			body = hint + strings.Repeat("\n", pad)
		} else {
			body = m.vp.View()
		}
		footer = m.styles.Session.Render("[p]ause  [m]ute  [n]/→ skip  [s]witch  [q]uit")
	}

	return strings.Join([]string{header.String(), body, footer}, "\n")
}

// activeSessionName is the header label for the followed session: its name (from
// the loaded session titles), else its short id, else "none".
func (m Model) activeSessionName() string {
	if m.active == "" {
		return "none"
	}
	for _, s := range m.sessions {
		if s.ID == m.active && s.Title != "" {
			return truncateLabel(s.Title, 44)
		}
	}
	if len(m.active) > 8 {
		return m.active[:8]
	}
	return m.active
}

// truncateLabel shortens s to at most n runes, with a trailing ellipsis.
func truncateLabel(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// statusBadge renders the current playback state for the header.
func (m Model) statusBadge() string {
	switch {
	case m.muted:
		return m.styles.Paused.Render("🔇 muted")
	case m.paused:
		return m.styles.Paused.Render("⏸ paused")
	case m.speaking:
		return m.styles.Speaking.Render("● speaking")
	default:
		return m.styles.Session.Render("○ idle")
	}
}

// waitForEvent blocks on the daemon channel and returns the Event as a tea.Msg,
// re-issued from Update to keep consuming — no polling. A closed channel yields
// an eventsClosedMsg.
func waitForEvent(ch <-chan Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return ev
	}
}

// eventsClosedMsg signals the daemon event channel closed.
type eventsClosedMsg struct{}

// ageString renders a coarse "Ns/Nm/Nh/Nd ago" relative time, mirroring the
// Node sessions formatter.
func ageString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// Run wires a tea.Program to the daemon and blocks until quit; cancelling ctx
// (SIGINT/quit) triggers the daemon's bounded drain then returns. The program
// runs in the alternate screen and is context-cancellable so an external
// shutdown tears the UI down cleanly.
func Run(ctx context.Context, m Model) error {
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	if err != nil && ctx.Err() != nil {
		// A ctx-cancelled program returns tea.ErrProgramKilled; that's a clean
		// external shutdown, not a failure.
		return nil
	}
	return err
}
