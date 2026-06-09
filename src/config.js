import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'fs';
import { homedir } from 'os';
import { join } from 'path';

const CONFIG_DIR = join(homedir(), '.claude-says');
const CONFIG_FILE = join(CONFIG_DIR, 'config.json');
const SOCKET_PATH = '/tmp/claude-says.sock';

const DEFAULT_CONFIG = {
  provider: 'macos',
  macos: {
    voice: 'Samantha',
    rate: 200,
  },
  google: {
    voice: 'en-US-Neural2-D',
    languageCode: 'en-US',
    audioEncoding: 'LINEAR16',
    sampleRateHertz: 24000,
  },
  elevenlabs: {
    voiceId: '21m00Tcm4TlvDq8ikWAM',
    modelId: 'eleven_turbo_v2_5',
  },
  playback: {
    method: 'afplay',
  },
  textProcessor: {
    minChunkLength: 10,
    maxChunkLength: 500,
  },
  narrator: {
    enabled: false,
    provider: 'gemini',
    gemini: {
      model: 'gemini-2.5-flash',
    },
  },
};

export function loadConfig() {
  if (!existsSync(CONFIG_FILE)) {
    return { ...DEFAULT_CONFIG };
  }
  try {
    const raw = readFileSync(CONFIG_FILE, 'utf-8');
    const saved = JSON.parse(raw);
    return { ...DEFAULT_CONFIG, ...saved };
  } catch {
    return { ...DEFAULT_CONFIG };
  }
}

export function saveConfig(config) {
  if (!existsSync(CONFIG_DIR)) {
    mkdirSync(CONFIG_DIR, { recursive: true });
  }
  writeFileSync(CONFIG_FILE, JSON.stringify(config, null, 2));
}

export { SOCKET_PATH, CONFIG_DIR, CONFIG_FILE, DEFAULT_CONFIG };
