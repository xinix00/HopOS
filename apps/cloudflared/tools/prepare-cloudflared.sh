#!/bin/sh
# Zet een bouwbare cloudflared klaar in build/cloudflared-patched.
#
# Waarom dit nodig is: cloudflared's `diagnostic`-pakket heeft alleen
# platform-implementaties voor linux, darwin en windows (traceroute via os/exec,
# /proc, WMI). Onder GOOS=tamago mist daardoor NetworkCollectorImpl en
# SystemCollectorImpl en compileert het pakket niet — terwijl de rest van
# cloudflared, quic-go-fork incluis, wél gewoon voor tamago bouwt.
#
# Go's -overlay zou de nette oplossing zijn, maar die weigert bestanden onder
# GOMODCACHE te vervangen ("Files beneath GOMODCACHE must not be replaced").
# Dus: de gepinde module wordt uit de cache gekopieerd, onze twee
# fallback-bestanden gaan erin, en go.mod verwijst er met een replace naar. De
# patch-bestanden staan in patch/ en zijn precies wat een upstream-PR zou
# toevoegen; verdwijnt de behoefte (upstream lost het op), dan meldt dit script
# dat en kan de replace eruit.
#
# Idempotent: gewoon opnieuw draaien na een versiebump in go.mod.
#
#   tools/prepare-cloudflared.sh          # klaarzetten
#   tools/prepare-cloudflared.sh --clean  # weggooien
set -e
cd "$(dirname "$0")/.."

DEST="build/cloudflared-patched"
MOD="github.com/cloudflare/cloudflared"

if [ "${1:-}" = "--clean" ]; then
	rm -rf build
	echo "OK: build/ weg (go-commando's in deze module falen tot je dit script weer draait)" >&2
	exit 0
fi

# De gepinde versie uit go.mod — niet via `go list -m`, want de replace
# hieronder wijst naar een map die er nog niet is en dan laadt de module niet.
VERSION=$(awk -v mod="$MOD" '$1 == mod && $2 ~ /^v/ { print $2; exit }' go.mod)
if [ -z "$VERSION" ]; then
	echo "!! geen require-regel voor $MOD in go.mod" >&2
	exit 1
fi
echo "cloudflared $VERSION" >&2

# Downloaden in een eigen tijdelijke module, zodat de kapotte replace van deze
# module er niet tussen zit.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
cat > "$TMP/go.mod" <<EOF
module cfdownload

go 1.26.4

require $MOD $VERSION

replace github.com/urfave/cli/v2 => github.com/ipostelnik/cli/v2 v2.3.1-0.20210324024421-b6ea8234fe3d
replace github.com/quic-go/quic-go => github.com/chungthuang/quic-go v0.45.1-0.20250428085412-43229ad201fd
EOF
(cd "$TMP" && GOWORK=off GOFLAGS=-mod=mod go mod download "$MOD")
SRC=$(cd "$TMP" && GOWORK=off GOFLAGS=-mod=mod go list -m -f '{{.Dir}}' "$MOD")
if [ ! -d "$SRC" ]; then
	echo "!! module niet in de cache gevonden: $SRC" >&2
	exit 1
fi

# Kopie maken (de modulecache is read-only) en onze bestanden erin.
rm -rf "$DEST"
mkdir -p "$(dirname "$DEST")"
cp -R "$SRC" "$DEST"
chmod -R u+w "$DEST"

for pair in "patch/network/collector_other.go:diagnostic/network" \
            "patch/diagnostic/system_collector_other.go:diagnostic"; do
	file=${pair%%:*}
	dir=${pair##*:}
	target="$DEST/$dir/$(basename "$file")"
	if [ -e "$target" ]; then
		echo "!! $dir/$(basename "$file") bestaat al upstream — patch mogelijk overbodig, controleer" >&2
	fi
	cp "$file" "$target"
	echo "  + $dir/$(basename "$file")" >&2
done

echo "OK: $DEST klaar — nu bouwt tools/build.sh (en go test ./...)" >&2
