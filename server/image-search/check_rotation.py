import http.client
import socket
import sys


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(5)
        self.sock.connect('/run/echowear-images/search.sock')


client = UnixHTTP('localhost')
client.request('POST', '/search', b'{"query":"test"}', {'Content-Type': 'application/json'})
response = client.getresponse()
body = response.read()
assert response.status == 200 and len(body) == 115200
assert body == bytes([int(sys.argv[1])]) * 115200
print('Search worker saw expected credential generation.')
