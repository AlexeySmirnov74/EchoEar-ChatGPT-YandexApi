"""Offline browser-like test: TLS proxy, authentication, CSRF, key rotation."""
import http.client
import http.cookies
import re
import socket
import ssl
import urllib.parse

context = ssl.create_default_context(cafile='/run/certs/cert.pem')
origin = 'https://unit.example:18444'
old_key = 'FAKE_YANDEX_CREDENTIAL_FIRST_A'
new_key = 'FAKE_YANDEX_CREDENTIAL_SECOND_B'
password = 'test password long enough'


class LocalTLS(http.client.HTTPSConnection):
    def connect(self):
        raw = socket.create_connection(('127.0.0.1', 18444), timeout=5)
        self.sock = context.wrap_socket(raw, server_hostname='unit.example')


def request(method, path, form=None, cookie='', origin_value=origin):
    client = LocalTLS('unit.example', 18444, timeout=5)
    headers = {'Origin': origin_value}
    if cookie:
        headers['Cookie'] = cookie
    data = None
    if form is not None:
        data = urllib.parse.urlencode(form).encode()
        headers['Content-Type'] = 'application/x-www-form-urlencoded'
    client.request(method, path, data, headers)
    response = client.getresponse()
    result = response.status, {name.lower(): value for name, value in response.getheaders()}, response.read().decode()
    client.close()
    assert old_key not in result[2] and new_key not in result[2] and password not in result[2]
    return result


status, headers, body = request('GET', '/admin/login')
assert status == 200
cookie = http.cookies.SimpleCookie(headers['set-cookie'])
item = cookie['echowear_admin_session']
assert item['secure'] and item['httponly'] and item['samesite'] == 'Strict'
previous_sid = item.value
session = 'echowear_admin_session=' + item.value
csrf = re.search(r'name="csrf" value="([^"]+)"', body).group(1)
form = {'csrf': csrf, 'username': 'owner', 'password': password}
assert request('POST', '/admin/login', form, session, 'https://evil.example')[0] == 400
status, headers, body = request('POST', '/admin/login', form, session)
assert status == 303
cookie = http.cookies.SimpleCookie(headers['set-cookie'])
assert cookie['echowear_admin_session'].value != previous_sid
session = 'echowear_admin_session=' + cookie['echowear_admin_session'].value
status, headers, body = request('GET', '/admin/', cookie=session)
assert status == 200 and 'folder_first' in body
assert "frame-ancestors 'none'" in headers['content-security-policy']
csrf = re.search(r'name="csrf" value="([^"]+)"', body).group(1)
status, headers, body = request('POST', '/admin/settings',
    {'csrf': csrf, 'folder': 'folder_second', 'key': new_key}, session)
assert status == 200 and 'Сохранено' in body
print('TLS login, session rotation, CSRF and credential update verified without echoing credentials.')
