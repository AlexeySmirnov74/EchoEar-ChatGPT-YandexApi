"""Check fake TLS bootstrap, authorization, and the real voice hello offline."""
import base64
import http.client
import json
import os
import socket
import ssl
import struct

ctx = ssl._create_unverified_context()
secret = open('/run/credentials/ota_secret', encoding='ascii').read().strip()
c = http.client.HTTPSConnection('127.0.0.1', 18444, context=ctx, timeout=8)
c.request('POST', '/' + secret, b'{}')
r = c.getresponse()
assert r.status == 200
bootstrap = json.loads(r.read())
token = bootstrap['websocket']['token']
assert bootstrap['websocket']['url'].startswith('wss://')
assert bootstrap['server_time']['timezone_offset'] == 300
c.close()


def upgrade(auth):
    s = ctx.wrap_socket(socket.create_connection(('127.0.0.1', 18444), timeout=8), server_hostname='localhost')
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(('GET /xiaozhi/ws HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n'
               f'Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\nProtocol-Version: 3\r\nDevice-Id: test-device\r\nAuthorization: Bearer {auth}\r\n\r\n').encode())
    response = b''
    while not response.endswith(b'\r\n\r\n'):
        part = s.recv(1)
        if not part or len(response) > 16384:
            raise AssertionError('invalid handshake')
        response += part
    return s, response


s, response = upgrade('incorrect')
assert response.startswith(b'HTTP/1.1 403')
s.close()
s, response = upgrade(token)
assert response.startswith(b'HTTP/1.1 101')
payload = json.dumps({'type': 'hello', 'version': 3, 'transport': 'websocket', 'features': {'persistent_idle': True}}).encode()
assert len(payload) < 126
mask = os.urandom(4)
s.sendall(bytes([0x81, 0x80 | len(payload)]) + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))


def exact(n):
    data = b''
    while len(data) < n:
        part = s.recv(n - len(data))
        if not part:
            raise EOFError
        data += part
    return data


first, second = exact(2)
size = second & 127
if size == 126:
    size = struct.unpack('!H', exact(2))[0]
elif size == 127:
    size = struct.unpack('!Q', exact(8))[0]
assert first & 15 == 1 and not second & 128 and size < 65536
assert json.loads(exact(size))['type'] == 'hello'
s.close()
print('TLS bootstrap, rejection, authenticated WebSocket and voice hello verified.')
