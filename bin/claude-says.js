#!/usr/bin/env node

import { program } from 'commander';
import { Daemon } from '../src/daemon.js';
import { runSetup } from '../src/setup.js';
import { discoverSessions } from '../src/sessions.js';
import { listProviders } from '../src/tts.js';
import readline from 'readline';
import { readFileSync } from 'fs';

// Single source of truth for the version: the installed package.json.
const pkg = JSON.parse(readFileSync(new URL('../package.json', import.meta.url), 'utf-8'));

program
  .name('claude-says')
  .description('Real-time text-to-speech companion for Claude Code')
  .version(pkg.version);

program
  .command('start', { isDefault: true })
  .description('Start the speak daemon')
  .option('-p, --provider <name>', `TTS provider (${listProviders().join(', ')})`)
  .option('-s, --session <id>', 'Listen to a specific session ID')
  .option('-l, --list', 'List available sessions and pick one')
  .option('-r, --rate <number>', 'Speech rate in words per minute (default: 200)', parseInt)
  .option('-v, --voice <name>', 'macOS voice name (use "claude-says voices" to list)')
  .option('-n, --narrator', 'Enable narrator mode (LLM rephrases output before speaking)')
  .option('--narrator-provider <name>', 'Narrator LLM provider (default: gemini)')
  .action(async (options) => {
    // If --list, show session picker first
    if (options.list) {
      const picked = await pickSession();
      if (!picked) {
        console.log('No session selected.');
        process.exit(0);
      }
      options.session = picked.sessionId;
      options.transcriptPath = picked.transcriptPath;
    }

    let daemon;
    try {
      daemon = new Daemon({
        provider: options.provider,
        session: options.session,
        transcriptPath: options.transcriptPath,
        narrator: options.narrator,
        narratorProvider: options.narratorProvider,
        rate: options.rate,
        voice: options.voice,
      });
    } catch (err) {
      // e.g. an unknown --provider / --narrator-provider: show a clean message
      // instead of a raw stack trace.
      console.error(`Error: ${err.message}`);
      process.exit(1);
    }

    // Graceful shutdown
    const shutdown = async () => {
      await daemon.stop();
      process.exit(0);
    };
    process.on('SIGINT', shutdown);
    process.on('SIGTERM', shutdown);

    try {
      await daemon.start();
    } catch (err) {
      console.error(`Failed to start: ${err.message}`);
      await daemon.stop().catch(() => {});
      process.exit(1);
    }

    // Interactive controls
    setupControls(daemon);
  });

program
  .command('setup')
  .description('Configure TTS provider and install Claude Code hook')
  .option('-p, --provider <name>', `TTS provider (${listProviders().join(', ')})`)
  .action(async (options) => {
    const success = await runSetup({ provider: options.provider });
    process.exit(success ? 0 : 1);
  });

program
  .command('sessions')
  .description('List discovered Claude Code sessions')
  .action(() => {
    const sessions = discoverSessions();
    if (sessions.length === 0) {
      console.log('No sessions found.');
      return;
    }
    console.log('Recent Claude Code sessions:\n');
    for (const s of sessions.slice(0, 20)) {
      console.log(`  ${s.sessionId.slice(0, 8)}  ${s.projectName}  (${s.lastActiveFormatted})`);
    }
    console.log(`\nTotal: ${sessions.length} sessions`);
  });

program
  .command('providers')
  .description('List available TTS providers')
  .action(() => {
    console.log('Available TTS providers:');
    for (const p of listProviders()) {
      console.log(`  - ${p}`);
    }
  });

program
  .command('voices')
  .description('List available macOS TTS voices')
  .option('-a, --all', 'Show all voices (including non-English)')
  .action(async (options) => {
    const { execFile } = await import('child_process');
    execFile('say', ['-v', '?'], (err, stdout) => {
      if (err) {
        console.error('Failed to list voices. Are you on macOS?');
        process.exit(1);
      }
      const lines = stdout.trim().split('\n');
      const voices = lines.map(line => {
        const match = line.match(/^(.+?)\s{2,}(\S+)/);
        if (!match) return null;
        return { name: match[1].trim(), locale: match[2].trim() };
      }).filter(Boolean);

      const filtered = options.all ? voices : voices.filter(v => v.locale.startsWith('en_'));

      console.log(`Available macOS voices${options.all ? '' : ' (English)'}:\n`);
      for (const v of filtered) {
        console.log(`  ${v.name.padEnd(30)} ${v.locale}`);
      }
      if (!options.all) {
        console.log(`\nShowing English voices only. Use --all to see all ${voices.length} voices.`);
      }
      console.log(`\nUsage: claude-says --voice "Daniel"`);
    });
  });

program.parse();

async function pickSession() {
  const sessions = discoverSessions();
  if (sessions.length === 0) {
    console.log('No sessions found.');
    return null;
  }

  console.log('\nSelect a Claude Code session to listen to:\n');
  const display = sessions.slice(0, 15);
  display.forEach((s, i) => {
    console.log(`  ${i + 1}. ${s.sessionId.slice(0, 8)}  ${s.projectName}  (${s.lastActiveFormatted})`);
  });
  console.log(`  0. Listen to all sessions\n`);

  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((resolve) => {
    rl.question('Enter number: ', (answer) => {
      rl.close();
      const num = parseInt(answer, 10);
      if (num === 0) {
        resolve(null);
      } else if (num >= 1 && num <= display.length) {
        resolve(display[num - 1]);
      } else {
        resolve(null);
      }
    });
  });
}

function setupControls(daemon) {
  if (!process.stdin.isTTY) return;

  readline.emitKeypressEvents(process.stdin);
  process.stdin.setRawMode(true);

  // Restore the terminal to cooked mode on exit so quitting (or a crash) never
  // leaves the shell without echo / line editing.
  const restoreTty = () => {
    try { if (process.stdin.isTTY) process.stdin.setRawMode(false); } catch {}
  };
  process.on('exit', restoreTty);

  console.log('Controls: [p]ause/resume  [s]witch session  [q]uit\n');

  let paused = false;
  let pendingSessions = null; // when set, the next digit picks a session to switch to

  process.stdin.on('keypress', async (str, key) => {
    if (!key) return;
    if (key.ctrl && key.name === 'c') {
      await daemon.stop();
      process.exit(0);
    }

    // If we just printed the session list, consume the next digit as the choice.
    if (pendingSessions) {
      const choices = pendingSessions;
      pendingSessions = null;
      if (str === '0') {
        daemon.switchSession(null);
        console.log('Now listening to all sessions (via hooks).\n');
      } else {
        const n = parseInt(str, 10);
        if (Number.isInteger(n) && n >= 1 && n <= choices.length) {
          const picked = choices[n - 1];
          daemon.switchSession(picked.sessionId);
          console.log(`Switched to ${picked.sessionId.slice(0, 8)} ${picked.projectName}\n`);
        } else {
          console.log('Session switch cancelled.\n');
        }
      }
      return;
    }

    switch (key.name) {
      case 'p':
        paused = !paused;
        if (paused) {
          daemon.audioQueue.pause();
          console.log('[Paused]');
        } else {
          daemon.audioQueue.resume();
          console.log('[Resumed]');
        }
        break;

      case 's': {
        // Show the list, then capture the next digit (raw mode = one key) as the
        // selection and actually switch via daemon.switchSession().
        const sessions = discoverSessions().slice(0, 9);
        if (sessions.length === 0) {
          console.log('No sessions found.');
          break;
        }
        console.log('\nSessions:');
        sessions.forEach((s, i) => {
          const active = daemon.activeSession === s.sessionId ? ' *' : '';
          console.log(`  ${i + 1}. ${s.sessionId.slice(0, 8)} ${s.projectName}${active}`);
        });
        console.log('  0. All sessions');
        console.log('Press a number to switch (any other key cancels)...');
        pendingSessions = sessions;
        break;
      }

      case 'q':
        await daemon.stop();
        process.exit(0);
    }
  });
}
