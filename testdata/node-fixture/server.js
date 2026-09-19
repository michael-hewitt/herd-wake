// Dependency-free HTTP server for the e2e suite: the shape of a plain
// `node server.js` project whose port arrives through the environment
// (like `node --import tsx src/server.ts` with PORT set), as opposed to the
// Vite fixture's npm script. Uses only Node built-ins, so nothing needs an
// `npm ci`.
//
// Contract:
//   - port: the PORT environment variable, else `--port <n>` on the command
//     line; anything else is a usage error (exit 2)
//   - listens on 127.0.0.1 only
//   - GET / answers 200 text/plain "node-fixture ok port=<port> pid=<pid>"
//   - SIGTERM (and SIGINT) close the listener and exit 0
'use strict';

const http = require('node:http');

function resolvePort() {
  if (process.env.PORT) return Number(process.env.PORT);
  const i = process.argv.indexOf('--port');
  if (i !== -1 && process.argv[i + 1]) return Number(process.argv[i + 1]);
  return NaN;
}

const port = resolvePort();
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  console.error('node-fixture: set PORT or pass --port <n>');
  process.exit(2);
}

const server = http.createServer((req, res) => {
  res.writeHead(200, { 'content-type': 'text/plain' });
  res.end(`node-fixture ok port=${port} pid=${process.pid}\n`);
});

server.listen(port, '127.0.0.1', () => {
  console.log(`node-fixture: listening on http://127.0.0.1:${port}`);
});

function shutdown(signal) {
  console.log(`node-fixture: ${signal} received, closing`);
  server.close(() => process.exit(0));
  // Keep-alive connections would otherwise hold close() open; do not wait
  // on them.
  server.closeAllConnections();
}
process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
