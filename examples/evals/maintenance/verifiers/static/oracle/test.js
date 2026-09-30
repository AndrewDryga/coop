const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { createServer } = require('./server');

function request(server, url, method = 'GET') {
  return new Promise((resolve, reject) => {
    const call = http.request({ host: '127.0.0.1', port: server.address().port, path: url, method }, reply => {
      const chunks = [];
      reply.on('data', chunk => chunks.push(chunk));
      reply.on('end', () => resolve({ status: reply.statusCode, headers: reply.headers,
        body: Buffer.concat(chunks).toString() }));
    });
    call.on('error', reject);
    call.setTimeout(2000, () => call.destroy(new Error('HTTP response timed out')));
    call.end();
  });
}

test('serves the original index file', async () => {
  const server = createServer(path.join(__dirname, 'public'));
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    const response = await request(server, '/');
    assert.equal(response.status, 200);
    assert.match(response.body, /Ready/);
  } finally {
    await new Promise(resolve => server.close(resolve));
  }
});

test('query, HEAD, decoded names and root containment', async () => {
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'static-test-'));
  const root = path.join(temporary, 'public');
  fs.mkdirSync(root);
  fs.mkdirSync(path.join(root, 'nested'));
  fs.writeFileSync(path.join(root, 'index.html'), 'hello');
  fs.writeFileSync(path.join(root, 'space name.txt'), 'text');
  fs.writeFileSync(path.join(root, 'nested', 'ok.txt'), 'nested');
  fs.writeFileSync(path.join(temporary, 'outside.txt'), 'private');
  fs.symlinkSync(path.join(temporary, 'outside.txt'), path.join(root, 'link.txt'));
  const server = createServer(root);
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  try {
    assert.equal((await request(server, '/index.html?q=1')).body, 'hello');
    const head = await request(server, '/index.html?q=1', 'HEAD');
    assert.equal(head.status, 200);
    assert.equal(head.body, '');
    assert.equal(Number(head.headers['content-length']), 5);
    assert.equal((await request(server, '/space%20name.txt')).body, 'text');
    assert.equal((await request(server, '/nested/ok.txt')).body, 'nested');
    for (const url of ['/..%2foutside.txt', '/link.txt']) {
      const denied = await request(server, url);
      assert.notEqual(denied.status, 200);
      assert.ok(!denied.body.includes('private'));
    }
    const malformed = await request(server, '/bad%zz');
    assert.ok(malformed.status >= 400 && malformed.status < 500);
  } finally {
    await new Promise(resolve => server.close(resolve));
    fs.rmSync(temporary, { recursive: true, force: true });
  }
});
