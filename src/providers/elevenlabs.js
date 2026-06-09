import { BaseTTSProvider } from './base.js';
import https from 'https';

export class ElevenLabsTTSProvider extends BaseTTSProvider {
  constructor(config) {
    super(config);
    this.apiKey = process.env.ELEVENLABS_API_KEY || '';
  }

  async synthesize(text) {
    const cfg = this.config.elevenlabs || {};
    const modelId = cfg.modelId || 'eleven_turbo_v2_5';

    // Get a usable voice — either configured or fetch user's first available
    const voiceId = cfg.voiceId || await this._getDefaultVoice();

    const body = JSON.stringify({
      text,
      model_id: modelId,
      voice_settings: {
        stability: 0.5,
        similarity_boost: 0.75,
      },
    });

    const audio = await this._request(voiceId, body);
    return { audio, format: 'mp3' };
  }

  _getDefaultVoice() {
    if (this._cachedVoiceId) return Promise.resolve(this._cachedVoiceId);

    return new Promise((resolve, reject) => {
      const options = {
        hostname: 'api.elevenlabs.io',
        path: '/v1/voices',
        method: 'GET',
        headers: {
          'xi-api-key': this.apiKey,
        },
      };

      const req = https.request(options, (res) => {
        let body = '';
        res.on('data', (d) => body += d);
        res.on('end', () => {
          try {
            const data = JSON.parse(body);
            // Pick first available voice
            const voice = data.voices?.[0];
            if (voice) {
              this._cachedVoiceId = voice.voice_id;
              resolve(voice.voice_id);
            } else {
              reject(new Error('No voices available'));
            }
          } catch (e) {
            reject(e);
          }
        });
      });

      req.on('error', reject);
      req.end();
    });
  }

  _request(voiceId, body) {
    return new Promise((resolve, reject) => {
      const options = {
        hostname: 'api.elevenlabs.io',
        path: `/v1/text-to-speech/${voiceId}`,
        method: 'POST',
        headers: {
          'Accept': 'audio/mpeg',
          'Content-Type': 'application/json',
          'xi-api-key': this.apiKey,
          'Content-Length': Buffer.byteLength(body),
        },
      };

      const req = https.request(options, (res) => {
        if (res.statusCode !== 200) {
          // Drain and reject with the status only — the body may contain
          // account/voice details that shouldn't land in console logs.
          res.resume();
          res.on('end', () => reject(new Error(`ElevenLabs API error ${res.statusCode}`)));
          return;
        }

        const chunks = [];
        res.on('data', (chunk) => chunks.push(chunk));
        res.on('end', () => resolve(Buffer.concat(chunks)));
      });

      req.on('error', reject);
      req.write(body);
      req.end();
    });
  }

  async validate() {
    if (!this.apiKey) {
      return { ok: false, error: 'ELEVENLABS_API_KEY environment variable not set' };
    }
    try {
      const result = await this.synthesize('test');
      if (result.audio && result.audio.length > 0) {
        return { ok: true };
      }
      return { ok: false, error: 'Empty audio response' };
    } catch (err) {
      return { ok: false, error: err.message };
    }
  }

  get audioExtension() {
    return 'mp3';
  }
}
