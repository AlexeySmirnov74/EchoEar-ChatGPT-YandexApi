import http.client
import json
import os
import socket
import tempfile
import threading
import unittest
from unittest.mock import patch

import secret_store
import service


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__('localhost', timeout=3)
        self.path = path
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(self.path)


class DynamicCredentialTests(unittest.TestCase):
    def test_each_request_loads_new_atomic_bundle(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = os.path.join(tmp, 'credentials.json')
            sock = os.path.join(tmp, 'search.sock')
            key1 = 'FIRST_TEST_CREDENTIAL_0123456789'
            key2 = 'SECOND_TEST_CREDENTIAL_0123456789'
            secret_store.write_bundle(path, 'folder_first', key1)
            server = service.Server(sock, '', '', credential_loader=lambda: secret_store.read_bundle(path))
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            try:
                with patch.object(service, 'run_search', return_value=bytes(service.FRAME_BYTES)) as run:
                    def send():
                        conn = UnixHTTP(sock)
                        conn.request('POST', '/search', json.dumps({'query': 'cat'}), {'Content-Type': 'application/json'})
                        response = conn.getresponse()
                        status = response.status
                        response.read()
                        conn.close()
                        return status
                    self.assertEqual(send(), 200)
                    self.assertEqual(run.call_args.args[1:3], ('folder_first', key1))
                    secret_store.write_bundle(path, 'folder_second', key2)
                    self.assertEqual(send(), 200)
                    self.assertEqual(run.call_args.args[1:3], ('folder_second', key2))
                    os.unlink(path)
                    self.assertEqual(send(), 503)
                    self.assertEqual(run.call_count, 2)
            finally:
                server.shutdown()
                server.server_close()
                thread.join()


if __name__ == '__main__':
    unittest.main()
