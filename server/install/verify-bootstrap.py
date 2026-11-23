"""Check OTA over loopback TLS without logging the credentials or URL."""
import json
import socket
import ssl
from pathlib import Path


config = {}
for line in Path('/etc/echowear/config.env').read_text(encoding='utf-8').splitlines():
    key, value = line.split('=', 1)
    config[key] = value
domain = config['ECHOWEAR_DOMAIN']
secret = Path('/etc/echowear/gateway/ota_secret').read_text(encoding='ascii').strip()
expected_token = Path('/etc/echowear/gateway/ws_token').read_text(encoding='ascii').strip()
with socket.create_connection(('127.0.0.1', 18444), timeout=4) as raw:
    with ssl.create_default_context().wrap_socket(raw, server_hostname=domain) as conn:
        request = f'POST /{secret} HTTP/1.1\r\nHost: {domain}\r\nContent-Length: 0\r\nConnection: close\r\n\r\n'
        conn.sendall(request.encode('ascii'))
        response = bytearray()
        while len(response) < 32768:
            chunk = conn.recv(4096)
            if not chunk:
                break
            response.extend(chunk)
header, body = bytes(response).split(b'\r\n\r\n', 1)
if not header.startswith(b'HTTP/1.1 200 '):
    raise RuntimeError('Gateway bootstrap returned an unexpected status')
data = json.loads(body)
if data['websocket']['url'] != f'wss://{domain}:18444/xiaozhi/ws' or data['websocket']['token'] != expected_token:
    raise RuntimeError('Gateway bootstrap response does not match local settings')
if data['server_time']['timezone_offset'] != 300:
    raise RuntimeError('Unexpected timezone offset')
