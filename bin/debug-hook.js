#!/usr/bin/env node
// Debug hook: logs everything it receives to a file
import { writeFileSync } from 'fs';
import { join } from 'path';
import { tmpdir } from 'os';

const LOG = join(tmpdir(), 'claude-speak-debug.log');
let input = '';

process.stdin.setEncoding('utf-8');
process.stdin.on('data', (chunk) => { input += chunk; });
process.stdin.on('end', () => {
  const ts = new Date().toISOString();
  // Owner-only: this log captures full hook payloads (session ids, paths).
  writeFileSync(LOG, `[${ts}] ${input}\n`, { flag: 'a', mode: 0o600 });
  process.exit(0);
});
setTimeout(() => process.exit(0), 1000);
