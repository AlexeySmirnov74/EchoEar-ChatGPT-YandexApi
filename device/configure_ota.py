#!/usr/bin/env python3
"""Create a private ESP-IDF sdkconfig from the distributed reference."""

import argparse
import getpass
import os
from pathlib import Path
import re
from urllib.parse import urlsplit


def configure(reference: Path, output: Path, url: str) -> None:
    if any(ord(c) < 33 or ord(c) > 126 or c in '\\"' for c in url):
        raise ValueError("Invalid characters in OTA URL")
    if not url.startswith("https://"):
        raise ValueError("OTA URL must begin with https://")
    try:
        parsed = urlsplit(url)
        port = parsed.port
    except ValueError:
        raise ValueError("Invalid OTA URL") from None
    if (parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
            or parsed.password is not None or "#" in url or port == 0):
        raise ValueError("OTA URL must be HTTPS without credentials or fragment")
    # Match the device's restrictive authority parser so the saved address
    # can actually be used for its WSS connection and firmware updates.
    authority = url[len("https://"):].split("/", 1)[0]
    if (any(c in authority for c in "@?#%[]") or authority.count(":") > 1
            or not re.fullmatch(r"[A-Za-z0-9.-]+", parsed.hostname)
            or parsed.hostname.startswith(".") or parsed.hostname.endswith(".")):
        raise ValueError("Unsupported OTA URL authority")
    for old_domain in ("tenclass.net", "xiaozhi.me"):
        if parsed.hostname == old_domain or parsed.hostname.endswith("." + old_domain):
            raise ValueError("This OTA service is not supported")
    if ":" in authority:
        port_text = authority.rsplit(":", 1)[1]
        if not port_text or (len(port_text) > 1 and port_text.startswith("0")):
            raise ValueError("Unsupported OTA URL port")
    source = reference.read_text(encoding="utf-8")
    expression = r'^CONFIG_OTA_URL="[^\r\n]*"$'
    if len(re.findall(expression, source, flags=re.MULTILINE)) != 1:
        raise ValueError("Reference must contain exactly one CONFIG_OTA_URL")
    configured = re.sub(expression, lambda _: 'CONFIG_OTA_URL="' + url + '"',
                        source, flags=re.MULTILINE)
    # O_EXCL protects existing personal configurations; mode protects new ones on Unix.
    fd = os.open(output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as stream:
        stream.write(configured)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reference", type=Path, default=Path("sdkconfig.echowear-reference"))
    parser.add_argument("--output", type=Path, default=Path("sdkconfig"))
    args = parser.parse_args()
    try:
        url = getpass.getpass("Personal HTTPS OTA URL (hidden): ")
        configure(args.reference, args.output, url)
    except (OSError, ValueError) as error:
        print(f"Configuration was not created: {error}")
        return 1
    print("Private configuration created. Build the firmware from this directory.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
