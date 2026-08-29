#!/bin/sh
# Release-assets: bouwt de boot-images uit de tree, tekent ze en hangt ze aan
# de GitHub-release — de "signed images" waar gethop.org naar verwijst:
#
#   tools/release.sh v1.2.2        # bestaande release: assets uploaden
#   tools/release.sh v1.3.0        # nieuwe tag: release + assets aanmaken
#
# Artefacten, elk in twee smaken — gui (default) en headless (GUI=0):
# headless is geen uitgezette gui maar een build waar geen enkele regel
# gui-code in gelinkt zit.
#
# dd-bare images (gunzip | dd, medium boot — het hoofdpad, élk board):
#   hopos-uefi[-headless].img.gz         elke UEFI-arm64-machine (USB-stick)
#   hopos-rpi5[-headless].img.gz         Pi 5
#   hopos-rpi4[-headless].img.gz         Pi 4
#   hopos-radxa-zero3[-headless].img.gz  Radxa Zero 3E (donor-U-Boot ingebakken)
#   hopos-licheerv-headless.img.gz       LicheeRV Nano (alleen headless: geen
#                                        framebuffer op dat silicium; cfg
#                                        ingebakken)
#
# raw bootobject (geen boot-medium: iBoot laadt het bestand zelf):
#   hopos-m4-headless.img.gz             Mac mini M4 — installeren met kmutil
#                                        configure-boot (--raw --entry-point
#                                        2048); cfg ingebakken, en headless is
#                                        de enige smaak (nog geen framebuffer)
#
# drop-in-updates voor een bestaand boot-medium:
#   BOOTAA64.EFI / BOOTAA64-headless.EFI
#                     naar EFI/BOOT/ op een bestaande FAT-stick (de
#                     headless-variant daar hernoemen naar BOOTAA64.EFI)
#   hopos-rpi5[-headless].zip   Pi 5 — uitpakken op de SD-bootfs
#   hopos-rpi4[-headless].zip   Pi 4 — idem
#   hopos-radxa-zero3[-headless].zip     de drie bootpartitie-bestanden
#   SHA256SUMS(.sig)  ed25519-handtekening + verificatiesleutels
#
# Tekenen: `ssh-keygen -Y` (overal aanwezig, geen keyring-gedoe). Privésleutel
# in ~/.hopos/release_key (buiten de repo, naast de andere geheimen); de
# publieke kant (tools/release_key.pub + tools/allowed_signers) hoort in git
# én zit in elke release. Verificatie voor gebruikers:
#
#   ssh-keygen -Y verify -f allowed_signers -I hello@gethop.org \
#       -n gethop-release -s SHA256SUMS.sig < SHA256SUMS \
#     && shasum -a 256 -c SHA256SUMS
set -e

TAG="${1:?gebruik: tools/release.sh vX.Y.Z}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
KEY="$HOME/.hopos/release_key"
SIGNER="hello@gethop.org"

# 0. Schone tree: binaries horen bij code die in de repo staat. Bewuste
#    uitzondering via RELEASE_ALLOW_DIRTY=1 (alleen als de dirt de release-
#    tooling zelf is — daarna committen).
if [ -n "$(git -C "$DIR" status --porcelain)" ] && [ -z "$RELEASE_ALLOW_DIRTY" ]; then
	echo "FOUT: working tree niet schoon — eerst committen (of RELEASE_ALLOW_DIRTY=1)" >&2
	exit 1
fi

# 1. Release-sleutel (eenmalig): dedicated ed25519 zonder passphrase (machine-
#    sleutel op een vertrouwde werkplek; roteren = nieuw paar committen).
if [ ! -f "$KEY" ]; then
	umask 077
	mkdir -p "$HOME/.hopos"
	ssh-keygen -q -t ed25519 -N "" -C "hopos-release" -f "$KEY"
	echo "nieuwe release-sleutel: $KEY" >&2
fi
cp "$KEY.pub" "$DIR/tools/release_key.pub"
printf '%s namespaces="gethop-release" %s\n' "$SIGNER" "$(cat "$KEY.pub")" \
	> "$DIR/tools/allowed_signers"

DIST="$DIR/out-release/$TAG"
rm -rf "$DIST"
mkdir -p "$DIST"

# gzimg: het dd-bare kaart/stick-image van een board-script naar de release.
# De imagescripts slaan het img LUID over als firmware/donor ontbreekt (en
# ruimen dan een oud exemplaar op); dan hier ook overslaan i.p.v. falen —
# de overige assets zijn compleet, zelfde regel als de licheerv-donor.
gzimg() { # gzimg <bron.img> <asset-naam zonder .gz>
	if [ -f "$1" ]; then
		gzip -9 -n -c "$1" > "$DIST/$2.gz"
	else
		echo ">> OVERGESLAGEN: $2.gz — $1 is niet gebouwd (firmware/donor ontbreekt?)" >&2
	fi
}

# 2. UEFI via HET bouwrecept (image/uefi-run.sh agent; BUILD_ONLY stopt vóór
#    QEMU), in beide smaken: het dd-bare stick-image (hoofdpad) + de losse PE
#    (update van een bestaande stick). Headless éérst en gui laatst, zodat de
#    tree na afloop in de default-staat (gui) achterblijft. Alléén de PE en
#    het vers gebouwde img worden meegenomen — nooit iets anders uit de
#    gitignorede uefi-esp-agent/, daar wonen node-configs met geheimen (het
#    img krijgt zijn config als template uit image/, zie uefi-run.sh).
echo ">> hopos-uefi-headless.img.gz + BOOTAA64-headless.EFI (uefi-run.sh agent, GUI=0, build-only)" >&2
BUILD_ONLY=1 GUI=0 "$DIR/image/uefi-run.sh" agent >/dev/null
cp "$DIR/uefi-esp-agent/EFI/BOOT/BOOTAA64.EFI" "$DIST/BOOTAA64-headless.EFI"
gzimg "$DIR/metal/out/hopos-uefi.img" hopos-uefi-headless.img
echo ">> hopos-uefi.img.gz + BOOTAA64.EFI (uefi-run.sh agent, build-only)" >&2
BUILD_ONLY=1 "$DIR/image/uefi-run.sh" agent >/dev/null
cp "$DIR/uefi-esp-agent/EFI/BOOT/BOOTAA64.EFI" "$DIST/"
gzimg "$DIR/metal/out/hopos-uefi.img" hopos-uefi.img

# 3. Pi-zips: drop-in op de SD-bootfs — precies de bestandsnamen die de
#    firmware verwacht (config.txt wijst de kernel aan), niets hernoemen;
#    alleen de zip-naam draagt de smaak. Zelfde volgorde: headless, dan gui.
#    Elke smaak krijgt zijn standaardconfig mee als hopos.cfg (de TEMPLATES
#    uit image/ — nooit sd-*/hopos.cfg: daar wonen de echte sleutels). Eén
#    edit (hopos.apikey) en de node boot: gui = een desktop, headless = een
#    kale node die op werk wacht (of hopos.init[]-regels in het template).
echo ">> hopos-rpi5[-headless] + hopos-rpi4[-headless] (zip + img.gz)" >&2
CFGGUI="$DIST/.cfg-gui"
CFGHL="$DIST/.cfg-headless"
mkdir -p "$CFGGUI" "$CFGHL"
cp "$DIR/image/hopos-gui.cfg" "$CFGGUI/hopos.cfg"
cp "$DIR/image/hopos-headless.cfg" "$CFGHL/hopos.cfg"
GUI=0 "$DIR/image/rpi5-agent.sh" >/dev/null
(cd "$DIR/sd-rpi5" && zip -q -j "$DIST/hopos-rpi5-headless.zip" hop-agent5.img config.txt "$CFGHL/hopos.cfg")
gzimg "$DIR/metal/out/hopos-rpi5.img" hopos-rpi5-headless.img
"$DIR/image/rpi5-agent.sh" >/dev/null
(cd "$DIR/sd-rpi5" && zip -q -j "$DIST/hopos-rpi5.zip" hop-agent5.img config.txt "$CFGGUI/hopos.cfg")
gzimg "$DIR/metal/out/hopos-rpi5.img" hopos-rpi5.img
GUI=0 "$DIR/image/rpi4-agent.sh" >/dev/null
(cd "$DIR/sd-rpi4" && zip -q -j "$DIST/hopos-rpi4-headless.zip" kernel8.img config.txt "$CFGHL/hopos.cfg")
gzimg "$DIR/metal/out/hopos-rpi4.img" hopos-rpi4-headless.img
"$DIR/image/rpi4-agent.sh" >/dev/null
(cd "$DIR/sd-rpi4" && zip -q -j "$DIST/hopos-rpi4.zip" kernel8.img config.txt "$CFGGUI/hopos.cfg")
gzimg "$DIR/metal/out/hopos-rpi4.img" hopos-rpi4.img
rm -rf "$CFGGUI" "$CFGHL"

# 3b. De UEFI-sticks krijgen dezelfde templates als losse assets: naast
#     EFI/BOOT/BOOTAA64.EFI in de stick-root zetten (headless-variant daar
#     hernoemen naar hopos.cfg), apikey invullen, booten.
cp "$DIR/image/hopos-gui.cfg" "$DIST/hopos.cfg"
cp "$DIR/image/hopos-headless.cfg" "$DIST/hopos-headless.cfg"

# 3b2. Radxa Zero 3E (RK3566): het script bouwt een compleet dd-baar
#      kaart-image (donor-U-Boot raw op zijn gemeten LBA's + onze FAT met
#      extlinux) — dát is het hoofd-asset, want part2 van een donor-kaart is
#      EFI-getypeerd en mount nergens, dus "zet drie bestanden op de
#      bootpartitie" kon niemand uitvoeren. De zip blijft bestaan voor het
#      bijwerken van een kaart die al met ons image geschreven is (die
#      partitie mount wél gewoon). Het script kiest zélf de goede config per
#      smaak (gui = desktop met catalogus, kaal = welcome). Headless eerst,
#      gui laatst — zelfde reden als hierboven: de tree blijft in de
#      default-staat achter.
echo ">> hopos-radxa-zero3[-headless] (zip + img.gz)" >&2
GUI=0 "$DIR/image/radxa-zero3.sh" >/dev/null
(cd "$DIR/metal/out" && zip -q -j "$DIST/hopos-radxa-zero3-headless.zip" hopos-radxa.img extlinux.conf hopos.cfg)
gzimg "$DIR/metal/out/hopos-radxa-zero3.img" hopos-radxa-zero3-headless.img
"$DIR/image/radxa-zero3.sh" >/dev/null
(cd "$DIR/metal/out" && zip -q -j "$DIST/hopos-radxa-zero3.zip" hopos-radxa.img extlinux.conf hopos.cfg)
gzimg "$DIR/metal/out/hopos-radxa-zero3.img" hopos-radxa-zero3.img

# 3c. RISC-V (LicheeRV Nano): een compleet SD-kaart-image, want dit board boot
#     niet van een los bestand — ons image vervangt OpenSBI in het
#     MONITOR-slot van fip.bin. Twee gevolgen voor de release: het asset is een
#     dd-baar .img (gzip: de FAT-partitie is grotendeels leeg), en de config zit
#     ÍNGEBAKKEN — HopOS heeft geen SD-driver, dus er is geen bestand om op de
#     kaart te bewerken. Wat erin zit is de headless-default (het
#     hopos-headless.cfg-asset hierboven — er zijn precies TWEE default-configs,
#     gui en headless, en géén board-specifieke derde); een eigen config is
#     herbouwen met CFG=...
#
#     Alleen bouwen als de Sipeed-donor aanwezig is (vendor-bestand, gitignored
#     in image/licheerv/ — zelfde default als licheerv-agent.sh): zonder slaat
#     de release deze smaak LUIDRUCHTIG over i.p.v. te falen — de rest van de
#     assets is compleet.
if [ -f "${LICHEERV_DONOR:-$DIR/image/licheerv/donor-fip.bin}" ]; then
	# -headless in de assetnaam, óók al is er geen andere smaak: overal elders
	# betekent een naam zónder -headless de gui-smaak, en dat is dit niet.
	echo ">> hopos-licheerv-headless.img.gz (RISC-V, headless-config ingebakken)" >&2
	"$DIR/image/licheerv-agent.sh" >/dev/null
	gzip -9 -n -c "$DIR/metal/out/hopos-licheerv.img" > "$DIST/hopos-licheerv-headless.img.gz"
else
	echo ">> OVERGESLAGEN: hopos-licheerv-headless.img.gz — donor-fip ontbreekt" >&2
	echo "   (zet LICHEERV_DONOR, zie image/licheerv-agent.sh)" >&2
fi

# 3d. Apple silicon (Mac mini M4): geen dd-baar medium, want dit board laadt geen
#     kaart maar een BESTAND — iBoot legt het raw bootobject ergens in DRAM neer
#     (het adres verschilt per boot) en springt naar offset 0x800. Vandaar de
#     twee stubs vooraan (board/apple/bootstub.s): 0x0 is waar een core uit reset
#     landt, 0x800 waar de firmware de boot-core aflevert, en samen verplaatsen
#     ze het image naar zijn linkadres. Installeren doet de eigenaar zelf, uit
#     1TR, met kmutil configure-boot.
#
#     De config gaat MEE IN het image (CFG=, -tags embedcfg), om exact dezelfde
#     reden als bij de LicheeRV: er is geen bestandssysteem dat wij kunnen lezen,
#     dus er is achteraf niets te bewerken. Wat erin zit is de headless-default —
#     hetzelfde hopos-headless.cfg-asset als hierboven, géén board-eigen derde.
#
#     -headless is hier de enige smaak, en dat is geen keuze maar het silicium:
#     de display-firmware (DCP) komt op M4 niet op, dus er is geen framebuffer om
#     een desktop op te zetten (board/apple/apple.go). Zelfde naamgeving als de
#     LicheeRV: de smaak staat in de naam, ook al is er maar één.
#
#     Alleen bouwen met de tamago-fork ernaast (branch hopos-highram — RAM op
#     1TiB valt buiten tamago's vlakke 39-bit-map; image/apple/go.work wijst
#     ernaar). Ontbreekt die, dan slaat de release deze smaak LUIDRUCHTIG over
#     i.p.v. te falen — precies zoals bij de Sipeed-donor hierboven.
if [ -d "${TAMAGO_FORK:-$DIR/../tamago}" ]; then
	echo ">> hopos-m4-headless.img.gz (Apple M4, raw bootobject, cfg ingebakken)" >&2
	AGENT=1 CFG="$DIR/image/hopos-headless.cfg" "$DIR/image/apple-m4.sh" >/dev/null
	gzimg "$DIR/metal/out/hopos-apple.img" hopos-m4-headless.img
else
	echo ">> OVERGESLAGEN: hopos-m4-headless.img.gz — tamago-fork ontbreekt" >&2
	echo "   (zet TAMAGO_FORK, zie image/apple/go.work)" >&2
fi

# 4. Checksums + handtekening (over de checksum-lijst: één .sig dekt alles),
#    met zelf-verificatie vóór publicatie.
echo ">> tekenen + verifiëren" >&2
cd "$DIST"
shasum -a 256 * > SHA256SUMS
ssh-keygen -Y sign -q -f "$KEY" -n gethop-release SHA256SUMS
cp "$DIR/tools/release_key.pub" "$DIR/tools/allowed_signers" .
ssh-keygen -Y verify -q -f allowed_signers -I "$SIGNER" \
	-n gethop-release -s SHA256SUMS.sig < SHA256SUMS

# 5. Uploaden: bestaande release krijgt de assets erbij (--clobber ververst),
#    een nieuwe tag krijgt een verse release.
cd "$DIR"
NOTES="Prebuilt, signed boot images — https://gethop.org/hopos/ for the 5-minute quickstart.

**Images — flash and boot.** \`gunzip\`, \`dd\` to an SD card or USB stick, done: firmware/boot chain is already on it, nothing to rename or copy. The boot partition is plain FAT, so it mounts on macOS/Windows/Linux afterwards — \`hopos.cfg\` stays editable and a kernel update is a file copy.

- **hopos-uefi.img.gz** — any UEFI arm64 box, dd to a USB stick
- **hopos-rpi5.img.gz** — Raspberry Pi 5
- **hopos-rpi4.img.gz** — Raspberry Pi 4
- **hopos-radxa-zero3.img.gz** — Radxa Zero 3E (RK3566), vendor U-Boot chain included on its raw sectors
- **hopos-licheerv-headless.img.gz** — LicheeRV Nano (RISC-V; headless is the only flavour — no framebuffer on that silicon): this board has no SD driver, so its config is **baked into the image** — the baked config IS the headless default (\`hopos-headless.cfg\` below); to change it, rebuild with \`CFG=~/my-node.cfg image/licheerv-agent.sh /dev/diskN\`.
- **hopos-m4-headless.img.gz** — Mac mini M4 (Apple silicon). Not a card image: iBoot loads this **file** as the machine's boot object, wherever it lands in DRAM, so the image carries its own relocation stub. Install it from 1TR on a machine set to Permissive Security: \`kmutil configure-boot -c hopos-m4.img --raw --entry-point 2048 --lowest-virtual-address 0 -v "/Volumes/Macintosh HD"\`. Like the LicheeRV it has **no readable boot filesystem**, so its config is baked in — the baked config IS the headless default (\`hopos-headless.cfg\` below); to change it, rebuild with \`AGENT=1 CFG=~/my-node.cfg image/apple-m4.sh\`. Headless is the only flavour here too: the display firmware does not come up on M4, so there is no framebuffer.
- \`*-headless.img.gz\` — the same images built with \`GUI=0\` (**zero GUI code linked**) and the headless config.

macOS example: \`diskutil unmountDisk /dev/diskN && gunzip -c hopos-rpi5.img.gz | sudo dd of=/dev/rdiskN bs=4m\`

**Updates — files onto an existing boot medium:**

- **hopos-rpi5.zip / hopos-rpi4.zip** — unzip onto the SD bootfs
- **hopos-radxa-zero3.zip** — the three boot-partition files (\`extlinux.conf\` points U-Boot at our image); the partition of a card written from our .img mounts everywhere
- **BOOTAA64.EFI** — the bare UEFI PE, for refreshing an existing stick: copy to \`EFI/BOOT/\`, put \`hopos.cfg\` (below) in the stick root. Headless: rename \`BOOTAA64-headless.EFI\` to \`BOOTAA64.EFI\`.
- **hopos.cfg** — the default GUI config (also inside the images and zips): a full desktop — display, launcher and app catalog, no addresses to fill in and **no edit required to boot**. The API ships open (\`hopos.insecure=1\`) so a written card is a working node; set \`hopos.apikey\` and drop that line before the node leaves a LAN you trust.
- **hopos-headless.cfg** — the headless default (inside the \`*-headless\` images/zips as \`hopos.cfg\`): same keys, no desktop — seed your own \`hopos.init[]\` jobs. For a UEFI stick, rename it to \`hopos.cfg\`.

Verify: \`ssh-keygen -Y verify -f allowed_signers -I $SIGNER -n gethop-release -s SHA256SUMS.sig < SHA256SUMS && shasum -a 256 -c SHA256SUMS\`"
# PRERELEASE=1 markeert de release als pre-release (chain-beta.sh takt hierop
# af): GitHub houdt `latest` dan op de nieuwste STABIELE versie, en dat is waar
# de README en de docs naar linken. Eén release-route dus, met een vlag voor de
# beta-variant — niet een tweede script dat dit nabouwt.
PRE=""
[ -n "${PRERELEASE:-}" ] && PRE="--prerelease"
if gh release view "$TAG" >/dev/null 2>&1; then
	echo ">> assets uploaden naar bestaande release $TAG" >&2
	gh release upload "$TAG" --clobber "$DIST"/*
else
	echo ">> nieuwe release $TAG${PRE:+ (pre-release)}" >&2
	# shellcheck disable=SC2086 — $PRE is een losse vlag of leeg
	gh release create "$TAG" $PRE --title "HopOS $TAG" --notes "$NOTES" "$DIST"/*
fi
echo "KLAAR: https://github.com/xinix00/HopOS/releases/tag/$TAG" >&2
