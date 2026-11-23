"""Decode exactly one frame with Linux resource limits; run without network in production."""
import os
import resource
import sys
from pathlib import Path

# -I removes the script directory; add only this known application directory.
sys.path.insert(0, str(Path(__file__).resolve().parent))

def limit(kind, value):
    resource.setrlimit(kind, (value, value))


def main():
    limit(resource.RLIMIT_AS, 256 * 1024 * 1024)
    limit(resource.RLIMIT_CPU, 3)
    limit(resource.RLIMIT_FSIZE, 240 * 240 * 2)
    limit(resource.RLIMIT_NOFILE, 32)
    os.umask(0o077)
    from yandex_image_search import MAX_IMAGE_BYTES, prepare_frame
    data = sys.stdin.buffer.read(MAX_IMAGE_BYTES + 1)
    output = prepare_frame(data)
    sys.stdout.buffer.write(output)
    sys.stdout.buffer.flush()


if __name__ == '__main__':
    main()
