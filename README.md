# claude-says

[![release](https://img.shields.io/github/v/release/Sudhanshu069/claude-says?logo=github&color=success)](https://github.com/Sudhanshu069/claude-says/releases/latest)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg?logo=go&logoColor=white)](https://go.dev)
[![license](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)
[![platform: macOS](https://img.shields.io/badge/platform-macOS-black.svg?logo=apple)](#requirements)
[![single binary](https://img.shields.io/badge/deploy-single%20binary-brightgreen.svg)](#install)

Stop staring at your terminal waiting for Claude Code to finish. **claude-says** reads Claude's output aloud in real-time so you can step away, stretch, or keep working — and just listen.

A single, dependency-free binary with a live TUI: watch (and hear) what Claude is building, mute or skip a sentence, and switch sessions on a keystroke.

> **macOS only** — playback uses `afplay` and the default voice uses the built-in `say` command.

## Highlights

- 🔊 **Real-time & local.** Speaks each of Claude's replies as it's written, using the macOS `say` voice — no API keys, no cloud, nothing leaves your machine.
- 🧹 **Clean by default.** Skips interstitial filler ("Let me check.", "Got it.") and back-to-back repeats so you hear the substance, not the chatter. `--verbatim` reads everything.
- 🎛️ **Live controls.** Pause, **mute**, **skip the current sentence**, or switch sessions — all from the TUI on a keystroke.
- ⚙️ **Remembers your setup.** `--voice`, `--rate`, and `--volume` are saved automatically, so a bare `claude-says start` reuses them.
- 🗣️ **Optional narrator.** Rephrase output into a spoken summary via Google Gemini (obvious secrets redacted before they leave) — or a fully **local ollama** model that never phones home.
- 📦 **One static binary.** No Node, no npm, no runtime dependencies.

> ### ⚙️ A Go rewrite
> This is a ground-up **Go port** of the original Node.js project (which was no longer maintained). It ships as one static binary — no `node_modules`, no npm — with a real Bubble Tea TUI and an audio pipeline rebuilt to fix a class of ordering/race bugs the Node daemon had. See [What changed from the Node version](#what-changed-from-the-node-version).

## Install

### One-liner (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/Sudhanshu069/claude-says/main/install.sh | sh
```

Detects your chip, downloads the latest macOS release, **verifies its SHA-256 checksum**, and installs `claude-says` — no `sudo` needed on Apple Silicon: it installs to `/usr/local/bin` when that's writable, otherwise to `~/.local/bin` (override either with `BINDIR=…`). Then run `claude-says start`.

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

Instead of reading Claude's output verbatim, narrator mode runs the text through an LLM that rephrases it into a short, spoken-friendly summary before it's voiced — less "reading markdown out loud," more "a colleague telling you what just happened." Two backends:

```bash
# Cloud (Google Gemini) — highest quality, sends text off-machine:
export GEMINI_API_KEY=your-key
claude-says start --narrator

# Local (ollama) — nothing leaves your machine:
claude-says start --narrator --narrator-provider ollama   # needs `ollama serve` + a model
```

> **Privacy — the cloud narrator sends text off-machine.** Narrator mode is **off by default**. With the default **gemini** backend, each new block of Claude's output is sent to Google's Gemini API to be rephrased. As a safety backstop, `claude-says` **redacts obvious secrets** (API keys, tokens, JWTs, private-key blocks, `password=…`/`token=…` assignments) to `[REDACTED]` before the request leaves — but redaction is best-effort, so treat enabling gemini as "session text goes to Google." The API key is read only from `GEMINI_API_KEY` and is never written to disk.
>
> For a narrator that **never** leaves the machine, use `--narrator-provider ollama` (talks to a local `ollama` server at `http://localhost:11434`; set `narrator.ollama.model`/`endpoint` in config to customize). The default macOS `say` path — narrator off — is fully local too.

## Commands

```bash
claude-says start              # Start the daemon + TUI (auto-detects most recent session)
claude-says start -l           # Pick a session interactively
claude-says start -s <id>      # Listen to a specific session
claude-says start --narrator   # Enable LLM narrator mode
claude-says start --voice "Daniel" --rate 150 --volume 0.8   # remembered next time
claude-says start --verbatim   # Hear everything, no filtering (see below)
claude-says voices             # List available macOS voices
claude-says --version
```

### Settings just stick

Preference flags — `--voice`, `--rate`, `--volume`, `--flush-delay`, `--min-chunk`, `--max-chunk` — are **remembered automatically**: set them once and a bare `claude-says start` uses them next time (it prints a one-line `Remembered …` confirmation and writes `~/.claude-says/config.json`). No `--save` flag.

Per-run and privacy flags are deliberately **not** persisted: `--narrator` (it sends text to Google — see below), `--verbatim`, `--no-dedupe`, `--skip`, `--session`, `--list`. You opt into those each run.

### Filtering what gets spoken

`claude-says` aims to sound clean out of the box, so it filters noise **by default** — you don't configure anything to get a good experience. Only the assistant's prose is ever voiced (tool calls are never read), code fences, markdown, URLs, and file paths are stripped, and on top of that:

- **Filler is trimmed.** Whole-sentence acknowledgements ("Got it.", "Makes sense.") and *short* action announcements ("Let me check.", "Now I'll do that.") are dropped. Substantive sentences are kept even when they open with "Let me" — e.g. *"Let me check the config for the timeout."* is spoken; the length is the guard.
- **Repeats are collapsed.** A sentence identical to the one just spoken is dropped.

If you'd rather hear more (or everything), opt out:

| Flag | Effect |
|------|--------|
| `--verbatim` | Turn **all** filtering off — speak the raw stream exactly as written (also ignores `--skip`). |
| `--no-dedupe` | Allow consecutive identical sentences. |
| `--skip <text>` | *Add* your own mute: drop any sentence containing `<text>` (case-insensitive, repeatable). |

Filtered sentences are dropped before they're queued, so nothing ever stalls or plays out of order. All of this persists in the config file (`textProcessor.dedupe`, `textProcessor.filterFiller`, `textProcessor.skip`).

## Controls (in the TUI)

| Key | Action |
|-----|--------|
| `p` / `space` | Pause / Resume (holds the queue; resume replays the current sentence) |
| `m` | Mute / Unmute (silences output and discards sentences while muted — no backlog) |
| `n` / `→` | Skip the current sentence and move to the next |
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

Then: `claude-says --voice <Tab>` cycles Daniel, Grandma, Karen, Samantha, … and `--narrator-provider <Tab>` offers `gemini` / `ollama`.

## Configuration

Settings live in `~/.claude-says/config.json` (owner-only, `0600`) and are merged over the defaults — you only need the keys you want to change. CLI flags override the file for that run.

```json
{
  "provider": "macos",
  "macos": { "voice": "Samantha", "rate": 200, "volume": 0 },
  "textProcessor": { "minChunkLength": 10, "maxChunkLength": 500, "flushDelay": 1500, "dedupe": true, "filterFiller": true, "skip": [] },
  "narrator": {
    "enabled": false,
    "provider": "gemini",
    "gemini": { "model": "gemini-2.5-flash" },
    "ollama": { "model": "llama3.2", "endpoint": "http://localhost:11434" }
  }
}
```

## How It Works

1. The daemon **watches the active Claude Code session transcript** directly (fsnotify + a safety poll) and picks up each new assistant text block.
2. The text processor splits it into sentences, strips markdown/code/URL noise, drops filler and duplicates (on by default), and (optionally) rephrases it through the narrator.
3. Each sentence is synthesized by macOS `say` and played in strict order by an **epoch-fenced audio queue**, so out-of-order synth results never play out of sequence — and switching sessions can never bleed the old session's audio into the new one.

See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) for the design.

## Project layout

```
cmd/claude-says/     CLI (Cobra): start [default] + voices + uninstall; preference flags auto-persist
internal/config      ~/.claude-says/config.json (0600, atomic writes)
internal/logx        structured logging (slog: pretty on a TTY, JSON when piped)
internal/session     Claude Code session discovery under ~/.claude/projects
internal/transcript  transcript watcher (fsnotify + safety poll, UUID dedup)
internal/textproc    sentence chunking + markdown/URL/code cleaning + content filters (dedupe/filler/skip)
internal/audio       epoch-fenced ordered queue (pause/mute/skip) + afplay player
internal/tts         provider interface + macOS `say`
internal/narrator    narrator interface + gemini (cloud, redacts secrets before egress) + ollama (local)
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
