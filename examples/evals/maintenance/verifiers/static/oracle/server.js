const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

function createServer(root) {
  const realRoot = fs.realpathSync(root);
  return http.createServer((request, response) => {
    if (request.method !== 'GET' && request.method !== 'HEAD') {
      response.writeHead(405);
      response.end();
      return;
    }
    let name;
    try {
      name = decodeURIComponent(request.url.split('?')[0]);
    } catch {
      response.writeHead(400);
      response.end();
      return;
    }
    if (name.includes('\\') || name.includes('\0')) {
      response.writeHead(400);
      response.end();
      return;
    }
    const candidate = path.resolve(realRoot, name === '/' ? 'index.html' : name.slice(1));
    if (!candidate.startsWith(realRoot + path.sep)) {
      response.writeHead(403);
      response.end();
      return;
    }
    fs.realpath(candidate, (error, realFile) => {
      if (error) {
        response.writeHead(error.code === 'ENOENT' ? 404 : 500);
        response.end();
        return;
      }
      if (!realFile.startsWith(realRoot + path.sep)) {
        response.writeHead(403);
        response.end();
        return;
      }
      fs.readFile(realFile, (readError, bytes) => {
        if (readError) {
          response.writeHead(500);
          response.end();
          return;
        }
        const type = realFile.endsWith('.html') ? 'text/html' : 'text/plain';
        response.writeHead(200, { 'Content-Type': type, 'Content-Length': bytes.length });
        response.end(request.method === 'HEAD' ? undefined : bytes);
      });
    });
  });
}

module.exports = { createServer };

if (require.main === module) {
  const server = createServer(path.resolve(process.argv[2] || 'public'));
  server.listen(Number(process.argv[3] || 8080), '127.0.0.1');
}
