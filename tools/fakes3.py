#!/usr/bin/env python3
"""Een kleine S3-nep voor de QEMU-toets van de store-ops (tools/qemu-test-store.sh).

Eén bucket, path-style (`/<bucket>/<key>`), in het geheugen: PUT, GET, HEAD,
DELETE en ListObjectsV2 (`GET /<bucket>?list-type=2&prefix=...`). SigV4 wordt
niet nagerekend, maar een verzoek zonder `Authorization: AWS4-HMAC-SHA256`
krijgt 403: zo bewijst de toets dat Hop tekent. Een PUT met
`x-amz-content-sha256` moet een body hebben die daarbij past (anders
400 BadDigest), zoals S3 zelf. HTTP/1.1, zodat `Expect: 100-continue` van
een gestroomde PUT een `100 Continue` krijgt.

Elk verzoek komt als één regel op stdout: `S3 <METHODE> <key> <status>`.

    python3 tools/fakes3.py <poort> <bucket>
"""

import hashlib
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from xml.sax.saxutils import escape

OBJECTS = {}
BUCKET = "hop"


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def say(self, key, status):
        print(f"S3 {self.command} {key} {status}", flush=True)

    def reply(self, status, body=b"", headers=None, key=""):
        self.send_response(status)
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)
        self.say(key, status)

    def target(self):
        url = urllib.parse.urlsplit(self.path)
        parts = url.path.lstrip("/").split("/", 1)
        bucket = urllib.parse.unquote(parts[0])
        key = urllib.parse.unquote(parts[1]) if len(parts) > 1 else ""
        return bucket, key, urllib.parse.parse_qs(url.query)

    def gate(self):
        bucket, key, query = self.target()
        auth = self.headers.get("Authorization", "")
        if not auth.startswith("AWS4-HMAC-SHA256 "):
            self.drain()
            self.reply(403, b"<Error><Code>AccessDenied</Code></Error>", key=key)
            return None
        if bucket != BUCKET:
            self.drain()
            self.reply(404, b"<Error><Code>NoSuchBucket</Code></Error>", key=key)
            return None
        return key, query

    def drain(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        if n:
            self.rfile.read(n)

    def do_PUT(self):
        g = self.gate()
        if g is None:
            return
        key, _ = g
        n = int(self.headers.get("Content-Length", "0") or "0")
        body = self.rfile.read(n) if n else b""
        want = self.headers.get("x-amz-content-sha256", "")
        got = hashlib.sha256(body).hexdigest()
        if want and want != "UNSIGNED-PAYLOAD" and want != got:
            self.reply(400, b"<Error><Code>BadDigest</Code></Error>", key=key)
            return
        OBJECTS[key] = body
        etag = '"' + hashlib.md5(body).hexdigest() + '"'
        self.reply(200, b"", {"ETag": etag}, key=key)

    def do_GET(self):
        g = self.gate()
        if g is None:
            return
        key, query = g
        if key == "" and query.get("list-type") == ["2"]:
            prefix = query.get("prefix", [""])[0]
            keys = sorted(k for k in OBJECTS if k.startswith(prefix))
            items = "".join(
                f"<Contents><Key>{escape(k)}</Key><Size>{len(OBJECTS[k])}</Size></Contents>"
                for k in keys
            )
            xml = (
                '<?xml version="1.0" encoding="UTF-8"?>'
                '<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">'
                f"<Name>{BUCKET}</Name><Prefix>{escape(prefix)}</Prefix>"
                f"<KeyCount>{len(keys)}</KeyCount><IsTruncated>false</IsTruncated>"
                f"{items}</ListBucketResult>"
            ).encode()
            self.reply(200, xml, {"Content-Type": "application/xml"}, key="?list prefix=" + prefix)
            return
        if key not in OBJECTS:
            self.reply(404, b"<Error><Code>NoSuchKey</Code></Error>", key=key)
            return
        body = OBJECTS[key]
        etag = '"' + hashlib.md5(body).hexdigest() + '"'
        self.reply(200, body, {"ETag": etag}, key=key)

    def do_HEAD(self):
        self.do_GET()

    def do_DELETE(self):
        g = self.gate()
        if g is None:
            return
        key, _ = g
        OBJECTS.pop(key, None)
        self.reply(204, b"", key=key)


def main():
    global BUCKET
    port = int(sys.argv[1])
    BUCKET = sys.argv[2]
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    print(f"S3 listening on 127.0.0.1:{port} bucket {BUCKET}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
