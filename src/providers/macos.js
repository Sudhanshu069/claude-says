import { BaseTTSProvider } from './base.js';
import { execFile } from 'child_process';
import { unlinkSync, chmodSync } from 'fs';
import { randomUUID } from 'crypto';
import { join } from 'path';
import { tmpdir } from 'os';

/**
 * macOS built-in TTS using the `say` command.
 * Zero config, no API keys, lowest latency.
 */
export class MacOSTTSProvider extends BaseTTSProvider {
  constructor(config) {
    super(config);
    this.voice = config.macos?.voice || 'Samantha';
    this.rate = config.macos?.rate || 200;
  }

  async synthesize(text) {
    // Unpredictable name: avoids collisions between concurrent synth calls and
    // stops other local processes guessing/reading the rendered audio.
    const outFile = join(tmpdir(), `claude-speak-${randomUUID()}.aiff`);

    await new Promise((resolve, reject) => {
      execFile('say', [
        '-v', this.voice,
        '-r', String(this.rate),
        '-o', outFile,
        text,
      ], (error) => {
        if (error) reject(error);
        else resolve();
      });
    });

    // Restrict to owner before reading; the file briefly holds spoken content.
    try { chmodSync(outFile, 0o600); } catch {}

    const { readFileSync } = await import('fs');
    const audio = readFileSync(outFile);
    try { unlinkSync(outFile); } catch {}

    return { audio, format: 'aiff' };
  }

  async validate() {
    try {
      await this.synthesize('test');
      return { ok: true };
    } catch (err) {
      return { ok: false, error: err.message };
    }
  }

  get audioExtension() {
    return 'aiff';
  }
}
