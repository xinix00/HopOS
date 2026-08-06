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
# dd-bare kaart-images (gunzip | dd, kaart boot — het hoofdpad):
#   hopos-rpi5[-headless].img.gz         Pi 5
#   hopos-rpi4[-headless].img.gz         Pi 4
#   hopos-radxa-zero3[-headless].img.gz  Radxa Zero 3E (donor-U-Boot ingebakken)
#   hopos-licheerv.img.gz                LicheeRV Nano (headless, cfg ingebakken)
#
# drop-in-updates voor een bestaande kaart + UEFI:
#   BOOTAA64.EFI / BOOTAA64-headless.EFI
#                     elke UEFI-arm64-machine — naar EFI/BOOT/ op een
#                     FAT-stick (de headless-variant daar hernoemen naar
#                     BOOTAA64.EFI)
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

# 2. UEFI-images via HET bouwrecept (image/uefi-run.sh agent; BUILD_ONLY stopt
#    vóór QEMU), in beide smaken. Headless éérst en gui laatst, zodat de tree
#    na afloop in de default-staat (gui) achterblijft. Alléén de PE wordt
#    meegenomen — nooit iets anders uit de gitignorede uefi-esp-agent/, daar
#    wonen node-configs met geheimen.
echo ">> BOOTAA64-headless.EFI (uefi-run.sh agent, GUI=0, build-only)" >&2
BUILD_ONLY=1 GUI=0 "$DIR/image/uefi-run.sh" agent >/dev/null
cp "$DIR/uefi-esp-agent/EFI/BOOT/BOOTAA64.EFI" "$DIST/BOOTAA64-headless.EFI"
echo ">> BOOTAA64.EFI (uefi-run.sh agent, build-only)" >&2
BUILD_ONLY=1 "$DIR/image/uefi-run.sh" agent >/dev/null
cp "$DIR/uefi-esp-agent/EFI/BOOT/BOOTAA64.EFI" "$DIST/"

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
# gzimg: het dd-bare kaart-image van een board-script naar de release.
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
#     kaart te bewerken. Configureren = herbouwen met CFG=... Daarom gaat het
#     template als los asset mee, zodat je ziet wát erin zit.
#
#     Alleen bouwen als de Sipeed-donor aanwezig is (vendor-bestand, gitignored
#     in image/licheerv/ — zelfde default als licheerv-agent.sh): zonder slaat
#     de release deze smaak LUIDRUCHTIG over i.p.v. te falen — de rest van de
#     assets is compleet.
if [ -f "${LICHEERV_DONOR:-$DIR/image/licheerv/donor-fip.bin}" ]; then
	echo ">> hopos-licheerv.img.gz (RISC-V, headless, config ingebakken)" >&2
	CFG="$DIR/image/hopos-licheerv.cfg" "$DIR/image/licheerv-agent.sh" >/dev/null
	gzip -9 -n -c "$DIR/metal/out/hopos-licheerv.img" > "$DIST/hopos-licheerv.img.gz"
	cp "$DIR/image/hopos-licheerv.cfg" "$DIST/hopos-licheerv.cfg"
else
	echo ">> OVERGESLAGEN: hopos-licheerv.img.gz — donor-fip ontbreekt" >&2
	echo "   (zet LICHEERV_DONOR, zie image/licheerv-agent.sh)" >&2
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

**Card images — flash and boot.** \`gunzip\`, \`dd\` to an SD card, done: firmware/U-Boot is already on it. The boot partition is plain FAT, so it mounts on macOS/Windows/Linux afterwards — \`hopos.cfg\` stays editable and a kernel update is a file copy.

- **hopos-rpi5.img.gz** — Raspberry Pi 5
- **hopos-rpi4.img.gz** — Raspberry Pi 4
- **hopos-radxa-zero3.img.gz** — Radxa Zero 3E (RK3566), vendor U-Boot chain included on its raw sectors
- **hopos-licheerv.img.gz** — LicheeRV Nano (RISC-V, headless): this board has no SD driver, so its config is **baked into the image** — \`hopos-licheerv.cfg\` is what went into this build; to change it, rebuild with \`CFG=~/my-node.cfg image/licheerv-agent.sh /dev/diskN\`.
- \`*-headless.img.gz\` — the same cards built with \`GUI=0\` (**zero GUI code linked**) and the headless config.

macOS example: \`diskutil unmountDisk /dev/diskN && gunzip -c hopos-rpi5.img.gz | sudo dd of=/dev/rdiskN bs=4m\`

**Updates & UEFI — files onto an existing boot partition:**

- **hopos-rpi5.zip / hopos-rpi4.zip** — unzip onto the SD bootfs
- **hopos-radxa-zero3.zip** — the three boot-partition files (\`extlinux.conf\` points U-Boot at our image); the partition of a card written from our .img mounts everywhere
- **BOOTAA64.EFI** — any UEFI arm64 box: copy to \`EFI/BOOT/\` on a FAT USB stick, put \`hopos.cfg\` (below) in the stick root. Headless: rename \`BOOTAA64-headless.EFI\` to \`BOOTAA64.EFI\`.
- **hopos.cfg** — the default GUI config (also inside the images and zips): a full desktop — display, launcher and app catalog, no addresses to fill in and **no edit required to boot**. The API ships open (\`hopos.insecure=1\`) so a written card is a working node; set \`hopos.apikey\` and drop that line before the node leaves a LAN you trust.
- **hopos-headless.cfg** — the headless default (inside the \`*-headless\` images/zips as \`hopos.cfg\`): same keys, no desktop — seed your own \`hopos.init[]\` jobs. For a UEFI stick, rename it to \`hopos.cfg\`.

Verify: \`ssh-keygen -Y verify -f allowed_signers -I $SIGNER -n gethop-release -s SHA256SUMS.sig < SHA256SUMS && shasum -a 256 -c SHA256SUMS\`"
if gh release view "$TAG" >/dev/null 2>&1; then
	echo ">> assets uploaden naar bestaande release $TAG" >&2
	gh release upload "$TAG" --clobber "$DIST"/*
else
	echo ">> nieuwe release $TAG" >&2
	gh release create "$TAG" --title "HopOS $TAG" --notes "$NOTES" "$DIST"/*
fi
echo "KLAAR: https://github.com/xinix00/HopOS/releases/tag/$TAG" >&2
