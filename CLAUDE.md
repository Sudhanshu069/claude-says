# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> **⚙️ Primary implementation is now Go.** `claude-says` was rewritten from Node.js to a single static Go binary (`github.com/Sudhanshu069/claude-code-speak`). The **Go source under `cmd/` and `internal/` is authoritative**; the Node source under `bin/` and `src/` is retained only as a reference during the transition and is described in the "Legacy Node implementation" sections below. New work goes in Go.

## Go implementation (authoritative)

Single binary, macOS-only. Build/run:

```bash
go build -o claude-says ./cmd/claude-says   # build
go run ./cmd/claude-says start              # daemon + TUI
go build ./... && go vet ./... && go test ./...   # must stay green
```

Package layout:

```
cmd/claude-says/     Cobra CLI (start[default]/setup/sessions/providers/voices) + hidden `hook` subcommand
internal/config      ~/.claude-says/config.json (camelCase JSON, 0600, atomic writes, DefaultConfig)
internal/logx        log/slog logger — pretty on a TTY, JSON when piped; level via env
internal/session     session discovery under ~/.claude/projects (MostRecent, FindTranscript)
internal/transcript  transcript watcher: fsnotify + 200ms safety poll, offset read, UUID dedup, EOF start
internal/textproc    fence strip, noise filter, sentence split, markdown clean, monotonic seq (block-seam separator)
internal/audio       epoch-fenced ordered queue (queue.go) + afplay player (player.go)
internal/ipc         Unix-domain-socket NDJSON server + client (0600 socket, lstat guard)
internal/tts         Provider interface + registry; macos / google / elevenlabs
internal/narrator    Narrator interface (total Narrate) + gemini
internal/daemon      orchestrator: context-cancellable pipeline, session switch, graceful drain
internal/tui         Bubble Tea model/update/view; consumes daemon events via channel
```

**Load-bearing invariant — the epoch fence.** The audio queue keys every item by `{epoch, seq}`. A session switch/reset bumps the epoch; a single drain goroutine plays strictly in `seq` order for the current epoch and drops results from a stale epoch on arrival. This is what makes cross-session audio bleed, the CPU-hang, and stranded-sentence bugs from the Node version structurally impossible. **Do not** reintroduce a shared `draining` bool or a busy-loop; preserve the single-owner drain goroutine + channels + `context.Context` cancellation.

Stack: Bubble Tea + Lipgloss + Bubbles (TUI), Cobra (CLI), fsnotify (watcher), `lmittmann/tint` (log coloring). Go 1.26. Idiomatic Go throughout — small interfaces, channels, contexts, `errors.Is/As`.

---

## Legacy Node implementation (reference only)

The sections below describe the original Node.js app under `bin/`/`src/`. It is kept for reference until the Go port fully retires it (the `setup` wizard is the thinnest area of parity). Prefer the Go source above for anything new.

### Project Overview

`claude-says` is a real-time text-to-speech companion for Claude Code CLI. It runs as a background daemon that listens for Claude Code's text output and speaks it aloud using a TTS provider. macOS-only (uses `afplay` for playback). The CLI command is `claude-says`; the GitHub repository is `Sudhanshu069/claude-code-speak`.

## Architecture

Two runtime components communicate over a Unix domain socket (`~/.claude-says/claude-says.sock`):

1. **Hook script** (`bin/claude-says-hook.js`) — Installed as a Claude Code `Stop` hook. Reads the session transcript, extracts new assistant text since last invocation (tracked via byte-offset state files in `/tmp/claude-says-state/`), and sends it to the daemon. Must complete within 3s to avoid blocking Claude's output.

2. **Daemon** (`bin/claude-says.js` → `src/daemon.js`) — Long-running process with two text ingestion paths:
   - **TranscriptWatcher** (`src/transcript-watcher.js`) — Watches a JSONL transcript file via `fs.watch` for near-instant reaction, with a 200ms safety poll as a fallback. Emits `text` events for new assistant messages. Deduplicates by UUID.
   - **IPC fallback** — Receives text from hooks via Unix socket when no transcript is being watched.

### Data Flow

```
Claude Code transcript (JSONL)
  -> TranscriptWatcher (poll) OR Hook -> IPC socket
  -> TextProcessor (sentence splitting, markdown/noise filtering)
  -> [Optional] Narrator (LLM rephrasing via Gemini)
  -> TTS Provider (synthesize to audio buffer)
  -> AudioQueue (sequence-ordered FIFO)
  -> AudioPlayer (afplay)
```

### Key Modules

- `src/daemon.js` — Orchestrator. Wires all components together, handles session switching, auto-detects most recent session.
- `src/logger.js` — pino-based operational logger for the daemon (see [Logging](#logging)).
- `src/text-processor.js` — Buffers streaming text, splits at sentence boundaries, strips markdown/URLs/code blocks, filters noise.
- `src/audio-queue.js` — Sequence-ordered FIFO. Plays audio in order regardless of when TTS responses arrive.
- `src/ipc.js` — Unix socket IPC. Newline-delimited JSON protocol. Exports `IPCServer` (daemon) and `sendToSocket` (hook).
- `src/tts.js` — Provider factory. Providers in `src/providers/` extend `BaseTTSProvider` with `synthesize(text)` and `validate()`.
- `src/narrator.js` — Narrator factory. Narrators in `src/narrators/` rephrase text via LLM before TTS.
- `src/sessions.js` — Discovers Claude Code sessions from `~/.claude/projects/`.
- `src/config.js` — Config from `~/.claude-says/config.json`. Exports `SOCKET_PATH`, `DEFAULT_CONFIG`.

### Runtime Paths

- Config: `~/.claude-says/config.json`
- Socket: `~/.claude-says/claude-says.sock`
- Hook state: `/tmp/claude-says-state/`
- Audio temp files: `/tmp/claude-says-audio/`

### Logging

The daemon's operational logging goes through [`pino`](https://getpino.io) via `src/logger.js`, which exports a singleton `logger`.

- **Verbosity** is set by the `LOG_LEVEL` env var (default `info`): `trace`, `debug`, `info`, `warn`, `error`, `fatal`, `silent`.
- **TTY** (interactive terminal) → human-readable, colorized lines via `pino-pretty`.
- **Piped/redirected** (no TTY) → structured NDJSON, one object per line — ideal for log files, `jq`, or a log collector.
- `pino-pretty` is attached as a **synchronous stream** (not a worker-thread transport) so the final lines aren't lost when the daemon `process.exit()`s on shutdown. If `pino-pretty` is absent, logging falls back to NDJSON cleanly.

Usage and conventions:

```js
import { logger } from './logger.js';
logger.info('started');
logger.warn(`degraded: ${reason}`);
logger.error(`failed: ${err.message}`);
```

- Inside `Daemon`, the legacy `this._log(msg)` helper routes to `logger.info` (empty spacer calls are ignored); error/degraded paths call `logger.error`/`logger.warn` directly.
- **Operational logs only.** Interactive prompts and wizard/CLI output (`src/setup.js`, the start-controls in `bin/claude-says.js`) intentionally stay on `console.*` — they are user-facing UI, not logs.
- Example: `LOG_LEVEL=debug node bin/claude-says.js start` for verbose output; `node bin/claude-says.js start | jq` for JSON logs.

## Commands

```bash
npm i                           # install dependencies
npm start                       # start the daemon
npm run setup                   # configure TTS provider and install hook

node bin/claude-says.js start             # start daemon
node bin/claude-says.js start -p macos    # start with specific TTS provider
node bin/claude-says.js start -l          # pick a session interactively
node bin/claude-says.js start --narrator  # enable LLM narrator mode
node bin/claude-says.js setup             # run setup wizard
node bin/claude-says.js sessions          # list discovered sessions
node bin/claude-says.js providers         # list available TTS providers
node bin/debug-hook.js                     # debug hook execution manually
```

## Extending

### Adding a TTS Provider

Create `src/providers/yourprovider.js` extending `BaseTTSProvider` with `synthesize(text)` and `validate()` methods, then register in `src/tts.js`.

### Adding a Narrator

Create `src/narrators/yournarrator.js` with a `narrate(text)` method, then register in `src/narrator.js`.

## Tech Stack

- Node.js >= 18, ES modules (`"type": "module"`)
- `commander` for CLI, `pino` + `pino-pretty` for logging, `@google-cloud/text-to-speech` (optional dep) for Google TTS
- macOS-specific: `afplay` for playback, `say` for macOS TTS
- No test framework, no TypeScript, no bundler
