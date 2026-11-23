"""Check the local admin service via the gateway without creating a session."""
import socket
import ssl
from pathlib import Path

config = dict(line.split('=', 1) for line in Path('/etc/echowear/config.env').read_text(encoding='utf-8').splitlines())
domain = config['ECHOWEAR_DOMAIN']
with socket.create_connection(('127.0.0.1', 18444), timeout=4) as raw:
    with ssl.create_default_context().wrap_socket(raw, server_hostname=domain) as conn:
        conn.settimeout(4)
        conn.sendall(f'GET /admin/health HTTP/1.1\r\nHost: {domain}:18444\r\nConnection: close\r\n\r\n'.encode('ascii'))
        response = conn.recv(4096)
if not response.startswith(b'HTTP/1.1 200 '):
    raise RuntimeError('Admin health check is unavailable through the gateway')
