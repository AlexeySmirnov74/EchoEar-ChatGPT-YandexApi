"""Isolated decoder: one bounded request per Unix socket, no network or key."""
import os
import socket
import struct
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import yandex_image_search as adapter

SOCKET_PATH = '/run/echowear/decoder.sock'


def read_exact(conn, count):
    result = bytearray()
    while len(result) < count:
        part = conn.recv(min(65536, count - len(result)))
        if not part:
            raise ValueError('Incomplete request')
        result.extend(part)
    return bytes(result)


def serve_one(conn):
    conn.settimeout(8)
    try:
        size = struct.unpack('!I', read_exact(conn, 4))[0]
        if not 0 < size <= adapter.MAX_IMAGE_BYTES:
            raise ValueError('Invalid image length')
        data = read_exact(conn, size)
        frame = adapter.prepare_frame_isolated(data)
        if len(frame) != adapter.FRAME_SIZE * adapter.FRAME_SIZE * 2:
            raise ValueError('Invalid frame size')
        conn.sendall(struct.pack('!I', len(frame)) + frame)
    except (ValueError, OSError, adapter.SearchError):
        try:
            conn.sendall(struct.pack('!I', 0))
        except OSError:
            pass


def main():
    os.umask(0o077)
    try:
        os.unlink(SOCKET_PATH)
    except FileNotFoundError:
        pass
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
        server.bind(SOCKET_PATH)
        os.chmod(SOCKET_PATH, 0o600)
        server.listen(2)
        while True:
            conn, _ = server.accept()
            with conn:
                serve_one(conn)


if __name__ == '__main__':
    main()
