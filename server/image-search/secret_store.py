"""Small atomic, owner-only Yandex credential bundle shared with search."""
import json
import os
import re
import stat
import tempfile

MAX_BYTES = 8192
FOLDER = re.compile(r'[A-Za-z0-9_-]{8,100}\Z', re.ASCII)
KEY = re.compile(r'[A-Za-z0-9_-]{20,4096}\Z', re.ASCII)


class CommitUncertain(Exception):
    """The file was replaced but syncing its directory failed."""


def validate(folder, key):
    if not isinstance(folder, str) or not FOLDER.fullmatch(folder):
        raise ValueError('invalid folder')
    if not isinstance(key, str) or not KEY.fullmatch(key):
        raise ValueError('invalid key')
    return folder, key


def read_bundle(path):
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0)
    fd = os.open(path, flags)
    with os.fdopen(fd, 'rb') as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or info.st_size > MAX_BYTES:
            raise ValueError('invalid credential file')
        data = handle.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError('invalid credential file')
    value = json.loads(data)
    if not isinstance(value, dict) or set(value) != {'folder_id', 'api_key'}:
        raise ValueError('invalid credential file')
    return validate(value['folder_id'], value['api_key'])


def write_bundle(path, folder, key):
    validate(folder, key)
    parent = os.path.dirname(path) or '.'
    fd, temporary = tempfile.mkstemp(prefix='.credentials-', dir=parent)
    try:
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, 'w', encoding='utf-8') as handle:
            json.dump({'folder_id': folder, 'api_key': key}, handle, separators=(',', ':'))
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        try:
            directory = os.open(parent, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        except OSError as exc:
            raise CommitUncertain() from exc
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)
