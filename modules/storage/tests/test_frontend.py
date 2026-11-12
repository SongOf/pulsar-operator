"""Console delivery and API contracts without sockets or a browser.

These checks exercise the real HTTP parser and server response generation using
an in-memory transport. They do not claim to execute JavaScript or render CSS.
"""
import io
import json
import tempfile
import unittest
from html.parser import HTMLParser
from urllib.parse import urljoin

from diskfleet.api import App, handler


class Document(HTMLParser):
    def __init__(self):
        super().__init__()
        self.ids = []
        self.assets = []
        self.inline_handlers = []
        self.language = None

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "html":
            self.language = attrs.get("lang")
        if "id" in attrs:
            self.ids.append(attrs["id"])
        if tag == "script" and attrs.get("src"):
            self.assets.append((attrs["src"], "javascript"))
        if tag == "link" and attrs.get("rel") == "stylesheet":
            self.assets.append((attrs["href"], "text/css"))
        self.inline_handlers.extend(key for key in attrs if key.startswith("on"))


class FrontendDeliveryTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory()
        cls.app = App(state_dir=cls.tmp.name, token="console-contract-test-token-24")

    @classmethod
    def tearDownClass(cls):
        cls.tmp.cleanup()

    def http(self, path, authenticated=False):
        auth = f"Authorization: Bearer {self.app.token}\r\n" if authenticated else ""
        request = f"GET {path} HTTP/1.0\r\nHost: localhost\r\n{auth}\r\n".encode()

        class Transport:
            def __init__(self):
                self.output = io.BytesIO()

            def makefile(self, *args, **kwargs):
                return io.BytesIO(request)

            def sendall(self, data):
                self.output.write(data)

            def settimeout(self, timeout):
                pass

        transport = Transport()
        handler(self.app)(transport, ("127.0.0.1", 12345), None)
        header, body = transport.output.getvalue().split(b"\r\n\r\n", 1)
        lines = header.decode().split("\r\n")
        headers = dict(line.split(": ", 1) for line in lines[1:])
        return int(lines[0].split()[1]), headers, body

    def api(self, path):
        status, _, body = self.http(path, authenticated=True)
        self.assertEqual(200, status)
        return json.loads(body)

    def test_complete_chinese_entrypoint_and_assets_work_before_authentication(self):
        status, headers, body = self.http("/")
        self.assertEqual(200, status)
        self.assertIn("text/html", headers["Content-Type"])
        self.assertEqual(len(body), int(headers["Content-Length"]))
        document = Document()
        document.feed(body.decode())
        self.assertEqual("zh-CN", document.language)
        self.assertEqual(len(document.ids), len(set(document.ids)))
        self.assertIn("main", document.ids)
        self.assertIn("dialog", document.ids)
        self.assertFalse(document.inline_handlers)
        self.assertGreaterEqual(len(document.assets), 2)
        for url, mime in document.assets:
            self.assertTrue(url.startswith("./static/"), "Console assets must support both standalone and gateway paths")
            asset_status, asset_headers, source = self.http(urljoin("/", url))
            self.assertEqual(200, asset_status)
            self.assertIn(mime, asset_headers["Content-Type"])
            self.assertGreater(len(source), 100)
            self.assertIn("nosniff", asset_headers["X-Content-Type-Options"])
        self.assertIn("script-src 'self'", headers["Content-Security-Policy"])
        self.assertIn("frame-ancestors 'self'", headers["Content-Security-Policy"])
        self.assertEqual("SAMEORIGIN", headers["X-Frame-Options"])

    def test_embedded_entrypoint_preserves_controls_and_only_allows_same_origin_frames(self):
        status, headers, body = self.http("/?embedded=1")
        self.assertEqual(200, status)
        self.assertIn("frame-ancestors 'self'", headers["Content-Security-Policy"])
        self.assertNotIn("frame-ancestors *", headers["Content-Security-Policy"])
        self.assertEqual("SAMEORIGIN", headers["X-Frame-Options"])
        document = Document()
        document.feed(body.decode())
        for control in ("token-button", "mode", "content", "dialog", "refresh-button"):
            self.assertIn(control, document.ids)
        for url, mime in document.assets:
            self.assertEqual(200, self.http(urljoin("/", url))[0])

    def test_relative_assets_resolve_beneath_storage_gateway_prefix(self):
        _, _, body = self.http("/")
        document = Document()
        document.feed(body.decode())
        self.assertIn("pulsar-console-link", document.ids)
        for url, mime in document.assets:
            gateway_url = urljoin("/storage/", url)
            self.assertTrue(gateway_url.startswith("/storage/static/"))
            # The gateway removes its /storage prefix before forwarding requests.
            status, headers, source = self.http(gateway_url.removeprefix("/storage"))
            self.assertEqual(200, status)
            self.assertIn(mime, headers["Content-Type"])
            self.assertGreater(len(source), 100)

    def test_static_route_does_not_expose_project_files(self):
        self.assertEqual(404, self.http("/static/../api.py")[0])
        self.assertEqual(404, self.http("/static/nonexistent.js")[0])

    def test_public_mode_config_and_private_dashboard(self):
        status, _, body = self.http("/api/config")
        self.assertEqual(200, status)
        self.assertEqual({"mode": "demo", "tokenRequired": True}, json.loads(body))
        self.assertEqual(401, self.http("/api/overview")[0])
        summary = self.api("/api/overview")
        self.assertEqual("demo", summary["mode"])
        self.assertTrue(set(("nodes", "disks", "pools", "volumes", "capacityBytes", "allocatedBytes", "availableBytes")).issubset(summary["totals"]))
        self.assertIsInstance(summary["recentEvents"], list)
        self.assertIsInstance(summary["alerts"], list)
        self.assertEqual(summary["totals"]["capacityBytes"], summary["totals"]["allocatedBytes"] + summary["totals"]["availableBytes"])

    def test_console_record_contracts_and_node_filter_pagination(self):
        required = {
            "nodes": {"name", "zone", "state", "disks", "capacityBytes", "allocatedBytes"},
            "disks": {"id", "name", "node", "path", "serial", "sizeBytes", "media", "state", "reasons", "poolId"},
            "pools": {"id", "name", "node", "vgName", "capacityBytes", "allocatedBytes", "availableBytes", "state", "storageClass"},
            "volumes": {"id", "name", "namespace", "poolId", "node", "sizeBytes", "status", "storageClass", "pvName", "mountPath", "createdAt"},
            "events": {"id", "time", "level", "action", "message", "node"},
        }
        for collection, fields in required.items():
            with self.subTest(collection=collection):
                page = self.api(f"/api/{collection}?limit=12&offset=0")
                self.assertTrue({"items", "total", "limit", "offset"}.issubset(page))
                self.assertTrue(page["items"])
                self.assertTrue(fields.issubset(page["items"][0]), f"Missing console fields: {fields - page['items'][0].keys()}")
        nodes = self.api("/api/nodes?limit=12&offset=12")
        self.assertEqual(12, len(nodes["items"]))
        selected = nodes["items"][0]["name"]
        disks = self.api(f"/api/disks?limit=12&offset=0&node={selected}")
        self.assertTrue(all(disk["node"] == selected for disk in disks["items"]))
        self.assertEqual(nodes["items"][0]["disks"], disks["total"])


if __name__ == "__main__":
    unittest.main()
