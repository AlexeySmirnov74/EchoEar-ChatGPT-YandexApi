'use strict';

const {test, before, after} = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const https = require('node:https');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {execFileSync} = require('node:child_process');
const {createGateway} = require('./gateway-alarm-clock');
const {MAX_REQUEST, MAX_RESPONSE} = require('./admin-proxy');

let tempDir, tls;
before(() => {
  tempDir = fs.mkdtempSync(path.join(os.tmpdir(), 'echo-admin-test-'));
  const key = path.join(tempDir, 'key.pem'), cert = path.join(tempDir, 'cert.pem');
  execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key,
    '-out', cert, '-days', '1', '-subj', '/CN=localhost'], {stdio:'ignore'});
  tls = {key:fs.readFileSync(key), cert:fs.readFileSync(cert)};
});
after(() => fs.rmSync(tempDir, {recursive:true, force:true}));

const listen = server => new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
let socketSequence=0;
const listenUnix = server => new Promise(resolve => {
  const name=path.join(tempDir, `admin-${socketSequence++}.sock`);
  server.listen(name, () => resolve(name));
});
const close = server => new Promise(resolve => {server.closeAllConnections(); server.close(resolve);});
const config = adminSocketPath => ({otaPath:'/'+'a'.repeat(64), token:'b'.repeat(64), hostname:'example.test', publicPort:18444, upstreamPort:12800, adminSocketPath, adminTimeoutMs:200});
function request(port, url, {method='GET', headers={}, body='', secure=true} = {}) {
  return new Promise((resolve, reject) => {
    const req = (secure ? https : http).request({host:'127.0.0.1', port, path:url, method, headers, rejectUnauthorized:false}, res => {
      const chunks=[]; res.on('data', c=>chunks.push(c));
      res.on('end',()=>resolve({status:res.statusCode, headers:res.headers, body:Buffer.concat(chunks).toString()}));
    });
    req.on('error', reject); req.end(body);
  });
}

test('HTTPS proxy keeps cookies and location and replaces all client forwarding headers', async () => {
  let seen;
  const backend = http.createServer((req,res)=>{
    const chunks=[]; req.on('data',c=>chunks.push(c)); req.on('end',()=>{
      seen={url:req.url, method:req.method, headers:req.headers, body:Buffer.concat(chunks).toString()};
      res.writeHead(302, {'set-cookie':'session=example; Secure; HttpOnly; SameSite=Strict', location:'/admin/home', 'content-security-policy':"default-src 'self'"}); res.end('ok');
    });
  });
  const bp=await listenUnix(backend); const gateway=createGateway(config(bp),tls); const gp=await listen(gateway);
  try {
    const result=await request(gp,'/admin/login?mode=1',{method:'POST',body:'a=b',headers:{
      cookie:'before=1', origin:'https://example.test:18444', 'content-type':'application/x-www-form-urlencoded',
      'x-forwarded-for':'203.0.113.99', 'x-forwarded-proto':'http', 'x-forwarded-host':'evil.test',
      forwarded:'for=203.0.113.99', 'x-real-ip':'203.0.113.99', 'cf-connecting-ip':'203.0.113.99',
      'x-original-url':'/bad', authorization:'Bearer unrelated'
    }});
    assert.equal(result.status,302); assert.equal(result.headers.location,'/admin/home');
    assert.match(result.headers['set-cookie'][0],/Secure; HttpOnly; SameSite=Strict/);
    assert.equal(result.headers['content-security-policy'],"default-src 'self'");
    assert.equal(result.headers['cache-control'],'no-store'); assert.equal(result.body,'ok');
    assert.equal(seen.url,'/admin/login?mode=1'); assert.equal(seen.body,'a=b');
    assert.equal(seen.headers.cookie,'before=1'); assert.equal(seen.headers.origin,'https://example.test:18444');
    assert.equal(seen.headers['x-forwarded-for'],'127.0.0.1'); assert.equal(seen.headers['x-forwarded-proto'],'https');
    for(const h of ['forwarded','x-real-ip','cf-connecting-ip','x-forwarded-host','x-original-url','authorization']) assert.equal(seen.headers[h],undefined);
  } finally {await close(gateway);await close(backend);}
});

test('rejects plaintext, traversal, methods and oversized request before forwarding', async () => {
  let count=0; const backend=http.createServer((_req,res)=>{count++;res.end('bad');});
  const bp=await listenUnix(backend); const secure=createGateway(config(bp),tls), plain=createGateway(config(bp));
  const sp=await listen(secure), pp=await listen(plain);
  try {
    assert.equal((await request(pp,'/admin',{secure:false})).status,403);
    assert.equal((await request(sp,'/admin/%2e%2e/secret')).status,400);
    assert.equal((await request(sp,'/admin/x',{method:'PUT'})).status,405);
    assert.equal((await request(sp,'/admin/x',{method:'POST',body:'x'.repeat(MAX_REQUEST+1)})).status,413);
    assert.equal(count,0);
  } finally {await close(secure);await close(plain);await close(backend);}
});

test('upstream failure, response limit and deadline return safe errors', async () => {
  const backend=http.createServer((req,res)=>{
    if(req.url==='/admin/large'){res.end('x'.repeat(MAX_RESPONSE+1));return;}
    if(req.url==='/admin/hang')return;
    req.socket.destroy();
  });
  const bp=await listenUnix(backend), gateway=createGateway(config(bp),tls), gp=await listen(gateway);
  try {
    assert.equal((await request(gp,'/admin/fail')).status,502);
    assert.equal((await request(gp,'/admin/large')).status,502);
    assert.equal((await request(gp,'/admin/hang')).status,504);
  } finally {await close(gateway);await close(backend);}
});

test('limits concurrent stalled admin requests and releases slots on timeout', async () => {
  const backend=http.createServer((_req,_res)=>{});
  const bp=await listenUnix(backend), gateway=createGateway(config(bp),tls), gp=await listen(gateway);
  try {
    const statuses=await Promise.all(Array.from({length:5},()=>request(gp,'/admin/hang')));
    assert.deepEqual(statuses.map(x=>x.status).sort(), [503,504,504,504,504]);
    assert.equal((await request(gp,'/admin/hang')).status,504);
  } finally {await close(gateway);await close(backend);}
});
