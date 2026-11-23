import hashlib
import json
import struct
import unittest
from unittest.mock import patch

import check_flash_backup as guard


def sample_backup():
    data = bytearray(b"\xff" * guard.FLASH_SIZE)
    data[0] = 0xE9
    struct.pack_into("<H", data, 12, 9)
    entries = b"".join(struct.pack("<HBBII16sI", 0x50AA, *entry) for entry in guard.EXPECTED_PARTITIONS)
    data[0x8000:0x8000 + len(entries)] = entries
    record = b"\xeb\xeb" + b"\xff" * 14 + hashlib.md5(entries).digest()
    data[0x8000 + len(entries):0x8020 + len(entries)] = record
    index = json.dumps({"version": 1}).encode()
    body = struct.pack("<32sIIHH", b"index.json", len(index), 0, 0, 0) + b"ZZ" + index
    assets = struct.pack("<III", 1, sum(body) & 0xFFFF, len(body)) + body
    data[0x800000:0x800000 + len(assets)] = assets
    return bytes(data)


class BackupGuardTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.before = sample_backup()

    def test_compatible_backup(self):
        guard.validate_backup(self.before)

    def test_missing_assets(self):
        damaged = bytearray(self.before)
        damaged[0x800000:0x80000C] = b"\xff" * 12
        with self.assertRaisesRegex(ValueError, "assets"):
            guard.validate_backup(damaged)

    def test_partition_checksum(self):
        damaged = bytearray(self.before)
        damaged[0x8004] ^= 1
        with self.assertRaisesRegex(ValueError, "checksum"):
            guard.validate_backup(damaged)

    def test_different_layout_with_valid_checksum(self):
        damaged = bytearray(self.before)
        damaged[0x8004] ^= 1
        size = len(guard.EXPECTED_PARTITIONS) * 32
        damaged[0x8000 + size + 16:0x8000 + size + 32] = hashlib.md5(damaged[0x8000:0x8000 + size]).digest()
        with self.assertRaisesRegex(ValueError, "layout"):
            guard.validate_backup(damaged)

    def test_asset_checksum(self):
        damaged = bytearray(self.before)
        damaged[0x800038] ^= 1
        with self.assertRaisesRegex(ValueError, "checksum"):
            guard.validate_backup(damaged)

    def test_wrong_length_and_chip(self):
        with self.assertRaises(ValueError):
            guard.validate_backup(self.before[:-1])
        damaged = bytearray(self.before)
        damaged[12] = 0
        with self.assertRaisesRegex(ValueError, "ESP32-S3"):
            guard.validate_backup(damaged)

    def test_exact_update_and_protected_regions(self):
        app = b"\xe9" + b"test" * 1000
        after = bytearray(self.before)
        after[guard.APP_OFFSET:guard.APP_OFFSET + guard.SECTOR] = b"\xff" * guard.SECTOR
        after[guard.APP_OFFSET:guard.APP_OFFSET + len(app)] = app
        ota = b"\xff" * guard.OTA_SIZE
        with patch.object(guard, "APP_SHA256", hashlib.sha256(app).hexdigest()):
            guard.verify_update(self.before, bytes(after), app, ota)
            for position in (0, 0x8000, 0x9000, 0x410000, 0x800000):
                changed = bytearray(after)
                changed[position] ^= 1
                with self.subTest(position=position), self.assertRaisesRegex(ValueError, "readback"):
                    guard.verify_update(self.before, bytes(changed), app, ota)

    def test_reject_wrong_application(self):
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            guard.validate_app(b"unapproved")

    def test_reject_nonblank_ota(self):
        app = b"test"
        with patch.object(guard, "APP_SHA256", hashlib.sha256(app).hexdigest()):
            with self.assertRaisesRegex(ValueError, "8192"):
                guard.verify_update(self.before, self.before, app, b"\0" * guard.OTA_SIZE)


if __name__ == "__main__":
    unittest.main()
