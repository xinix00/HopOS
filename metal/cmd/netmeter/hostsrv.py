#!/usr/bin/env python3
# hostsrv.py — de host-helft van de netmeter-bank: draait op de Mac (of welke
# machine hostBase ook aanwijst) en serveert wat de fasen nodig hebben:
#
#   /small   1KiB  — storm-fasen + de klok (netmeter leest de Date-header:
#                    zonder wandtijd faalt elke TLS-validatie op het board)
#   /blob64  N MiB — pull-local, het RX-plafond; deterministisch (vaste seed)
#                    zodat de sha over runs heen vergelijkbaar is
#
# Starten: python3 metal/cmd/netmeter/hostsrv.py  (poort 8099, alle interfaces)
#          BLOB_MB=16 python3 …                   (kleinere blob)
#
# DEZE SERVER IS ZELF EEN MEETINSTRUMENT, en dat is geen luxe: een headless
# board zonder UART-dongle (LicheeRV, Radxa) heeft geen console om zijn
# NETMETER-regels op te zetten. Daarom klokt de server elke transfer zelf —
# hoe lang hij erover doet om zijn bytes kwijt te raken IS het RX-plafond van
# het board (TCP duwt niet harder dan de ontvanger leest). Per request één
# regel, en bij afsluiten (Ctrl-C) een samenvatting per pad.
#
# BLOB_MB verdient een woord: 64MiB is de default, maar de pull-fase op het
# board heeft een timeout van 180s. Een board dat ~180KB/s haalt, heeft voor
# 64MiB ~6 minuten nodig en breekt dus af. Bij een vermoeden van traagheid dus
# eerst klein meten (BLOB_MB=16), en pas groot als het tempo dat toelaat. De
# server hoeft daarvoor niks te weten van de fase: het board vraagt /blob64 en
# hasht wat het krijgt.

import hashlib
import os
import random
import sys
import time
from collections import defaultdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8099
BLOB_MB = int(os.environ.get("BLOB_MB", "64"))

# Vaste seed "HPOS" — zelfde afspraak als de serve-fase op het board: elke
# start serveert dezelfde bytes, dus elke run dezelfde sha.
BLOB = random.Random(0x48504F53).randbytes(BLOB_MB * 1024 * 1024)
SMALL = (b"HopOS netmeter small blob " * 40)[:1024]

# Per pad: aantal requests, totaal bytes, totale tijd. De storm-fasen zijn
# honderden kleine requests — daar zegt het gemiddelde meer dan elke regel.
stats = defaultdict(lambda: [0, 0, 0.0])


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        body = {"/small": SMALL, "/blob64": BLOB}.get(self.path)
        if body is None:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()

        # De klok start ná de headers: wat we willen weten is hoe snel de
        # ontvanger het lichaam wegwerkt, niet hoe snel Python een header
        # formatteert.
        # In brokken schrijven, niet in één keer: bij een afgebroken transfer
        # (de pull-fase op het board heeft een timeout) willen we weten hoevéél
        # er door ging vóór het afbrak — "12 MiB in 180s" is een tempo-getal,
        # "mislukt" is er geen.
        t0 = time.monotonic()
        sent, note = 0, ""
        try:
            for off in range(0, len(body), 256 << 10):
                chunk = body[off:off + (256 << 10)]
                self.wfile.write(chunk)
                sent += len(chunk)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError, TimeoutError) as e:
            note = f" ABORTED after {sent} bytes ({type(e).__name__})"
        dur = time.monotonic() - t0

        s = stats[self.path]
        s[0] += 1
        s[1] += sent
        s[2] += dur

        # Eén regel per transfer, maar niet 400 regels voor een storm: kleine
        # bodies worden alleen samengevat (bij afsluiten), grote altijd
        # geprint — een blob is een meting, een /small is een tik.
        if len(body) >= 1 << 20 or note:
            mbps = (sent / (1 << 20)) / dur if dur > 0 else 0
            print(f"{self.client_address[0]} {self.path} {sent}B "
                  f"{dur*1000:.0f}ms {mbps:.2f} MB/s{note}", flush=True)

    def log_message(self, fmt, *args):
        pass  # eigen, compactere regels hierboven


def summary():
    print("\n--- hostsrv samenvatting ---", flush=True)
    for path, (n, byts, dur) in sorted(stats.items()):
        if n == 0:
            continue
        mb = byts / (1 << 20)
        print(f"{path:9s} {n:4d} req  {mb:8.2f} MiB  {dur:7.2f}s  "
              f"{mb/dur if dur else 0:6.2f} MB/s  {n/dur if dur else 0:7.1f} req/s", flush=True)


if __name__ == "__main__":
    sha = hashlib.sha256(BLOB).hexdigest()
    print(f"hostsrv: :{PORT}  /small {len(SMALL)}B  /blob64 {BLOB_MB}MiB sha={sha[:16]}", flush=True)
    srv = ThreadingHTTPServer(("", PORT), Handler)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        summary()
