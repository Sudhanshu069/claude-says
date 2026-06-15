# Code Review & Bug Report — claude-says (claude-code-speak)

**Method:** every module was reviewed for correctness, robustness, and quality;
each candidate bug was then independently re-checked by two adversarial verifiers
(one trying to *reproduce* it, one trying to *refute* it) against the actual code.
Findings below are the ones that survived verification. A short list of
**rejected false positives** is included at the end to document the rigor.

Severity is about runtime impact in this app, not abstract risk:
**High** = wrong/garbled output or a stuck pipeline a user will hit;
**Medium** = misbehaves in edge cases or degrades gracefully;
**Low** = quality/maintainability.

> **Remediation status (2026-06-16).** Many High/Medium items were already fixed on
> `main` via the security-hardening and rebrand PRs (B1, B3, B5–B9, B13, B18–B20,
> B24, B25). The `harden-and-fixes` branch additionally resolves **B4** (deep config
> merge), **B10** (watcher `stopped` flag kills the session-switch race), **B14**
> (`tts.js` default aligned to `macos`), **B16** (`stop()` now drains the final
> sentence instead of discarding it), and **B17** (session-switch tears the watcher
> down so the hook fallback re-enables, and an `fs.watch` error falls back to the
> poll instead of crashing the daemon) — plus two issues found during that work but
> not in the original list: the fenced-code filter no longer sticks "in code block"
> forever after a single stray fence, and `_cleanForSpeech` no longer mangles
> `snake_case` identifiers or arithmetic like `2 * 2`. **Still open:** B2, B11, B12,
> B15, B21–B23, and the `aiff`→`.wav` extension half of B14.

## Summary

| # | Sev | Type | Bug | Location |
|---|---|---|---|---|
| B1 | **High** | bug | Code-block filter is a per-chunk toggle count → code gets spoken | [text-processor.js:19-27](../src/text-processor.js#L19) |
| B2 | **High** | bug | Sentence splitter breaks on abbreviations & version numbers (`e.g.`, `1.2.3`) | [text-processor.js:69](../src/text-processor.js#L69) |
| B3 | **High** | bug | Pausing mid-sentence permanently drops the rest of that sentence | [audio-queue.js:99-107](../src/audio-queue.js#L99), [player.js:36](../src/player.js#L36) |
| B4 | **High** | bug | Shallow config merge wipes sibling nested defaults | [config.js:44](../src/config.js#L44) |
| B5 | **High** | bug | macOS temp filename collides when two synths share a millisecond | [macos.js:19](../src/providers/macos.js#L19) |
| B6 | **High** | bug | Synchronous `readFileSync` blocks the event loop inside async `synthesize` | [macos.js:33-34](../src/providers/macos.js#L33) |
| B7 | **High** | bug | Hook commits its offset before (and regardless of) successful delivery → silent text loss | [claude-says-hook.js:73-84](../bin/claude-says-hook.js#L73), [ipc.js:83-92](../src/ipc.js#L83) |
| B8 | **High** | bug | Hook offset past EOF after truncation/rotation → all new text skipped | [claude-says-hook.js:42,46-47](../bin/claude-says-hook.js#L42) |
| B9 | **High** | bug | Hook reads to EOF & commits offset even on an incomplete final line → message lost | [claude-says-hook.js:47,55,73](../bin/claude-says-hook.js#L47) |
| B10 | **High** | bug | Daemon session-switch race: stale watcher poll speaks old session's text as the new one | [daemon.js:85-127](../src/daemon.js#L85) |
| B11 | **High** | robustness | ElevenLabs buffers the entire audio response in memory | [elevenlabs.js:90-92](../src/providers/elevenlabs.js#L90) |
| B12 | **High*** | latent | AudioQueue has no gap recovery — a missing `seq` stalls playback forever | [audio-queue.js:59-62](../src/audio-queue.js#L59) |
| B13 | **Med** | bug | TTY raw mode never restored on exit → terminal left broken | [claude-says.js:127](../bin/claude-says.js#L127) |
| B14 | **Med** | correctness | macOS returns `aiff` but player writes `.wav`; `tts.js` default `google` ≠ config default `macos` | [player.js:23](../src/player.js#L23), [tts.js:12](../src/tts.js#L12) |
| B15 | **Med** | bug | `100ms` IPC send timeout + immediate `resolve` can drop large messages before flush | [ipc.js:75-94](../src/ipc.js#L75) |
| B16 | **Med** | correctness | `stop()` doesn't await playback; `flush()` then `clear()` discards the final sentence | [daemon.js:164-170](../src/daemon.js#L164) |
| B17 | **Med** | quality | Dual watcher+IPC path keyed on watcher *existence*, not liveness → text lost if watcher dies | [daemon.js:44-49](../src/daemon.js#L44) |
| B18 | **Med** | correctness | `decodeProjectDir` turns every `-` into `/` → wrong display name for dashed dirs | [sessions.js:81](../src/sessions.js#L81) |
| B19 | **Med** | robustness | Transcript read corrupts UTF-8 multibyte chars split at the byte boundary | [transcript-watcher.js:54,60](../src/transcript-watcher.js#L54) |
| B20 | **Med** | robustness | Watcher never resets offset on truncation/rotation (`size <= offset` returns) | [transcript-watcher.js:46](../src/transcript-watcher.js#L46) |
| B21 | **Med** | quality | Force-flush word-break can still split mid-word on long spaceless runs | [text-processor.js:86-95](../src/text-processor.js#L86) |
| B22 | **Low** | quality | `enqueue()` promise chain is fire-and-forget; `_drain()` errors swallowed | [audio-queue.js:33-52](../src/audio-queue.js#L33) |
| B23 | **Low** | quality | Socket `error`/`close` handlers swallow errors; possible double-delete | [ipc.js:40-46](../src/ipc.js#L40) |
| B24 | **Low** | quality | `say` temp-file cleanup `catch {}` hides leaks | [macos.js:35](../src/providers/macos.js#L35) |
| B25 | **Low** | quality | `'s'` switch-session control is a non-functional placeholder | [claude-says.js:150-167](../bin/claude-says.js#L150) |

`*` B12 is high-impact *if* triggered but is not reachable in the current wiring — see its entry.

---

## High-impact bugs

### B1 — Code-block filtering is a per-chunk toggle count
[src/text-processor.js:19-27](../src/text-processor.js#L19)

`feed()` counts every ` ``` ` in the *current chunk* and flips `inCodeBlock` once
per occurrence, then decides whether to skip based on the **final** state:

```js
const fenceMatches = text.match(/```/g);
if (fenceMatches) for (const _ of fenceMatches) this.inCodeBlock = !this.inCodeBlock;
if (this.inCodeBlock) return;          // ← whole chunk kept or dropped
this.buffer += text;
```

- A chunk containing a **complete** fenced block has an even number of fences, so
  `inCodeBlock` ends `false` and the **entire chunk — including the code — is
  buffered and spoken.**
- Across chunks, the opening fence in chunk A and closing fence in chunk B leak the
  code text that precedes the closing fence in B.
- A literal ` ``` ` inside single-backtick inline code wrongly toggles the state.

**Fix:** walk the buffer in order, slicing on fence boundaries and emitting only
the non-code spans, instead of counting toggles per chunk.

### B2 — Sentence splitter breaks on abbreviations / versions
[src/text-processor.js:69](../src/text-processor.js#L69)

`/([.!?])\s+/g` splits after any `.`/`!`/`?` + space. "Use `e.g.` the helper."
splits into "Use e" / "g. the helper"; "Version 1. 2. 3 shipped" fractures. Result
is choppy, mid-word speech.

**Fix:** only split when the terminator is followed by a capital
(`/([.!?])\s+(?=[A-Z])/g`) and/or keep an abbreviation deny-list.

### B3 — Pause drops the rest of the current sentence
[src/audio-queue.js:99-107](../src/audio-queue.js#L99) + [src/player.js:36-37](../src/player.js#L36)

`pause()` calls `player.stop()`, which `kill()`s `afplay`. The player's callback
treats a killed process as success (`error.killed → resolve()`), so the
`await this.player.play()` in `_drain()` resolves normally; the queue then marks
the entry `done` and increments `nextToPlay`
([audio-queue.js:83-85](../src/audio-queue.js#L83)). On `resume()` it continues
from the **next** sentence — the interrupted one is gone, never replayed.

**Fix:** on pause, remember the current `seq` and its audio; on resume, replay
that entry from the start instead of advancing past it.

### B4 — Shallow config merge wipes nested defaults
[src/config.js:44](../src/config.js#L44)

```js
return { ...DEFAULT_CONFIG, ...saved };
```

Saving one nested key (e.g. `{ google: { voice: 'X' } }`) replaces the **whole**
`google` object, dropping `languageCode`, `audioEncoding`, `sampleRateHertz`.
Providers then read `undefined` and silently fall back to hardcoded literals,
diverging from the user's intent.

**Fix:** deep-merge saved config over defaults.

### B5 — macOS temp filename collision
[src/providers/macos.js:19](../src/providers/macos.js#L19)

`claude-says-${Date.now()}.aiff` collides when two `synthesize()` calls land in
the same millisecond. Because the daemon fires synthesis without awaiting
([daemon.js:41](../src/daemon.js#L41)), concurrent calls are normal under fast
streaming — two `say` processes then write/read the same path, corrupting audio.

**Fix:** add `crypto.randomBytes`/`randomUUID` entropy (also see
[SECURITY_AUDIT.md](SECURITY_AUDIT.md) S4).

### B6 — Sync `readFileSync` inside async `synthesize`
[src/providers/macos.js:33-34](../src/providers/macos.js#L33)

The default provider reads the rendered `.aiff` with `readFileSync`, blocking the
single event loop while the file loads. Under rapid synthesis this stutters the
whole daemon. (The player's `writeFileSync` at [player.js:26](../src/player.js#L26)
is the same anti-pattern, lower impact.)

**Fix:** use `fs/promises` `readFile`.

### B7 — Hook commits offset before/independent of delivery
[bin/claude-says-hook.js:73-84](../bin/claude-says-hook.js#L73) + [src/ipc.js:83-92](../src/ipc.js#L83)

The hook writes the new offset (`writeFileSync(stateFile, …length)`,
[:73](../bin/claude-says-hook.js#L73)) **before** calling `sendToSocket`, and
`sendToSocket` `resolve()`s even when the daemon is down or the send times out
([ipc.js:85,91](../src/ipc.js#L85)). So if the daemon isn't running, the offset
advances and that text is **never re-sent** — permanent silent loss once the
daemon returns.

**Fix:** only advance the offset after a confirmed send; make `sendToSocket`
reject (or return a status) on connect-error/timeout so the hook can decide.

### B8 — Offset past EOF after truncation/rotation
[bin/claude-says-hook.js:42,46-47](../bin/claude-says-hook.js#L42)

`fullTranscript.slice(lastOffset)` returns `''` when the stored `lastOffset`
exceeds the current length (truncated/rotated file), so the early-exit at
[:49-51](../bin/claude-says-hook.js#L49) fires and **new content is skipped
forever**.

**Fix:** clamp/validate — `if (lastOffset > fullTranscript.length) lastOffset = 0`.

### B9 — Incomplete final line lost
[bin/claude-says-hook.js:47,55,73](../bin/claude-says-hook.js#L47)

The hook slices to EOF, `split('\n')`, and then sets the new offset to the **full
length**. If the last line is incomplete (Claude mid-write), it fails `JSON.parse`
(silently caught, [:69](../bin/claude-says-hook.js#L69)) yet the offset still
advances past it, so the completed line is never re-read from its start. Unlike the
watcher — which correctly keeps the incomplete tail in `this.buffer`
([transcript-watcher.js:62](../src/transcript-watcher.js#L62)) — the hook has no
carry-over. *Mitigation:* it is installed only as a **Stop** hook, where the
transcript is usually already complete, so real-world hits are rare.

**Fix:** only consume up to the last `\n`; store that position as the offset.

### B10 — Session-switch race
[src/daemon.js:85-127](../src/daemon.js#L85)

`switchSession()` calls `watcher.stop()` (clears the poll timer) then `clear()` +
`reset()` and starts a new watcher. But a poll already in flight in
`_readNewContent()` will finish and `emit('text', …)` **old-session** content
([transcript-watcher.js:43-71](../src/transcript-watcher.js#L43)); the processor —
now reset for the new session — assigns it fresh `seq`s and speaks it as the new
session. (Old in-flight `synthesize` promises can likewise `enqueue` into the
freshly cleared queue.)

**Fix:** set a "stopped" flag in the watcher and skip emits after `stop()`; tag
events with a session id and drop mismatches at the daemon.

### B11 — ElevenLabs buffers whole response in memory
[src/providers/elevenlabs.js:90-92](../src/providers/elevenlabs.js#L90)

`Buffer.concat(chunks)` accumulates the full MP3 in memory. A long narration
(tens of MB) spikes memory with no backpressure.

**Fix:** stream the response to a temp file (or to the player) instead of
buffering, or cap synthesis length.

### B12 — No gap recovery in AudioQueue *(latent)*
[src/audio-queue.js:59-62](../src/audio-queue.js#L59)

`_drain()` breaks the moment `queue.get(nextToPlay)` is missing, and nothing ever
advances past a never-enqueued `seq` — playback stalls permanently and the queue
grows unbounded. **Currently not reachable:** every emitted `seq` is enqueued
([daemon.js:82](../src/daemon.js#L82)) and `reset()`/`clear()` zero `seq`/
`nextToPlay` in lockstep on session switch. It is logged here because the
invariant is implicit and fragile — any future path that assigns a `seq` without
enqueuing (or throws between emit and enqueue) deadlocks audio.

**Fix:** add gap tolerance — skip a `seq` that hasn't been enqueued within a
timeout, or assert contiguity.

---

## Medium / Low (condensed)

- **B13** [claude-says.js:127](../bin/claude-says.js#L127) — `setRawMode(true)`
  is never paired with `setRawMode(false)`; quitting (or a crash) leaves the
  terminal in raw mode. Add a `process.on('exit', …)` restore.
- **B14** [player.js:23](../src/player.js#L23) / [tts.js:12](../src/tts.js#L12) —
  the macOS provider returns `format:'aiff'` but the player only maps `mp3`→`.mp3`
  else `.wav`, so `.aiff` data is written to a `.wav` file (works only because
  `afplay` sniffs content). Separately, `tts.js` defaults to `'google'` while
  `config.js` defaults to `'macos'`; align them.
- **B15** [ipc.js:75-94](../src/ipc.js#L75) — `sendToSocket` resolves right after
  `write()`/`end()` and a `100ms` timeout can `destroy()` the socket before a
  large message flushes. Wait for the write callback / `drain`, or raise the
  timeout.
- **B16** [daemon.js:164-170](../src/daemon.js#L164) — `stop()` isn't async w.r.t.
  playback (`afplay` can outlive the daemon), and `flush()` emits a final sentence
  that the immediately-following `clear()` discards.
- **B17** [daemon.js:44-49](../src/daemon.js#L44) — the IPC fallback is gated on
  `!this.watcher` (object existence). A watcher that has silently stopped emitting
  still suppresses IPC, so text is dropped. Track liveness, not existence.
- **B18** [sessions.js:81](../src/sessions.js#L81) — `dir.replace(/-/g,'/')` turns
  a project dir like `-Users-me-claude-code-speak` into
  `/Users/me/claude/code/speak`. Display-only, but wrong for any dashed directory
  (including this repo). The encoding is ambiguous; match Claude Code's real scheme.
- **B19** [transcript-watcher.js:54,60](../src/transcript-watcher.js#L54) —
  `buf.toString('utf-8')` on a byte range whose end splits a multibyte character
  yields `U+FFFD`; the orphaned continuation bytes decode to garbage next poll.
  Low frequency (the boundary is usually a newline). Carry trailing bytes across
  reads.
- **B20** [transcript-watcher.js:46](../src/transcript-watcher.js#L46) — `if
  (stat.size <= this.offset) return;` never resets on truncation, so a rotated file
  is ignored. Detect `size < offset` and reset to 0.
- **B21** [text-processor.js:86-95](../src/text-processor.js#L86) — the
  last-space word-break falls back to a hard cut at `maxChunkLength`, so a long
  spaceless run is still split mid-word.
- **B22** [audio-queue.js:33-52](../src/audio-queue.js#L33) — the `enqueue`
  promise chain returns nothing and swallows any `_drain()` error; surface them via
  an `error` event.
- **B23** [ipc.js:40-46](../src/ipc.js#L40) — `socket.on('error', …)` and
  `'close'` both just delete from the set with no logging (and can double-delete).
  Log errors; guard cleanup.
- **B24** [macos.js:35](../src/providers/macos.js#L35) — `try { unlinkSync } catch
  {}` hides temp-file leaks; at least log.
- **B25** [claude-says.js:150-167](../bin/claude-says.js#L150) — pressing `s`
  only prints a list and tells you to restart with `-s`; it is not a working
  switch. (Also, the `keypress` handler can throw if `key` is `undefined` for some
  raw bytes.)

---

## Rejected false positives (verified *not* bugs)

Documented so the report's signal is trustworthy:

- **"Out-of-order seq from the async narrator"** — *not a bug.* `seq` is assigned
  before narration, but the sequence-ordered `AudioQueue` reorders by `seq`
  regardless of TTS completion order, which is the whole point of the queue
  ([daemon.js:69-82](../src/daemon.js#L69), [audio-queue.js:55-97](../src/audio-queue.js#L55)).
- **"`_drain()` reentrancy race"** — *not a bug.* The guard
  `if (this.draining) return; this.draining = true;`
  ([audio-queue.js:56-57](../src/audio-queue.js#L56)) is fully synchronous with no
  `await` between check and set; on a single-threaded event loop two `_drain()`
  calls cannot interleave there.
- **"UUID dedup eviction assumes Set order"** — *not a bug.* JS `Set` iteration is
  guaranteed insertion order, so `slice(-500)` keeps the newest UUIDs
  ([transcript-watcher.js:88-91](../src/transcript-watcher.js#L88)).
- **"Watcher loses partial JSON lines"** — *not a bug in the watcher.* It keeps the
  incomplete trailing line in `this.buffer` across polls
  ([transcript-watcher.js:62](../src/transcript-watcher.js#L62)). (The **hook** does
  have this issue — see B9 — because it uses a different, carry-over-less model.)
- **`isUUID` regex** ([sessions.js:76](../src/sessions.js#L76)) — correct for
  standard UUIDs.

---

## Suggested first fixes (highest value / lowest effort)

1. **B4** deep-merge config — a few lines, prevents silent provider misconfig.
2. **B7 + B15** make `sendToSocket` report failure and order offset-commit after
   delivery — stops silent text loss.
3. **B5/B6** random temp names + async read in the macOS provider (the default
   path) — removes collisions and event-loop stalls.
4. **B1/B2** rework the code-fence and sentence-split logic — the most
   user-noticeable output-quality bugs.
5. **B13** restore raw mode on exit — cheap, high annoyance if hit.
