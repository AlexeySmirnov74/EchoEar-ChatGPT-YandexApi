"""Stage admin state from an existing server-side key; never print its value."""
import argparse
import getpass
import json
import os
from pathlib import Path
import secrets
import stat

import passwords
import secret_store


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--existing-key-file', type=Path, required=True)
    parser.add_argument('--folder-id', required=True, help='Yandex Cloud folder ID; not an API key')
    parser.add_argument('--state-dir', type=Path, required=True)
    parser.add_argument('--uid', type=int, default=10001)
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise SystemExit('Run as root so the isolated service can read its state.')
    if not args.state_dir.is_absolute() or not args.existing_key_file.is_absolute():
        raise SystemExit('Use absolute paths.')
    try:
        args.state_dir.mkdir(mode=0o700, parents=False, exist_ok=False)
    except OSError:
        raise SystemExit('Cannot create a new state directory. Existing files were not modified.') from None
    try:
        source = os.open(args.existing_key_file, os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0))
        with os.fdopen(source, 'r', encoding='utf-8') as handle:
            info = os.fstat(handle.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_size > 4096:
                raise ValueError('invalid key file')
            key = handle.read(4097).strip()
        secret_store.validate(args.folder_id, key)
        name = input('Login: ')
        first = getpass.getpass('Password (at least 16 characters): ')
        second = getpass.getpass('Repeat password: ')
        if not secrets.compare_digest(first.encode('utf-8'), second.encode('utf-8')):
            raise ValueError('passwords differ')
        password_path = args.state_dir / 'password.json'
        bundle_dir = args.state_dir / 'state'
        bundle_dir.mkdir(mode=0o700)
        socket_dir = args.state_dir / 'socket'
        socket_dir.mkdir(mode=0o700)
        bundle_path = bundle_dir / 'yandex.json'
        passwords.create(str(password_path), name, first)
        # This directory was just created exclusively and is not yet used by any service.
        secret_store.write_bundle(str(bundle_path), args.folder_id, key)
        os.chown(password_path, args.uid, args.uid)
        os.chmod(password_path, 0o400)
        os.chown(bundle_path, args.uid, args.uid)
        os.chown(bundle_dir, args.uid, args.uid)
        # Gateway has only group access to the socket directory, not to state.
        os.chown(socket_dir, args.uid, 0)
        os.chmod(socket_dir, 0o2770)
        os.chown(args.state_dir, args.uid, args.uid)
    except (OSError, ValueError, json.JSONDecodeError, secret_store.CommitUncertain):
        raise SystemExit('Preparation stopped. Inspect the new state directory manually; existing key was not modified.') from None
    finally:
        key = None
    print('Isolated admin state prepared. No running service was changed.')


if __name__ == '__main__':
    main()
