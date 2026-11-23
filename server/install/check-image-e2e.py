"""Call the real search HTTP socket and verify the real decoder's frame."""
import http.client
import socket


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(10)
        self.sock.connect('/run/echowear-images/search.sock')


c = UnixHTTP('localhost')
c.request('POST', '/search', b'{"query":"test"}', {'Content-Type': 'application/json'})
response = c.getresponse()
frame = response.read()
assert response.status == 200, response.status
assert len(frame) == 240 * 240 * 2, len(frame)
assert frame == b'\x00\xf8' * (240 * 240), 'unexpected RGB565 frame'
print('Synthetic image traversed search worker and isolated decoder.')
