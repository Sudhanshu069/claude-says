import { statSync, openSync, readSync, closeSync, watch } from 'fs';
import { StringDecoder } from 'string_decoder';
import { EventEmitter } from 'events';

// fs.watch (FSEvents on macOS) reacts to new transcript content in ~1ms once
// armed, so real output is spoken almost immediately. A safety poll stays as a
// fallback at the original 200ms cadence — it covers the brief window before
// fs.watch arms, file rotation, and any platform where fs.watch is flaky, so
// correctness never depends on fs.watch and latency never regresses.
// _readNewContent() is offset-guarded and idempotent, so being triggered by
// both the watcher and the poll only ever reads new bytes once.
const SAFETY_POLL_MS = 200;

/**
 * Watches a Claude Code transcript JSONL file in real-time.
 * Emits 'text' events for each new assistant text block as it's written.
 * Event-driven via fs.watch, with a periodic poll as a reliability fallback.
 */
export class TranscriptWatcher extends EventEmitter {
  constructor(transcriptPath) {
    super();
    this.path = transcriptPath;
    this.offset = 0;
    this.pollTimer = null;
    this.fsWatcher = null;
    this.buffer = '';
    // Boundary-safe UTF-8 decoder: holds a trailing partial multibyte sequence
    // until the next read completes it, instead of emitting U+FFFD garbage.
    this.decoder = new StringDecoder('utf8');
    this.processedLines = new Set();
  }

  start() {
    // Start from the end of the file (only process new content)
    try {
      const stat = statSync(this.path);
      this.offset = stat.size;
    } catch {
      this.offset = 0;
    }

    // Event-driven: react immediately when the file changes.
    try {
      this.fsWatcher = watch(this.path, () => this._readNewContent());
    } catch {
      this.fsWatcher = null; // fall back to polling only
    }

    // Safety-net poll: still pick up changes if fs.watch misses any (e.g. file
    // rotation) or isn't available on the platform.
    this.pollTimer = setInterval(() => this._readNewContent(), SAFETY_POLL_MS);

    this.emit('watching', { path: this.path });
  }

  stop() {
    if (this.pollTimer) {
      clearInterval(this.pollTimer);
      this.pollTimer = null;
    }
    if (this.fsWatcher) {
      this.fsWatcher.close();
      this.fsWatcher = null;
    }
  }

  _readNewContent() {
    try {
      const stat = statSync(this.path);
      // File shrank (truncated/rotated in place) — restart from the top rather
      // than waiting forever for it to grow past a now-stale offset.
      if (stat.size < this.offset) {
        this.offset = 0;
        this.buffer = '';
        this.decoder = new StringDecoder('utf8');
      }
      if (stat.size <= this.offset) return;

      this.emit('newdata', { bytes: stat.size - this.offset });

      // Read only the new bytes
      const fd = openSync(this.path, 'r');
      const newSize = stat.size - this.offset;
      const buf = Buffer.alloc(newSize);
      readSync(fd, buf, 0, newSize, this.offset);
      closeSync(fd);

      this.offset = stat.size;

      // Append to line buffer and process complete lines. decoder.write carries
      // any trailing partial multibyte char across to the next read.
      this.buffer += this.decoder.write(buf);
      const lines = this.buffer.split('\n');
      this.buffer = lines.pop(); // keep incomplete last line in buffer

      for (const line of lines) {
        if (!line.trim()) continue;
        this._processLine(line);
      }
    } catch (err) {
      this.emit('error', err);
    }
  }

  _processLine(line) {
    try {
      const entry = JSON.parse(line);

      // Only process assistant messages with text content
      if (entry.type !== 'assistant') return;
      if (!entry.message?.content) return;

      // Use UUID to avoid processing the same message twice
      // (Claude may write partial then complete messages)
      const uuid = entry.uuid;
      if (uuid && this.processedLines.has(uuid)) return;
      if (uuid) this.processedLines.add(uuid);

      // Limit memory: keep only last 1000 UUIDs
      if (this.processedLines.size > 1000) {
        const entries = [...this.processedLines];
        this.processedLines = new Set(entries.slice(-500));
      }

      // Extract text blocks (skip tool_use blocks)
      for (const block of entry.message.content) {
        if (block.type === 'text' && block.text) {
          const sessionId = entry.sessionId || entry.session_id || 'unknown';
          this.emit('text', {
            session_id: sessionId,
            text: block.text,
            timestamp: Date.now(),
          });
        }
      }
    } catch {
      // Skip malformed lines
    }
  }
}
