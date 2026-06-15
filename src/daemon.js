import { IPCServer } from './ipc.js';
import { TranscriptWatcher } from './transcript-watcher.js';
import { TextProcessor } from './text-processor.js';
import { AudioQueue } from './audio-queue.js';
import { AudioPlayer } from './player.js';
import { createProvider } from './tts.js';
import { createNarrator } from './narrator.js';
import { loadConfig } from './config.js';
import { findTranscriptPath, getMostRecentSession } from './sessions.js';
import { logger } from './logger.js';

export class Daemon {
  constructor(options = {}) {
    this.config = loadConfig();
    if (options.provider) {
      this.config.provider = options.provider;
    }

    // macOS voice/rate overrides
    if (options.rate || options.voice) {
      this.config.macos = {
        ...this.config.macos,
        ...(options.rate && { rate: options.rate }),
        ...(options.voice && { voice: options.voice }),
      };
    }

    // Narrator mode
    if (options.narrator) {
      this.config.narrator = {
        ...this.config.narrator,
        enabled: true,
        ...(options.narratorProvider && { provider: options.narratorProvider }),
      };
    }

    this.ipc = new IPCServer();
    this.player = new AudioPlayer();
    this.ttsProvider = createProvider(this.config);
    this.audioQueue = new AudioQueue(this.player);
    this.processor = new TextProcessor(this.config.textProcessor);
    this.narrator = this.config.narrator?.enabled ? createNarrator(this.config) : null;
    this.watcher = null;

    // Shutdown coordination: while stopping we stop accepting new text and wait
    // for the in-flight synth/playback to finish so the final sentence isn't cut.
    this._stopping = false;
    this._inFlightSynth = new Set();

    // Which session to watch
    this.activeSession = options.session || null;
    this.transcriptPath = options.transcriptPath || null;

    this._setupEventHandlers();
  }

  _setupEventHandlers() {
    // Text processor emits sentences → synthesize and queue. Track the in-flight
    // synth promise so stop() can wait for the final flushed sentence to be
    // enqueued before judging the queue drained.
    this.processor.on('sentence', ({ seq, text }) => {
      const p = this._synthesizeAndQueue(seq, text).catch(() => {});
      this._inFlightSynth.add(p);
      p.finally(() => this._inFlightSynth.delete(p));
    });

    // IPC fallback: handle text from hooks only when not watching a transcript.
    // Validate shape and bound the size so a rogue local client can't push a
    // huge payload into the pipeline (and on to paid cloud TTS/LLM APIs).
    const MAX_IPC_TEXT = 100 * 1024; // 100 KB of text per message
    this.ipc.on('message', (msg) => {
      if (this._stopping) return;
      if (msg && msg.type === 'text' && typeof msg.text === 'string' && !this.watcher) {
        this.processor.feed(msg.text.slice(0, MAX_IPC_TEXT));
      }
    });

    // Audio queue events
    this.audioQueue.on('playing', ({ seq, queueSize }) => {
      this._log(`Playing #${seq} (${queueSize} queued)`);
    });

    this.audioQueue.on('error', ({ seq, error }) => {
      logger.error(`Audio error #${seq}: ${error.message}`);
    });

    this.audioQueue.on('drained', () => {
      this._log('Waiting for more text...');
    });
  }

  async _synthesizeAndQueue(seq, text) {
    let finalText = text;

    // If narrator is enabled, rephrase through LLM first
    if (this.narrator) {
      this._log(`Narrating: "${text.slice(0, 50)}..."`);
      try {
        finalText = await this.narrator.narrate(text);
        this._log(`Narrated: "${finalText.slice(0, 70)}${finalText.length > 70 ? '...' : ''}"`);
      } catch (err) {
        logger.warn(`Narrator failed, using raw text: ${err.message}`);
      }
    } else {
      this._log(`TTS: "${finalText.slice(0, 70)}${finalText.length > 70 ? '...' : ''}"`);
    }

    const audioPromise = this.ttsProvider.synthesize(finalText);
    this.audioQueue.enqueue(seq, audioPromise);
  }

  _startWatching(transcriptPath) {
    if (this.watcher) {
      this.watcher.stop();
    }

    this.watcher = new TranscriptWatcher(transcriptPath);

    this.watcher.on('text', ({ text }) => {
      if (this._stopping) return;
      this._log(`Got text (${text.length} chars): "${text.slice(0, 50)}..."`);
      this.processor.feed(text);
    });

    this.watcher.on('newdata', ({ bytes }) => {
      this._log(`Transcript grew by ${bytes} bytes`);
    });

    this.watcher.on('watching', ({ path }) => {
      this._log(`Watching transcript: ${path}`);
    });

    this.watcher.on('error', (err) => {
      logger.error(`Watcher error: ${err.message}`);
    });

    this.watcher.start();
  }

  switchSession(sessionId) {
    if (this._stopping) return; // mid-shutdown: don't clear the draining queue
    this.activeSession = sessionId;
    this.audioQueue.clear();
    this.processor.reset();

    if (sessionId) {
      const path = findTranscriptPath(sessionId);
      if (path) {
        this._startWatching(path);
      } else {
        this._log(`No transcript found for session ${sessionId.slice(0, 8)}`);
      }
    } else {
      // Tear the watcher down (not just leave it running) so the IPC/hook
      // fallback — gated on !this.watcher — actually re-enables. Otherwise a
      // lingering watcher would keep hook text suppressed while we claim to be
      // "listening via hooks only".
      if (this.watcher) {
        this.watcher.stop();
        this.watcher = null;
      }
      this._log('Listening to all sessions (via hooks only)');
    }
  }

  async start() {
    await this.ipc.start();
    this._log(`claude-says started (tts: ${this.config.provider}${this.narrator ? ', narrator: ' + this.config.narrator.provider : ''})`);

    // If a specific transcript path was given, watch it
    if (this.transcriptPath) {
      this._startWatching(this.transcriptPath);
      return;
    }

    // If a session ID was given, find its transcript
    if (this.activeSession) {
      const path = findTranscriptPath(this.activeSession);
      if (path) {
        this._startWatching(path);
      } else {
        this._log(`No transcript found for session ${this.activeSession.slice(0, 8)}`);
        this._log('Will listen via hooks instead.');
      }
      return;
    }

    // Auto-detect: watch the most recently active session
    const recent = getMostRecentSession();
    if (recent) {
      this.activeSession = recent.sessionId;
      this._log(`Auto-detected session: ${recent.sessionId.slice(0, 8)} (${recent.projectName})`);
      this._startWatching(recent.transcriptPath);
    } else {
      this._log('No sessions found. Will listen via hooks.');
    }

    this._log('');
  }

  async stop({ drain = true, timeoutMs = 10000 } = {}) {
    // Stop accepting new text first, then flush the final buffered sentence and
    // let what's already queued finish playing — bounded by timeoutMs so a stuck
    // TTS request can never hang shutdown. Previously stop() flushed the final
    // sentence and immediately clear()ed the queue, discarding it before it
    // could play (and cutting off whatever was mid-playback).
    //
    // Idempotent: a second quit signal arriving mid-drain returns immediately,
    // so pressing q / Ctrl-C again exits now instead of starting a second drain.
    if (this._stopping) return;
    this._stopping = true;
    if (this.watcher) {
      this.watcher.stop();
      this.watcher = null;
    }

    this.processor.flush();
    // Only drain when the queue is actually playing. A paused queue never
    // advances (AudioQueue._drain bails while paused), so draining it would just
    // spin to the deadline — a paused quit should exit immediately.
    if (drain && !this.audioQueue.paused) {
      await this._drainAudio(timeoutMs);
    }

    this.audioQueue.clear();
    await this.ipc.stop();
    this._log('Stopped.');
  }

  // Wait for in-flight synthesis to enqueue and the audio queue to finish
  // playing, capped at timeoutMs. The final flushed sentence may still be
  // synthesizing (esp. via the narrator/cloud path), so we first wait for the
  // tracked synth promises (bounded), then poll until the queue empties or time
  // runs out. The poll's 50ms timers are short-lived and self-clearing, so
  // nothing lingers past the (deadline-bounded) drain.
  async _drainAudio(timeoutMs) {
    const deadline = Date.now() + timeoutMs;
    await this._withTimeout(Promise.allSettled([...this._inFlightSynth]), timeoutMs);
    while (this.audioQueue.size > 0 && Date.now() < deadline) {
      await this._delay(50);
    }
  }

  // Resolve when `promise` settles or after `ms`, clearing the timer either way
  // so a fast-settling promise never leaves a long timeout armed on the loop.
  _withTimeout(promise, ms) {
    return new Promise((resolve) => {
      const t = setTimeout(resolve, ms);
      const done = () => { clearTimeout(t); resolve(); };
      promise.then(done, done);
    });
  }

  _delay(ms) {
    return new Promise((resolve) => setTimeout(resolve, ms));
  }

  // Operational info logging routes through pino (see src/logger.js).
  // Timestamps/levels are added by the logger; empty spacer calls are ignored.
  _log(msg) {
    if (msg) logger.info(msg);
  }
}
