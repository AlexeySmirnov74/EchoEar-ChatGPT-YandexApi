"""Minimal offline WebSocket peer for the OpenAI Realtime session handshake."""
import base64
import hashlib
import json
import socketserver
import struct


def exact(conn, count):
    result = b''
    while len(result) < count:
        part = conn.recv(count - len(result))
        if not part:
            raise EOFError
        result += part
    return result


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        conn = self.request
        conn.settimeout(12)
        header = b''
        while not header.endswith(b'\r\n\r\n') and len(header) < 16384:
            header += exact(conn, 1)
        headers = {}
        for line in header.split(b'\r\n')[1:]:
            if b':' in line:
                name, value = line.split(b':', 1)
                headers[name.lower()] = value.strip()
        accept = base64.b64encode(hashlib.sha1(headers[b'sec-websocket-key'] + b'258EAFA5-E914-47DA-95CA-C5AB0DC85B11').digest())
        conn.sendall(b'HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ' + accept + b'\r\n\r\n')
        print('Mock Realtime connected.', flush=True)
        message = b''
        while True:
            try:
                first, second = exact(conn, 2)
                size = second & 127
                if size == 126:
                    size = struct.unpack('!H', exact(conn, 2))[0]
                elif size == 127:
                    size = struct.unpack('!Q', exact(conn, 8))[0]
                if size > 262144 or not second & 128:
                    return
                mask = exact(conn, 4)
                payload = bytes(b ^ mask[i % 4] for i, b in enumerate(exact(conn, size)))
                if first & 15 == 8:
                    return
                if first & 15 not in (0, 1):
                    continue
                message += payload
                if len(message) > 262144:
                    return
                if not first & 128:
                    continue
                data, message = message, b''
                if json.loads(data).get('type') == 'session.update':
                    event = json.dumps({'type': 'session.updated', 'session': {'audio': {'input': {'turn_detection': None}}}}, separators=(',', ':')).encode()
                    conn.sendall(bytes([0x81, len(event)]) + event)
                    print('Mock session updated.', flush=True)
            except (EOFError, OSError, ValueError):
                return


class Server(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


with Server(('127.0.0.1', 19090), Handler) as server:
    server.serve_forever()
