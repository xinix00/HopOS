#!/bin/sh
# Release-assets: bouwt de boot-images uit de tree, tekent ze en hangt ze aan
# de GitHub-release — de "signed images" waar gethop.org naar verwijst:
#
#   tools/release.sh v1.2.2        # bestaande release: assets uploaden
#   tools/release.sh v1.3.0        # nieuwe tag: release + assets aanmaken
#
# Artefacten (drop-in), elk in twee smaken — gui (default) en headless
# (GUI=0): headless is geen uitgezette gui maar een build waar geen enkele
# regel gui-code in gelinkt zit.
#   BOOTAA64.EFI / BOOTAA64-headless.EFI
#                     elke UEFI-arm64-machine — naar EFI/BOOT/ op een
#                     FAT-stick (de headless-variant daar hernoemen naar
#                     BOOTAA64.EFI)
#   hopos-rpi5[-headless].zip   Pi 5 — uitpakken op de SD-bootfs
#   hopos-rpi4[-headless].zip   Pi 4 — idem
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
echo ">> hopos-rpi5[-headless].zip + hopos-rpi4[-headless].zip" >&2
GUI=0 "$DIR/image/rpi5-agent.sh" >/dev/null
(cd "$DIR/sd-rpi5" && zip -q -j "$DIST/hopos-rpi5-headless.zip" hop-agent5.img config.txt)
"$DIR/image/rpi5-agent.sh" >/dev/null
(cd "$DIR/sd-rpi5" && zip -q -j "$DIST/hopos-rpi5.zip" hop-agent5.img config.txt)
GUI=0 "$DIR/image/rpi4-agent.sh" >/dev/null
(cd "$DIR/sd-rpi4" && zip -q -j "$DIST/hopos-rpi4-headless.zip" kernel8.img config.txt)
"$DIR/image/rpi4-agent.sh" >/dev/null
(cd "$DIR/sd-rpi4" && zip -q -j "$DIST/hopos-rpi4.zip" kernel8.img config.txt)

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

- **BOOTAA64.EFI** — any UEFI arm64 box: copy to \`EFI/BOOT/\` on a FAT USB stick, add \`hopos.cfg\`
- **hopos-rpi5.zip** — Raspberry Pi 5: unzip onto the SD bootfs
- **hopos-rpi4.zip** — Raspberry Pi 4: unzip onto the SD bootfs
- **\`*-headless\`** — the same images built with \`GUI=0\`: not a disabled GUI but **zero GUI code linked**. For UEFI, rename \`BOOTAA64-headless.EFI\` to \`BOOTAA64.EFI\` on the stick.

Verify: \`ssh-keygen -Y verify -f allowed_signers -I $SIGNER -n gethop-release -s SHA256SUMS.sig < SHA256SUMS && shasum -a 256 -c SHA256SUMS\`"
if gh release view "$TAG" >/dev/null 2>&1; then
	echo ">> assets uploaden naar bestaande release $TAG" >&2
	gh release upload "$TAG" --clobber "$DIST"/*
else
	echo ">> nieuwe release $TAG" >&2
	gh release create "$TAG" --title "HopOS $TAG" --notes "$NOTES" "$DIST"/*
fi
echo "KLAAR: https://github.com/xinix00/HopOS/releases/tag/$TAG" >&2
