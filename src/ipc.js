import net from 'net';
import { existsSync, unlinkSync, chmodSync } from 'fs';
import { EventEmitter } from 'events';
import { SOCKET_PATH } from './config.js';

// Cap per-connection buffering so a client that never sends a newline
// can't grow daemon memory without bound. Real messages are a sentence or two.
const MAX_BUFFER_BYTES = 1024 * 1024; // 1 MB

export class IPCServer extends EventEmitter {
  constructor() {
    super();
    this.server = null;
    this.clients = new Set();
  }

  start() {
    return new Promise((resolve, reject) => {
      // Clean up a stale socket. unlink removes the path entry itself (it does
      // not follow a symlink), so this can't be redirected to another file.
      if (existsSync(SOCKET_PATH)) {
        try {
          unlinkSync(SOCKET_PATH);
        } catch (err) {
          if (err.code === 'EACCES') {
            reject(new Error(
              `Cannot remove stale socket at ${SOCKET_PATH} (permission denied). ` +
              `Another user may own it. Try: sudo rm ${SOCKET_PATH}`
            ));
            return;
          }
          throw err;
        }
      }

      this.server = net.createServer((socket) => {
        this.clients.add(socket);
        let buffer = '';

        socket.on('data', (data) => {
          buffer += data.toString();
          if (buffer.length > MAX_BUFFER_BYTES) {
            // Oversized, newline-less stream — drop the connection.
            buffer = '';
            socket.destroy();
            return;
          }
          const lines = buffer.split('\n');
          buffer = lines.pop();

          for (const line of lines) {
            if (line.trim()) {
              try {
                const msg = JSON.parse(line);
                this.emit('message', msg);
              } catch {
                // skip malformed messages
              }
            }
          }
        });

        socket.on('close', () => {
          this.clients.delete(socket);
        });

        socket.on('error', () => {
          this.clients.delete(socket);
        });
      });

      this.server.on('error', reject);
      this.server.listen(SOCKET_PATH, () => {
        // Restrict the socket to the owning user. On macOS the filesystem
        // permission is enforced on connect, so this blocks other local
        // accounts from injecting speech into the daemon.
        try {
          chmodSync(SOCKET_PATH, 0o600);
        } catch {
          // best-effort; listen already succeeded
        }
        resolve();
      });
    });
  }

  stop() {
    return new Promise((resolve) => {
      for (const client of this.clients) {
        client.destroy();
      }
      this.clients.clear();

      if (this.server) {
        this.server.close(() => {
          if (existsSync(SOCKET_PATH)) {
            unlinkSync(SOCKET_PATH);
          }
          resolve();
        });
      } else {
        resolve();
      }
    });
  }
}

export function sendToSocket(data) {
  // Resolves true when the message was handed to the daemon, false when the
  // daemon was unreachable or the send timed out. Callers (the hook) use this
  // to avoid advancing past text that was never delivered. Never rejects.
  return new Promise((resolve) => {
    let settled = false;
    const done = (delivered) => {
      if (settled) return;
      settled = true;
      resolve(delivered);
    };

    const client = net.createConnection(SOCKET_PATH, () => {
      client.write(JSON.stringify(data) + '\n');
      client.end();
      done(true);
    });

    client.on('error', () => {
      // Daemon not running / unreachable.
      done(false);
    });

    // Timeout to avoid blocking Claude's display; treat as non-delivery.
    client.setTimeout(100, () => {
      client.destroy();
      done(false);
    });
  });
}
