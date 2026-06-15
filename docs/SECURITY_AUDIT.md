# Security Audit — claude-says (claude-code-speak)

**Scope:** all runtime code under [src/](../src/) and [bin/](../bin/), plus the
dependency manifest. **Method:** parallel review of seven attack surfaces
(command execution, IPC, temp files, secrets/privacy, untrusted input, written
config, supply chain), with every candidate finding independently re-checked by
three adversarial verifiers (exploitability / threat-model fit / false-positive).
Only findings confirmed against the actual code are listed below.

## Threat model

This is a **local, single-user macOS developer tool** running as a background
daemon for the logged-in user. The realistic adversaries are:

- **(a) Other local processes / users** on a shared or multi-user machine —
  predictable `/tmp` paths, an unauthenticated socket, world-readable artifacts.
- **(b) Attacker-influenced transcript content** — could it reach a shell, exhaust
  resources, or be exfiltrated?
- **(c) Secrets & privacy** — API keys and the user's assistant output leaving the
  machine to third-party APIs.
- **(d) Integrity of files the tool writes** — `~/.claude/settings.json` and the
  hook command.

A remote network attacker is **out of scope** (no inbound network listener; all
sockets are local Unix domain sockets and all HTTP is outbound over TLS).

## Findings at a glance

| # | Severity | Finding | Location |
|---|---|---|---|
| S1 | **High** | Unauthenticated, world-accessible IPC socket — any local process can inject speech | [config.js:7](../src/config.js#L7), [ipc.js:15-50](../src/ipc.js#L15) |
| S2 | **High** | Unbounded IPC receive buffer → local memory-exhaustion DoS | [ipc.js:21-26](../src/ipc.js#L21) |
| S3 | **High** | Privacy: transcript text exfiltrated to external LLM/TTS APIs without explicit consent | [gemini.js:21-47](../src/narrators/gemini.js#L21), [elevenlabs.js](../src/providers/elevenlabs.js), [google.js](../src/providers/google.js) |
| S4 | **Medium** | Predictable, world-readable temp audio in `/tmp` (TOCTOU + content disclosure) | [player.js:6,12,24](../src/player.js#L24), [macos.js:19](../src/providers/macos.js#L19) |
| S5 | **Medium** | World-readable hook state/log dirs leak session activity; unsanitized `session_id` in path | [claude-says-hook.js:15,35-39,73](../bin/claude-says-hook.js#L35) |
| S6 | **Medium** | No IPC message size/schema validation — oversized text forwarded to paid cloud APIs | [ipc.js:28-37](../src/ipc.js#L28) |
| S7 | **Medium** | Setup reports success / saves config even when hook install fails; non-atomic `settings.json` write | [setup.js:64-81,103,133](../src/setup.js#L64) |
| S8 | **Low** | Hook command resolves `node` via `PATH` (PATH-hijack → code exec, defense-in-depth) | [setup.js:104](../src/setup.js#L104) |
| S9 | **Low** | Gemini API key placed in URL query string | [gemini.js:69](../src/narrators/gemini.js#L69) |
| S10 | **Low** | ElevenLabs error response body surfaced into daemon console logs | [elevenlabs.js:86](../src/providers/elevenlabs.js#L86) |
| S11 | **Low** | Hook trusts `transcript_path` / stdin JSON (arbitrary file read, no size cap) | [claude-says-hook.js:18-27,46](../bin/claude-says-hook.js#L18) |
| S12 | **Low** | Optional `@google-cloud/text-to-speech` adds ~106-package supply-chain surface | [package.json](../package.json), [package-lock.json](../package-lock.json) |
| S13 | **Info** | API keys read from environment variables (visible in process env) | [gemini.js:16](../src/narrators/gemini.js#L16), [elevenlabs.js:7](../src/providers/elevenlabs.js#L7) |

---

## Remediation status

The code-level findings have been fixed in the working tree. Each change was
chosen to preserve behavior; all were verified with `node --check` and a
functional smoke test (socket round-trip, macOS synthesis, hook accept/reject).

| # | Status | What changed |
|---|---|---|
| S1 | ✅ Fixed (hardened in `harden-and-fixes`) | Initially `chmodSync(SOCKET_PATH, 0o600)` after `listen()`. The hardening branch goes further: the socket is **relocated out of `/tmp` into `~/.claude-says/`** (owner-owned `0700` dir) and the bind/cleanup uses `lstat` so it only ever unlinks/binds a real socket and never follows a planted symlink — closing the residual `/tmp` TOCTOU and startup-DoS vectors. [config.js](../src/config.js), [ipc.js](../src/ipc.js) |
| S2 | ✅ Fixed | 1 MB per-connection receive cap; oversized stream `destroy()`s the socket. [ipc.js](../src/ipc.js) |
| S3 | ⚠️ Partial / by-config | Default is the on-device `macos` provider with narrator off, so nothing leaves the machine by default. Explicit consent UX for cloud modes is still a doc/UX TODO. |
| S4 | ✅ Fixed | Random `randomUUID()` filenames; dirs `mode 0o700`, files `mode 0o600`. Also fixes the same-millisecond collision bug (B5). [player.js](../src/player.js), [macos.js](../src/providers/macos.js) |
| S5 | ✅ Fixed | State dir `mode 0o700`; `session_id` sanitized (`[^A-Za-z0-9-]→_`); `transcript_path` confined to `~/.claude`; debug logs `mode 0o600`. [claude-says-hook.js](../bin/claude-says-hook.js), [debug-hook.js](../bin/debug-hook.js) |
| S6 | ✅ Fixed | IPC `message` handler validates shape and caps text at 100 KB. [daemon.js](../src/daemon.js) |
| S7 | ✅ Fixed | Atomic `temp+rename` settings write; `Array.isArray` guard on `hooks.Stop`; setup now returns/reports honestly. [setup.js](../src/setup.js) |
| S8 | ✅ Fixed | Hook command uses absolute `process.execPath` (quoted) instead of bare `node`. [setup.js](../src/setup.js) |
| S9 | ✅ Fixed | Gemini key moved from URL to the `x-goog-api-key` header. [gemini.js](../src/narrators/gemini.js) |
| S10 | ✅ Fixed | API error responses no longer fold the body into the error/log. [elevenlabs.js](../src/providers/elevenlabs.js), [gemini.js](../src/narrators/gemini.js) |
| S11 | ✅ Fixed | `transcript_path` confined to `~/.claude`; stdin capped at 10 MB. [claude-says-hook.js](../bin/claude-says-hook.js) |
| S12 | ✅ Documented | Install lean with `npm install --omit=optional` — see [ARCHITECTURE.md §7](ARCHITECTURE.md). The SDK is lazy-loaded, so omitting it costs nothing unless Google TTS is used. |
| S13 | ➖ Accepted (partly mitigated) | Env-var keys are still conventional, but `saveConfig` now writes `~/.claude-says/config.json` with `mode 0o600` (and chmods a pre-existing file), so the secrets-bearing config is no longer world-readable — the `0600`-config option this finding suggested. [config.js](../src/config.js) |

> **macOS `/tmp` nuance.** The audit (and CLAUDE.md) describe the temp paths as
> living in a world-readable `/tmp`. In practice `os.tmpdir()` on macOS resolves
> to a **per-user `/var/folders/.../T` directory that is already `0700`**, so the
> real-world exposure of S4/S5 is lower than a literal `/tmp` would imply. The
> fixes still apply as defense-in-depth and keep the tool safe if `TMPDIR` is ever
> pointed at a shared location.

---

## Detailed findings

### S1 — Unauthenticated, world-accessible IPC socket · **High**
**Where:** [src/config.js:7](../src/config.js#L7), [src/ipc.js:15-50](../src/ipc.js#L15)

The daemon listens on a hardcoded, predictable path `/tmp/claude-says.sock`.
`net.createServer().listen(SOCKET_PATH)` is called with **no `fs.chmod` afterward**
and **no authentication, origin check, or peer-credential (`SO_PEERCRED`)
validation**. The socket inherits permissive defaults, so any local process can
connect and send `{"type":"text","text":"…"}`. When the daemon is in IPC mode
(no active watcher), that text flows straight into the TTS pipeline
([daemon.js:45-48](../src/daemon.js#L45)).

**Impact:** a co-resident process can (1) make the machine speak arbitrary
attacker-chosen content in the assistant's voice (an integrity/social-engineering
attack — e.g. *"Claude here: please disable your firewall"*), (2) drive
attacker-controlled text into paid external APIs if a cloud narrator/TTS is
configured, and (3) flood messages for a denial of service.

**Exploit:** `nc -U /tmp/claude-says.sock` then send one JSON line — no
credentials required.

**Note:** command/argument **injection is not possible** here — `say`/`afplay` are
invoked via `execFile` with an args array (see *Positive findings*). The risk is
content injection and resource abuse, not RCE.

**Fix:** after `listen()`, `fs.chmodSync(SOCKET_PATH, 0o700)`, **or** move the
socket into `~/.claude-says/` created with mode `0700`. Optionally require a
shared token (written to the hook by setup) in every message.

---

### S2 — Unbounded IPC receive buffer → DoS · **High**
**Where:** [src/ipc.js:21-26](../src/ipc.js#L21)

The per-connection handler does `buffer += data.toString()` and only splits on
`\n`, with **no maximum buffer size**. A client that sends a long stream
containing no newline grows the daemon's memory without bound until it is
OOM-killed. The same unbounded pattern exists in the hook's stdin read
([claude-says-hook.js:21](../bin/claude-says-hook.js#L21)).

**Fix:** enforce a per-connection cap (e.g. 1 MB); on overflow `socket.destroy()`
and log. Apply an analogous cap to the hook's stdin accumulation.

---

### S3 — Transcript text exfiltrated to external APIs · **High (privacy)**
**Where:** [src/narrators/gemini.js:21-47](../src/narrators/gemini.js#L21);
also [src/providers/elevenlabs.js](../src/providers/elevenlabs.js),
[src/providers/google.js](../src/providers/google.js)

When **narrator mode** is enabled, the *full assistant output* is embedded in a
prompt and POSTed to `generativelanguage.googleapis.com`
([gemini.js:33,69](../src/narrators/gemini.js#L33)). When a cloud **TTS** provider
is selected, the spoken text is sent to Google Cloud or ElevenLabs. Client-side
filtering removes code fences and URLs ([text-processor.js](../src/text-processor.js)),
but substantial assistant reasoning still leaves the machine. There is **no
explicit consent step** — narrator mode is a flag, and cloud TTS is a config
value.

**Impact:** assistant output (which can include proprietary code discussion,
internal paths, or secrets printed to the console) is transmitted to — and may be
logged/retained by — Google or ElevenLabs.

**Fix:** require explicit opt-in with a clear warning when enabling narrator mode
or a cloud TTS provider; document the data flow in the README; keep the default
**`macos` provider with narrator off**, which keeps all content on-device.

---

### S4 — Predictable, world-readable temp audio · **Medium**
**Where:** [src/player.js:6,12,24](../src/player.js#L24),
[src/providers/macos.js:19](../src/providers/macos.js#L19)

Temp directories are created with `mkdirSync(dir, { recursive: true })` and **no
`mode`**, so they default to `0755`. Audio files are named with a millisecond
timestamp (`chunk-${Date.now()}`, `claude-says-${Date.now()}.aiff`) — predictable
and enumerable — and written with default permissions. These files contain the
**synthesized audio of the user's assistant output**. There is also a small TOCTOU
window between `writeFileSync` and `execFile('afplay', …)` /
`execFile('say', …)` during which another local user could swap the file or a
symlink.

**Impact:** on a shared host, another user can read or race the synthesized audio
of private session content.

**Fix:** create dirs `mode: 0o700` and files `0o600` (e.g. `openSync(path,'w',0o600)`);
use `crypto.randomUUID()`/`randomBytes` instead of `Date.now()`; or relocate under
`~/.claude-says/`. Best: pipe the buffer to `afplay` via stdin and avoid the disk
write entirely.

---

### S5 — World-readable hook state/logs; unsanitized `session_id` · **Medium**
**Where:** [bin/claude-says-hook.js:15-16,35-39,73](../bin/claude-says-hook.js#L35)

`/tmp/claude-says-state/` (per-session byte offsets, filenames are session UUIDs)
and `/tmp/claude-says-hook.log` are created with default `0755`/`0644`. The
filenames reveal which sessions are active and when. Separately, `session_id`
comes from the hook's stdin JSON and is concatenated directly into the state-file
path (`join(STATE_DIR, ` `${sessionId}.offset` `)`) with **no UUID validation**, so
a `../`-bearing value would traverse out of `STATE_DIR`.

**Impact:** information disclosure about user activity; path-traversal write **iff**
the hook input is attacker-controlled (in normal use it comes from trusted Claude
Code, so this is defense-in-depth).

**Fix:** create the dir `mode: 0o700`; validate `session_id` with the existing
`isUUID` regex ([sessions.js:76](../src/sessions.js#L76)) before using it in a path;
move state under `~/.claude-says/`.

---

### S6 — No IPC message size/schema validation · **Medium**
**Where:** [src/ipc.js:28-37](../src/ipc.js#L28)

A message only has to `JSON.parse` successfully; there is no schema or length
check. The `text` field is passed unbounded to the narrator and TTS providers,
so a single crafted message can push a very large payload to a **paid** cloud API.

**Fix:** validate `type`/shape and cap `text` length before emitting.

---

### S7 — Setup over-reports success; non-atomic settings write · **Medium**
**Where:** [src/setup.js:64-81](../src/setup.js#L64), [:103](../src/setup.js#L103), [:133](../src/setup.js#L133)

If `installHook()` fails, the wizard prints "you may need to add it manually" but
**continues**, saves config, and **returns `true`** — callers can't tell a partial
setup from a full one. `settings.hooks.Stop` is assumed to be an array
([:103](../src/setup.js#L103)); a non-array value makes `.some()` throw (caught,
but setup silently fails). `writeFileSync(settings.json)` ([:133](../src/setup.js#L133))
is **non-atomic**, so an interrupted write can corrupt the user's
`~/.claude/settings.json`.

**Fix:** return a truthful success flag; guard with `Array.isArray`; write to a
temp file and `renameSync` for atomicity.

---

### S8 — Hook command relies on `PATH` for `node` · **Low (defense-in-depth)**
**Where:** [src/setup.js:104](../src/setup.js#L104)

The installed hook command is `node <abs-path-to-script>`. The script path is
absolute, but `node` is resolved via `PATH` at hook-execution time. An attacker
who can prepend a directory to the user's `PATH` could shadow `node` and gain code
execution when the Stop hook fires. (This is the standard Claude Code hook
pattern; the prerequisite — controlling `PATH` — already implies significant
access.)

**Fix:** record the absolute `process.execPath` at setup time and use it in the
hook command, or rely on the script's shebang with an absolute interpreter.

---

### S9 — Gemini API key in URL query string · **Low**
**Where:** [src/narrators/gemini.js:69](../src/narrators/gemini.js#L69)

`?key=${this.apiKey}` puts the secret in the request path. Although Google
documents this form, keys-in-URLs are more prone to ending up in proxy/access
logs than header-borne credentials.

**Fix:** send the key in the `x-goog-api-key` header instead.

---

### S10 — API error body surfaced into logs · **Low**
**Where:** [src/providers/elevenlabs.js:86](../src/providers/elevenlabs.js#L86)

ElevenLabs error responses are embedded into the thrown `Error` message and
propagate to the daemon console ([daemon.js:57](../src/daemon.js#L57)). (The
equivalent Gemini error at [gemini.js:82](../src/narrators/gemini.js#L82) is in
practice **swallowed** — `narrate()` catches and falls back to raw text
([gemini.js:45](../src/narrators/gemini.js#L45)) — so it does not reach logs at
runtime.)

**Fix:** log only the HTTP status code; keep full bodies out of console output.

---

### S11 — Hook trusts `transcript_path` / stdin JSON · **Low**
**Where:** [bin/claude-says-hook.js:18-27,46](../bin/claude-says-hook.js#L18)

`transcript_path` from stdin is read with no validation, so whatever path is
supplied is read, parsed as JSONL, and any "assistant text" within is forwarded
to the daemon (and possibly to cloud APIs). stdin is accumulated without a size
cap. In normal operation this input comes from trusted Claude Code, so it is
**not directly exploitable**, but it is unvalidated.

**Fix:** constrain `transcript_path` to `~/.claude/projects/`; cap stdin size.

---

### S12 — Optional heavyweight dependency = supply-chain surface · **Low**
**Where:** [package.json](../package.json), [package-lock.json](../package-lock.json)

`@google-cloud/text-to-speech` is declared `optional`, but `npm install` installs
optional deps by default, pulling **~106 transitive packages** (gRPC, protobufjs,
google-auth-library, gaxios, node-fetch, yargs, …). Each is added attack surface
for a feature most users (default `macos` provider) never use.

**Fix:** document `npm install --omit=optional`; the SDK is already lazy-loaded at
runtime, so omitting it costs nothing unless Google TTS is selected. Run
`npm audit` periodically.

---

### S13 — API keys in environment variables · **Info**
**Where:** [src/narrators/gemini.js:16](../src/narrators/gemini.js#L16),
[src/providers/elevenlabs.js:7](../src/providers/elevenlabs.js#L7)

`GEMINI_API_KEY` / `ELEVENLABS_API_KEY` are read from `process.env`. This is
conventional, but env vars are visible to the process tree and may appear in shell
history or crash dumps.

**Fix (optional):** document using the macOS Keychain, or a `0600` config file,
for key storage.

---

## Positive findings (verified safe)

- **No shell injection.** `say` and `afplay` are launched with
  `child_process.execFile` and an **args array**, so the synthesized text and
  config-derived voice/rate are passed as discrete argv entries with no shell
  interpretation ([macos.js:22-27](../src/providers/macos.js#L22),
  [player.js:29](../src/player.js#L29)). Confirmed not exploitable.
  > **Correction (`harden-and-fixes`).** This was over-stated: while *shell*
  > injection is impossible, `say` **argument/flag injection (CWE-88)** was
  > possible — text beginning with `-` (e.g. `-f`/`-o`) was parsed by `say` as an
  > option rather than spoken. The branch adds a `--` end-of-options marker before
  > the text so attacker-influenced content is always spoken literally.
  > `afplay` is unaffected (its only argv is a `randomUUID` temp path).
- **No inbound network.** All listeners are local Unix sockets; all third-party
  traffic is outbound HTTPS.
- **Heavy SDK is lazy.** `@google-cloud/text-to-speech` loads via `await import`
  only when the Google provider runs ([google.js:11](../src/providers/google.js#L11)).

## Investigated and dismissed

- **Stale-socket TOCTOU** at [ipc.js:15-16](../src/ipc.js#L15) — daemon-startup
  only, in the user's own `/tmp`; negligible.
- **ReDoS** in `_cleanForSpeech` regexes ([text-processor.js:110-117](../src/text-processor.js#L110))
  — the patterns are not nested-quantifier catastrophic; low risk.
- **Config not encrypted** ([config.js:50-55](../src/config.js#L50)) — the file
  lives in `~` with normal permissions and is not designed to hold secrets.

## Prioritized remediation

1. **Lock down the socket** (S1) and **cap the IPC buffer** (S2) — both are small,
   high-value changes in [ipc.js](../src/ipc.js).
2. **Add explicit consent + docs** for cloud narrator/TTS (S3).
3. **Tighten `/tmp` usage** — `0700` dirs, `0600` files, random names, or move to
   `~/.claude-says/` (S4, S5).
4. **Make setup honest and atomic** (S7); validate `session_id` (S5).
5. Header-borne API key (S9), log hygiene (S10), `--omit=optional` guidance (S12).
