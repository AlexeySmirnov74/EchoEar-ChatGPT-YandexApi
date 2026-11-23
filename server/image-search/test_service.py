import http.client
import json
import os
import socket
import struct
import stat
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

import decoder_service
import service


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=3)
        self.path = path
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(3)
        self.sock.connect(self.path)


def delayed_worker(pipe, query, folder, key):
    time.sleep(10)
    pipe.send_bytes(b'a' * service.FRAME_BYTES)


def empty_worker(pipe, query, folder, key):
    pipe.send_bytes(service.NO_RESULTS_WIRE)
    pipe.close()


class ServiceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.path = os.path.join(self.tmp.name, 's.sock')
        self.server = service.Server(self.path, 'folder', 'private-key')
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.tmp.cleanup()

    def request(self, body, path='/search', content_type='application/json'):
        client = UnixHTTP(self.path)
        client.request('POST', path, body, {'Content-Type': content_type})
        r = client.getresponse()
        result = (r.status, r.getheader('Content-Type'), r.read())
        client.close()
        return result

    def test_frame_contract_without_external_calls(self):
        frame = b'\x00\xf8' * (service.FRAME_BYTES // 2)
        with patch.object(service, 'run_search', return_value=frame) as search:
            status, kind, content = self.request(json.dumps({'query': 'rabbit'}))
        self.assertEqual((status, kind, content), (200, 'application/octet-stream', frame))
        self.assertEqual(search.call_count, 1)

    def test_rejections_and_generic_error(self):
        with patch.object(service, 'run_search', return_value=None) as search:
            for body in ('{}', '{"query":""}', '{"query":"x","other":1}', '[]', '{', 'x' * 2050):
                self.assertEqual(self.request(body)[0], 400)
            self.assertEqual(self.request('{"query":"x"}', '/other')[0], 404)
            self.assertEqual(self.request('{"query":"x"}', content_type='text/plain')[0], 415)
            status, _, content = self.request('{"query":"x"}')
            self.assertEqual(status, 502)
            self.assertNotIn(b'private-key', content)
            self.assertEqual(search.call_count, 1)

    def test_empty_search_has_distinct_empty_http_response(self):
        with patch.object(service,'run_search',return_value=service.NO_RESULTS):
            status,_,body=self.request('{"query":"private search"}')
        self.assertEqual((status,body),(204,b''))

    def test_child_protocol_preserves_empty_vs_other_failures(self):
        class Pipe:
            def __init__(self):self.data=None
            def send_bytes(self,data):self.data=data
            def close(self):pass
        p=Pipe()
        with patch.object(service.adapter,'search_frame',side_effect=service.adapter.NoSearchResults('private search')):
            service.worker(p,'private search','folder','private-key')
        self.assertEqual(p.data,service.NO_RESULTS_WIRE)
        p=Pipe()
        with patch.object(service.adapter,'search_frame',side_effect=service.adapter.SearchError('secret URL')):
            service.worker(p,'private search','folder','private-key')
        self.assertEqual(p.data,b'')

    def test_spawned_empty_worker_is_not_generic_failure(self):
        client,peer=socket.socketpair()
        try:
            with patch.object(service,'worker',empty_worker):
                self.assertIs(service.run_search('x','folder','key',client,timeout=4),service.NO_RESULTS)
        finally:
            client.close();peer.close()

    def test_busy_is_bounded(self):
        self.assertTrue(self.server.slots.acquire(blocking=False))
        self.assertTrue(self.server.slots.acquire(blocking=False))
        try:
            self.assertEqual(self.request('{"query":"x"}')[0], 503)
        finally:
            self.server.slots.release()
            self.server.slots.release()

    def test_search_socket_mode_and_inherits_setgid_parent(self):
        directory = os.path.join(self.tmp.name, 'sockdir')
        os.mkdir(directory)
        os.chmod(directory, 0o2710)
        path = os.path.join(directory, 'search.sock')
        server = service.create_server(path, 'folder', 'key')
        try:
            info = os.stat(path)
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o660)
            self.assertEqual(info.st_gid, os.stat(directory).st_gid)
            self.assertTrue(stat.S_ISSOCK(info.st_mode))
        finally:
            server.server_close()

    def test_search_deadline_kills_child(self):
        client, peer = socket.socketpair()
        try:
            with patch.object(service, 'worker', delayed_worker):
                start = time.monotonic()
                self.assertIsNone(service.run_search('x', 'folder', 'key', client, timeout=0.3))
                self.assertLess(time.monotonic() - start, 3)
        finally:
            client.close()
            peer.close()

    def test_disconnected_client_cancels_child(self):
        client, peer = socket.socketpair()
        peer.close()
        try:
            with patch.object(service, 'worker', delayed_worker):
                start = time.monotonic()
                self.assertIsNone(service.run_search('x', 'folder', 'key', client, timeout=10))
                self.assertLess(time.monotonic() - start, 3)
        finally:
            client.close()

    def test_decoder_protocol_size_and_frame(self):
        client, server = socket.socketpair()
        frame = bytes(service.FRAME_BYTES)
        with patch.object(decoder_service.adapter, 'prepare_frame_isolated', return_value=frame):
            thread = threading.Thread(target=decoder_service.serve_one, args=(server,))
            thread.start()
            client.sendall(struct.pack('!I', 3) + b'abc')
            self.assertEqual(struct.unpack('!I', service.read_exact(client, 4))[0], len(frame))
            self.assertEqual(service.read_exact(client, len(frame)), frame)
            thread.join(3)
        client.close()
        server.close()


if __name__ == '__main__':
    unittest.main()
