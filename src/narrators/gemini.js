import https from 'https';

const SYSTEM_PROMPT = `You are a concise narrator commentating on an AI coding assistant's actions in real-time.

Rules:
- Summarize what the assistant is doing in 1-2 short, conversational sentences
- Skip code snippets, file paths, and technical details
- Focus on the intent and action: "Claude is fixing the bug" not "Claude is editing line 42 of src/foo.js"
- Use present tense: "Claude is reading...", "Claude found...", "Claude is now editing..."
- Never use markdown formatting
- If the text is just a brief status update, keep your summary equally brief
- Maximum 2 sentences`;

export class GeminiNarrator {
  constructor(config) {
    this.apiKey = process.env.GEMINI_API_KEY || '';
    const geminiConfig = config.narrator?.gemini || {};
    this.model = geminiConfig.model || 'gemini-2.5-flash';
  }

  async narrate(text) {
    if (!this.apiKey) {
      return text; // fallback to raw text
    }

    try {
      const body = JSON.stringify({
        system_instruction: {
          parts: [{ text: SYSTEM_PROMPT }],
        },
        contents: [
          {
            parts: [{ text: `Narrate this AI assistant output:\n\n${text}` }],
          },
        ],
        generationConfig: {
          maxOutputTokens: 100,
          temperature: 0.3,
        },
      });

      const response = await this._request(body);
      const narrated = response?.candidates?.[0]?.content?.parts?.[0]?.text;
      return narrated || text;
    } catch {
      return text; // fallback on error
    }
  }

  async validate() {
    if (!this.apiKey) {
      return { ok: false, error: 'GEMINI_API_KEY environment variable not set' };
    }
    try {
      const result = await this.narrate('I am reading the config file to check the settings.');
      if (result && result.length > 0) {
        return { ok: true };
      }
      return { ok: false, error: 'Empty response' };
    } catch (err) {
      return { ok: false, error: err.message };
    }
  }

  _request(body) {
    return new Promise((resolve, reject) => {
      const options = {
        hostname: 'generativelanguage.googleapis.com',
        path: `/v1beta/models/${this.model}:generateContent`,
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Content-Length': Buffer.byteLength(body),
          // Key in a header, not the URL — keeps it out of proxy/access logs.
          'x-goog-api-key': this.apiKey,
        },
      };

      const req = https.request(options, (res) => {
        let data = '';
        res.on('data', (chunk) => data += chunk);
        res.on('end', () => {
          if (res.statusCode !== 200) {
            // Don't fold the response body into the error — it can reach logs.
            reject(new Error(`Gemini API error ${res.statusCode}`));
            return;
          }
          try {
            resolve(JSON.parse(data));
          } catch (e) {
            reject(e);
          }
        });
      });

      req.on('error', reject);
      req.setTimeout(10000, () => {
        req.destroy();
        reject(new Error('Gemini API timeout'));
      });
      req.write(body);
      req.end();
    });
  }
}
