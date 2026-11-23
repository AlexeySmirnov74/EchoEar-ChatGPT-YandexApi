import contextlib
import http.client
import http.cookies
import io
import os
from pathlib import Path
import re
import tempfile
import threading
import unittest
import urllib.parse
from unittest import mock

import app
import passwords
import secret_store

OLD_KEY = 'OLD_TEST_CREDENTIAL_0123456789'
NEW_KEY = 'NEW_TEST_CREDENTIAL_9876543210'
PASSWORD = 'testing password 12345'
ORIGIN = 'https://unit.example:18444'


class AdminTests(unittest.TestCase):
    def test_health_is_local_and_does_not_consume_sessions(self):
        for _ in range(8):
            status, headers, body = self.request('GET', '/admin/health', peer='127.0.0.1')
            self.assertEqual((status, body), (200, 'ok'))
            self.assertNotIn('Set-Cookie', headers)
        self.assertEqual(len(self.server.sessions.items), 0)
        self.assertEqual(self.request('GET', '/admin/health', peer='::ffff:127.0.0.1')[0], 200)
        self.assertEqual(self.request('GET', '/admin/health', peer='203.0.113.1')[0], 404)

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.bundle = os.path.join(self.tmp.name, 'credentials.json')
        self.passfile = os.path.join(self.tmp.name, 'password.json')
        secret_store.write_bundle(self.bundle, 'folder_initial', OLD_KEY)
        passwords.create(self.passfile, 'owner', PASSWORD)
        self.server = app.Server(('127.0.0.1', 0), passwords.load(self.passfile), self.bundle, ORIGIN)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.port = self.server.server_address[1]

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.tmp.cleanup()

    def request(self, method, path, form=None, cookie='', origin=ORIGIN, proto='https', peer='203.0.113.1'):
        conn = http.client.HTTPConnection('127.0.0.1', self.port, timeout=5)
        headers = {'Origin': origin, 'X-Forwarded-Proto': proto, 'X-Forwarded-For': peer}
        if cookie:
            headers['Cookie'] = cookie
        body = None
        if form is not None:
            body = urllib.parse.urlencode(form).encode()
            headers['Content-Type'] = 'application/x-www-form-urlencoded'
        conn.request(method, path, body, headers)
        response = conn.getresponse()
        result = (response.status, dict(response.getheaders()), response.read().decode())
        conn.close()
        return result

    def login_form(self):
        status, headers, body = self.request('GET', '/admin/login')
        self.assertEqual(status, 200)
        cookie = http.cookies.SimpleCookie(headers['Set-Cookie'])
        sid = cookie[app.COOKIE].value
        self.assertTrue(cookie[app.COOKIE]['secure'])
        self.assertTrue(cookie[app.COOKIE]['httponly'])
        self.assertEqual(cookie[app.COOKIE]['samesite'], 'Strict')
        csrf = re.search(r'name="csrf" value="([^"]+)"', body).group(1)
        return f'{app.COOKIE}={sid}', csrf, body

    def login(self):
        cookie, csrf, _ = self.login_form()
        status, headers, body = self.request('POST', '/admin/login',
            {'csrf': csrf, 'username': 'owner', 'password': PASSWORD}, cookie)
        self.assertEqual(status, 303)
        self.assertNotIn(PASSWORD, body)
        new_cookie = http.cookies.SimpleCookie(headers['Set-Cookie'])
        new_sid = new_cookie[app.COOKIE].value
        self.assertNotIn(new_sid, cookie)
        cookie = f'{app.COOKIE}={new_sid}'
        status, _, body = self.request('GET', '/admin/', cookie=cookie)
        self.assertEqual(status, 200)
        self.assertNotIn(OLD_KEY, body)
        csrf = re.search(r'name="csrf" value="([^"]+)"', body).group(1)
        return cookie, csrf, body

    def test_authenticated_update_never_echoes_either_key(self):
        cookie, csrf, body = self.login()
        self.assertIn('folder_initial', body)
        status, _, body = self.request('POST', '/admin/settings',
            {'csrf': csrf, 'folder': 'folder_new123', 'key': NEW_KEY}, cookie)
        self.assertEqual(status, 200)
        self.assertNotIn(OLD_KEY, body)
        self.assertNotIn(NEW_KEY, body)
        self.assertEqual(secret_store.read_bundle(self.bundle), ('folder_new123', NEW_KEY))
        self.assertEqual(os.stat(self.bundle).st_mode & 0o777, 0o600)

    def test_directory_sync_failure_is_reported_as_uncertain(self):
        cookie, csrf, _ = self.login()
        with mock.patch.object(secret_store.os, 'fsync', side_effect=[None, OSError('injected')]):
            status, _, body = self.request('POST', '/admin/settings',
                {'csrf': csrf, 'folder': 'folder_new123', 'key': NEW_KEY}, cookie)
        self.assertEqual(status, 503)
        self.assertIn('могли сохраниться', body)
        self.assertNotIn(NEW_KEY, body)
        self.assertEqual(secret_store.read_bundle(self.bundle), ('folder_new123', NEW_KEY))

    def test_blank_key_keeps_existing_and_bad_values_do_not_overwrite(self):
        cookie, csrf, _ = self.login()
        status, _, _ = self.request('POST', '/admin/settings',
            {'csrf': csrf, 'folder': 'folder_new123', 'key': ''}, cookie)
        self.assertEqual(status, 200)
        self.assertEqual(secret_store.read_bundle(self.bundle), ('folder_new123', OLD_KEY))
        status, _, _ = self.request('POST', '/admin/settings',
            {'csrf': csrf, 'folder': 'bad', 'key': NEW_KEY}, cookie)
        self.assertEqual(status, 400)
        self.assertEqual(secret_store.read_bundle(self.bundle), ('folder_new123', OLD_KEY))

    def test_origin_https_and_csrf_required(self):
        cookie, csrf, _ = self.login()
        form = {'csrf': csrf, 'folder': 'folder_new123', 'key': NEW_KEY}
        self.assertEqual(self.request('POST', '/admin/settings', form, cookie, origin='https://evil.example')[0], 400)
        self.assertEqual(self.request('POST', '/admin/settings', form, cookie, proto='http')[0], 400)
        self.assertEqual(self.request('POST', '/admin/settings', dict(form, csrf='bad'), cookie)[0], 403)
        self.assertEqual(self.request('POST', '/admin/settings', dict(form, csrf='ё'), cookie)[0], 403)
        self.assertEqual(secret_store.read_bundle(self.bundle), ('folder_initial', OLD_KEY))

    def test_login_rate_limit_and_generic_failure(self):
        cookie, csrf, _ = self.login_form()
        for attempt in range(6):
            status, headers, body = self.request('POST', '/admin/login',
                {'csrf': csrf, 'username': 'owner', 'password': 'wrong'}, cookie)
            self.assertEqual(status, 200 if attempt < 5 else 429)
            self.assertNotIn(OLD_KEY, body)
            if status == 200:
                parsed = http.cookies.SimpleCookie(headers['Set-Cookie'])
                cookie = f'{app.COOKIE}={parsed[app.COOKIE].value}'
                csrf = re.search(r'name="csrf" value="([^"]+)"', body).group(1)

    def test_logout_invalidates_session(self):
        cookie, csrf, _ = self.login()
        status, headers, _ = self.request('POST', '/admin/logout', {'csrf': csrf}, cookie)
        self.assertEqual(status, 303)
        self.assertIn('Max-Age=0', headers['Set-Cookie'])
        status, _, body = self.request('GET', '/admin/', cookie=cookie)
        self.assertEqual(status, 200)
        self.assertIn('Вход', body)

    def test_no_sensitive_values_in_server_stderr(self):
        capture = io.StringIO()
        with contextlib.redirect_stderr(capture):
            cookie, csrf, _ = self.login()
            self.request('POST', '/admin/settings', {'csrf': csrf, 'folder': 'folder_new123', 'key': NEW_KEY}, cookie)
        for value in (OLD_KEY, NEW_KEY, PASSWORD, csrf):
            self.assertNotIn(value, capture.getvalue())

    def test_password_hash_is_not_plaintext_or_overwritten(self):
        raw = Path(self.passfile).read_text()
        self.assertNotIn(PASSWORD, raw)
        with self.assertRaises(FileExistsError):
            passwords.create(self.passfile, 'owner', 'another long password')

    def test_bundle_refuses_symlink(self):
        link = os.path.join(self.tmp.name, 'link')
        os.symlink(self.bundle, link)
        with self.assertRaises(OSError):
            secret_store.read_bundle(link)

    def test_unauthenticated_sessions_cannot_be_filled_by_one_peer(self):
        first = self.request('GET', '/admin/login', peer='203.0.113.5')
        cookie = first[1]['Set-Cookie'].split(';', 1)[0]
        for _ in range(10):
            repeated = self.request('GET', '/admin/login', cookie=cookie, peer='203.0.113.5')
            self.assertEqual(repeated[0], 200)
            self.assertTrue(repeated[1]['Set-Cookie'].startswith(cookie + ';'))
        self.assertEqual(self.request('GET', '/admin/login', peer='203.0.113.5')[0], 200)
        self.assertEqual(self.request('GET', '/admin/login', peer='203.0.113.5')[0], 200)
        self.assertEqual(self.request('GET', '/admin/login', peer='203.0.113.5')[0], 429)
        self.assertEqual(self.request('GET', '/admin/login', peer='203.0.113.6')[0], 200)

    def test_non_ascii_password_supported(self):
        target = os.path.join(self.tmp.name, 'unicode-password.json')
        password = 'сложный пароль для колонки'
        passwords.create(target, 'owner', password)
        self.assertTrue(passwords.verify(passwords.load(target), 'owner', password))
        self.assertFalse(passwords.verify(passwords.load(target), 'owner', password + '!'))


if __name__ == '__main__':
    unittest.main()
