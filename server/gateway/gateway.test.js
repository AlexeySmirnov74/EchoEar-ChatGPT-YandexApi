'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const net = require('node:net');
const { once } = require('node:events');
const { createGateway, loadTlsCredentials, resolveBindAddress } = require('./gateway-alarm-clock');

const secret = 'a'.repeat(64);
const token = 'b'.repeat(64);
const listen = server => new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
const close = server => new Promise(resolve => server.close(resolve));

test('bind address defaults to loopback and requires explicit public exposure', () => {
  assert.equal(resolveBindAddress({}), '127.0.0.1');
  assert.equal(resolveBindAddress({GATEWAY_BIND_ADDRESS: '127.0.0.1'}), '127.0.0.1');
  assert.equal(resolveBindAddress({GATEWAY_BIND_ADDRESS: '0.0.0.0'}), '0.0.0.0');
  for (const value of ['', '::', 'localhost', '192.0.2.1']) {
    assert.throws(() => resolveBindAddress({GATEWAY_BIND_ADDRESS: value}), /Invalid GATEWAY_BIND_ADDRESS/);
  }
});

test('TLS credentials use the systemd defaults or both explicitly configured paths', () => {
  const files = new Map([
    [require('node:path').join('secret-dir', 'tls_cert'), 'default-cert\n'],
    [require('node:path').join('secret-dir', 'tls_key'), 'default-key\n'],
    ['/run/certs/live/example/fullchain.pem', 'mounted-cert\n'],
    ['/run/certs/live/example/privkey.pem', 'mounted-key\n']
  ]);
  const readFile = filename => {
    assert.ok(files.has(filename));
    return files.get(filename);
  };
  assert.deepEqual(loadTlsCredentials('secret-dir', {}, readFile), {cert: 'default-cert', key: 'default-key'});
  assert.deepEqual(loadTlsCredentials('secret-dir', {TLS_CERT_PATH: '/run/certs/live/example/fullchain.pem', TLS_KEY_PATH: '/run/certs/live/example/privkey.pem'}, readFile), {cert: 'mounted-cert', key: 'mounted-key'});
  assert.throws(() => loadTlsCredentials('secret-dir', {TLS_CERT_PATH: '/run/certs/live/example/fullchain.pem'}, readFile), /configured together/);
  assert.throws(() => loadTlsCredentials('secret-dir', {TLS_KEY_PATH: '/run/certs/live/example/privkey.pem'}, readFile), /configured together/);
});
function request(port, method, url) {
  return new Promise((resolve, reject) => {
    const req = http.request({host: '127.0.0.1', port, method, path: url}, res => {
      const chunks = [];
      res.on('data', chunk => chunks.push(chunk));
      res.on('end', () => resolve({status: res.statusCode, body: Buffer.concat(chunks).toString()}));
    });
    req.on('error', reject);
    req.end();
  });
}
function upgrade(port, credential, resource = '/xiaozhi/ws') {
  return new Promise((resolve, reject) => {
    const socket = net.connect(port, '127.0.0.1');
    socket.once('error', reject);
    socket.once('connect', () => {
      socket.write(`GET ${resource} HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: aGVsbG9IZWxsb0hlbGxvIQ==\r\nProtocol-Version: 3\r\nDevice-Id: 01:02:03:04:05:06\r\nAuthorization: ${credential}\r\n\r\n`);
    });
    let data = Buffer.alloc(0);
    const receive = chunk => {
      data = Buffer.concat([data, chunk]);
      if (data.includes('\r\n\r\n')) {
        socket.removeListener('data', receive);
        resolve({socket, response: data.toString()});
      }
    };
    socket.on('data', receive);
  });
}

test('bootstrap and websocket authorization, forwarding, and secret isolation', async () => {
  let backendCount = 0;
  let seenHeaders;
  const sockets = new Set();
  const backend = http.createServer();
  backend.on('connection', socket => { sockets.add(socket); socket.on('close', () => sockets.delete(socket)); });
  backend.on('upgrade', (req, socket, head) => {
    backendCount++;
    seenHeaders = req.headers;
    socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n');
    if (head.length) socket.write(head);
    socket.on('data', data => socket.write(data));
  });
  const backendPort = await listen(backend);
  const gateway = createGateway({otaPath: '/' + secret, token, hostname: 'example.com', publicPort: 18443, upstreamPort: backendPort});
  const port = await listen(gateway);
  try {
    assert.equal((await request(port, 'POST', '/unknown')).status, 404);
    assert.equal((await request(port, 'GET', '/' + secret)).status, 404);
    assert.equal((await request(port, 'POST', '/' + secret + '?q=1')).status, 404);
    const response = await request(port, 'POST', '/' + secret);
    assert.equal(response.status, 200);
    const received = JSON.parse(response.body);
    assert.deepEqual(received.websocket, {url: 'wss://example.com:18443/xiaozhi/ws', token, version: 3});
    assert.equal(received.server_time.timezone_offset, 300);
    assert.ok(Math.abs(received.server_time.timestamp - Date.now()) < 3000);
    const denied = await upgrade(port, 'Bearer invalid');
    assert.match(denied.response, /^HTTP\/1\.1 403/);
    denied.socket.destroy();
    const wrongPath = await upgrade(port, 'Bearer ' + token, '/unrelated');
    assert.match(wrongPath.response, /^HTTP\/1\.1 403/);
    wrongPath.socket.destroy();
    assert.equal(backendCount, 0);
    const allowed = await upgrade(port, 'Bearer ' + token);
    assert.match(allowed.response, /^HTTP\/1\.1 101/);
    assert.equal(backendCount, 1);
    assert.equal(seenHeaders.authorization, undefined);
    assert.equal(seenHeaders['protocol-version'], '3');
    assert.equal(seenHeaders['device-id'], '01:02:03:04:05:06');
    const echoed = new Promise(resolve => allowed.socket.once('data', resolve));
    allowed.socket.write(Buffer.from([0x82, 0x00, 0xff, 0x42]));
    assert.deepEqual(await echoed, Buffer.from([0x82, 0x00, 0xff, 0x42]));
    allowed.socket.destroy();
  } finally {
    for (const socket of sockets) socket.destroy();
    gateway.closeAllConnections();
    await close(gateway);
    await close(backend);
  }
});

test('session limit covers pending handshakes and releases on disconnect and backend failure', async () => {
  let backendCount = 0;
  const backendSockets = new Set();
  const backend = http.createServer();
  backend.on('connection', socket => { backendSockets.add(socket); socket.on('close', () => backendSockets.delete(socket)); });
  let signalPending;
  const pendingReached = new Promise(resolve => { signalPending = resolve; });
  backend.on('upgrade', (_req, socket) => {
    backendCount++;
    if (backendCount === 1) { signalPending(); return; }
    // Simulate a backend failure before its websocket handshake completes.
    socket.destroy();
  });
  const backendPort = await listen(backend);
  const gateway = createGateway({otaPath: '/' + secret, token, hostname: 'example.com', publicPort: 18443, upstreamPort: backendPort, maxSessions: 1});
  const port = await listen(gateway);
  const clients = new Set();
  gateway.on('connection', socket => { clients.add(socket); socket.once('close', () => clients.delete(socket)); });
  let pending;
  try {
    pending = net.connect(port, '127.0.0.1');
    await once(pending, 'connect');
    pending.write(`GET /xiaozhi/ws HTTP/1.1\r\nHost: example.com\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: aGVsbG9IZWxsb0hlbGxvIQ==\r\nAuthorization: Bearer ${token}\r\n\r\n`);
    await pendingReached;
    const blocked = await upgrade(port, 'Bearer ' + token);
    assert.match(blocked.response, /^HTTP\/1\.1 503/);
    blocked.socket.destroy();
    assert.equal(backendCount, 1);
    const serverSidePending = [...clients][0];
    const closed = once(serverSidePending, 'close');
    pending.destroy();
    await closed;
    const failed = await upgrade(port, 'Bearer ' + token);
    assert.match(failed.response, /^HTTP\/1\.1 502/);
    // Wait for the gateway side to close, ensuring the slot is released.
    const failedServerSide = [...clients].find(socket => socket.remotePort === failed.socket.localPort);
    const failedClosed = failedServerSide && !failedServerSide.destroyed ? once(failedServerSide, 'close') : Promise.resolve();
    failed.socket.destroy();
    await failedClosed;
    const retry = await upgrade(port, 'Bearer ' + token);
    assert.match(retry.response, /^HTTP\/1\.1 502/);
    retry.socket.destroy();
    assert.equal(backendCount, 3);
  } finally {
    if (pending) pending.destroy();
    for (const socket of clients) socket.destroy();
    for (const socket of backendSockets) socket.destroy();
    gateway.closeAllConnections();
    await close(gateway);
    await close(backend);
  }
});

test('established sessions release their slots on close', async () => {
  const backendSockets = new Set();
  const backend = http.createServer();
  backend.on('connection', socket => { backendSockets.add(socket); socket.once('close', () => backendSockets.delete(socket)); });
  backend.on('upgrade', (_req, socket) => socket.write('HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: test\r\n\r\n'));
  const backendPort = await listen(backend);
  const gateway = createGateway({otaPath: '/' + secret, token, hostname: 'example.com', publicPort: 18443, upstreamPort: backendPort});
  const port = await listen(gateway);
  const connections = new Set();
  gateway.on('connection', socket => { connections.add(socket); socket.once('close', () => connections.delete(socket)); });
  const clients = [];
  try {
    for (let i = 0; i < 2; i++) {
      const connection = await upgrade(port, 'Bearer ' + token);
      clients.push(connection.socket);
      assert.match(connection.response, /^HTTP\/1\.1 101/);
    }
    const blocked = await upgrade(port, 'Bearer ' + token);
    clients.push(blocked.socket);
    assert.match(blocked.response, /^HTTP\/1\.1 503/);
    const peer = [...connections].find(socket => socket.remotePort === clients[0].localPort);
    const closed = once(peer, 'close');
    clients[0].destroy();
    await closed;
    const retry = await upgrade(port, 'Bearer ' + token);
    clients.push(retry.socket);
    assert.match(retry.response, /^HTTP\/1\.1 101/);
  } finally {
    for (const socket of clients) socket.destroy();
    for (const socket of connections) socket.destroy();
    for (const socket of backendSockets) socket.destroy();
    gateway.closeAllConnections();
    await close(gateway);
    await close(backend);
  }
});
