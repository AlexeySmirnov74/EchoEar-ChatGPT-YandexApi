"""Offline test worker: encode which fake credential reached a spawned worker."""
import sys
sys.path.insert(0, '/app')
import service


def worker(pipe, query, folder, key):
    try:
        marker = 17 if key.endswith('A') and folder == 'folder_first' else 34 if key.endswith('B') and folder == 'folder_second' else 0
        pipe.send_bytes(bytes([marker]) * service.FRAME_BYTES)
    finally:
        pipe.close()


service.worker = worker
if __name__ == '__main__':
    service.main()
