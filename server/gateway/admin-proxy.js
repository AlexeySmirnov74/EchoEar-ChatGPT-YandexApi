'use strict';

const http = require('node:http');

const MAX_REQUEST = 64 * 1024;
const MAX_RESPONSE = 2 * 1024 * 1024;
const DEADLINE_MS = 15000;
const REQUEST_HEADERS = new Set([
  'accept', 'accept-language', 'content-type', 'cookie', 'origin', 'referer',
  'user-agent', 'x-csrf-token', 'x-requested-with',
  'sec-fetch-site', 'sec-fetch-mode', 'sec-fetch-dest'
]);
const HOP_HEADERS = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade', 'content-length'
]);

function isAdminPath(url) {
  return typeof url === 'string' && /^\/admin(?:\/|\?|$)/.test(url);
}

function validPath(url) {
  const path = url.split('?', 1)[0];
  if (path.includes('\\') || /%(?:2f|5c|2e|25)/i.test(path)) return false;
  return !path.split('/').some(part => part === '.' || part === '..');
}

function proxyAdmin(req, res, socketPath, timeoutMs = DEADLINE_MS) {
  // Plain HTTP is supported by the legacy gateway for tests, not for admin.
  if (!req.socket.encrypted) { res.writeHead(403, {'Connection': 'close'}); res.end(); return; }
  if (!['GET', 'HEAD', 'POST'].includes(req.method)) { res.writeHead(405, {'Connection': 'close'}); res.end(); return; }
  if (!validPath(req.url) || !req.socket.remoteAddress) { res.writeHead(400, {'Connection': 'close'}); res.end(); return; }
  let requestSize = 0;
  const chunks = [];
  let upstream;
  let finished = false;
  const reply = status => {
    if (finished || res.destroyed) return;
    finished = true;
    clearTimeout(deadline);
    if (upstream) upstream.destroy();
    res.writeHead(status, {'Cache-Control': 'no-store', 'Connection': 'close'});
    res.end();
    req.resume();
  };
  const deadline = setTimeout(() => reply(504), timeoutMs);
  deadline.unref();
  req.on('error', () => { if (upstream) upstream.destroy(); clearTimeout(deadline); });
  req.on('aborted', () => { if (upstream) upstream.destroy(); clearTimeout(deadline); });
  res.on('close', () => { clearTimeout(deadline); if (!finished && upstream) upstream.destroy(); });
  req.on('data', chunk => {
    requestSize += chunk.length;
    if (requestSize > MAX_REQUEST) { reply(413); return; }
    if (!finished) chunks.push(chunk);
  });
  req.on('end', () => {
    if (finished) return;
    const headers = {'host': 'localhost',
      'x-forwarded-for': req.socket.remoteAddress,
      'x-forwarded-proto': 'https',
      'content-length': String(requestSize),
      'connection': 'close'};
    const connectionTokens = String(req.headers.connection || '').toLowerCase().split(',').map(s => s.trim());
    for (const [key, value] of Object.entries(req.headers)) {
      if (REQUEST_HEADERS.has(key) && !connectionTokens.includes(key) && value !== undefined) headers[key] = value;
    }
    upstream = http.request({socketPath, path: req.url, method: req.method, headers}, response => {
      const parts = [];
      let size = 0;
      response.on('error', () => reply(502));
      response.on('data', chunk => {
        size += chunk.length;
        if (size > MAX_RESPONSE) { response.destroy(); reply(502); return; }
        parts.push(chunk);
      });
      response.on('end', () => {
        if (finished) return;
        finished = true;
        clearTimeout(deadline);
        const out = {};
        const connectionTokens = String(response.headers.connection || '').toLowerCase().split(',').map(s => s.trim());
        for (const [key, value] of Object.entries(response.headers)) {
          if (!HOP_HEADERS.has(key) && !connectionTokens.includes(key) && value !== undefined) out[key] = value;
        }
        out['cache-control'] = 'no-store';
        out['connection'] = 'close';
        out['content-length'] = String(size);
        res.writeHead(response.statusCode || 502, out);
        res.end(Buffer.concat(parts));
      });
    });
    upstream.on('error', () => reply(502));
    upstream.end(Buffer.concat(chunks));
  });
}

module.exports = {isAdminPath, proxyAdmin, MAX_REQUEST, MAX_RESPONSE};
