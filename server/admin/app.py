"""HTTPS-proxy-only administration panel for a local Yandex credential bundle."""
import collections
import fcntl
import html
import http.cookies
import http.server
import ipaddress
import os
import secrets
import socketserver
import stat
import sys
import threading
import time
import urllib.parse
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import passwords
import secret_store

MAX_BODY = 8192
SESSION_SECONDS = 1800
LOGIN_SECONDS = 300
SESSION_LIMIT = 256
PREAUTH_LIMIT = 64
PREAUTH_PER_PEER = 3
COOKIE = 'echowear_admin_session'


class Sessions:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.lock = threading.Lock()
        self.items = {}

    def new(self, peer):
        with self.lock:
            self._purge()
            pending = [value for value in self.items.values() if not value[0]]
            if (len(self.items) >= SESSION_LIMIT or len(pending) >= PREAUTH_LIMIT or
                    sum(value[3] == peer for value in pending) >= PREAUTH_PER_PEER):
                return None
            sid, csrf = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
            self.items[sid] = [False, csrf, self.clock() + LOGIN_SECONDS, peer]
            return sid, csrf

    def get(self, sid):
        with self.lock:
            self._purge()
            value = self.items.get(sid)
            return tuple(value) if value else None

    def authenticate(self, sid):
        with self.lock:
            self._purge()
            value = self.items.pop(sid, None)
            if not value:
                return None
            new_sid, csrf = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
            self.items[new_sid] = [True, csrf, self.clock() + SESSION_SECONDS, value[3]]
            return new_sid, csrf

    def delete(self, sid):
        with self.lock:
            self.items.pop(sid, None)

    def _purge(self):
        now = self.clock()
        for sid in [sid for sid, value in self.items.items() if value[2] <= now]:
            del self.items[sid]


class LoginLimit:
    def __init__(self, clock=time.monotonic):
        self.clock = clock
        self.lock = threading.Lock()
        self.attempts = collections.deque()

    def permit(self, peer):
        now = self.clock()
        with self.lock:
            while self.attempts and self.attempts[0][0] <= now - 600:
                self.attempts.popleft()
            if len(self.attempts) >= 100 or sum(item[1] == peer for item in self.attempts) >= 5:
                return False
            self.attempts.append((now, peer))
            return True


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def setup(self):
        super().setup()
        self.connection.settimeout(10)

    def log_message(self, format, *args):
        pass

    def reply(self, status, body='', headers=()):
        data = body.encode('utf-8')
        self.send_response(status)
        self.send_header('Content-Type', 'text/html; charset=utf-8')
        self.send_header('Content-Length', str(len(data)))
        self.send_header('Cache-Control', 'no-store')
        self.send_header('Pragma', 'no-cache')
        self.send_header('X-Content-Type-Options', 'nosniff')
        self.send_header('X-Frame-Options', 'DENY')
        self.send_header('Referrer-Policy', 'no-referrer')
        self.send_header('Content-Security-Policy', "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
        self.send_header('Connection', 'close')
        for name, value in headers:
            self.send_header(name, value)
        self.end_headers()
        self.close_connection = True
        if data:
            self.wfile.write(data)

    def session(self):
        try:
            cookies = http.cookies.SimpleCookie()
            cookies.load(self.headers.get('Cookie', ''))
            value = cookies.get(COOKIE)
            sid = value.value if value else ''
            if not sid or len(sid) > 128:
                return '', None
            return sid, self.server.sessions.get(sid)
        except (http.cookies.CookieError, ValueError):
            return '', None

    @staticmethod
    def cookie(value):
        return f'{COOKIE}={value}; Path=/admin; HttpOnly; Secure; SameSite=Strict'

    def page(self, title, content):
        return ('<!doctype html><html lang="ru"><meta charset="utf-8">'
                '<meta name="viewport" content="width=device-width,initial-scale=1">'
                f'<title>{html.escape(title)}</title>'
                '<style>body{font:16px system-ui;max-width:36rem;margin:3rem auto;padding:0 1rem}'
                'label,input,button{display:block}input{box-sizing:border-box;width:100%;padding:.6rem;margin:.4rem 0 1rem}'
                'button{padding:.6rem 1rem}form{margin:1rem 0}</style>'
                f'<h1>{html.escape(title)}</h1>{content}</html>')

    def login_page(self, notice=''):
        sid, previous = self.session()
        if previous and not previous[0]:
            csrf = previous[1]
        else:
            peer = self.peer()
            if peer is None:
                return self.reply(403, 'Запрос отклонён')
            created = self.server.sessions.new(peer)
            if created is None:
                return self.reply(429, 'Слишком много сеансов. Попробуйте позже.', [('Retry-After', '300')])
            sid, csrf = created
        body = self.page('Вход', f'<p>{html.escape(notice)}</p>'
            '<form method="post" action="/admin/login">'
            f'<input type="hidden" name="csrf" value="{csrf}">'
            '<label>Логин<input name="username" maxlength="64" autocomplete="username" required></label>'
            '<label>Пароль<input type="password" name="password" maxlength="128" autocomplete="current-password" required></label>'
            '<button>Войти</button></form>')
        return self.reply(200, body, [('Set-Cookie', self.cookie(sid))])

    def dashboard(self, session, notice=''):
        try:
            folder, _ = secret_store.read_bundle(self.server.bundle_path)
        except (OSError, ValueError):
            return self.reply(503, self.page('Настройки', '<p>Настройки недоступны.</p>'))
        csrf = session[1]
        body = self.page('Яндекс Картинки', f'<p>{html.escape(notice)}</p>'
            '<p>Ключ хранится на сервере. Введите новый ключ только для замены; существующий ключ не отображается.</p>'
            '<form method="post" action="/admin/settings">'
            f'<input type="hidden" name="csrf" value="{csrf}">'
            f'<label>Идентификатор каталога<input name="folder" value="{html.escape(folder, quote=True)}" maxlength="100" required></label>'
            '<label>Новый API-ключ<input type="password" name="key" maxlength="4096" autocomplete="new-password"></label>'
            '<button>Сохранить</button></form>'
            '<form method="post" action="/admin/logout">'
            f'<input type="hidden" name="csrf" value="{csrf}"><button>Выйти</button></form>')
        return self.reply(200, body)

    def read_form(self):
        if self.headers.get('Origin') != self.server.public_origin:
            raise ValueError('origin')
        if self.headers.get('X-Forwarded-Proto') != 'https':
            raise ValueError('not https')
        if self.headers.get('Transfer-Encoding') or len(self.headers.get_all('Content-Length', [])) != 1:
            raise ValueError('length')
        content_type = self.headers.get('Content-Type', '').split(';', 1)[0].strip().lower()
        length = self.headers.get('Content-Length', '')
        if content_type != 'application/x-www-form-urlencoded' or not length.isascii() or not length.isdigit() or not 0 < int(length) <= MAX_BODY:
            raise ValueError('form')
        raw = self.rfile.read(int(length))
        if len(raw) != int(length):
            raise ValueError('short body')
        form = urllib.parse.parse_qs(raw.decode('utf-8'), keep_blank_values=True, strict_parsing=True, errors='strict', max_num_fields=8)
        if any(len(value) != 1 for value in form.values()):
            raise ValueError('duplicate')
        return {key: value[0] for key, value in form.items()}

    def peer(self):
        try:
            return str(ipaddress.ip_address(self.headers['X-Forwarded-For']))
        except (ValueError, KeyError):
            return None

    def do_GET(self):
        if self.path == '/admin/health':
            peer = self.peer()
            address = ipaddress.ip_address(peer) if peer is not None else None
            if address is not None and getattr(address, 'ipv4_mapped', None) is not None:
                address = address.ipv4_mapped
            if address is None or not address.is_loopback:
                return self.reply(404)
            return self.reply(200, 'ok')
        if self.path not in ('/admin', '/admin/', '/admin/login'):
            return self.reply(404, 'Не найдено')
        _, session = self.session()
        if session and session[0] and self.path != '/admin/login':
            return self.dashboard(session)
        return self.login_page()

    def do_POST(self):
        if self.path not in ('/admin/login', '/admin/settings', '/admin/logout'):
            return self.reply(404, 'Не найдено')
        try:
            form = self.read_form()
            sid, session = self.session()
            submitted = form.get('csrf', '')
            if not session or not submitted.isascii() or not secrets.compare_digest(submitted, session[1]):
                return self.reply(403, 'Запрос отклонён')
            if self.path == '/admin/login':
                if set(form) != {'csrf', 'username', 'password'} or session[0]:
                    return self.reply(400, 'Некорректный запрос')
                peer = self.peer()
                if peer is None:
                    return self.reply(403, 'Запрос отклонён')
                if not self.server.login_limit.permit(peer):
                    return self.reply(429, 'Слишком много попыток. Попробуйте позже.', [('Retry-After', '600')])
                if not passwords.verify(self.server.password_record, form['username'], form['password']):
                    self.server.sessions.delete(sid)
                    return self.login_page('Неверный логин или пароль.')
                result = self.server.sessions.authenticate(sid)
                if result is None:
                    return self.reply(403, 'Сеанс истёк')
                return self.reply(303, headers=[('Location', '/admin/'), ('Set-Cookie', self.cookie(result[0]))])
            if not session[0]:
                return self.reply(403, 'Требуется вход')
            if self.path == '/admin/logout':
                if set(form) != {'csrf'}:
                    return self.reply(400, 'Некорректный запрос')
                self.server.sessions.delete(sid)
                return self.reply(303, headers=[('Location', '/admin/login'), ('Set-Cookie', self.cookie('') + '; Max-Age=0')])
            if set(form) != {'csrf', 'folder', 'key'}:
                return self.reply(400, 'Некорректный запрос')
            with self.server.update_lock:
                _, old_key = secret_store.read_bundle(self.server.bundle_path)
                new_key = form['key'] or old_key
                secret_store.write_bundle(self.server.bundle_path, form['folder'], new_key)
            return self.dashboard(session, 'Сохранено. Поиск использует настройки при следующем запросе; ключ не проверен в Яндексе.')
        except secret_store.CommitUncertain:
            return self.reply(503, 'Настройки могли сохраниться, но подтверждение записи не получено. Проверьте работу поиска.')
        except (ValueError, UnicodeError, OSError):
            return self.reply(400, 'Не удалось сохранить настройки. Проверьте введённые данные.')


class ServerMixin:
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, password_record, bundle_path, public_origin):
        super().__init__(address, Handler)
        self.password_record = password_record
        self.bundle_path = bundle_path
        self.public_origin = public_origin
        self.sessions = Sessions()
        self.login_limit = LoginLimit()
        self.update_lock = threading.Lock()
        self.slots = threading.BoundedSemaphore(16)

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            request.close()
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
        pass


class Server(ServerMixin, http.server.ThreadingHTTPServer):
    """TCP harness for isolated tests only; production uses UnixServer."""


class UnixServer(ServerMixin, socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    pass


def main():
    origin = os.environ['ECHOWEAR_ADMIN_ORIGIN']
    parsed = urllib.parse.urlsplit(origin)
    if (parsed.scheme != 'https' or not parsed.hostname or parsed.username is not None or parsed.password is not None
            or parsed.path or parsed.query or parsed.fragment or not parsed.port):
        raise SystemExit('Invalid public admin origin')
    record = passwords.load('/run/secrets/admin_password_hash')
    path = '/var/lib/echowear-admin/yandex.json'
    secret_store.read_bundle(path)
    socket_path = '/run/echowear-admin/admin.sock'
    lock_path = '/run/echowear-admin/admin.lock'
    lock_fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | getattr(os, 'O_NOFOLLOW', 0), 0o600)
    with os.fdopen(lock_fd, 'r+') as lock:
        if not stat.S_ISREG(os.fstat(lock.fileno()).st_mode):
            raise SystemExit('Invalid admin lock')
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        if os.path.lexists(socket_path):
            info = os.lstat(socket_path)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.geteuid():
                raise SystemExit('Invalid existing admin socket')
            os.unlink(socket_path)
        old_umask = os.umask(0o077)
        try:
            with UnixServer(socket_path, record, path, origin) as server:
                # Dedicated parent directory is setgid root for gateway access.
                os.chmod(socket_path, 0o660)
                server.serve_forever()
        finally:
            os.umask(old_umask)


if __name__ == '__main__':
    main()
