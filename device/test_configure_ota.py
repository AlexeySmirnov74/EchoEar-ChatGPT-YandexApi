import tempfile
import unittest
from pathlib import Path

from configure_ota import configure


class ConfigureOtaTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.reference = Path(self.tmp.name) / "reference"
        self.output = Path(self.tmp.name) / "sdkconfig"
        self.reference.write_text('CONFIG_BOARD_TYPE_ECHOEAR_2ST=y\nCONFIG_OTA_URL="https://example.invalid/"\n')

    def test_valid_url_and_other_settings_preserved(self):
        configure(self.reference, self.output, "https://device.example:18444/private-token")
        self.assertEqual(self.output.read_text(), 'CONFIG_BOARD_TYPE_ECHOEAR_2ST=y\nCONFIG_OTA_URL="https://device.example:18444/private-token"\n')
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o600)

    def test_rejects_untrusted_shapes_without_creating_file(self):
        for url in ("http://example.test/", "HTTPS://example.test/", "https://user:pass@example.test/", "https://example.test/#fragment", "https://example.test/#", "https://api.tenclass.net/", "https://child.xiaozhi.me/", "https://example.test/\ninjected", "https://example.test/\"oops", "https://example.test:99999/", "https://example.test:0443/", "https://example.test?token=foo", "https://example.test./", "https://[::1]/"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                configure(self.reference, self.output, url)
            self.assertFalse(self.output.exists())

    def test_does_not_replace_existing_configuration(self):
        self.output.write_text("keep me")
        with self.assertRaises(FileExistsError):
            configure(self.reference, self.output, "https://example.test/")
        self.assertEqual(self.output.read_text(), "keep me")

    def test_rejects_missing_or_duplicate_setting(self):
        for source in ("nothing\n", 'CONFIG_OTA_URL="a"\nCONFIG_OTA_URL="b"\n'):
            self.reference.write_text(source)
            with self.assertRaises(ValueError):
                configure(self.reference, self.output, "https://example.test/")
            self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()
