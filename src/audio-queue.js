import { EventEmitter } from 'events';

/**
 * Ordered FIFO audio queue. Ensures chunks play in exact sequence order
 * with no overlap or gaps, regardless of when TTS responses arrive.
 */
export class AudioQueue extends EventEmitter {
  constructor(player) {
    super();
    this.player = player;
    this.queue = new Map(); // seq → { audioPromise, status }
    this.nextToPlay = 1;
    this.draining = false;
    this.paused = false;
  }

  /**
   * Add an audio chunk to the queue.
   * @param {number} seq - Sequence number (must be monotonically increasing)
   * @param {Promise<{audio: Buffer, format: string}>} audioPromise - Promise resolving to audio data
   */
  enqueue(seq, audioPromise) {
    this.queue.set(seq, {
      audioPromise,
      audio: null,
      format: null,
      status: 'pending',
    });

    this.emit('queued', { seq, queueSize: this.queue.size });

    // When the audio is ready, mark it and try to drain
    audioPromise
      .then((result) => {
        const entry = this.queue.get(seq);
        if (entry) {
          entry.audio = result.audio;
          entry.format = result.format;
          entry.status = 'ready';
          this._drain();
        }
      })
      .catch((err) => {
        // On TTS error, mark as failed and skip
        const entry = this.queue.get(seq);
        if (entry) {
          entry.status = 'failed';
          entry.error = err;
          this.emit('error', { seq, error: err });
          this._drain();
        }
      });
  }

  async _drain() {
    if (this.draining || this.paused) return;
    this.draining = true;

    while (true) {
      const entry = this.queue.get(this.nextToPlay);

      if (!entry) break; // nothing at this sequence yet

      if (entry.status === 'pending') break; // not ready yet, wait

      if (entry.status === 'failed') {
        // Skip failed entries
        this.queue.delete(this.nextToPlay);
        this.nextToPlay++;
        continue;
      }

      if (entry.status === 'ready') {
        entry.status = 'playing';
        this.emit('playing', { seq: this.nextToPlay, queueSize: this.queue.size });

        let interrupted = false;
        try {
          const result = await this.player.play(entry.audio, entry.format);
          interrupted = !!(result && result.interrupted);
        } catch (err) {
          this.emit('error', { seq: this.nextToPlay, error: err });
        }

        // pause()/clear() killed playback (player resolves with interrupted).
        // Leave this entry 'ready' and stop draining so resume() replays it from
        // the start instead of silently losing the sentence. (This also makes a
        // clear()-during-playback exit the loop cleanly without advancing
        // nextToPlay over the now-empty queue.)
        if (interrupted) {
          entry.status = 'ready';
          break;
        }

        entry.status = 'done';
        this.queue.delete(this.nextToPlay);
        this.nextToPlay++;
        this.emit('played', { seq: this.nextToPlay - 1, queueSize: this.queue.size });
      }

      if (this.paused) break;
    }

    this.draining = false;

    if (this.queue.size === 0) {
      this.emit('drained');
    }
  }

  pause() {
    this.paused = true;
    this.player.stop();
  }

  resume() {
    this.paused = false;
    this._drain();
  }

  clear() {
    this.queue.clear();
    this.nextToPlay = 1;
    this.paused = false;
    this.draining = false;
    this.player.stop();
    this.emit('drained');
  }

  get size() {
    return this.queue.size;
  }

  get currentSeq() {
    return this.nextToPlay;
  }
}
