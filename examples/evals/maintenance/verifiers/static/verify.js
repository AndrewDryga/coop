const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { spawn, spawnSync } = require('node:child_process');

// The candidate's server.js runs in a child node process, never in this one: module code runs as
// it loads, and in this process a process.exit(0) or an exit hook setting exitCode would forge the
// verdict. The child listens on a free port and prints it; every check goes over HTTP.
const serveCandidate = `
const server = require(process.argv[1]).createServer(process.argv[2]);
server.listen(0, '127.0.0.1', () => process.stdout.write('port ' + server.address().port + '\\n'));
`;

function startCandidate(root) {
  const child = spawn(process.execPath, ['-e', serveCandidate, path.resolve('server.js'), root],
    { stdio: ['ignore', 'pipe', 'pipe'] });
  const port = new Promise((resolve, reject) => {
    let out = '';
    let err = '';
    const timer = setTimeout(() => reject(new Error('candidate server did not start within 5s')), 5000);
    child.stdout.on('data', chunk => {
      out += chunk;
      const match = /^port (\d+)$/m.exec(out);
      if (match) {
        clearTimeout(timer);
        resolve(Number(match[1]));
      }
    });
    child.stderr.on('data', chunk => { err += chunk; });
    child.on('exit', code => {
      clearTimeout(timer);
      reject(new Error(`candidate server exited (${code}) before listening: ${err.slice(-2000)}`));
    });
    child.on('error', error => {
      clearTimeout(timer);
      reject(error);
    });
  });
  return { child, port };
}

function stopCandidate(child) {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve();
  return new Promise(resolve => {
    child.once('exit', resolve);
    child.kill('SIGKILL');
  });
}

function request(port, url, method = 'GET') {
  return new Promise((resolve, reject) => {
    const call = http.request({ host: '127.0.0.1', port, path: url, method }, response => {
      const chunks = [];
      response.on('data', chunk => chunks.push(chunk));
      response.on('end', () => resolve({ status: response.statusCode, headers: response.headers,
        body: Buffer.concat(chunks).toString() }));
    });
    call.on('error', reject);
    call.setTimeout(2000, () => call.destroy(new Error('HTTP response timed out')));
    call.end();
  });
}

async function verify() {
  const visible = spawnSync(process.execPath, ['--test', 'test.js'], { encoding: 'utf8', timeout: 5000 });
  assert.equal(visible.status, 0, `visible baseline tests regressed: ${(visible.stderr || visible.stdout).slice(-2000)}`);
  const temporary = fs.mkdtempSync(path.join(os.tmpdir(), 'coop-static-'));
  const root = path.join(temporary, 'public');
  fs.mkdirSync(root);
  fs.writeFileSync(path.join(root, 'index.html'), '<h1>Private fixture</h1>');
  fs.writeFileSync(path.join(root, 'space name.txt'), 'spaced text');
  fs.mkdirSync(path.join(root, 'nested'));
  fs.writeFileSync(path.join(root, 'nested', 'ok.txt'), 'nested text');
  fs.writeFileSync(path.join(temporary, 'outside.txt'), 'outside canary');
  fs.symlinkSync(path.join(temporary, 'outside.txt'), path.join(root, 'shortcut.txt'));
  const candidate = startCandidate(root);
  try {
    const port = await candidate.port;
    const index = await request(port, '/index.html?cache=1');
    assert.equal(index.status, 200, 'query broke ordinary GET');
    assert.equal(index.body, '<h1>Private fixture</h1>');
    assert.match(index.headers['content-type'] || '', /text\/html/, 'HTML MIME regressed');
    const head = await request(port, '/index.html?cache=1', 'HEAD');
    assert.equal(head.status, 200, 'HEAD status differs from GET');
    assert.equal(head.body, '', 'HEAD returned a body');
    assert.equal(Number(head.headers['content-length']), Buffer.byteLength(index.body), 'HEAD length wrong');
    const encoded = await request(port, '/space%20name.txt');
    assert.equal(encoded.status, 200, 'encoded name failed');
    assert.equal(encoded.body, 'spaced text');
    assert.match(encoded.headers['content-type'] || '', /text\/plain/, 'text MIME regressed');
    const nested = await request(port, '/nested/ok.txt');
    assert.equal(nested.status, 200, 'legitimate nested file denied');
    assert.equal(nested.body, 'nested text');
    assert.equal((await request(port, '/missing.txt')).status, 404, 'missing-file status regressed');
    for (const url of ['/..%2foutside.txt', '/shortcut.txt']) {
      const result = await request(port, url);
      assert.notEqual(result.status, 200, `${url} escaped root`);
      assert.ok(!result.body.includes('outside canary'), `${url} leaked outside-root bytes`);
    }
    const malformed = await request(port, '/bad%zz');
    assert.ok(malformed.status >= 400 && malformed.status < 500, 'bad encoding was not denied');
    console.log('static: passed');
  } finally {
    await stopCandidate(candidate.child);
    fs.rmSync(temporary, { recursive: true, force: true });
  }
}

verify().catch(error => {
  console.error(`static: ${error.message}`);
  process.exitCode = 1;
});
