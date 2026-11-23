'use strict';

// Authenticated XiaoZhi bootstrap and WebSocket relay. No request logging.
const http = require('node:http');
const https = require('node:https');
const fs = require('node:fs');
const crypto = require('node:crypto');
const path = require('node:path');
const {isAdminPath, proxyAdmin} = require('./admin-proxy');

function equalSecret(a, b) {
  const digest = value => crypto.createHash('sha256').update(value, 'utf8').digest();
  return crypto.timingSafeEqual(digest(a || ''), digest(b));
}

function loadTlsCredentials(credentials, env = process.env, readFile = fs.readFileSync) {
  const certPath = env.TLS_CERT_PATH;
  const keyPath = env.TLS_KEY_PATH;
  if (Boolean(certPath) !== Boolean(keyPath)) throw new Error('TLS_CERT_PATH and TLS_KEY_PATH must be configured together');
  const read = filename => readFile(filename, 'utf8').trim();
  return {
    cert: read(certPath || path.join(credentials, 'tls_cert')),
    key: read(keyPath || path.join(credentials, 'tls_key'))
  };
}

function resolveBindAddress(env = process.env) {
  const address = env.GATEWAY_BIND_ADDRESS ?? '127.0.0.1';
  if (address !== '127.0.0.1' && address !== '0.0.0.0') throw new Error('Invalid GATEWAY_BIND_ADDRESS');
  return address;
}

function createGateway(config, tls) {
  if (!/^\/[A-Za-z0-9_-]{32,}$/.test(config.otaPath)) throw new Error('Invalid OTA secret');
  if (!/^[A-Za-z0-9_-]{32,}$/.test(config.token)) throw new Error('Invalid websocket token');
  if (typeof config.hostname !== 'string' || !/^[a-zA-Z0-9.-]+$/.test(config.hostname)) throw new Error('Invalid hostname');
  if (!Number.isInteger(config.publicPort) || config.publicPort < 1 || config.publicPort > 65535) throw new Error('Invalid public port');
  if (!Number.isInteger(config.upstreamPort) || config.upstreamPort < 1 || config.upstreamPort > 65535) throw new Error('Invalid upstream port');
  const adminSocketPath = config.adminSocketPath ?? '/run/echowear-admin/admin.sock';
  if (typeof adminSocketPath !== 'string' || !path.posix.isAbsolute(adminSocketPath) || adminSocketPath.includes('\0')) throw new Error('Invalid admin socket path');
  const adminTimeoutMs = config.adminTimeoutMs ?? 15000;
  if (!Number.isInteger(adminTimeoutMs) || adminTimeoutMs < 50 || adminTimeoutMs > 15000) throw new Error('Invalid admin timeout');
  const maxSessions = config.maxSessions ?? 2;
  if (!Number.isInteger(maxSessions) || maxSessions < 1 || maxSessions > 2) throw new Error('Invalid session limit');
  let activeSessions = 0;
  let activeAdminRequests = 0;
  const wsUrl = `wss://${config.hostname}:${config.publicPort}/xiaozhi/ws`;
  const handler = (req, res) => {
    if (isAdminPath(req.url)) {
      if (activeAdminRequests >= 4) {
        res.writeHead(503, {'Cache-Control': 'no-store', 'Connection': 'close'});
        res.end();
        req.resume();
        return;
      }
      activeAdminRequests++;
      res.once('close', () => { activeAdminRequests--; });
      proxyAdmin(req, res, adminSocketPath, adminTimeoutMs);
      return;
    }
    if (req.method !== 'POST' || !equalSecret(req.url, config.otaPath)) {
      res.writeHead(404, {'Cache-Control': 'no-store', 'Connection': 'close'});
      res.end();
      return;
    }
    // Firmware POSTs device metadata; it is intentionally not recorded.
    let bytes = 0;
    const deadline = setTimeout(() => req.destroy(), 15000);
    deadline.unref();
    res.once('close', () => clearTimeout(deadline));
    req.on('error', () => {});
    req.on('data', chunk => { bytes += chunk.length; if (bytes > 65536) req.destroy(); });
    req.on('end', () => {
      const now = Date.now();
      // OTA firmware adds the offset itself and stores a local-time epoch.
      // Refuse visibly invalid host clocks rather than asserting false readiness.
      const server_time = now >= 1700000000000 && now < 4102358400000
        ? {timestamp: now, timezone_offset: 300} : undefined;
      const body = JSON.stringify({websocket: {url: wsUrl, token: config.token, version: 3}, server_time});
      res.writeHead(200, {'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body), 'Cache-Control': 'no-store', 'Connection': 'close'});
      res.end(body);
    });
  };
  const server = tls ? https.createServer({ ...tls, minVersion: 'TLSv1.2', maxHeaderSize: 8192, handshakeTimeout: 10000 }, handler)
                     : http.createServer({maxHeaderSize: 8192}, handler);
  // Bound unauthenticated sockets, including incomplete TLS handshakes.
  server.maxConnections = 32;
  server.headersTimeout = 10000;
  server.requestTimeout = 15000;
  server.on('clientError', (_err, socket) => socket.destroy());
  server.on('upgrade', (req, client, clientHead) => {
    client.on('error', () => {});
    const reject = status => {
      if (!client.destroyed) {
        client.end(`HTTP/1.1 ${status}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
        const deadline = setTimeout(() => client.destroy(), 5000);
        deadline.unref();
        client.once('close', () => clearTimeout(deadline));
      }
    };
    if (req.method !== 'GET' || req.url !== '/xiaozhi/ws' || !equalSecret(req.headers.authorization, `Bearer ${config.token}`)) {
      reject('403 Forbidden');
      return;
    }
    if (activeSessions >= maxSessions) { reject('503 Service Unavailable'); return; }
    // Count from authorization, including a pending upstream handshake, until
    // the client socket actually closes. No idle timeout on an established call.
    activeSessions++;
    client.once('close', () => { activeSessions--; });
    // Drain early websocket bytes while the upstream handshake is pending so a
    // disconnected peer cannot occupy a slot until timeout. Bound the buffer.
    const earlyChunks = [];
    let earlySize = clientHead.length;
    if (earlySize > 65536) { client.destroy(); return; }
    const captureEarly = chunk => {
      earlySize += chunk.length;
      if (earlySize > 65536) { client.destroy(); return; }
      earlyChunks.push(chunk);
    };
    client.on('data', captureEarly);
    client.once('end', () => client.destroy());
    // Only protocol headers needed by StackChan; never forward the bearer.
    const headers = { host: `127.0.0.1:${config.upstreamPort}`, connection: 'Upgrade', upgrade: 'websocket' };
    for (const name of ['sec-websocket-key', 'sec-websocket-version', 'sec-websocket-protocol', 'protocol-version', 'device-id', 'client-id']) {
      if (typeof req.headers[name] === 'string') headers[name] = req.headers[name];
    }
    const upstream = http.request({host: '127.0.0.1', port: config.upstreamPort, path: '/xiaozhi/ws', method: 'GET', headers, timeout: 10000});
    const fail = () => reject('502 Bad Gateway');
    upstream.once('timeout', () => upstream.destroy());
    upstream.once('error', fail);
    upstream.once('response', response => { response.resume(); fail(); });
    client.once('error', () => upstream.destroy());
    client.once('close', () => upstream.destroy());
    upstream.once('upgrade', (response, remote, remoteHead) => {
      if (client.destroyed) { remote.destroy(); return; }
      const h = response.headers;
      if (typeof h['sec-websocket-accept'] !== 'string') { remote.destroy(); fail(); return; }
      let result = 'HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n';
      for (const name of ['sec-websocket-accept', 'sec-websocket-protocol']) {
        if (typeof h[name] === 'string') result += `${name}: ${h[name]}\r\n`;
      }
      client.write(result + '\r\n');
      if (remoteHead.length) client.write(remoteHead);
      if (clientHead.length) remote.write(clientHead);
      client.removeListener('data', captureEarly);
      for (const chunk of earlyChunks) remote.write(chunk);
      remote.on('error', () => client.destroy());
      client.on('error', () => remote.destroy());
      remote.on('close', () => client.destroy());
      client.on('close', () => remote.destroy());
      client.pipe(remote).pipe(client);
    });
    upstream.end();
  });
  return server;
}

if (require.main === module) {
  const credentials = process.env.CREDENTIALS_DIRECTORY;
  if (!credentials) throw new Error('systemd credentials are required');
  const read = name => fs.readFileSync(path.join(credentials, name), 'utf8').trim();
  const config = {
    hostname: process.env.GATEWAY_HOSTNAME,
    publicPort: Number(process.env.GATEWAY_PORT),
    upstreamPort: Number(process.env.UPSTREAM_PORT || '12800'),
    otaPath: '/' + read('ota_secret'), token: read('ws_token'),
    adminSocketPath: process.env.ADMIN_SOCKET_PATH || '/run/echowear-admin/admin.sock'
  };
  const server = createGateway(config, loadTlsCredentials(credentials));
  server.listen(config.publicPort, resolveBindAddress());
}

module.exports = {createGateway, loadTlsCredentials, resolveBindAddress};
