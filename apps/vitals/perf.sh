#!/bin/sh
# Drie metingen tegen een draaiende vitals in één keer: download (/blob),
# upload (/sink) en de NVMe door het system-callpad (test disk). De eerste
# twee klokt curl aan de clientkant, de derde draait in de app en komt via
# /api/state terug. Vergelijk de disk-cijfers met de Stat-vloer die de test
# meegeeft: alles daarboven is hopfs + schijf, alles daaronder is het LAN-pad.
#
#   apps/vitals/perf.sh <host[:port]> [mb]      poort default 8090, mb default 64
#
# Nodig op de client: curl en python3 (upload en JSON).
set -e
H="${1:?usage: perf.sh <host[:port]> [mb]}"
MB="${2:-64}"
case "$H" in *:*) ;; *) H="$H:8090" ;; esac
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

curl -sf -m 5 "http://$H/ping" >/dev/null || { echo "no vitals at $H" >&2; exit 1; }
echo "vitals at $H, $MB MB per test"

# Download: de node zendt, curl klokt.
dl=$(curl -s -o /dev/null -w '%{speed_download}' "http://$H/blob?mb=$MB")
printf 'download  %8.1f MB/s   (node -> client, /blob)\n' "$(echo "$dl" | awk '{print $1/1e6}')"

# Upload: leanhttp begrenst een body op 1 MiB, dus de upload gaat als een reeks
# PUTs van 1 MiB over één keep-alive-verbinding; de node telt de reeks bij
# elkaar op (/sink → "up") en de client klokt de wandtijd. Per request zit er
# één round-trip aan overhead in — dat is de prijs van de bodygrens.
ul=$(python3 - "$H" "$MB" <<'PY'
import http.client, sys, time
host, port = sys.argv[1].rsplit(":", 1)
mb = int(sys.argv[2])
chunk = b"\0" * (1 << 20)
c = http.client.HTTPConnection(host, int(port), timeout=60)
t0 = time.time()
for _ in range(mb):
    c.request("PUT", "/sink", body=chunk, headers={"Content-Length": str(len(chunk))})
    r = c.getresponse()
    r.read()
    if r.status != 200:
        sys.exit("sink answered %d" % r.status)
print(mb * (1 << 20) / (time.time() - t0) / 1e6)
PY
)
printf 'upload    %8.1f MB/s   (client -> node, /sink, 1 MiB per request)\n' "$ul"

# Disk: in de app, via het system-callpad; wachten tot hij klaar is.
if ! curl -sf "http://$H/api/run?test=disk&mb=$MB" >/dev/null; then
	echo "disk: could not start (another test running?)" >&2
	exit 1
fi
n=0
while [ "$(curl -s "http://$H/api/state" | python3 -c 'import json,sys; print(json.load(sys.stdin)["running"])')" != "" ]; do
	n=$((n + 1))
	[ $n -gt 600 ] && { echo "disk: still running after 10 min, giving up" >&2; exit 1; }
	sleep 1
done
curl -s "http://$H/api/state" | python3 -c '
import json, sys
r = json.load(sys.stdin)["results"].get("disk")
if not r:
    print("disk: no result"); sys.exit(1)
if r.get("error"):
    print("disk: ERROR", r["error"]); sys.exit(1)
m = {x["name"]: x for x in r["metrics"]}
print("disk      %8.1f MB/s   write, %d KiB per call" % (m["write"]["value"], m["chunk"]["value"]))
print("          %8.1f MB/s   read" % m["read"]["value"])
print("          %8.1f MB/s   4 KiB writes (database pattern)" % m["write 4k"]["value"])
print("          %8.0f us     system-call floor (stat, no disk)" % m["call floor p50"]["value"])
for l in r.get("lines", []):
    print("          " + l)
'
