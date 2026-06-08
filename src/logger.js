import pino from 'pino';

/**
 * Operational logger for the daemon.
 *
 * - Verbosity is controlled by the LOG_LEVEL env var (default: "info").
 *   Levels: trace, debug, info, warn, error, fatal, silent.
 * - When attached to an interactive terminal (TTY) it prints human-friendly,
 *   colorized lines via pino-pretty.
 * - When piped/redirected (no TTY) it emits structured NDJSON — ideal for log
 *   files, `jq`, or shipping to a log collector.
 *
 * pino-pretty is used as a synchronous stream (not a worker-thread transport)
 * so the final lines aren't lost when the daemon calls process.exit() on
 * shutdown. If pino-pretty isn't installed, we fall back to NDJSON cleanly.
 *
 * This logger is for the daemon's operational output. Interactive prompts and
 * wizard/CLI output (setup.js, the start-controls in bin/claude-speak.js) stay
 * on console.* — they are user-facing UI, not logs.
 */
const level = process.env.LOG_LEVEL || 'info';

let destination; // undefined → pino writes NDJSON to stdout
if (process.stdout.isTTY) {
  try {
    const { default: pretty } = await import('pino-pretty');
    destination = pretty({
      colorize: true,
      translateTime: 'SYS:HH:MM:ss',
      ignore: 'pid,hostname',
    });
  } catch {
    // pino-pretty unavailable — fall back to NDJSON; logging still works.
  }
}

export const logger = destination ? pino({ level }, destination) : pino({ level });

export default logger;
