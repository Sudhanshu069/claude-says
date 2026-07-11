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

Detects your chip, downloads the latest macOS release, **verifies its SHA-256 checksum**, and installs `claude-says` to `/usr/local/bin` (override with `BINDIR=…`). Then run `claude-says start`.

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
claude-says uninstall            # removes ~/.claude-says (pass --keep-config to keep it)
rm "$(command -v claude-says)"   # then remove the binary
```

claude-says stores only `~/.claude-says/config.json` — it installs nothing into Claude Code, so `uninstall` just clears that config (and prints the binary path to delete).

## Quick Start

```bash
# 1. Start in one terminal — opens the TUI (auto-detects your most recent session)
claude-says start

# 2. Use Claude Code in another terminal as normal
claude
```

That's it. When Claude responds, you'll hear it spoken aloud and see it scroll in the TUI.

## Why?

- Claude Code runs can take minutes — you shouldn't have to watch text scroll the whole time.
- You might miss when Claude asks for input or confirmation.
- Sometimes you just want to code from the couch.

## Requirements

- **macOS** (uses `afplay` for playback, `say` for the voice)
- **Go 1.26+** to build (the resulting binary has no runtime deps)
- **Claude Code CLI** installed (claude-says follows its session transcripts)

## Voice

Speech uses the built-in macOS `say` command — no API keys, no cloud, no setup.

```bash
claude-says voices                    # List English voices
claude-says voices --all              # List all voices
claude-says start --voice "Daniel"    # Use a specific voice
claude-says start --rate 150          # Slower (words per minute, default 200)
claude-says start --voice "Karen" --rate 150
```

## Narrator mode (optional)

Instead of reading Claude's output verbatim, narrator mode runs the text through an LLM that rephrases it into a short, spoken-friendly summary before it's voiced — less "reading markdown out loud," more "a colleague telling you what just happened." Powered by Google Gemini.

```bash
export GEMINI_API_KEY=your-key
claude-says start --narrator
```

> **Privacy — data leaves your machine.** Narrator mode is **off by default**. When you enable it, each new block of Claude's output is sent to Google's Gemini API (`generativelanguage.googleapis.com`) to be rephrased. The prompt asks Gemini to skip code and file paths, but the **raw text is transmitted in full** before any filtering — so it can include source code, file contents, secrets, or other sensitive material from your session. The API key is read only from `GEMINI_API_KEY` and is never written to disk. Leave `--narrator` off if you don't want session text sent to a third party. Everything else (the default macOS `say` path) runs entirely locally.

## Commands

```bash
claude-says start              # Start the daemon + TUI (auto-detects most recent session)
claude-says start -l           # Pick a session interactively
claude-says start -s <id>      # Listen to a specific session
claude-says start --narrator   # Enable LLM narrator mode
claude-says start --voice "Daniel"
claude-says start --rate 150
claude-says start --skip "let me" --skip "now i" --dedupe   # Quiet the chatter (see below)
claude-says voices             # List available macOS voices
claude-says --version
```

### Filtering what gets spoken

Only the assistant's prose is voiced (tool calls are never read), and code fences, markdown, URLs, and file paths are already stripped. Two flags trim the rest of the noise:

| Flag | Effect |
|------|--------|
| `--skip <text>` | Mute any sentence containing `<text>` (case-insensitive). Repeatable — e.g. `--skip "let me" --skip "now i'll"` silences the interstitial "Let me check…" / "Now I'll…" filler between tool calls. |
| `--dedupe` | Collapse a sentence that's identical to the one just spoken (consecutive only). |

Filtered sentences are dropped before they're queued, so nothing stalls or plays out of order. Both also persist in the config file (`textProcessor.skip`, `textProcessor.dedupe`).

## Controls (in the TUI)

| Key | Action |
|-----|--------|
| `p` | Pause / Resume |
| `s` | Switch session (shows each session by name) |
| `q` | Quit (drains the current sentence, then exits) |

## Shell completion

Tab-complete `--voice` (cycles the English macOS voices):

```bash
# zsh — write the completion into your fpath, then restart the shell:
mkdir -p ~/.zfunc && claude-says completion zsh > ~/.zfunc/_claude-says
# ensure ~/.zfunc is on fpath before compinit in ~/.zshrc:
#   fpath=(~/.zfunc $fpath); autoload -U compinit && compinit

# bash:
claude-says completion bash | sudo tee /usr/local/etc/bash_completion.d/claude-says >/dev/null

# fish:
claude-says completion fish > ~/.config/fish/completions/claude-says.fish
```

Then: `claude-says --voice <Tab>` cycles Daniel, Grandma, Karen, Samantha, …

## Configuration

Settings live in `~/.claude-says/config.json` (owner-only, `0600`) and are merged over the defaults — you only need the keys you want to change. CLI flags override the file for that run.

```json
{
  "provider": "macos",
  "macos": { "voice": "Samantha", "rate": 200 },
  "textProcessor": { "minChunkLength": 10, "maxChunkLength": 500, "flushDelay": 1500, "skip": [], "dedupe": false },
  "narrator": {
    "enabled": false,
    "provider": "gemini",
    "gemini": { "model": "gemini-2.5-flash" }
  }
}
```

## How It Works

1. The daemon **watches the active Claude Code session transcript** directly (fsnotify + a safety poll) and picks up each new assistant text block.
2. The text processor splits it into sentences and strips markdown/code/URL noise, and (optionally) rephrases it through the narrator.
3. Each sentence is synthesized by macOS `say` and played in strict order by an **epoch-fenced audio queue**, so out-of-order synth results never play out of sequence — and switching sessions can never bleed the old session's audio into the new one.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## Project layout

```
cmd/claude-says/     CLI (Cobra): start [default] + voices
internal/config      ~/.claude-says/config.json (0600, atomic writes)
internal/logx        structured logging (slog: pretty on a TTY, JSON when piped)
internal/session     Claude Code session discovery under ~/.claude/projects
internal/transcript  transcript watcher (fsnotify + safety poll, UUID dedup)
internal/textproc    sentence chunking + markdown/URL/code cleaning
internal/audio       epoch-fenced ordered queue + afplay player
internal/tts         provider interface + macOS `say`
internal/narrator    LLM narrator interface + gemini
internal/daemon      orchestrator (context-cancellable pipeline)
internal/tui         Bubble Tea TUI
```

## What changed from the Node version

The Go rewrite rebuilds the internals to fix bugs the Node daemon shipped with:

- **No cross-session audio bleed / no CPU-hang / no stranded sentences.** The audio queue is *epoch-fenced*: every session reset bumps a generation counter, and a single drain goroutine plays strictly in order. Stale in-flight results are dropped instead of overwriting the new session's slot — the root cause of the Node audio-queue bug cluster.
- **Bounded calls.** Every synth and narrator request has a context deadline; a hung call can no longer stall the pipeline.
- **Failures are visible and survivable.** A synth error degrades a single sentence and is logged; it never kills the daemon.
- **A real TUI** — live spoken-text log with queue/epoch/playing counters — instead of the raw-mode keypress handler.
- **One binary.** No `node_modules`, no npm, no optional-dependency install dance.

## Extending

### Add a TTS provider

Implement the `Provider` interface in `internal/tts` (`Synthesize(ctx, text) ([]byte, format, error)` and `Validate(ctx) error`) and return it from `New` in `internal/tts/tts.go`.

### Add a narrator

Implement the `Narrator` interface in `internal/narrator` (`Narrate(ctx, text) string` — total, never errors) and register it in `internal/narrator/narrator.go`.

## Maintainers

- **Sudhanshu Singh** ([@Sudhanshu069](https://github.com/Sudhanshu069)) — maintainer of this Go rewrite
- **Abhishek Raj** ([@abhishek141001](https://github.com/abhishek141001)) — original author of the Node.js version

Issues and PRs welcome at [github.com/Sudhanshu069/claude-says](https://github.com/Sudhanshu069/claude-says).

## License

MIT
