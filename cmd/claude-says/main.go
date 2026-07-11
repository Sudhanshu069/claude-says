// Command claude-says is the single static binary for the TTS companion: a root
// command that runs `start` by default, plus `voices` for listing macOS voices.
// It follows a Claude Code transcript and speaks new assistant text via macOS
// `say`.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Sudhanshu069/claude-says/internal/config"
	"github.com/Sudhanshu069/claude-says/internal/daemon"
	"github.com/Sudhanshu069/claude-says/internal/logx"
	"github.com/Sudhanshu069/claude-says/internal/session"
	"github.com/Sudhanshu069/claude-says/internal/tts"
	"github.com/Sudhanshu069/claude-says/internal/tui"
)

// shutdownDrainTimeout bounds the audio drain on quit so a stuck player can
// never wedge shutdown (mirrors the Node bounded stop()).
const shutdownDrainTimeout = 5 * time.Second

// version is surfaced by `claude-says --version` (mirrors Node commander
// .version(pkg.version)). A var, not a const, so a release build can inject it
// via -ldflags "-X main.version=...".
var version = "2.0.0-dev"

// startOptions holds the CLI flags for the default/start action.
type startOptions struct {
	Session          string
	List             bool
	Rate             int
	Voice            string
	Narrator         bool
	NarratorProvider string
	Skip             []string
	Dedupe           bool
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

// newRootCmd builds the root command. It runs start by default (RunE=runStart)
// and holds the start flags, and registers every subcommand so Cobra routes
// e.g. `claude-says setup` correctly.
func newRootCmd() *cobra.Command {
	opts := &startOptions{}
	root := &cobra.Command{
		Use:           "claude-says",
		Short:         "Real-time text-to-speech companion for Claude Code",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd, opts)
		},
	}
	bindStartFlags(root.Flags(), opts)
	registerStartCompletions(root)
	root.AddCommand(
		newStartCmd(),
		newVoicesCmd(),
		newUninstallCmd(),
	)
	return root
}

// newStartCmd is the explicit `start` subcommand; it shares bindStartFlags.
func newStartCmd() *cobra.Command {
	opts := &startOptions{}
	cmd := &cobra.Command{
		Use:           "start",
		Short:         "Start the speak daemon",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStart(cmd, opts)
		},
	}
	bindStartFlags(cmd.Flags(), opts)
	registerStartCompletions(cmd)
	return cmd
}

// newVoicesCmd lists available TTS voices.
func newVoicesCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:           "voices",
		Short:         "List available macOS TTS voices",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVoices(all)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Show all voices (including non-English)")
	return cmd
}

// newUninstallCmd removes the ~/.claude-says config dir. It does not delete the
// binary itself (a running program can't reliably remove itself) — it prints the
// path to remove.
func newUninstallCmd() *cobra.Command {
	var keepConfig bool
	cmd := &cobra.Command{
		Use:           "uninstall",
		Short:         "Remove ~/.claude-says config (and print the binary path to delete)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(keepConfig)
		},
	}
	cmd.Flags().BoolVar(&keepConfig, "keep-config", false, "Keep ~/.claude-says (config)")
	return cmd
}

// runUninstall removes ~/.claude-says (unless --keep-config) and points at the
// binary. claude-says no longer installs anything into Claude Code, so there is
// no Stop hook to strip from ~/.claude/settings.json.
func runUninstall(keepConfig bool) error {
	fmt.Println("claude-says — Uninstall")
	fmt.Println()

	// Remove ~/.claude-says (config.json), unless kept.
	switch {
	case keepConfig:
		fmt.Println("  · Kept ~/.claude-says (--keep-config)")
	default:
		if dir, err := config.ConfigDir(); err != nil {
			fmt.Printf("  ! Could not resolve the config dir: %v\n", err)
		} else if _, err := os.Stat(dir); os.IsNotExist(err) {
			fmt.Println("  · No ~/.claude-says directory")
		} else if err := os.RemoveAll(dir); err != nil {
			fmt.Printf("  ! Could not remove %s: %v\n", dir, err)
		} else {
			fmt.Printf("  ✓ Removed %s (config)\n", dir)
		}
	}

	// Point at the binary — a running program can't cleanly delete itself.
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		fmt.Printf("\n  To remove the binary:  rm %s\n", exe)
	}
	fmt.Println("\nDone. If the daemon is still running, quit it (press q in the TUI, or kill the process).")
	return nil
}

// bindStartFlags registers the start flags on fs, bound to o. Both the root and
// the explicit `start` command call this so the two paths stay identical.
func bindStartFlags(fs *pflag.FlagSet, o *startOptions) {
	fs.StringVarP(&o.Session, "session", "s", "", "Listen to a specific session ID")
	fs.BoolVarP(&o.List, "list", "l", false, "List available sessions and pick one")
	fs.IntVarP(&o.Rate, "rate", "r", 0, "Speech rate in words per minute (default: 200)")
	fs.StringVarP(&o.Voice, "voice", "v", "", "macOS voice name")
	fs.BoolVarP(&o.Narrator, "narrator", "n", false, "Enable narrator mode")
	fs.StringVar(&o.NarratorProvider, "narrator-provider", "", "Narrator LLM provider (default: gemini)")
	fs.StringArrayVar(&o.Skip, "skip", nil, "Mute spoken sentences containing this text (case-insensitive; repeatable)")
	fs.BoolVar(&o.Dedupe, "dedupe", false, "Collapse consecutive identical sentences")
}

// registerStartCompletions wires shell tab-completion for the start flags:
// `--voice <TAB>` cycles the installed macOS voices.
func registerStartCompletions(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("voice", completeVoice)
}

// completeVoice offers the installed macOS `say` voices for --voice completion,
// each tagged with its language as the completion description, prefix-filtered by
// what the user has typed (case-insensitive) so a name like Gr narrows to
// Grandma/Grandpa across every shell rather than relying on shell-side matching.
func completeVoice(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	cfg := config.DefaultConfig()
	cfg.Provider = "macos"
	prov, err := tts.New(&cfg)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	lister, ok := prov.(tts.VoiceLister)
	if !ok {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	voices, err := lister.Voices(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// English voices only — matching the `voices` command default — so cycling
	// isn't flooded with the same name in a dozen languages. Non-English voices
	// remain settable manually (`--voice "Anna"`) and via `voices --all`.
	prefix := strings.ToLower(toComplete)
	comps := make([]string, 0, len(voices))
	for _, v := range voices {
		if !strings.HasPrefix(strings.ToLower(v.Language), "en") {
			continue
		}
		if prefix == "" || strings.HasPrefix(strings.ToLower(v.Name), prefix) {
			comps = append(comps, v.Name+"\t"+v.Language)
		}
	}
	return comps, cobra.ShellCompDirectiveNoFileComp
}

// applyOverrides layers CLI flags onto a loaded config.
func applyOverrides(cfg config.Config, o startOptions) config.Config {
	if o.Rate != 0 {
		cfg.Macos.Rate = o.Rate
	}
	if o.Voice != "" {
		cfg.Macos.Voice = o.Voice
	}
	if o.Narrator {
		cfg.Narrator.Enabled = true
		if o.NarratorProvider != "" {
			cfg.Narrator.Provider = o.NarratorProvider
		}
	}
	// --skip flags ADD to any patterns already in the config file.
	if len(o.Skip) > 0 {
		cfg.TextProcessor.Skip = append(cfg.TextProcessor.Skip, o.Skip...)
	}
	if o.Dedupe {
		cfg.TextProcessor.Dedupe = true
	}
	return cfg
}

// runStart loads config, applies overrides, resolves the session to follow, and
// runs the daemon. When stdout is a TTY it drives the Bubble Tea UI (which mirrors
// transcript text into a scrolling log and forwards p/s/q controls back to the
// daemon); otherwise it runs headless until a signal arrives.
func runStart(cmd *cobra.Command, o *startOptions) error {
	cfg, _ := config.Load()
	cfg = applyOverrides(cfg, *o)

	sessionID, transcriptPath, proceed, err := resolveSession(o)
	if err != nil {
		return err
	}
	if !proceed {
		// --list picker was cancelled (no session and not "all").
		fmt.Println("No session selected.")
		return nil
	}

	d, err := daemon.New(cfg, daemon.Options{
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
		NarratorOn:     cfg.Narrator.Enabled,
	})
	if err != nil {
		return err
	}

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	ctx, cancel := context.WithCancel(sigCtx)
	defer cancel()

	if !isTerminal(os.Stdout) {
		// Headless: the daemon owns the whole pipeline; just run until a signal.
		logx.Init()
		errc := make(chan error, 1)
		go func() { errc <- d.Run(ctx) }()
		<-ctx.Done()
		d.Stop(shutdownDrainTimeout)
		cancel()
		<-errc
		return nil
	}

	// TUI mode. Redirect operational logs to a file so slog lines never corrupt
	// the alt-screen render.
	if lf := openDaemonLog(); lf != nil {
		logx.InitTo(lf, false)
	}

	daemonErr := make(chan error, 1)
	go func() { daemonErr <- d.Run(ctx) }()

	// Drive the UI straight off the daemon's OWN event stream and control channel:
	// the viewport shows the cleaned sentences the daemon actually speaks (its
	// EventText), the header queue=/epoch=/playing counters follow its queue
	// events, and p/s/q keys flow into the daemon loop (the sole owner of
	// epoch/seq stamping). No second watcher, no mirror.
	sessions, _ := session.DiscoverWithTitles()
	m := tui.New(cfg, d.Events(), d.Controls(), sessions)
	runErr := tui.Run(ctx, m)

	// Drain audio (bounded), then tear the daemon down. Stop is idempotent and
	// cancel() unblocks anything still selecting on ctx.
	d.Stop(shutdownDrainTimeout)
	cancel()
	<-daemonErr
	return runErr
}

// resolveSession picks the transcript to follow: an explicit --session, an
// interactive --list pick, or the most-recent session. An empty (id, path) with
// proceed=true means start idle with no session; proceed=false means the --list
// picker was cancelled and the caller should exit cleanly.
func resolveSession(o *startOptions) (sessionID, transcriptPath string, proceed bool, err error) {
	switch {
	case o.List:
		picked, all, ok := pickSessionInteractive()
		if !ok && !all {
			return "", "", false, nil // cancelled
		}
		if all {
			return "", "", true, nil // start idle, no session
		}
		return picked.ID, picked.TranscriptPath, true, nil
	case o.Session != "":
		path, ok, ferr := session.FindTranscript(o.Session)
		if ferr != nil {
			return "", "", false, ferr
		}
		if !ok {
			// Unknown id: start anyway with an empty path. The daemon's
			// selectInitialSource logs "No transcript for <id>" and stays idle
			// until a switch points it at a real transcript.
			return o.Session, "", true, nil
		}
		return o.Session, path, true, nil
	default:
		s, ok, ferr := session.MostRecent()
		if ferr != nil || !ok {
			return "", "", true, nil // nothing yet: start idle
		}
		return s.ID, s.TranscriptPath, true, nil
	}
}

// sessionName is the human label for a session: its Title (ai-title / first
// prompt), falling back to the project basename when a transcript has neither.
func sessionName(s session.Info) string {
	if s.Title != "" {
		return s.Title
	}
	return filepath.Base(s.ProjectName)
}

// runVoices prints available macOS voices (all when true, else English only),
// mirroring the Node `voices` command output.
func runVoices(all bool) error {
	cfg := config.DefaultConfig()
	cfg.Provider = "macos"
	prov, err := tts.New(&cfg)
	if err != nil {
		return err
	}
	lister, ok := prov.(tts.VoiceLister)
	if !ok {
		return errors.New("this provider cannot list voices")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	voices, err := lister.Voices(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Failed to list voices. Are you on macOS?")
		return err
	}

	filtered := voices
	if !all {
		filtered = filtered[:0]
		for _, v := range voices {
			if strings.HasPrefix(strings.ToLower(v.Language), "en") {
				filtered = append(filtered, v)
			}
		}
	}

	suffix := " (English)"
	if all {
		suffix = ""
	}
	fmt.Printf("Available macOS voices%s:\n\n", suffix)
	for _, v := range filtered {
		fmt.Printf("  %-30s %s\n", v.Name, v.Language)
	}
	if !all {
		fmt.Printf("\nShowing English voices only. Use --all to see all %d voices.\n", len(voices))
	}
	fmt.Println()
	fmt.Println(`Usage: claude-says --voice "Daniel"`)
	return nil
}

// pickSessionInteractive prints a numbered session list and reads a choice from
// stdin. all=true means "start idle with no session"; ok=false with all=false
// means the user cancelled.
func pickSessionInteractive() (picked session.Info, all bool, ok bool) {
	sessions, err := session.DiscoverWithTitles()
	if err != nil || len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return session.Info{}, false, false
	}
	fmt.Println()
	fmt.Println("Select a Claude Code session to listen to:")
	fmt.Println()
	limit := len(sessions)
	if limit > 15 {
		limit = 15
	}
	display := sessions[:limit]
	for i, s := range display {
		fmt.Printf("  %d. %s\n       %s  %s  (%s)\n", i+1, sessionName(s), shortID(s.ID), filepath.Base(s.ProjectName), formatAge(s.LastActive))
	}
	fmt.Println("  0. Start with no session (idle)")
	fmt.Println()
	fmt.Print("Enter number: ")

	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return session.Info{}, false, false
	}
	if n == 0 {
		return session.Info{}, true, true
	}
	if n >= 1 && n <= len(display) {
		return display[n-1], false, true
	}
	return session.Info{}, false, false
}

// shortID returns the first 8 chars of a session id, or "all" when empty.
func shortID(id string) string {
	if id == "" {
		return "all"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// formatAge renders a coarse relative time, mirroring the Node formatAge.
func formatAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
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

// isTerminal reports whether f is an interactive terminal (drives TUI-vs-headless).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// openDaemonLog opens ~/.claude-says/daemon.log (append, 0600) for the TUI's
// redirected operational logs. Returns nil on failure (logging then stays on
// stderr, which the alt-screen hides — acceptable).
func openDaemonLog() *os.File {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}
