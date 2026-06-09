import { writeFileSync, unlinkSync, existsSync, mkdirSync } from 'fs';
import { execFile } from 'child_process';
import { randomUUID } from 'crypto';
import { join } from 'path';
import { tmpdir } from 'os';

const TEMP_DIR = join(tmpdir(), 'claude-speak-audio');

export class AudioPlayer {
  constructor() {
    this.currentProcess = null;
    if (!existsSync(TEMP_DIR)) {
      // Owner-only dir: the temp files hold the audio of the user's session.
      mkdirSync(TEMP_DIR, { recursive: true, mode: 0o700 });
    }
  }

  /**
   * Play an audio buffer. Returns a promise that resolves when playback finishes.
   * @param {Buffer} audioBuffer - Raw audio data
   * @param {string} format - Audio format ('wav', 'mp3')
   */
  play(audioBuffer, format = 'wav') {
    return new Promise((resolve, reject) => {
      const ext = format === 'mp3' ? '.mp3' : '.wav';
      // Unpredictable name (no other process can guess/enumerate it) and
      // owner-only perms. Random names also avoid same-millisecond collisions.
      const tmpFile = join(TEMP_DIR, `chunk-${randomUUID()}${ext}`);

      writeFileSync(tmpFile, audioBuffer, { mode: 0o600 });

      // Use afplay on macOS (built-in, no dependencies)
      this.currentProcess = execFile('afplay', [tmpFile], (error) => {
        this.currentProcess = null;
        // Clean up temp file
        try {
          if (existsSync(tmpFile)) unlinkSync(tmpFile);
        } catch {}

        if (error && error.killed) {
          resolve(); // stopped intentionally
        } else if (error) {
          reject(error);
        } else {
          resolve();
        }
      });
    });
  }

  stop() {
    if (this.currentProcess) {
      this.currentProcess.kill();
      this.currentProcess = null;
    }
  }
}
