#!/usr/bin/env node

/**
 * Claude Code hook script (PostToolUse + Stop).
 * Called after each tool call and when Claude finishes.
 * Reads new text from the transcript and forwards to daemon.
 * Uses a state file to track what's already been sent.
 */

import { sendToSocket } from '../src/ipc.js';
import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'fs';
import { join, resolve } from 'path';
import { tmpdir, homedir } from 'os';

const STATE_DIR = join(tmpdir(), 'claude-speak-state');
const DEBUG_LOG = join(tmpdir(), 'claude-speak-hook.log');
// Transcripts always live under ~/.claude — refuse to read anything else.
const CLAUDE_DIR = join(homedir(), '.claude');
const MAX_INPUT_BYTES = 10 * 1024 * 1024; // 10 MB stdin guard

let input = '';

process.stdin.setEncoding('utf-8');
process.stdin.on('data', (chunk) => {
  input += chunk;
  if (input.length > MAX_INPUT_BYTES) process.exit(0);
});

process.stdin.on('end', async () => {
  try {
    const data = JSON.parse(input);
    const rawSessionId = data.session_id || 'unknown';
    // Strip anything that isn't UUID-safe so the id can't traverse out of
    // STATE_DIR (e.g. a "../" payload). Real session ids are unaffected.
    const sessionId = String(rawSessionId).replace(/[^a-zA-Z0-9-]/g, '_') || 'unknown';
    const transcriptPath = data.transcript_path || '';

    if (!transcriptPath) {
      process.exit(0);
      return;
    }

    // Only ever read transcripts under ~/.claude — never an arbitrary path
    // handed to us.
    const resolvedTranscript = resolve(transcriptPath);
    if (!resolvedTranscript.startsWith(CLAUDE_DIR + '/')) {
      process.exit(0);
      return;
    }

    // State file tracks the byte offset we've already processed
    if (!existsSync(STATE_DIR)) {
      mkdirSync(STATE_DIR, { recursive: true, mode: 0o700 });
    }
    const stateFile = join(STATE_DIR, `${sessionId}.offset`);
    let lastOffset = 0;
    if (existsSync(stateFile)) {
      lastOffset = parseInt(readFileSync(stateFile, 'utf-8'), 10) || 0;
    }

    // Read the transcript from where we left off
    const fullTranscript = readFileSync(resolvedTranscript, 'utf-8');
    const newContent = fullTranscript.slice(lastOffset);

    if (!newContent.trim()) {
      process.exit(0);
      return;
    }

    // Parse new lines and collect assistant text
    const lines = newContent.split('\n');
    const texts = [];

    for (const line of lines) {
      if (!line.trim()) continue;
      try {
        const entry = JSON.parse(line);
        if (entry.type === 'assistant' && entry.message?.content) {
          for (const block of entry.message.content) {
            if (block.type === 'text' && block.text) {
              texts.push(block.text);
            }
          }
        }
      } catch {}
    }

    // Update state with new offset
    writeFileSync(stateFile, String(fullTranscript.length));

    // Send all new text to daemon
    if (texts.length > 0) {
      const combined = texts.join(' ');
      await sendToSocket({
        type: 'text',
        session_id: sessionId,
        text: combined,
        timestamp: Date.now(),
      });
    }
  } catch (err) {
    try {
      writeFileSync(DEBUG_LOG, `[${new Date().toISOString()}] ERROR: ${err.message}\n`, { flag: 'a', mode: 0o600 });
    } catch {}
  }

  process.exit(0);
});

setTimeout(() => process.exit(0), 3000);
