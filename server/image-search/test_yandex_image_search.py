import base64
import io
import json
import socket
import unittest
from unittest.mock import patch

from PIL import Image
import yandex_image_search as a


def response(xml: str) -> bytes:
    return json.dumps({'rawData': base64.b64encode(xml.encode()).decode()}).encode()


def png(color=(255, 0, 0)) -> bytes:
    im = Image.new('RGB', (360, 240), color)
    f = io.BytesIO()
    im.save(f, format='PNG')
    return f.getvalue()


def resolver(*args, **kwargs):
    return [(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('93.184.216.34', 443))]


class Response:
    def __init__(self, status=200, body=b'abc', headers=None):
        self.status, self.body, self.headers = status, io.BytesIO(body), headers or {}
    def getheader(self, name):
        return self.headers.get(name)
    def read1(self, n):
        return self.body.read(n)


class Connection:
    def __init__(self, host, addr, family):
        self.host, self.addr, self.family = host, addr, family
    def request(self, method, path, headers):
        self.request_args = method, path, headers
        Connection.requests.append((self.host, self.addr, method, path, headers))
    def getresponse(self):
        return Connection.responses.pop(0)
    def close(self):
        pass
    responses = []
    requests = []


class TestAdapter(unittest.TestCase):
    def setUp(self):
        Connection.requests.clear()
        Connection.responses.clear()

    def test_request_is_strict_and_direct(self):
        body = a.request_body('зайчик', 'folder')
        self.assertEqual(body['query']['familyMode'], 'FAMILY_MODE_STRICT')
        self.assertEqual(body['query']['queryText'], 'зайчик')
        self.assertEqual(body['query']['searchType'], 'SEARCH_TYPE_RU')
        self.assertEqual(a.API_URL, 'https://searchapi.api.cloud.yandex.net/v2/image/search')
        for q in ('', ' ', 'x' * 401, 'bad\nquery'):
            with self.assertRaises(a.SearchError): a.request_body(q, 'folder')

    def test_xml_only_image_links_and_namespaces(self):
        xml = '''<r xmlns="urn:test"><response><doc><image-link>https://a.example/img</image-link>
                 <html-link>https://site.example/page</html-link></doc><doc><html-link>https://page.example</html-link></doc>
                 <doc><image-link>https://a.example/img</image-link></doc><doc><image-link>http://b.example/img</image-link></doc>
                 </response></r>'''
        self.assertEqual(a.parse_candidates(response(xml)), [a.Candidate('https://a.example/img', 'https://site.example/page')])
        self.assertEqual(a.parse_candidates(response('<r/>')), [])

    def test_xml_rejects_entities_bad_encoding_and_oversize(self):
        bad = ['<!DOCTYPE x [<!ENTITY z "a">]><x>&z;</x>', '<r>']
        for xml in bad:
            with self.assertRaises(a.SearchError): a.parse_candidates(response(xml))
        with self.assertRaises(a.SearchError): a.parse_candidates(b'{"rawData":"####"}')
        with self.assertRaises(a.SearchError): a.parse_candidates(b'x' * (a.MAX_API_BYTES + 1))

    def test_private_and_mixed_dns_rejected_before_connection(self):
        for ip in ('127.0.0.1', '10.0.0.1', '169.254.169.254', '::1', '::ffff:127.0.0.1', '64:ff9b::a00:1', '192.168.1.1', '100.64.0.1'):
            self.assertFalse(a.global_ip(ip), ip)
        self.assertTrue(a.global_ip('93.184.216.34'))
        mixed = lambda *args, **kwargs: resolver() + [(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('10.0.0.2', 443))]
        with self.assertRaises(a.SearchError): a.fetch_image('https://example.com/a', resolver=mixed, connection_factory=Connection)
        self.assertEqual(Connection.requests, [])

    def test_url_validation(self):
        for url in ('http://example.com', 'https://user:pass@example.com/x', 'https://example.com:8080/x',
                    'https://example.com/a#frag', 'file:///etc/passwd', 'https://example.com/\r\nheader'):
            with self.assertRaises(a.SearchError, msg=url): a.validate_url(url)

    def test_redirect_validated_and_connections_are_pinned(self):
        Connection.responses[:] = [Response(302, headers={'Location':'https://next.example/b'}), Response(body=b'file')]
        self.assertEqual(a.fetch_image('https://example.com/a', resolver=resolver, connection_factory=Connection), b'file')
        self.assertEqual([r[0] for r in Connection.requests], ['example.com', 'next.example'])
        self.assertTrue(all(r[1][0] == '93.184.216.34' for r in Connection.requests))
        Connection.requests.clear()
        Connection.responses[:] = [Response(302, headers={'Location':'http://127.0.0.1/metadata'})]
        with self.assertRaises(a.SearchError): a.fetch_image('https://example.com/a', resolver=resolver, connection_factory=Connection)
        self.assertEqual(len(Connection.requests), 1)

    def test_download_limits_compression_and_redirects(self):
        for r in [Response(headers={'Content-Length':str(a.MAX_IMAGE_BYTES + 1)}),
                  Response(body=b'x' * (a.MAX_IMAGE_BYTES + 1)), Response(headers={'Content-Encoding':'gzip'}),
                  Response(302, headers={'Location':'/a'})]:
            Connection.responses[:] = [r] * 4
            with self.assertRaises(a.SearchError): a.fetch_image('https://example.com/a', resolver=resolver, connection_factory=Connection)

    def test_rgb565_byte_order_and_dimensions(self):
        frame = a.prepare_frame(png((255, 0, 0)))
        self.assertEqual(len(frame), 115200)
        self.assertEqual(frame[:2], bytes((0x00, 0xf8)))
        self.assertEqual(a.prepare_frame(png((0, 255, 0)))[:2], bytes((0xe0, 0x07)))
        self.assertEqual(a.prepare_frame(png((0, 0, 255)))[:2], bytes((0x1f, 0x00)))
        with self.assertRaises(a.SearchError): a.prepare_frame(b'not an image')

    def test_real_isolated_worker(self):
        self.assertEqual(a.prepare_frame_isolated(png())[:2], bytes((0x00, 0xf8)))
        with self.assertRaises(a.SearchError): a.prepare_frame_isolated(b'bad image')

    def test_search_failure_falls_back_once_and_returns_frame(self):
        xml = '<r><doc><image-link>https://a.example/a</image-link></doc><doc><image-link>https://b.example/b</image-link></doc></r>'
        urls = []
        def fetch(url):
            urls.append(url)
            if len(urls) == 1: raise a.SearchError('network')
            return png()
        got = a.search_frame('заяц', 'folder', 'key', post=lambda body, key: response(xml), fetch=fetch)
        self.assertEqual(len(got), 115200)
        self.assertEqual(len(urls), 2)

    def test_official_empty_code_and_explicit_empty_response(self):
        for xml in (
            '<yandexsearch><response><error code="15">sensitive query</error></response></yandexsearch>',
            '<yandexsearch><response><results><grouping/></results></response></yandexsearch>',
            '<yandexsearch><response><found priority="all">0</found></response></yandexsearch>',
        ):
            with self.subTest(xml=xml), self.assertRaises(a.NoSearchResults) as caught:
                a.search_frame('private query','folder','private-key',post=lambda *_:response(xml))
            self.assertNotIn('sensitive',str(caught.exception))

    def test_errors_invalid_results_and_failed_fetch_are_not_no_results(self):
        xmls=(
            '<yandexsearch><response><error code="42">private key</error></response></yandexsearch>',
            '<yandexsearch><response><error code="15"/><doc><image-link>https://a.example/x</image-link></doc></response></yandexsearch>',
            '<yandexsearch><response><found>10</found><results><grouping/></results></response></yandexsearch>',
            '<yandexsearch><response><results><grouping><group/></grouping></results></response></yandexsearch>',
            '<yandexsearch><response><doc><image-link>http://bad.example/x</image-link></doc></response></yandexsearch>',
            '<r/>',
        )
        for xml in xmls:
            with self.subTest(xml=xml), self.assertRaises(a.SearchError) as caught:
                a.search_frame('x','folder','key',post=lambda *_:response(xml))
            self.assertNotIsInstance(caught.exception,a.NoSearchResults)
        xml='<r><doc><image-link>https://a.example/x</image-link></doc></r>'
        for fetch, convert in ((lambda _: (_ for _ in ()).throw(a.SearchError('network')),lambda b:b),
                               (lambda _: b'bad',lambda _: (_ for _ in ()).throw(a.SearchError('decode')))):
            with self.assertRaises(a.SearchError) as caught:
                a.search_frame('x','folder','key',post=lambda *_:response(xml),fetch=fetch,convert=convert)
            self.assertNotIsInstance(caught.exception,a.NoSearchResults)


if __name__ == '__main__': unittest.main()
