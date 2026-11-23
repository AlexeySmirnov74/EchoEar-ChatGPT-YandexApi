"""Offline test provider: synthetic image while retaining actual search/decoder IPC."""
import io
import sys
sys.path.insert(0, '/app')
import service
from PIL import Image


def mock_worker(pipe, query, folder, key):
    try:
        buffer = io.BytesIO()
        Image.new('RGB', (240, 240), (255, 0, 0)).save(buffer, format='PNG')
        pipe.send_bytes(service.convert_over_socket(buffer.getvalue()))
    except Exception:
        pipe.send_bytes(b'')
    finally:
        pipe.close()


service.worker = mock_worker
if __name__ == '__main__':
    service.main()
