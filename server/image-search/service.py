"""Loopback-only experimental image search service; no request/secret logging."""
import http.server
import json
import multiprocessing
import os
import select
import socket
import socketserver
import struct
import sys
import threading
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import yandex_image_search as adapter

FRAME_BYTES = 240 * 240 * 2
MAX_BODY = 2048
DEADLINE = 20.0
SOCKET_PATH = '/run/echowear/decoder.sock'
SEARCH_SOCKET_PATH = '/run/echowear-images/search.sock'
NO_RESULTS = object()
NO_RESULTS_WIRE = b'\x01'


def read_exact(sock, size):
    chunks = bytearray()
    while len(chunks) < size:
        data = sock.recv(min(65536, size - len(chunks)))
        if not data:
            raise adapter.SearchError('Decoder disconnected')
        chunks.extend(data)
    return bytes(chunks)


def convert_over_socket(data):
    if not data or len(data) > adapter.MAX_IMAGE_BYTES:
        raise adapter.SearchError('Invalid image size')
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
            conn.settimeout(8)
            conn.connect(SOCKET_PATH)
            conn.sendall(struct.pack('!I', len(data)) + data)
            size = struct.unpack('!I', read_exact(conn, 4))[0]
            if size != FRAME_BYTES:
                raise adapter.SearchError('Decoder rejected image')
            return read_exact(conn, size)
    except (OSError, EOFError, struct.error) as exc:
        raise adapter.SearchError('Decoder unavailable') from exc


def worker(pipe, query, folder, key):
    try:
        frame = adapter.search_frame(query, folder, key, convert=convert_over_socket)
        if len(frame) != FRAME_BYTES:
            raise adapter.SearchError('Invalid frame')
        pipe.send_bytes(frame)
    except adapter.NoSearchResults:
        try:
            pipe.send_bytes(NO_RESULTS_WIRE)
        except (OSError, BrokenPipeError):
            pass
    except Exception:
        try:
            pipe.send_bytes(b'')
        except (OSError, BrokenPipeError):
            pass
    finally:
        pipe.close()


def client_closed(conn):
    try:
        if select.select([conn], [], [], 0)[0]:
            return conn.recv(1, socket.MSG_PEEK | socket.MSG_DONTWAIT) == b''
    except (OSError, ValueError):
        return True
    return False


def run_search(query, folder, key, conn, timeout=DEADLINE):
    # A separate process allows a hard deadline and cancellation even if DNS,
    # TLS, an upstream server or the decoder blocks; no work survives a client.
    ctx = multiprocessing.get_context('spawn')
    parent, child = ctx.Pipe(duplex=False)
    process = ctx.Process(target=worker, args=(child, query, folder, key), daemon=True)
    process.start()
    child.close()
    deadline = time.monotonic() + timeout
    try:
        while time.monotonic() < deadline:
            if client_closed(conn):
                return None
            if parent.poll(min(0.1, max(0, deadline - time.monotonic()))):
                try:
                    data = parent.recv_bytes(FRAME_BYTES)
                except (EOFError, OSError):
                    return None
                if data == NO_RESULTS_WIRE:
                    return NO_RESULTS
                return data if len(data) == FRAME_BYTES else None
            if not process.is_alive():
                return None
        return None
    finally:
        parent.close()
        if process.is_alive():
            process.terminate()
        process.join(timeout=1)
        if process.is_alive():
            process.kill()
            process.join(timeout=1)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def setup(self):
        super().setup()
        self.connection.settimeout(3)

    def log_message(self, format, *args):
        pass

    def reply(self, status, body=b'', content_type='text/plain'):
        self.send_response(status)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Connection', 'close')
        self.end_headers()
        self.close_connection = True
        if body:
            self.wfile.write(body)

    def do_POST(self):
        try:
            if self.path != '/search':
                return self.reply(404)
            if self.headers.get('Transfer-Encoding') or len(self.headers.get_all('Content-Length', [])) != 1:
                return self.reply(400)
            if self.headers.get('Content-Type', '').split(';', 1)[0].strip().lower() != 'application/json':
                return self.reply(415)
            length = self.headers.get('Content-Length', '')
            if not length.isascii() or not length.isdigit() or not 0 < int(length) <= MAX_BODY:
                return self.reply(400)
            raw = self.rfile.read(int(length))
            value = json.loads(raw)
            if not isinstance(value, dict) or set(value) != {'query'}:
                return self.reply(400)
            query = value['query']
            try:
                folder, key = self.server.credentials()
            except (OSError, ValueError, json.JSONDecodeError):
                return self.reply(503, b'Image search credentials unavailable')
            adapter.request_body(query, folder)
            result = run_search(query, folder, key, self.connection)
            if result is None:
                return self.reply(502, b'Image search unavailable')
            if result is NO_RESULTS:
                return self.reply(204)
            return self.reply(200, result, 'application/octet-stream')
        except (ValueError, UnicodeError, adapter.SearchError, OSError):
            try:
                self.reply(400, b'Invalid request')
            except OSError:
                pass


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, folder, key, concurrency=2, credential_loader=None):
        self.folder, self.key = folder, key
        self.credential_loader = credential_loader
        self.slots = threading.BoundedSemaphore(concurrency)
        super().__init__(address, Handler)

    def credentials(self):
        if self.credential_loader is not None:
            return self.credential_loader()
        return self.folder, self.key

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            try:
                request.sendall(b'HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\nConnection: close\r\n\r\n')
            except OSError:
                pass
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except BaseException:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()

    def handle_error(self, request, client_address):
        # HTTP input, search terms and paths must never leak through tracebacks.
        pass


def main():
    bundle_path = os.environ.get('ECHOWEAR_YANDEX_BUNDLE', '')
    if bundle_path:
        from secret_store import read_bundle
        credential_loader = lambda: read_bundle(bundle_path)
        folder, key = credential_loader()
    else:
        credential_loader = None
        folder = os.environ['YANDEX_FOLDER_ID']
        with open('/run/secrets/yandex_key', encoding='utf-8') as f:
            key = f.read(4097).strip()
    adapter.request_body('test', folder)
    if not key or len(key) > 4096 or '\n' in key or '\r' in key:
        raise SystemExit('Invalid credential')
    with create_server(SEARCH_SOCKET_PATH, folder, key, credential_loader) as server:
        server.serve_forever()


def create_server(path, folder, key, credential_loader=None):
    try:
        os.unlink(path)
    except FileNotFoundError:
        pass
    old_umask = os.umask(0o077)
    try:
        server = Server(path, folder, key, credential_loader=credential_loader)
        # The setgid parent directory assigns gid 0 to this socket. Group 0
        # clients can connect without broad directory write access or DAC caps.
        os.chmod(path, 0o660)
        return server
    finally:
        os.umask(old_umask)


if __name__ == '__main__':
    main()
