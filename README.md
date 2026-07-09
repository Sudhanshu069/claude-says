# claude-says

[![release](https://img.shields.io/github/v/release/Sudhanshu069/claude-says?logo=github&color=success)](https://github.com/Sudhanshu069/claude-says/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go&logoColor=white)](https://go.dev)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)
[![platform: macOS](https://img.shields.io/badge/platform-macOS-black.svg?logo=apple)](#requirements)
[![single binary](https://img.shields.io/badge/deploy-single%20binary-brightgreen.svg)](#install)

Stop staring at your terminal waiting for Claude Code to finish. **claude-says** reads Claude's output aloud in real-time so you can step away, stretch, or keep working — and just listen.

A single, dependency-free binary with a live TUI: watch (and hear) what Claude is building, switch sessions, and pause on a keystroke.

> **macOS only** — playback uses `afplay` and the default voice uses the built-in `say` command.

> ### ⚙️ Now a Go rewrite
> This is a ground-up **Go port** of the original Node.js project (which was no longer maintained). It ships as one static binary — no `node_modules`, no npm — with a real Bubble Tea TUI and an audio pipeline rebuilt to fix a class of ordering/race bugs the Node daemon had. See [What changed from the Node version](#what-changed-from-the-node-version).

## Install

### One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/Sudhanshu069/claude-says/main/install.sh | sh
```

Detects your chip, downloads the latest macOS release, **verifies its SHA-256 checksum**, and installs `claude-says` to `/usr/local/bin` (override with `BINDIR=…`). Then run `claude-says setup`.

<details>
<summary>…or download a release manually</summary>

Grab `claude-says_<version>_darwin_arm64.tar.gz` (or `_amd64`) from the [latest release](https://github.com/Sudhanshu069/claude-says/releases/latest) — one static binary, no runtime dependencies — then:

```bash
tar -xzf claude-says_*_darwin_arm64.tar.gz
sudo mv claude-says /usr/local/bin/
claude-says --version
```

Each release ships a `checksums.txt` (SHA-256) to verify against.
</details>

### With `go install`

```bash
go install github.com/Sudhanshu069/claude-says/cmd/claude-says@latest
# installs to $(go env GOBIN) or $(go env GOPATH)/bin — make sure that's on your PATH
```

### From source

```bash
git clone https://github.com/Sudhanshu069/claude-says.git
cd claude-says
go build -o claude-says ./cmd/claude-says
sudo mv claude-says /usr/local/bin/
```

Building requires **Go 1.26+**; the resulting binary has no runtime dependencies.

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/Sudhanshu069/claude-says/main/uninstall.sh | sh
```

Or, if you already have the binary:

```bash
claude-says uninstall     # removes the Claude Code Stop hook + ~/.claude-says
rm "$(command -v claude-says)"   # then remove the binary
```

`claude-says uninstall` reverses `setup`: it strips the claude-says Stop hook from `~/.claude/settings.json` (leaving your other settings and hooks untouched) and deletes `~/.claude-says` (config + socket). Pass `--keep-config` to keep your settings.

## Quick Start

```bash
# 1. Setup (installs the Claude Code hook, tests audio)
claude-says setup

# 2. Start in one terminal — opens the TUI
claude-says start

# 3. Use Claude Code in another terminal as normal
claude
```

That's it. When Claude responds, you'll hear it spoken aloud and see it scroll in the TUI.

## Why?

- Claude Code runs can take minutes — you shouldn't have to watch text scroll the whole time.
- You might miss when Claude asks for input or confirmation.
- Sometimes you just want to code from the couch.

## Requirements

- **macOS** (uses `afplay` for playback, `say` for the default voice)
- **Go 1.26+** to build (the resulting binary has no runtime deps)
- **Claude Code CLI** installed (`claude-says setup` registers a `Stop` hook with it)

## TTS Providers

| Provider | Setup | Latency | Cost |
|----------|-------|---------|------|
| `macos` (default) | None | Lowest (local) | Free |
| `google` | Service-account creds | ~1–2s / sentence | Pay per use |
| `elevenlabs` | API key (paid plan) | ~0.5–1s | Pay per use |

```bash
claude-says start --provider google
```

### macOS (default)

Works out of the box using the built-in `say` command. No API keys needed.

```bash
claude-says voices                    # List English voices
claude-says voices --all              # List all voices
claude-says start --voice "Daniel"    # Use a specific voice
claude-says start --rate 150          # Slower (words per minute, default 200)
claude-says start --voice "Karen" --rate 150
```

### Google Cloud TTS

```bash
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account-key.json
claude-says setup --provider google
```

### ElevenLabs

```bash
export ELEVENLABS_API_KEY=your-key
claude-says setup --provider elevenlabs
```

## Narrator mode (optional)

Instead of reading Claude's output verbatim, narrator mode runs the text through an LLM that rephrases it into a short, spoken-friendly summary before it's voiced — less "reading markdown out loud," more "a colleague telling you what just happened." Powered by Google Gemini.

```bash
export GEMINI_API_KEY=your-key
claude-says start --narrator
```

## Commands

```bash
claude-says start              # Start the daemon + TUI (auto-detects most recent session)
claude-says start -l           # Pick a session interactively
claude-says start -s <id>      # Listen to a specific session
claude-says start -p <name>    # Use a specific TTS provider
claude-says start --narrator   # Enable LLM narrator mode
claude-says start --voice "Daniel"
claude-says start --rate 150
claude-says setup              # Configure provider and install the Stop hook
claude-says sessions           # List Claude Code sessions
claude-says providers          # List available TTS providers
claude-says voices             # List available macOS voices
claude-says --version
```

## Controls (in the TUI)

| Key | Action |
|-----|--------|
| `p` | Pause / Resume |
| `s` | Switch session |
| `q` | Quit (drains the current sentence, then exits) |

## Configuration

Settings live in `~/.claude-says/config.json` (owner-only, `0600`) and are merged over the defaults — you only need the keys you want to change. CLI flags override the file for that run.

```json
{
  "provider": "macos",
  "macos": { "voice": "Samantha", "rate": 200 },
  "google": {
    "voice": "en-US-Neural2-D",
    "languageCode": "en-US",
    "audioEncoding": "LINEAR16",
    "sampleRateHertz": 24000
  },
  "elevenlabs": {
    "voiceId": "21m00Tcm4TlvDq8ikWAM",
    "modelId": "eleven_turbo_v2_5"
  },
  "playback": { "method": "afplay" },
  "textProcessor": { "minChunkLength": 10, "maxChunkLength": 500, "flushDelay": 1500 },
  "narrator": {
    "enabled": false,
    "provider": "gemini",
    "gemini": { "model": "gemini-2.5-flash" }
  }
}
```

## How It Works

1. A `Stop` hook in Claude Code fires after each response.
2. `claude-says` gets the new assistant text one of two ways: the daemon **watches the session transcript** directly (fsnotify + a safety poll) for the lowest latency, or the hook forwards it over a **Unix-domain socket** as a fallback.
3. The text processor splits it into sentences, strips markdown/code/URL noise, and (optionally) rephrases it through the narrator.
4. Each sentence is synthesized by the TTS provider and played in strict order by an **epoch-fenced audio queue**, so out-of-order synth results never play out of sequence — and switching sessions can never bleed the old session's audio into the new one.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## Project layout

```
cmd/claude-says/     CLI (Cobra) + the Stop-hook entry point
internal/config      ~/.claude-says/config.json (0600, atomic writes)
internal/logx        structured logging (slog: pretty on a TTY, JSON when piped)
internal/session     Claude Code session discovery under ~/.claude/projects
internal/transcript  transcript watcher (fsnotify + safety poll, UUID dedup)
internal/textproc    sentence chunking + markdown/URL/code cleaning
internal/audio       epoch-fenced ordered queue + afplay player
internal/ipc         Unix-domain-socket IPC (hook → daemon)
internal/tts         provider interface + macos / google / elevenlabs
internal/narrator    LLM narrator interface + gemini
internal/daemon      orchestrator (context-cancellable pipeline)
internal/tui         Bubble Tea TUI
```

## What changed from the Node version

The Go rewrite keeps the feature set but rebuilds the internals to fix bugs the Node daemon shipped with:

- **No cross-session audio bleed / no CPU-hang / no stranded sentences.** The audio queue is *epoch-fenced*: every session reset bumps a generation counter, and a single drain goroutine plays strictly in order. Stale in-flight results are dropped instead of overwriting the new session's slot — the root cause of the Node audio-queue bug cluster.
- **Bounded network calls.** Every Google/ElevenLabs/Gemini request has a context deadline; a hung provider can no longer stall the pipeline.
- **Failures are visible and survivable.** A provider error degrades a single sentence and is logged; it never kills the daemon.
- **A real TUI** — live spoken-text log with queue/epoch/playing counters — instead of the raw-mode keypress handler.
- **One binary.** No `node_modules`, no npm, no optional-dependency install dance.

## Extending

### Add a TTS provider

Implement the `Provider` interface in `internal/tts` (`Synthesize(ctx, text) ([]byte, format, error)` and `Validate(ctx) error`) and register it in `internal/tts/tts.go`.

### Add a narrator

Implement the `Narrator` interface in `internal/narrator` (`Narrate(ctx, text) string` — total, never errors) and register it in `internal/narrator/narrator.go`.

## Maintainers

- **Sudhanshu Singh** ([@Sudhanshu069](https://github.com/Sudhanshu069)) — maintainer of this Go rewrite
- **Abhishek Raj** ([@abhishek141001](https://github.com/abhishek141001)) — original author of the Node.js version

Issues and PRs welcome at [github.com/Sudhanshu069/claude-says](https://github.com/Sudhanshu069/claude-says).

## License

MIT
