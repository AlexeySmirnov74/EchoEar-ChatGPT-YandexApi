"""Create and verify a fixed-cost scrypt password hash."""
import base64
import getpass
import hashlib
import json
import os
import re
import secrets
import stat
import sys

USERNAME = re.compile(r'[A-Za-z0-9_.-]{3,64}\Z', re.ASCII)


def digest(password, salt):
    return hashlib.scrypt(password.encode('utf-8'), salt=salt, n=16384, r=8, p=1, dklen=32, maxmem=64 * 1024 * 1024)


def create(path, username, password):
    if not USERNAME.fullmatch(username) or len(password) < 16 or len(password) > 128:
        raise ValueError('invalid username or password length')
    salt = secrets.token_bytes(32)
    record = {'username': username, 'salt': base64.b64encode(salt).decode('ascii'),
              'digest': base64.b64encode(digest(password, salt)).decode('ascii')}
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w', encoding='utf-8') as handle:
        json.dump(record, handle, separators=(',', ':'))
        handle.flush()
        os.fsync(handle.fileno())


def load(path):
    fd = os.open(path, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
    with os.fdopen(fd, encoding='utf-8') as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > 2048:
            raise ValueError('invalid password file')
        value = json.load(handle)
    if set(value) != {'username', 'salt', 'digest'} or not USERNAME.fullmatch(value['username']):
        raise ValueError('invalid password file')
    salt = base64.b64decode(value['salt'], validate=True)
    hashed = base64.b64decode(value['digest'], validate=True)
    if len(salt) != 32 or len(hashed) != 32:
        raise ValueError('invalid password file')
    return value['username'], salt, hashed


def verify(record, username, password):
    if not isinstance(username, str) or not isinstance(password, str) or len(username) > 64 or len(password) > 128:
        return False
    name, salt, expected = record
    actual = digest(password, salt)
    return secrets.compare_digest(actual, expected) & secrets.compare_digest(username.encode('utf-8'), name.encode('utf-8'))


if __name__ == '__main__':
    if len(sys.argv) != 2:
        raise SystemExit('Usage: passwords.py FILE')
    name = input('Login: ')
    first = getpass.getpass('Password (at least 16 characters): ')
    second = getpass.getpass('Repeat password: ')
    if not secrets.compare_digest(first.encode('utf-8'), second.encode('utf-8')):
        raise SystemExit('Passwords differ')
    try:
        create(sys.argv[1], name, first)
    except (ValueError, OSError):
        raise SystemExit('Could not create password file; check input and ensure the file does not exist') from None
    print('Password hash created.')
