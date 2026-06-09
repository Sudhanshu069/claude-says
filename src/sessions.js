import { readdirSync, statSync, existsSync, openSync, readSync, closeSync } from 'fs';
import { join, basename } from 'path';
import { homedir } from 'os';

const CLAUDE_PROJECTS_DIR = join(homedir(), '.claude', 'projects');

/**
 * Discover active/recent Claude Code sessions.
 * Transcripts are JSONL files at ~/.claude/projects/<project-dir>/<session-id>.jsonl
 */
export function discoverSessions() {
  const sessions = [];

  try {
    const projectDirs = readdirSync(CLAUDE_PROJECTS_DIR);

    for (const projectDir of projectDirs) {
      const projectPath = join(CLAUDE_PROJECTS_DIR, projectDir);
      const stat = statSync(projectPath);
      if (!stat.isDirectory()) continue;

      try {
        const entries = readdirSync(projectPath);
        for (const entry of entries) {
          // Transcript files are <uuid>.jsonl
          if (!entry.endsWith('.jsonl')) continue;
          const sessionId = basename(entry, '.jsonl');
          if (!isUUID(sessionId)) continue;

          const entryPath = join(projectPath, entry);
          const entryStat = statSync(entryPath);

          const projectName = readProjectCwd(entryPath) || decodeProjectDir(projectDir);
          const lastActive = entryStat.mtimeMs;

          sessions.push({
            sessionId,
            projectDir,
            projectName,
            transcriptPath: entryPath,
            lastActive,
            lastActiveFormatted: formatAge(lastActive),
          });
        }
      } catch {
        // Skip inaccessible dirs
      }
    }
  } catch {
    // ~/.claude/projects/ doesn't exist yet
  }

  // Sort by most recently active
  sessions.sort((a, b) => b.lastActive - a.lastActive);
  return sessions;
}

/**
 * Find the transcript file path for a given session ID.
 */
export function findTranscriptPath(sessionId) {
  const sessions = discoverSessions();
  const match = sessions.find(s => s.sessionId === sessionId || s.sessionId.startsWith(sessionId));
  return match?.transcriptPath || null;
}

/**
 * Get the most recently active session.
 */
export function getMostRecentSession() {
  const sessions = discoverSessions();
  return sessions[0] || null;
}

function isUUID(str) {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(str);
}

function decodeProjectDir(dir) {
  // Fallback only. Project dirs encode the path with '-', but the encoding is
  // lossy (dashes inside a real directory name are indistinguishable from path
  // separators), so this can be wrong for dashed names. Prefer readProjectCwd.
  return dir.replace(/^-/, '/').replace(/-/g, '/');
}

function readProjectCwd(transcriptPath) {
  // Claude Code transcript records carry the real `cwd`. Read a bounded prefix
  // and return the first cwd found — accurate even for directories whose names
  // contain dashes (which decodeProjectDir would mangle into slashes).
  let fd;
  try {
    fd = openSync(transcriptPath, 'r');
    const buf = Buffer.alloc(8192);
    const bytes = readSync(fd, buf, 0, buf.length, 0);
    const text = buf.toString('utf-8', 0, bytes);
    for (const line of text.split('\n')) {
      if (!line.trim()) continue;
      try {
        const entry = JSON.parse(line);
        if (typeof entry.cwd === 'string' && entry.cwd) return entry.cwd;
      } catch { /* truncated last line in the window — ignore */ }
    }
    return null;
  } catch {
    return null;
  } finally {
    if (fd !== undefined) { try { closeSync(fd); } catch {} }
  }
}

function formatAge(timestampMs) {
  const age = Date.now() - timestampMs;
  const seconds = Math.floor(age / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}
