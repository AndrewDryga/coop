const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const path = require('node:path');
const { createServer } = require('./server');

test('serves the original index file', async () => {
  const server = createServer(path.join(__dirname, 'public'));
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const response = await new Promise((resolve, reject) => {
      http.get({ host: '127.0.0.1', port: server.address().port, path: '/' }, reply => {
        const chunks = [];
        reply.on('data', chunk => chunks.push(chunk));
        reply.on('end', () => resolve({ status: reply.statusCode, body: Buffer.concat(chunks).toString() }));
      }).on('error', reject);
    });
    assert.equal(response.status, 200);
    assert.match(response.body, /Ready/);
  } finally {
    await new Promise(resolve => server.close(resolve));
  }
});
