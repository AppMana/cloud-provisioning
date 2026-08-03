#!/usr/bin/env python3
"""Hermetic stand-in for k0s's GitHub releases API.

resolveK0sReleaseTag pages through releases until an empty page; this
serves one release tag ("<KUBELET_VERSION>.0") on page 1 and [] after,
so the controller resolves a real-looking tag with no internet access.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib.parse import parse_qs, urlparse

VERSION = os.environ["KUBELET_VERSION"]
PORT = int(sys.argv[1])


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        page = parse_qs(urlparse(self.path).query).get("page", ["1"])[0]
        body = json.dumps(
            [{"tag_name": f"{VERSION}.0"}] if page == "1" else []
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
