# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> **⚙️ Go single-binary app.** `claude-says` was rewritten from Node.js to a single static Go binary (`github.com/Sudhanshu069/claude-says`). The Go source under `cmd/` and `internal/` is authoritative; the original Node.js implementation has been removed (see the git history before the Go port for reference).

## Go implementation (authoritative)

Single binary, macOS-only. Build/run:

```bash
go build -o claude-says ./cmd/claude-says   # build
go run ./cmd/claude-says start              # daemon + TUI
go build ./... && go vet ./... && go test ./...   # must stay green
```

Package layout:

```
cmd/claude-says/     Cobra CLI: start [default] + voices + uninstall; preference flags (voice/rate/volume/flush-delay/min|max-chunk) auto-persist to config.json via persistPreferences (privacy/mode flags never do)
internal/config      ~/.claude-says/config.json (camelCase JSON, 0600, atomic writes, DefaultConfig)
internal/logx        log/slog logger — pretty on a TTY, JSON when piped; level via env
internal/session     session discovery under ~/.claude/projects (MostRecent, FindTranscript)
internal/transcript  transcript watcher: fsnotify + 200ms safety poll, offset read, UUID dedup, EOF start
internal/textproc    fence strip, noise filter, sentence split, markdown clean, content filters (dedupe + filler ON by default, --skip substrings; --verbatim opts out), monotonic seq (block-seam separator; every drop happens BEFORE seq++ so no gaps)
internal/audio       epoch-fenced ordered queue (queue.go: pause/resume, skip, mute/unmute — all preserve the single-owner drain; muted discards before play so no seq stall) + afplay player (player.go, --volume via afplayArgs)
internal/tts         Provider interface + macOS `say` (New always returns macOS; a stale cloud-provider config falls back to it)
internal/narrator    Narrator interface (total Narrate) + gemini
internal/daemon      orchestrator: context-cancellable pipeline, session switch, graceful drain; the text source is the transcript watcher (tests inject via the daemon's buffered `inject` channel)
internal/tui         Bubble Tea model/update/view; consumes daemon events via channel
```

**Trimmed to macOS-only (phase 1).** Cloud TTS (google, elevenlabs), the Claude Code Stop hook, the IPC Unix socket, and the `setup`/`sessions`/`providers` subcommands were removed (`uninstall` is kept); `go.mod` no longer pulls `golang.org/x/oauth2`. The daemon follows the session transcript exclusively — there is no hook/IPC fallback. Keep it this way unless a task explicitly re-adds a provider or text source.

**Load-bearing invariant — the epoch fence.** The audio queue keys every item by `{epoch, seq}`. A session switch/reset bumps the epoch; a single drain goroutine plays strictly in `seq` order for the current epoch and drops results from a stale epoch on arrival. This is what makes cross-session audio bleed, the CPU-hang, and stranded-sentence bugs from the Node version structurally impossible. **Do not** reintroduce a shared `draining` bool or a busy-loop; preserve the single-owner drain goroutine + channels + `context.Context` cancellation.

Stack: Bubble Tea + Lipgloss + Bubbles (TUI), Cobra (CLI), fsnotify (watcher), `lmittmann/tint` (log coloring). Go 1.26. Idiomatic Go throughout — small interfaces, channels, contexts, `errors.Is/As`.
