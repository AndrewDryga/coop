const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');

function createServer(root) {
  return http.createServer((request, response) => {
    const file = path.join(root, request.url === '/' ? 'index.html' : request.url);
    fs.readFile(file, (error, bytes) => {
      if (error) {
        response.writeHead(error.code === 'ENOENT' ? 404 : 500);
        response.end();
        return;
      }
      const type = file.endsWith('.html') ? 'text/html' : 'text/plain';
      response.writeHead(200, { 'Content-Type': type });
      response.end(bytes);
    });
  });
}

module.exports = { createServer };

if (require.main === module) {
  const server = createServer(path.resolve(process.argv[2] || 'public'));
  server.listen(Number(process.argv[3] || 8080), '127.0.0.1');
}
