#!/usr/bin/env python3
"""Read-only checks for the EchoEar-2ST v0.2.0 USB update.

Never opens a serial port or writes a device. Never prints backup contents.
Requires an existing compatible bootloader, partition table and asset archive.
"""
from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import struct

FLASH_SIZE = 0x1000000
APP_OFFSET = 0x20000
APP_SLOT_SIZE = 0x3F0000
OTA_OFFSET = 0xD000
OTA_SIZE = 0x2000
SECTOR = 0x1000
APP_SHA256 = "10ed4515afa947d32b1e747e1519e9cb71a71e0a33e2cc67b2815031aabd69d2"
EXPECTED_PARTITIONS = [
    (1, 2, 0x9000, 0x4000, b"nvs", 0),
    (1, 0, 0xD000, 0x2000, b"otadata", 0),
    (1, 1, 0xF000, 0x1000, b"phy_init", 0),
    (0, 0x10, APP_OFFSET, APP_SLOT_SIZE, b"ota_0", 0),
    (0, 0x11, 0x410000, APP_SLOT_SIZE, b"ota_1", 0),
    (1, 0x82, 0x800000, 0x800000, b"assets", 0),
]


def validate_backup(data: bytes) -> None:
    if len(data) != FLASH_SIZE:
        raise ValueError("Expected a complete 16 MiB flash backup")
    if data[0] != 0xE9 or struct.unpack_from("<H", data, 12)[0] != 9:
        raise ValueError("Expected an unencrypted ESP32-S3 bootloader at offset 0")
    table = data[0x8000:0x8C00]
    entries = []
    md5_found = False
    for offset in range(0, len(table), 32):
        item = table[offset:offset + 32]
        if item[:2] == b"\xaa\x50":
            if md5_found:
                raise ValueError("Partition entry follows the checksum")
            _, kind, subtype, address, size, label, flags = struct.unpack("<HBBII16sI", item)
            entries.append((kind, subtype, address, size, label.rstrip(b"\0"), flags))
        elif item[:16] == b"\xeb\xeb" + b"\xff" * 14:
            if md5_found or hashlib.md5(table[:offset]).digest() != item[16:]:
                raise ValueError("Partition-table checksum mismatch")
            md5_found = True
        elif item == b"\xff" * 32:
            if table[offset:] != b"\xff" * (len(table) - offset):
                raise ValueError("Unexpected data after the partition table")
            break
        else:
            raise ValueError("Unsupported partition-table record")
    if not md5_found or entries != EXPECTED_PARTITIONS:
        raise ValueError("Partition layout is incompatible with this release")
    assets = data[0x800000:]
    count, checksum, length = struct.unpack_from("<III", assets)
    entry_size = 44
    if not 0 < count <= 4096 or not count * entry_size <= length <= len(assets) - 12:
        raise ValueError("Existing display assets are missing or invalid; do not erase flash")
    if checksum != sum(assets[12:12 + length]) & 0xFFFF:
        raise ValueError("Existing display asset checksum mismatch")
    index = None
    for number in range(count):
        name, size, offset, _, _ = struct.unpack_from("<32sIIHH", assets, 12 + number * entry_size)
        start = 12 + count * entry_size + offset
        if start < 12 + count * entry_size or start + 2 + size > 12 + length:
            raise ValueError("Existing asset entry is out of bounds")
        if assets[start:start + 2] != b"ZZ":
            raise ValueError("Existing asset entry has invalid magic")
        if name.split(b"\0", 1)[0] == b"index.json":
            if index is not None:
                raise ValueError("Duplicate asset index")
            index = assets[start + 2:start + 2 + size]
    try:
        metadata = json.loads(index.rstrip(b"\0") if index is not None else b"")
    except (ValueError, UnicodeError):
        raise ValueError("Existing asset index is invalid") from None
    if not isinstance(metadata, dict) or metadata.get("version") != 1:
        raise ValueError("Only version 1 display assets are supported by this procedure")


def validate_app(app: bytes) -> None:
    if not app or len(app) > APP_SLOT_SIZE or hashlib.sha256(app).hexdigest() != APP_SHA256:
        raise ValueError("Application does not match the approved release SHA-256")


def verify_update(before: bytes, after: bytes, app: bytes, ota_initial: bytes) -> None:
    validate_backup(before)
    validate_app(app)
    if ota_initial != b"\xff" * OTA_SIZE:
        raise ValueError("Expected exactly 8192 erased bytes for the initial OTA metadata")
    expected = bytearray(before)
    erased_length = (len(app) + SECTOR - 1) // SECTOR * SECTOR
    expected[APP_OFFSET:APP_OFFSET + erased_length] = b"\xff" * erased_length
    expected[APP_OFFSET:APP_OFFSET + len(app)] = app
    expected[OTA_OFFSET:OTA_OFFSET + OTA_SIZE] = ota_initial
    if after != expected:
        raise ValueError("Flash readback differs from the exact expected update; do not reboot")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("backup", type=Path)
    parser.add_argument("--repeat", type=Path, help="Second read made without running the application")
    parser.add_argument("--after", type=Path, help="Full readback made before the first application boot")
    parser.add_argument("--app", type=Path)
    parser.add_argument("--ota-initial", type=Path)
    args = parser.parse_args()
    try:
        before = args.backup.read_bytes()
        validate_backup(before)
        if args.repeat and args.repeat.read_bytes() != before:
            raise ValueError("The two backup reads differ; do not write flash")
        if args.after:
            if not args.app or not args.ota_initial:
                parser.error("--after requires --app and --ota-initial")
            verify_update(before, args.after.read_bytes(), args.app.read_bytes(), args.ota_initial.read_bytes())
            print("PASS: exact USB update verified; bootloader, partitions, NVS, ota_1 and assets are unchanged.")
        else:
            if args.app:
                validate_app(args.app.read_bytes())
            print("PASS: backup layout and existing assets match this release.")
            if args.repeat:
                print("PASS: both complete backup reads are identical.")
    except (OSError, ValueError) as error:
        # Error messages contain only our diagnostics, not binary/JSON contents.
        if isinstance(error, OSError):
            parser.exit(1, "FAIL: unable to read an input file.\n")
        parser.exit(1, f"FAIL: {error}\n")


if __name__ == "__main__":
    main()
