"""Isolated experimental Yandex image search adapter. No key is loaded automatically.

The only output is a prepared 240x240 RGB565 little-endian frame. This module
does not expose a web service and is not connected to the production gateway.
"""
from __future__ import annotations

import base64
import binascii
import http.client
import io
import ipaddress
import json
import secrets
import socket
import ssl
import threading
import queue
import time
import subprocess
import sys
import tempfile
from pathlib import Path
import urllib.error
import urllib.parse
import urllib.request
import warnings
import xml.etree.ElementTree as ET
from dataclasses import dataclass
from typing import Callable

from PIL import Image, ImageOps, UnidentifiedImageError

API_URL = 'https://searchapi.api.cloud.yandex.net/v2/image/search'
MAX_API_BYTES = 2_000_000
MAX_XML_BYTES = 1_000_000
MAX_IMAGE_BYTES = 4_000_000
MAX_IMAGE_PIXELS = 16_000_000
MAX_REDIRECTS = 2
TIMEOUT_SECONDS = 8
FRAME_SIZE = 240
Image.MAX_IMAGE_PIXELS = MAX_IMAGE_PIXELS
_dns_slots = threading.BoundedSemaphore(2)


class SearchError(Exception):
    """Expected search, response, network or image preparation failure."""


class NoSearchResults(SearchError):
    """The search provider explicitly reported no documents."""


@dataclass(frozen=True)
class Candidate:
    image_url: str
    page_url: str | None = None


def request_body(query: str, folder_id: str) -> dict:
    if not isinstance(query, str) or not query.strip() or len(query) > 400:
        raise SearchError('Invalid search query')
    if any(ord(c) < 32 for c in query):
        raise SearchError('Invalid search query')
    if not isinstance(folder_id, str) or not folder_id or len(folder_id) > 50:
        raise SearchError('Invalid folder ID')
    return {
        'query': {
            'searchType': 'SEARCH_TYPE_RU',
            'queryText': query.strip(),
            'familyMode': 'FAMILY_MODE_STRICT',
            'page': '0',
        },
        'docsOnPage': '30',
        'folderId': folder_id,
    }


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _read_bounded(response, limit: int, deadline: float) -> bytes:
    chunks = []
    count = 0
    while True:
        if time.monotonic() >= deadline:
            raise SearchError('Download timed out')
        # read1 does not wait for the entire requested length on a slow stream.
        chunk = response.read1(min(65536, limit + 1 - count))
        if not chunk:
            break
        count += len(chunk)
        if count > limit:
            raise SearchError('Download exceeds the limit')
        chunks.append(chunk)
    if time.monotonic() >= deadline:
        raise SearchError('Download timed out')
    return b''.join(chunks)


def search_post(body: dict, api_key: str) -> bytes:
    """POST only to Yandex over verified TLS, never to redirects or proxies."""
    if not isinstance(api_key, str) or not api_key or any(c in api_key for c in '\r\n'):
        raise SearchError('Invalid API key')
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())
    req = urllib.request.Request(
        API_URL, data=json.dumps(body, ensure_ascii=False).encode('utf-8'),
        headers={'Authorization': 'Api-Key ' + api_key, 'Content-Type': 'application/json',
                 'Accept': 'application/json', 'Accept-Encoding': 'identity'}, method='POST')
    try:
        deadline = time.monotonic() + TIMEOUT_SECONDS
        with opener.open(req, timeout=TIMEOUT_SECONDS) as response:
            if response.status != 200:
                raise SearchError('Search service returned an error')
            data = _read_bounded(response, MAX_API_BYTES, deadline)
    except (urllib.error.URLError, OSError, TimeoutError) as exc:
        raise SearchError('Search service request failed') from exc
    if len(data) > MAX_API_BYTES:
        raise SearchError('Search response exceeds the limit')
    return data


def parse_candidates(response_json: bytes) -> list[Candidate]:
    if len(response_json) > MAX_API_BYTES:
        raise SearchError('Search response exceeds the limit')
    try:
        response = json.loads(response_json)
        if not isinstance(response, dict) or not isinstance(response.get('rawData'), str):
            raise ValueError('missing rawData')
        xml = base64.b64decode(response['rawData'], validate=True)
        if len(xml) > MAX_XML_BYTES:
            raise ValueError('XML exceeds limit')
        text = xml.decode('utf-8')
        # Reject DTD/entities rather than relying on parser-specific defaults.
        if '<!DOCTYPE' in text.upper() or '<!ENTITY' in text.upper():
            raise ValueError('DTD disabled')
        root = ET.fromstring(text)
    except (UnicodeError, ValueError, TypeError, binascii.Error, ET.ParseError) as exc:
        raise SearchError('Invalid or unsupported search response') from exc
    nodes = list(root.iter())
    if len(nodes) > 10000:
        raise SearchError('Search response contains too many fields')
    def localname(tag: object) -> str:
        return tag.rsplit('}', 1)[-1] if isinstance(tag, str) else ''
    # Yandex documents code 15 as absence of results. All other provider errors
    # are failures, even if the document list is empty. Never return upstream
    # error text, which may contain the query or other private information.
    errors = [e for e in nodes if localname(e.tag) == 'error']
    docs = [e for e in nodes if localname(e.tag) == 'doc']
    if errors:
        if len(errors) == 1 and errors[0].get('code') == '15' and not docs:
            raise NoSearchResults('No search results')
        raise SearchError('Search service returned an error')
    # No image links is not sufficient: documents can contain unsupported,
    # unsafe or changed URL fields. Only explicit empty result structures are
    # reported as no-results; ambiguous formats remain a generic failure.
    responses = [e for e in nodes if localname(e.tag) == 'response']
    if len(responses) == 1 and not docs:
        response_nodes = list(responses[0].iter())
        found = [e for e in response_nodes if localname(e.tag) == 'found']
        if found:
            if all((e.text or '').strip() == '0' for e in found):
                raise NoSearchResults('No search results')
            raise SearchError('Search returned incomplete results')
        for results in (e for e in response_nodes if localname(e.tag) == 'results'):
            grouping = [e for e in results.iter() if localname(e.tag) == 'grouping']
            if grouping and all(not any(localname(e.tag) == 'group' for e in g.iter()) for g in grouping):
                raise NoSearchResults('No search results')
    result: list[Candidate] = []
    seen: set[str] = set()
    for doc in nodes:
        if localname(doc.tag) != 'doc':
            continue
        fields = {localname(e.tag): (e.text or '').strip() for e in doc.iter()}
        image = fields.get('image-link', '')
        # Do not mistake html-link (the hosting page) for an image URL.
        if not image or image in seen or len(image) > 2048:
            continue
        try:
            validate_url(image)
        except SearchError:
            continue
        seen.add(image)
        result.append(Candidate(image, fields.get('html-link') or None))
        if len(result) >= 30:
            break
    return result


def validate_url(url: str) -> urllib.parse.SplitResult:
    if not isinstance(url, str) or not url or len(url) > 2048 or any(ord(c) <= 32 for c in url):
        raise SearchError('Invalid image URL')
    try:
        parts = urllib.parse.urlsplit(url)
        port = parts.port
        if (parts.scheme != 'https' or not parts.hostname or parts.username is not None or
                parts.password is not None or port not in (None, 443) or parts.fragment):
            raise SearchError('Unsupported image URL')
        host = parts.hostname.encode('idna').decode('ascii')
        if host.endswith('.') or '%' in host:
            raise SearchError('Unsupported image host')
    except (ValueError, UnicodeError) as exc:
        raise SearchError('Invalid image URL') from exc
    return parts


def global_ip(value: str) -> bool:
    try:
        addr = ipaddress.ip_address(value)
        if isinstance(addr, ipaddress.IPv6Address) and addr.ipv4_mapped:
            addr = addr.ipv4_mapped
        elif isinstance(addr, ipaddress.IPv6Address):
            # Do not allow address translation/tunnel ranges to conceal an IPv4
            # destination that the ordinary public-address check would miss.
            transitions = ('64:ff9b::/96', '64:ff9b:1::/48', '2002::/16', '2001::/32')
            if any(addr in ipaddress.ip_network(net) for net in transitions):
                return False
        return addr.is_global and not (addr.is_multicast or addr.is_unspecified or addr.is_reserved)
    except ValueError:
        return False


def resolve_public(host: str, resolver=socket.getaddrinfo):
    """Bound both DNS wait and number of abandoned DNS workers."""
    if not _dns_slots.acquire(blocking=False):
        raise SearchError('Image DNS is busy')
    result = queue.Queue(maxsize=1)
    def run():
        try:
            result.put((resolver(host, 443, type=socket.SOCK_STREAM), None))
        except Exception as exc:
            result.put((None, exc))
        finally:
            _dns_slots.release()
    threading.Thread(target=run, name='preview-dns', daemon=True).start()
    try:
        records, error = result.get(timeout=TIMEOUT_SECONDS)
    except queue.Empty as exc:
        raise SearchError('Image DNS lookup timed out') from exc
    if error is not None:
        raise SearchError('Image DNS lookup failed') from error
    if not records or any(not global_ip(r[4][0]) for r in records):
        raise SearchError('Image host has a non-public address')
    return records


class _PinnedHTTPS(http.client.HTTPSConnection):
    def __init__(self, host: str, sockaddr: tuple, family: int):
        super().__init__(host, 443, timeout=TIMEOUT_SECONDS, context=ssl.create_default_context())
        self._address = sockaddr
        self._family = family

    def connect(self):
        # Never resolve again after checking IPs. Preserve hostname for SNI and
        # certificate validation. Proxy settings cannot change this connection.
        sock = socket.socket(self._family, socket.SOCK_STREAM)
        sock.settimeout(TIMEOUT_SECONDS)
        try:
            sock.connect(self._address)
            self.sock = self._context.wrap_socket(sock, server_hostname=self.host)
        except BaseException:
            sock.close()
            raise


def fetch_image(url: str, *, resolver=socket.getaddrinfo,
                connection_factory=_PinnedHTTPS) -> bytes:
    """Download HTTPS image with DNS pinning, no credentials, bounded redirects/body."""
    current = url
    for redirects in range(MAX_REDIRECTS + 1):
        parts = validate_url(current)
        host = parts.hostname.encode('idna').decode('ascii')
        # Reject mixed answers too, avoiding unsafe fallback or split DNS.
        records = resolve_public(host, resolver)
        family, _, _, _, addr = records[secrets.randbelow(len(records))]
        if family not in (socket.AF_INET, socket.AF_INET6):
            raise SearchError('Unsupported image address family')
        conn = connection_factory(host, addr, family)
        path = urllib.parse.urlunsplit(('', '', parts.path or '/', parts.query, ''))
        try:
            deadline = time.monotonic() + TIMEOUT_SECONDS
            conn.request('GET', path, headers={'Accept': 'image/jpeg,image/png',
                                              'Accept-Encoding': 'identity',
                                              'User-Agent': 'EchoEar-ImageExperiment/1'})
            response = conn.getresponse()
            if response.status in (301, 302, 303, 307, 308):
                location = response.getheader('Location')
                if not location or redirects == MAX_REDIRECTS:
                    raise SearchError('Too many image redirects')
                current = urllib.parse.urljoin(current, location)
                continue
            if response.status != 200:
                raise SearchError('Image server returned an error')
            if (response.getheader('Content-Encoding') or 'identity').lower() != 'identity':
                raise SearchError('Compressed HTTP body not supported')
            declared = response.getheader('Content-Length')
            if declared is not None:
                if not declared.isascii() or not declared.isdigit() or int(declared) > MAX_IMAGE_BYTES:
                    raise SearchError('Image body exceeds the limit')
            data = _read_bounded(response, MAX_IMAGE_BYTES, deadline)
            if not data or len(data) > MAX_IMAGE_BYTES:
                raise SearchError('Image body exceeds the limit or is empty')
            return data
        except (OSError, http.client.HTTPException, TimeoutError, ValueError) as exc:
            raise SearchError('Image download failed') from exc
        finally:
            conn.close()
    raise SearchError('Too many image redirects')


def prepare_frame(data: bytes) -> bytes:
    if not data or len(data) > MAX_IMAGE_BYTES:
        raise SearchError('Invalid image size')
    try:
        with warnings.catch_warnings():
            warnings.simplefilter('error', Image.DecompressionBombWarning)
            with Image.open(io.BytesIO(data), formats=['JPEG', 'PNG']) as image:
                if image.width <= 0 or image.height <= 0 or image.width * image.height > MAX_IMAGE_PIXELS:
                    raise SearchError('Image dimensions exceed the limit')
                if getattr(image, 'n_frames', 1) != 1:
                    raise SearchError('Animated images are not supported')
                image.load()
                # Do not interpret untrusted EXIF metadata in this prototype.
                image = image.convert('RGB')
                image = ImageOps.fit(image, (FRAME_SIZE, FRAME_SIZE), method=Image.Resampling.LANCZOS)
                pixels = image.tobytes()
    except (UnidentifiedImageError, Image.DecompressionBombError, Image.DecompressionBombWarning,
            OSError, ValueError, SyntaxError) as exc:
        raise SearchError('Invalid or unsupported image') from exc
    result = bytearray(FRAME_SIZE * FRAME_SIZE * 2)
    for i in range(FRAME_SIZE * FRAME_SIZE):
        r, g, b = pixels[3*i:3*i+3]
        value = ((r >> 3) << 11) | ((g >> 2) << 5) | (b >> 3)
        result[2*i] = value & 255
        result[2*i+1] = value >> 8
    return bytes(result)


def prepare_frame_isolated(data: bytes) -> bytes:
    """Decode in a bounded child process; OS/container sandbox is still required."""
    if not data or len(data) > MAX_IMAGE_BYTES:
        raise SearchError('Invalid image size')
    worker = Path(__file__).with_name('frame_worker.py')
    try:
        # A temporary regular file, combined with the child's file-size limit,
        # prevents unlimited stdout from exhausting the parent process memory.
        with tempfile.TemporaryFile() as output:
            proc = subprocess.run([sys.executable, '-I', str(worker)], input=data,
                                  stdout=output, stderr=subprocess.DEVNULL,
                                  timeout=8, check=False, close_fds=True)
            if proc.returncode != 0:
                raise SearchError('Image conversion failed')
            output.seek(0)
            frame = output.read(FRAME_SIZE * FRAME_SIZE * 2 + 1)
            if len(frame) != FRAME_SIZE * FRAME_SIZE * 2:
                raise SearchError('Image conversion produced invalid output')
            return frame
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise SearchError('Image conversion failed') from exc


def search_frame(query: str, folder_id: str, api_key: str, *,
                 post: Callable[[dict, str], bytes] = search_post,
                 fetch: Callable[[str], bytes] = fetch_image,
                 convert: Callable[[bytes], bytes] = prepare_frame_isolated) -> bytes:
    """Search once, randomly try at most 3 candidates, return raw pixels."""
    candidates = parse_candidates(post(request_body(query, folder_id), api_key))
    if not candidates:
        raise SearchError('Search returned no usable image links')
    secrets.SystemRandom().shuffle(candidates)
    for candidate in candidates[:3]:
        try:
            return convert(fetch(candidate.image_url))
        except SearchError:
            continue
    raise SearchError('No usable image found')
