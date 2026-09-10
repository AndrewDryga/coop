'use strict';
// coop task-channel multiplexer — runs in the coop-owned HELPER container (never in the box).
//
// It listens on one unix socket that every MCP client in the box connects to through
// `socat STDIO UNIX-CONNECT:`, and carries all of those connections over its own stdio, which
// is coop's end of the channel. Frames are newline-delimited JSON, one per line, both ways:
//   helper -> coop   {"e":"ready"}                   the socket is bound and accepting
//                    {"c":N,"e":"open"}              client connection N appeared
//                    {"c":N,"d":"<one MCP line>"}    a line the client sent (no trailing newline)
//                    {"c":N,"e":"close"}             client connection N went away
//   coop -> helper   {"c":N,"d":"<one MCP line>"}    a line to deliver to client N
//                    {"c":N,"e":"close"}             hang up on client N
// EOF on stdin (coop is gone) closes every client and exits. The host side is mux.go.
const fs = require('fs');
const net = require('net');
const readline = require('readline');

const socketPath = process.argv[2];
if (!socketPath) {
  process.stderr.write('usage: mux.js <socket path>\n');
  process.exit(2);
}
// One client line may not exceed the same bound the host puts on a frame (4 MiB); a client that
// sends more without a newline is disconnected rather than buffered without limit.
const maxLine = 4 * 1024 * 1024;
const clients = new Map();
let nextID = 1;

function emit(frame) {
  process.stdout.write(JSON.stringify(frame) + '\n');
}

// allowHalfOpen: a client that shuts its write side (a `socat` whose stdin reached EOF after
// sending its requests) must still receive the replies coop is about to write. Without it node
// auto-closes the socket on read-EOF and the pending reply is lost. coop closes the socket itself
// when its session ends.
const server = net.createServer({ allowHalfOpen: true }, (socket) => {
  const id = nextID++;
  clients.set(id, socket);
  emit({ c: id, e: 'open' });
  socket.setEncoding('utf8');
  let pending = '';
  socket.on('data', (chunk) => {
    pending += chunk;
    let at;
    while ((at = pending.indexOf('\n')) >= 0) {
      emit({ c: id, d: pending.slice(0, at) });
      pending = pending.slice(at + 1);
    }
    if (pending.length > maxLine) socket.destroy();
  });
  socket.on('error', () => {});
  socket.on('close', () => {
    if (clients.delete(id)) emit({ c: id, e: 'close' });
  });
});
server.on('error', (err) => {
  process.stderr.write('coop task channel: ' + err.message + '\n');
  process.exit(1);
});
try { fs.unlinkSync(socketPath); } catch (e) { /* a fresh volume has no socket yet */ }
server.listen(socketPath, () => {
  // The box connects as the image's unprivileged user; the socket's inode must let it.
  fs.chmodSync(socketPath, 0o666);
  emit({ e: 'ready' });
});

const lines = readline.createInterface({ input: process.stdin, crlfDelay: Infinity });
lines.on('line', (line) => {
  let frame;
  try { frame = JSON.parse(line); } catch (e) { return; }
  const socket = clients.get(frame.c);
  if (!socket) return;
  if (frame.e === 'close') { socket.end(); return; }
  if (typeof frame.d === 'string') socket.write(frame.d + '\n');
});
lines.on('close', () => {
  for (const socket of clients.values()) socket.destroy();
  server.close(() => process.exit(0));
  setTimeout(() => process.exit(0), 1000).unref();
});
