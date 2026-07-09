// Command claude-says is the single static binary for the TTS companion. It
// mirrors the Node commander tree: a root command that runs `start` by default,
// plus setup, sessions, providers, voices, and hook subcommands. The Node
// bin/claude-says-hook.js becomes the `hook` subcommand (see hook.go) so there
// is exactly one binary.
package main

import (
	"bufio"
	"context"
	"encoding/json"
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

	"github.com/Sudhanshu069/claude-says/internal/audio"
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

// startOptions holds the CLI flags for the default/start action, mirroring the
// Node start command one-for-one.
type startOptions struct {
	Provider         string
	Session          string
	List             bool
	Rate             int
	Voice            string
	Narrator         bool
	NarratorProvider string
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
		newSetupCmd(),
		newUninstallCmd(),
		newSessionsCmd(),
		newProvidersCmd(),
		newVoicesCmd(),
		newHookCmd(),
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

// newSetupCmd configures the TTS provider and installs the Claude Code hook.
func newSetupCmd() *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:           "setup",
		Short:         "Configure TTS provider and install Claude Code hook",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(provider)
		},
	}
	cmd.Flags().StringVarP(&provider, "provider", "p", "", "TTS provider")
	_ = cmd.RegisterFlagCompletionFunc("provider", completeProvider)
	return cmd
}

// newSessionsCmd lists discovered Claude Code sessions.
func newSessionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "sessions",
		Short:         "List discovered Claude Code sessions",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessions()
		},
	}
}

// newProvidersCmd lists available TTS providers.
func newProvidersCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "providers",
		Short:         "List available TTS providers",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProviders()
		},
	}
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

// newHookCmd is the Claude Code Stop-hook entry point (see hook.go). The hidden
// --debug flag replaces Node's bin/debug-hook.js: it dumps the raw stdin payload
// to a 0600 temp log before normal processing.
func newHookCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "hook",
		Short:  "Claude Code Stop-hook: forward transcript text to the daemon",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHook(cmd, args)
		},
	}
	cmd.Flags().Bool("debug", false, "Dump the raw hook payload to a temp debug log")
	_ = cmd.Flags().MarkHidden("debug")
	return cmd
}

// bindStartFlags registers the start flags on fs, bound to o. Both the root and
// the explicit `start` command call this so the two paths stay identical.
func bindStartFlags(fs *pflag.FlagSet, o *startOptions) {
	fs.StringVarP(&o.Provider, "provider", "p", "", "TTS provider")
	fs.StringVarP(&o.Session, "session", "s", "", "Listen to a specific session ID")
	fs.BoolVarP(&o.List, "list", "l", false, "List available sessions and pick one")
	fs.IntVarP(&o.Rate, "rate", "r", 0, "Speech rate in words per minute (default: 200)")
	fs.StringVarP(&o.Voice, "voice", "v", "", "macOS voice name")
	fs.BoolVarP(&o.Narrator, "narrator", "n", false, "Enable narrator mode")
	fs.StringVar(&o.NarratorProvider, "narrator-provider", "", "Narrator LLM provider (default: gemini)")
}

// registerStartCompletions wires shell tab-completion for the start flags:
// `--voice <TAB>` cycles macOS voices, `--provider <TAB>` the TTS providers.
func registerStartCompletions(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("voice", completeVoice)
	_ = cmd.RegisterFlagCompletionFunc("provider", completeProvider)
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

// completeProvider offers the registered TTS providers for --provider completion.
func completeProvider(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	return tts.List(), cobra.ShellCompDirectiveNoFileComp
}

// applyOverrides layers CLI flags onto a loaded config, mirroring the Node
// daemon constructor's override precedence.
func applyOverrides(cfg config.Config, o startOptions) config.Config {
	if o.Provider != "" {
		cfg.Provider = o.Provider
	}
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
		Provider:       cfg.Provider,
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
// proceed=true means all-sessions/hook mode; proceed=false means the --list
// picker was cancelled and the caller should exit cleanly.
func resolveSession(o *startOptions) (sessionID, transcriptPath string, proceed bool, err error) {
	switch {
	case o.List:
		picked, all, ok := pickSessionInteractive()
		if !ok && !all {
			return "", "", false, nil // cancelled
		}
		if all {
			return "", "", true, nil // all-sessions mode
		}
		return picked.ID, picked.TranscriptPath, true, nil
	case o.Session != "":
		path, ok, ferr := session.FindTranscript(o.Session)
		if ferr != nil {
			return "", "", false, ferr
		}
		if !ok {
			// Node started the daemon anyway and listened via hooks rather than
			// hard-erroring. Proceed with the id and an empty path; the daemon's
			// selectInitialSource then logs "No transcript for <id>; listening
			// via hooks" and falls back to the hook/IPC source.
			return o.Session, "", true, nil
		}
		return o.Session, path, true, nil
	default:
		s, ok, ferr := session.MostRecent()
		if ferr != nil || !ok {
			return "", "", true, nil // nothing yet: fall back to all-sessions/hook mode
		}
		return s.ID, s.TranscriptPath, true, nil
	}
}

// runSetup mirrors Node src/setup.js: validate the provider, test playback,
// install the Stop hook, and persist config. It returns an error on any failing
// step so the CLI exits non-zero.
func runSetup(provider string) error {
	fmt.Println("claude-says — Setup")
	fmt.Println()

	cfg, _ := config.Load()
	if provider != "" {
		cfg.Provider = provider
	}

	fmt.Printf("TTS Provider: %s\n", cfg.Provider)
	fmt.Printf("Available providers: %s\n\n", strings.Join(providerNames(), ", "))

	prov, err := tts.New(&cfg)
	if err != nil {
		return err
	}

	fmt.Println("Validating TTS credentials...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if verr := prov.Validate(ctx); verr != nil {
		fmt.Fprintf(os.Stderr, "TTS validation failed: %v\n\n", verr)
		printProviderHelp(cfg.Provider)
		return errors.New("TTS validation failed")
	}
	fmt.Println("TTS credentials valid!")
	fmt.Println()

	fmt.Println("Testing audio playback...")
	audioBytes, format, serr := prov.Synthesize(ctx, "claude-says is ready.")
	if serr != nil {
		return fmt.Errorf("audio synthesis failed: %w", serr)
	}
	player, perr := audio.NewPlayer()
	if perr != nil {
		return fmt.Errorf("audio playback failed: %w", perr)
	}
	if err := player.Play(ctx, audioBytes, format); err != nil {
		return fmt.Errorf("audio playback failed: %w", err)
	}
	fmt.Println("Audio playback works!")
	fmt.Println()

	fmt.Println("Installing Stop hook...")
	hookInstalled := installHook()
	if hookInstalled {
		fmt.Println("Hook installed successfully!")
	} else {
		fmt.Println("Hook installation failed — you may need to add it manually.")
	}
	fmt.Println()

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Println("Configuration saved.")
	fmt.Println()

	if hookInstalled {
		fmt.Println("Setup complete! Start the daemon with:")
		fmt.Println("  claude-says")
		fmt.Println()
		fmt.Println("Then use Claude Code normally — you'll hear it speak.")
		return nil
	}
	fmt.Println("Setup finished WITH WARNINGS: the Stop hook is not installed.")
	fmt.Println("TTS is configured, but real-time speech via hooks is disabled")
	fmt.Println("until you add the hook to ~/.claude/settings.json manually.")
	return errors.New("Stop hook not installed")
}

// printProviderHelp prints the provider-specific credential hint on validation
// failure, mirroring Node setup.js.
func printProviderHelp(provider string) {
	switch provider {
	case "google":
		fmt.Println("Setup instructions for Google Cloud TTS:")
		fmt.Println("  1. Create a Google Cloud project")
		fmt.Println("  2. Enable the Text-to-Speech API")
		fmt.Println("  3. Create a service account key")
		fmt.Println("  4. Set GOOGLE_APPLICATION_CREDENTIALS=/path/to/key.json")
	case "elevenlabs":
		fmt.Println("Setup instructions for ElevenLabs:")
		fmt.Println("  1. Sign up at elevenlabs.io (paid plan required for API)")
		fmt.Println("  2. Get your API key from settings")
		fmt.Println("  3. Set ELEVENLABS_API_KEY=your-key")
	case "macos":
		fmt.Println("macOS say command should be available by default.")
		fmt.Println(`Try: say "hello" in your terminal.`)
	}
}

// runSessions prints discovered sessions, most-recent-first, each labelled with
// its session name (ai-title / first prompt) over a project + id + age line.
func runSessions() error {
	sessions, err := session.DiscoverWithTitles()
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}
	fmt.Println("Recent Claude Code sessions:")
	fmt.Println()
	limit := len(sessions)
	if limit > 20 {
		limit = 20
	}
	for _, s := range sessions[:limit] {
		fmt.Printf("  %s\n      %s  %s  (%s)\n", sessionName(s), shortID(s.ID), filepath.Base(s.ProjectName), formatAge(s.LastActive))
	}
	fmt.Printf("\nTotal: %d sessions\n", len(sessions))
	return nil
}

// sessionName is the human label for a session: its Title (ai-title / first
// prompt), falling back to the project basename when a transcript has neither.
func sessionName(s session.Info) string {
	if s.Title != "" {
		return s.Title
	}
	return filepath.Base(s.ProjectName)
}

// runProviders prints the registered TTS providers.
func runProviders() error {
	fmt.Println("Available TTS providers:")
	for _, p := range providerNames() {
		fmt.Printf("  - %s\n", p)
	}
	return nil
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

// installHook adds this binary's `hook` subcommand as a Claude Code Stop hook in
// ~/.claude/settings.json, atomically, preserving unrelated settings and pruning
// any stale pre-rename entry. Mirrors Node setup.js installHook.
func installHook() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	settingsDir := filepath.Join(home, ".claude")
	settingsFile := filepath.Join(settingsDir, "settings.json")

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	hookCommand := fmt.Sprintf("%q hook", exe)

	// Preserve every top-level settings key by decoding into a generic map.
	settings := map[string]any{}
	if data, rerr := os.ReadFile(settingsFile); rerr == nil {
		if jerr := json.Unmarshal(data, &settings); jerr != nil {
			settings = map[string]any{}
		}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	existing, _ := hooks["Stop"].([]any)

	var kept []any
	removedStale := false
	for _, g := range existing {
		if groupContains(g, "claude-speak-hook") || groupContains(g, "claude-says-hook.js") {
			removedStale = true
			continue
		}
		kept = append(kept, g)
	}

	alreadyInstalled := false
	for _, g := range kept {
		if groupContains(g, exe) {
			alreadyInstalled = true
			break
		}
	}

	if alreadyInstalled && !removedStale {
		fmt.Println("  Hook already installed.")
		return true
	}
	if alreadyInstalled {
		fmt.Println("  Removed a stale pre-rename hook entry.")
	} else {
		kept = append(kept, map[string]any{
			"matcher": "*",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": hookCommand,
					"timeout": 5,
				},
			},
		})
	}

	hooks["Stop"] = kept
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	tmp := settingsFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	if err := os.Rename(tmp, settingsFile); err != nil {
		os.Remove(tmp)
		fmt.Fprintf(os.Stderr, "  Error installing hook: %v\n", err)
		return false
	}
	return true
}

// newUninstallCmd removes the Claude Code Stop hook and the ~/.claude-says config
// dir, reversing `setup`. It does not delete the binary itself (a running program
// can't reliably remove itself) — it prints the path to remove.
func newUninstallCmd() *cobra.Command {
	var keepConfig bool
	cmd := &cobra.Command{
		Use:           "uninstall",
		Short:         "Remove the Claude Code hook and config (reverse of setup)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUninstall(keepConfig)
		},
	}
	cmd.Flags().BoolVar(&keepConfig, "keep-config", false, "Keep ~/.claude-says (config + socket)")
	return cmd
}

func runUninstall(keepConfig bool) error {
	fmt.Println("claude-says — Uninstall")
	fmt.Println()

	// 1. Remove the Stop hook from ~/.claude/settings.json.
	switch removed, err := removeHook(); {
	case err != nil:
		fmt.Printf("  ! Could not update the Stop hook: %v\n", err)
	case removed:
		fmt.Println("  ✓ Removed the claude-says Stop hook from ~/.claude/settings.json")
	default:
		fmt.Println("  · No claude-says Stop hook found")
	}

	// 2. Remove ~/.claude-says (config.json + socket), unless kept.
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
			fmt.Printf("  ✓ Removed %s (config + socket)\n", dir)
		}
	}

	// 3. Point at the binary — a running program can't cleanly delete itself.
	if exe, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
			exe = resolved
		}
		fmt.Printf("\n  To remove the binary:  rm %s\n", exe)
	}
	fmt.Println("\nDone. If the daemon is still running, quit it (press q in the TUI, or kill the process).")
	return nil
}

// removeHook deletes claude-says's Stop-hook group(s) from ~/.claude/settings.json,
// preserving every other setting and hook, and writes the result atomically. It is
// the inverse of installHook. Returns removed=true iff a matching group was found.
// A missing settings file (or no matching hook) is a no-op returning (false, nil).
func removeHook() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	settingsFile := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	settings := map[string]any{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return false, fmt.Errorf("settings.json is not valid JSON: %w", err)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false, nil
	}
	existing, _ := hooks["Stop"].([]any)

	exe, _ := os.Executable()
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}

	var kept []any
	removed := false
	for _, g := range existing {
		// Match any claude-says hook regardless of install path — installHook
		// writes a quoted path to the `claude-says` binary followed by ` hook` —
		// plus the current exe and the stale Node hook names.
		if groupContains(g, `claude-says" hook`) ||
			groupContains(g, "claude-says-hook.js") ||
			groupContains(g, "claude-speak-hook") ||
			(exe != "" && groupContains(g, exe)) {
			removed = true
			continue
		}
		kept = append(kept, g)
	}
	if !removed {
		return false, nil
	}

	// Tidy up: drop an empty Stop list and an empty hooks object.
	if len(kept) == 0 {
		delete(hooks, "Stop")
	} else {
		hooks["Stop"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	tmp := settingsFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, settingsFile); err != nil {
		os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// groupContains reports whether any command string in a Stop-hook group contains
// substr.
func groupContains(group any, substr string) bool {
	gm, ok := group.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := gm["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, ok := hm["command"].(string); ok && strings.Contains(cmd, substr) {
			return true
		}
	}
	return false
}

// pickSessionInteractive prints a numbered session list and reads a choice from
// stdin, mirroring the Node --list picker. all=true means "listen to all
// sessions"; ok=false with all=false means the user cancelled.
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
	fmt.Println("  0. Listen to all sessions")
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

// providerNames returns the registered TTS provider names in Node's display
// order (google, elevenlabs, macos); tts.List already returns them ordered.
func providerNames() []string {
	return tts.List()
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
