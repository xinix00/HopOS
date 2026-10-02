#!/bin/sh
# Bouwt een release van HopOS v3 op de host en zet hem klaar in
# target/release-<versie>/, met een SHA256SUMS. Publiceert niets: aan het eind
# staan de regels om te taggen en te publiceren, voor wie dat mag (xinix00).
#
#   sh tools/release.sh 3.0.0             bouwen, klaarzetten, de notes en de regels
#   sh tools/release.sh --dry-run 3.0.0   alleen bouwen en klaarzetten
#   HOP_DIR=pad sh tools/release.sh ...   de hop-repo (standaard ../hop/hop)
#   GO_APPS=0 sh tools/release.sh ...     de Go-apps overslaan
#
# Per board twee smaken, met de smaak in de naam:
#
#   headless   de kern zonder gui, met image/cfg/hop-config-headless.cfg
#   headfull   de kern met gui (en media op de O6N), met
#              image/cfg/hop-config-headfull.cfg
#
# Wat erin komt, per board de kaart of stick (gzip, dd-baar) en de
# flipbundel:
#
#   hopos-rpi4-<smaak>.img.gz      image/rpi4.sh, Hop in de initramfs
#   hopos-rpi5-<smaak>.img.gz      image/rpi5.sh
#   hopos-radxa-<smaak>.img.gz     image/radxa-zero3.sh
#   hopos-o6n-<smaak>.img.gz       image/uefi-run.sh plus tools/mkcard: de
#   hopos-altra-<smaak>.img.gz     ESP als stick (EFI/BOOT/BOOTAA64.EFI,
#                                  hopos.cfg, Hop als hopos-stage.elf)
#   hopos-apple-headless.img       image/apple-m4.sh met de config en Hop
#                                  ingebakken: het bootobject voor kmutil
#   hopos-licheerv-headless.img.gz image/licheerv-agent.sh, met de config
#                                  in het venster en zonder bewoner
#   hopos-<board>-<smaak>.flip     image/flip-bundle.sh met HOPOS_STAMP=<versie>
#                                  (niet voor de LicheeRV: riscv64)
#
# De Apple en de LicheeRV hebben geen gui-feature, dus alleen headless. En de
# apps van deze boom (de lijst van tools/gate.sh plus decode), als
# <app>-<arch>.elf (jobs/README.md): arm64 allemaal, riscv64 appspike en
# welcome. Die gaan ook naar de rollende release `apps`, waar de gedeelde
# configs welcome vandaan halen.
#
# Optioneel de Go-apps (docs/go-apps.md): staan ~/tamago-go/bin/go en
# OLD/tools/apps-release.sh er, dan bouwt dat script ze met PUBLISH=0, en
# gaan ze als <app>-<arch>-tamago.elf mee naar `apps`.
#
# Wat de gebruiker meegeeft: de versie, en HOP_DIR als de hop-repo niet naast
# deze staat. Een eigen config per machine hoort niet in een publieke
# release; die bouw je met CFG= en het image-script zelf.
set -eu

DIR="$(cd "$(dirname "$0")/.." && pwd)"
TARGET=aarch64-unknown-none-softfloat
RV=riscv64gc-unknown-none-elf
DRY=0
if [ "${1:-}" = --dry-run ]; then
	DRY=1
	shift
fi
VERSION="${1:-}"
case "$VERSION" in
[0-9]*.[0-9]*.[0-9]*) ;;
*)
	echo "gebruik: $0 [--dry-run] <versie>   (bijvoorbeeld 3.0.0 of 3.0.0-beta.1)" >&2
	exit 64
	;;
esac
OUT="$DIR/target/release-$VERSION"
NOTES="$DIR/target/release-$VERSION.notes.md"
HEADLESS="$DIR/image/cfg/hop-config-headless.cfg"
HEADFULL="$DIR/image/cfg/hop-config-headfull.cfg"
export HOP_DIR="${HOP_DIR:-$DIR/../hop/hop}"
# Het stempel op de bootregel van elke kern (hopos/build.rs), ook van de
# kaarten: zo zegt een node welke release hij draait.
export HOPOS_STAMP="$VERSION"
LRV_DONOR_SHA=d85e68836f57a9fcb1bbfba3c1ccf93d1b062ac168305d7f0cc83a72a796c6b9

cd "$DIR"
rm -rf "$OUT" "$NOTES"
mkdir -p "$OUT"
T0=$(date +%s)
step() { echo "== release $VERSION: $*" >&2; }
skip() { echo "== release $VERSION: OVERGESLAGEN: $*" >&2; }
# Een kaart gaat gzip'd mee (vooral nullen: 64 MB wordt een paar MB);
# -n houdt de tijd en de naam eruit, zodat de som reproduceerbaar is.
card() { gzip -9 -n -c "$1" >"$OUT/$2.gz"; }
# flip <board> <smaak> <GUI> <FEATURES>
flip() {
	GUI=$3 FEATURES=$4 sh image/flip-bundle.sh "$1" >/dev/null
	cp "target/hopos-$1.flip" "$OUT/hopos-$1-$2.flip"
}

# Hop, één keer voor arm64: elk board krijgt hetzelfde image als bewoner.
step "Hop (tools/hop-build.sh, $HOP_DIR)"
HOP_ELF="$(sh tools/hop-build.sh "$TARGET")"

for FLAVOR in headless headfull; do
	if [ "$FLAVOR" = headless ]; then
		G=0 CONF="$HEADLESS"
	else
		G=1 CONF="$HEADFULL"
	fi
	for b in rpi4 rpi5; do
		step "$b $FLAVOR"
		GUI=$G CFG="$CONF" APP="$HOP_ELF" ROLE=hop sh "image/$b.sh"
		card "target/hopos-$b.img" "hopos-$b-$FLAVOR.img"
		flip "$b" "$FLAVOR" "$G" ""
	done
	step "radxa $FLAVOR"
	GUI=$G CFG="$CONF" APP="$HOP_ELF" ROLE=hop sh image/radxa-zero3.sh
	card target/radxa-zero3/hopos-radxa-zero3.img "hopos-radxa-$FLAVOR.img"
	flip radxa "$FLAVOR" "$G" ""
	for b in o6n altra; do
		step "$b $FLAVOR"
		ESP="$DIR/target/release-esp-$b-$FLAVOR"
		rm -rf "$ESP"
		# De O6N heeft de VPU: headfull is daar de media-smaak (die zet gui
		# zelf aan, hopos/Cargo.toml).
		if [ "$b" = o6n ] && [ "$G" = 1 ]; then
			MEDIA=1 BOARD=$b CFG="$CONF" APP="$HOP_ELF" ROLE=hop ESP="$ESP" sh image/uefi-run.sh
			flip "$b" "$FLAVOR" 0 media
		else
			GUI=$G BOARD=$b CFG="$CONF" APP="$HOP_ELF" ROLE=hop ESP="$ESP" sh image/uefi-run.sh
			flip "$b" "$FLAVOR" "$G" ""
		fi
		# De stick: dezelfde vorm als de Go-stick (tag v2.2.8, image/uefi-run.sh
		# stap 3b), op het hopcfg-venster na. UEFI leest FAT16 van removable
		# media; na het flashen mount de partitie en is hopos.cfg te bewerken.
		cargo run -q -p mkcard -- -o "$ESP.img" -size 64 -start 8192 -label hopos -vollabel \
			"$ESP/EFI/BOOT/BOOTAA64.EFI=EFI/BOOT/BOOTAA64.EFI" "$ESP/hopos.cfg" \
			"$ESP/hopos-stage.elf" >&2
		card "$ESP.img" "hopos-$b-$FLAVOR.img"
		rm -rf "$ESP" "$ESP.img"
	done
done

step "apple headless"
CFG="$HEADLESS" EMBED="$HOP_ELF" sh image/apple-m4.sh
cp target/apple-m4/hopos-apple.img "$OUT/hopos-apple-headless.img"
flip apple headless 0 ""
skip "apple headfull: board-apple heeft geen gui-feature"

step "licheerv headless"
CFG="$HEADLESS" LICHEERV_DONOR_SHA256=$LRV_DONOR_SHA sh image/licheerv-agent.sh
card target/licheerv/hopos-licheerv.img hopos-licheerv-headless.img
skip "licheerv headfull: board-licheerv heeft geen gui-feature; en geen flipbundel (riscv64)"

# De apps: zonder debug-info, met de symbolen (de kern leest er RamStart en
# de stempel uit), zoals de image-scripts ze stagen.
step "apps"
OBJCOPY="$(ls "$(rustc --print sysroot)"/lib/rustlib/*/bin/rust-objcopy 2>/dev/null | head -1)"
ARM_APPS="appspike welcome bench display vitals cloudflared-lean syncprobe decode"
RV_APPS="appspike welcome"
cargo build --quiet --release --target "$TARGET" $(for a in $ARM_APPS; do printf ' -p %s' "$a"; done)
cargo build --quiet --release --target "$RV" $(for a in $RV_APPS; do printf ' -p %s' "$a"; done)
for a in $ARM_APPS; do
	"$OBJCOPY" --strip-debug "target/$TARGET/release/$a" "$OUT/$a-arm64.elf"
done
for a in $RV_APPS; do
	"$OBJCOPY" --strip-debug "target/$RV/release/$a" "$OUT/$a-riscv64.elf"
done

GO_ELFS=""
if [ "${GO_APPS:-1}" = 0 ]; then
	skip "Go-apps: GO_APPS=0"
elif [ ! -x "$HOME/tamago-go/bin/go" ] || [ ! -f OLD/tools/apps-release.sh ]; then
	skip "Go-apps: ~/tamago-go/bin/go of OLD/tools/apps-release.sh ontbreekt (docs/go-apps.md)"
else
	step "Go-apps (OLD/tools/apps-release.sh, PUBLISH=0)"
	# Niet fataal: de lijst van dat script noemt ook de satellieten van de
	# easy-repo's, en de Rust-release hangt niet aan de Go-apps.
	if PUBLISH=0 sh OLD/tools/apps-release.sh; then
		cp OLD/metal/out/apps/*-tamago.elf "$OUT/"
		GO_ELFS=" $OUT/*-tamago.elf"
	else
		skip "Go-apps: OLD/tools/apps-release.sh faalde (de reden staat erboven)"
	fi
fi

(cd "$OUT" && shasum -a 256 * >SHA256SUMS)
echo "" >&2
echo "release $VERSION klaar in $((($(date +%s) - T0) / 60)) min: $OUT" >&2
ls -l "$OUT" >&2
if [ "$DRY" = 1 ]; then
	echo "(--dry-run: geen notes, geen regels om te publiceren)" >&2
	exit 0
fi

# De notes: het sjabloon van elke release, met de som van elk bestand.
{
	cat <<EOF
HopOS v$VERSION, the Rust generation. Every board comes in two flavors:
**headless** (no gui, \`image/cfg/hop-config-headless.cfg\`) and **headfull**
(gui, plus media on the O6N, \`image/cfg/hop-config-headfull.cfg\`). Both
configs are open on your own LAN (no API key, console over TCP) and start
**welcome** on port 80; a node without \`hopos.node\` names itself after its
board and the tail of its MAC (\`rpi4-4c54\`).

| Board | Headless | Headfull |
| --- | --- | --- |
| Raspberry Pi 4 | \`hopos-rpi4-headless.img.gz\` | \`hopos-rpi4-headfull.img.gz\` |
| Raspberry Pi 5 | \`hopos-rpi5-headless.img.gz\` | \`hopos-rpi5-headfull.img.gz\` |
| Radxa Zero 3E | \`hopos-radxa-headless.img.gz\` | \`hopos-radxa-headfull.img.gz\` |
| Radxa Orion O6N (USB stick) | \`hopos-o6n-headless.img.gz\` | \`hopos-o6n-headfull.img.gz\` (media) |
| Ampere Altra (USB stick) | \`hopos-altra-headless.img.gz\` | \`hopos-altra-headfull.img.gz\` |
| Mac mini M4 | \`hopos-apple-headless.img\` (kmutil boot object) | none |
| Sipeed LicheeRV Nano | \`hopos-licheerv-headless.img.gz\` | none |

Flash a card or stick: \`gunzip -c hopos-rpi4-headless.img.gz | sudo dd of=/dev/rdiskN bs=4m\`.
The boot partition mounts afterwards; add \`hopos.node\`, \`hopos.apikey\` or
your own jobs in \`hopos.cfg\` (the UEFI sticks), \`cmdline.txt\` (the Pi's)
or the \`append\` line of \`extlinux/extlinux.conf\` (the Radxa). The M4 and
the LicheeRV carry their config inside the image: rebuild with \`CFG=\`.

Kernel flip bundles, \`hopos-<board>-<flavor>.flip\`, replace a running
kernel without a reboot (docs/flip.md): put one on a web server and
\`curl -X POST http://NODE:8080/flip -d '{"url":"...","sha256":"..."}'\`
with its sum from SHA256SUMS.

Apps (rolling release [apps](https://github.com/xinix00/HopOS/releases/tag/apps),
named \`<app>-<arch>.elf\`): appspike, welcome, bench, display, vitals,
cloudflared-lean, syncprobe, decode for arm64; appspike and welcome for
riscv64. Jobspecs in \`jobs/\`.

SHA256SUMS:

\`\`\`
EOF
	cat "$OUT/SHA256SUMS"
	echo '```'
} >"$NOTES"

cat <<EOF

De notes: $NOTES
Taggen en publiceren (als xinix00, GH_TOKEN; dit script voert niets uit):

  git tag -a v$VERSION -m "HopOS v$VERSION"
  git push origin v$VERSION
  gh release create v$VERSION $OUT/* --repo xinix00/HopOS --title "HopOS v$VERSION" --notes-file $NOTES
  gh release upload apps $OUT/*-arm64.elf $OUT/*-riscv64.elf$GO_ELFS --repo xinix00/HopOS --clobber
EOF
