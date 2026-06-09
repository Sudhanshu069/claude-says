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

    // Which session to watch
    this.activeSession = options.session || null;
    this.transcriptPath = options.transcriptPath || null;

    this._setupEventHandlers();
  }

  _setupEventHandlers() {
    // Text processor emits sentences → synthesize and queue
    this.processor.on('sentence', ({ seq, text }) => {
      this._synthesizeAndQueue(seq, text);
    });

    // IPC fallback: handle text from hooks only when not watching a transcript.
    // Validate shape and bound the size so a rogue local client can't push a
    // huge payload into the pipeline (and on to paid cloud TTS/LLM APIs).
    const MAX_IPC_TEXT = 100 * 1024; // 100 KB of text per message
    this.ipc.on('message', (msg) => {
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

  async stop() {
    this.processor.flush();
    this.audioQueue.clear();
    if (this.watcher) this.watcher.stop();
    await this.ipc.stop();
    this._log('Stopped.');
  }

  // Operational info logging routes through pino (see src/logger.js).
  // Timestamps/levels are added by the logger; empty spacer calls are ignored.
  _log(msg) {
    if (msg) logger.info(msg);
  }
}
