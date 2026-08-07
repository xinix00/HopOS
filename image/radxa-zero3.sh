#!/bin/sh -e
# Bouw HopOS voor de Radxa Zero 3E (Rockchip RK3566). Naast rpi5-agent.sh en
# licheerv-agent.sh; zelfde vorm, ander silicium.
#
#   image/radxa-zero3.sh          → de ECHTE agent (cmd/hopos, -tags rk3566)
#   GUI=0 image/radxa-zero3.sh    → de kale (headless) smaak; default is MÉT gui
#   CFG=~/mijn-node.cfg image/...  → met je eigen config (sleutels horen daar,
#                                    niet in de cfg-bestanden in de repo)
#   PROBE=1 image/radxa-zero3.sh  → de bring-up-probe (cmd/proberk3566)
#   EMBED=1 image/radxa-zero3.sh  → het acceptatiedraaiboek met de app INGEBAKKEN
#                                   (cmd/hopos-embed): bewijst de kooi zonder
#                                   netwerk, want de app-image reist in het binary
#
# De probe blijft bestaan omdat hij een ándere vraag beantwoordt dan de agent:
# hij meet dit silicium (EL, MPIDR, DTB, PSCI, GIC, GMAC) zonder van een
# netwerk of een plan af te hangen. Trede 1 en 2 zijn ermee gehaald; zie
# docs/archief/radxa-zero3.md.
#
# Boot-route (het verschil met de Pi's): dit bord boot via U-Boot, en die
# schrijven we niet zelf — de BROM leest hem raw van vaste LBA's, vóór de
# eerste partitie. Dit script bouwt daarom een compleet, dd-baar kaart-image
# (zelfde vorm als de LicheeRV): onze eigen MBR + FAT16-bootpartitie, met de
# U-Boot-keten (TPL/SPL + TF-A + U-Boot) als donor-bytes uit het officiële
# Radxa-image — eenmalig gedownload en gitignored gecachet, zie hieronder.
#
#   diskutil unmountDisk /dev/diskN
#   sudo dd if=metal/out/hopos-radxa-zero3.img of=/dev/rdiskN bs=4m
#
# Waarom een heel image en geen "zet drie bestanden op de bootpartitie van een
# Radxa-kaart" (de oude route): part2 van zo'n donor-kaart is als EFI-partitie
# getypeerd, dus macOS mount hem niet en niemand kán die bestanden er zonder
# raw-dd op zetten (gemeten 05-08). Onze eigen partitie is type 0x0C — die
# mount ná het flashen wél gewoon, dus hopos.cfg blijft bewerkbaar en een
# nieuwe kernel is een cp. Seriële console: USB-UART op de 40-pins header
# (pin 8=TX, 10=RX, 6=GND), 1500000 8N1 — de Rockchip-default.
#
# U-Boot's distro-boot vindt extlinux.conf, laadt het Image en `booti` springt
# op EL2 met x0=DTB — daarna is alles meetbaar op de UART.

TAMAGO="${TAMAGO:-$HOME/tamago-go/bin/go}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
. "$DIR/image/lib.sh"

cd "$DIR/metal"
mkdir -p out

# Wat bouwen we: de agent (default) of de probe (PROBE=1)?
if [ "${PROBE:-0}" = 1 ]; then
	TARGET=./cmd/proberk3566; NAME=proberk3566; TAGS="linkcpuinit"
elif [ "${EMBED:-0}" = 1 ]; then
	TARGET=./cmd/hopos-embed; NAME=hopos-radxa-embed; TAGS="rk3566 linkcpuinit"
	# De app-image die de main via go:embed meedraagt. Board-onafhankelijk (de
	# stage-2 ís de relocatie), dus één build dekt elk slot; het linkadres is de
	# slot-1-IPA. Blijft in de boom liggen als build-output — hij is gitignored.
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
		"$TAMAGO" build -tags linkcpuinit -trimpath \
		-ldflags "-w -T 0x50010000 -R 0x1000" -o cmd/hopos-embed/app.elf ./app/appspike
else
	# Default GUI, GUI=0 bouwt de kale (headless) smaak — zelfde knop als op de
	# Pi's. Dit bord heeft sinds 06-08 een echte scanout naar HDMI, dus "gui" is
	# hier geen QEMU-vinkje maar beeld op een monitor.
	GUITAG=""
	[ "${GUI:-1}" = 1 ] && GUITAG=" gui"
	TARGET=./cmd/hopos; NAME=hopos-radxa; TAGS="rk3566 linkcpuinit$GUITAG"
fi

# Linkadres 0x02210000 in het venster vanaf 0x02200000 (rk3566.RamBase):
# dezelfde +0x10000-vorm als de Pi (tekst boven tamago's paginatabellen). De
# basis is 2MB-uitgelijnd omdat een arm64-Image dat hoort te zijn.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
	"$TAMAGO" build -tags "$TAGS" -trimpath \
	-ldflags "-T 0x02210000 -R 0x1000" -o "out/$NAME.elf" "$TARGET"

# ELF → arm64 Image (mkkernel ZONDER -raw: booti wil de ARM\x64-header).
# -dram 0x200000 is GEMETEN (05-08): U-Boot rapporteert DRAM-start daar, en
# booti legt een niet-relocatable Image op dram+text_offset. Zonder deze som
# landde de kernel 2MB te hoog en zweeg hij na "Starting kernel".
cd "$DIR"
go run "$DIR/image/mkkernel/main.go" "$DIR/image/mkkernel/pe.go" \
	-elf "metal/out/$NAME.elf" -o "metal/out/$NAME.img" -load 0x02200000 -dram 0x200000

# extlinux-voorbeeld: distro-boot pakt de eerste entry. Het DTB komt uit
# U-Boot zelf (fdtdir op een Radxa-kaart, of diens control-DTB); de APPEND en
# INITRD bewijzen meteen de bootargs- en hopos.cfg-route van de agent-fase.
cat > metal/out/extlinux.conf <<EOF
# HopOS — Radxa Zero 3E (zie docs/archief/radxa-zero3.md). Deze config staat op
# de FAT-partitie "boot" (part2): U-Boot scant partities op volgorde en vindt
# hem vóór die van een Debian op part3.
#
# De APPEND-regel is het bootargs-kanaal en INITRD het configbestand-kanaal;
# beide gemeten werkend op 05-08 (board/rk3566/bootparam.go leest ze).
timeout 1
default hopos

label hopos
    kernel /$NAME.img
    initrd /hopos.cfg
    append hopos.node=radxa-1
EOF

# De standaardconfig meekopiëren naar out/, zodat "wat je flasht" één map is en
# je niet per ongeluk een oude hopos.cfg van de kaart laat staan. Eigen sleutels
# horen NIET in de repo: geef CFG=~/mijn-node.cfg mee en die wint.
#
# DEZELFDE bestanden als elk ander board (release.sh zet ze in de zips, de Pi's
# wijzen ernaar): de gui-smaak krijgt de desktop MET de app-catalogus, de kale
# smaak `welcome`. Dit board heeft even een eigen paar cfg's gehad, en dat is
# precies één ding waard geweest: een launcher zonder knoppen. De app-catalogus
# (hopos.apps[]) stond alleen in de standaard, dus wie hier iets verzint levert
# een desktop af waarin niets te starten valt. Board-specifieks hoort in de
# APPEND-regel van extlinux hierboven, niet in een tweede config.
DEFCFG="$DIR/image/hopos-headless.cfg"
[ "${GUI:-1}" = 1 ] && [ "${PROBE:-0}" != 1 ] && DEFCFG="$DIR/image/hopos-gui.cfg"
cp "${CFG:-$DEFCFG}" metal/out/hopos.cfg

# Donor-boot: de bytes LBA 64..32767 van het officiële Radxa Zero 3-image —
# idbloader (RKNS-blok, LBA 64) + u-boot.itb (LBA 16384), sámen de keten
# TPL/SPL + TF-A + U-Boot die de BROM raw leest, plus het (lege) U-Boot-
# env-gebied ertussen. Vendor-bytes, dus gitignored en eenmalig gedownload;
# de download pakt alléén de eerste 16MiB van het 2,5GB-image uit en breekt
# dan af. Gepind op release b6 mét hash: dit zijn exact de bytes waarmee alle
# metingen van 05/06-08 gedaan zijn (docs/archief/radxa-zero3.md).
DONOR="${RADXA_DONOR:-$DIR/image/radxa/donor-boot.bin}"
DONOR_URL="https://github.com/radxa-build/radxa-zero3/releases/download/b6/radxa-zero3_debian_bullseye_cli_b6.img.xz"
DONOR_SHA="9a582a0c6fcd8b41d5627284aa9b831a46a08e7606c6fc31901f48725354a7b9"
if [ ! -f "$DONOR" ]; then
	echo "== donor-boot ophalen (eenmalig, ~10MB van de stream): $DONOR_URL ==" >&2
	mkdir -p "$(dirname "$DONOR")"
	python3 - "$DONOR_URL" "$DONOR.part" <<'PYEOF'
# Stream-download + xz-decompressie van precies de eerste 16MiB, dan de MBR/GPT
# van de donor (LBA 0..63) eraf: die tabel is van óns (mkcard). python3-lzma
# i.p.v. curl|xz zodat er geen xz-binary nodig is.
import lzma, sys, urllib.request
WANT, SKIP = 16 << 20, 64 * 512
dec = lzma.LZMADecompressor()
buf = bytearray()
with urllib.request.urlopen(sys.argv[1]) as r:
    while len(buf) < WANT:
        chunk = r.read(1 << 20)
        if not chunk:
            sys.exit(f"stream ended at {len(buf)} bytes — expected {WANT}")
        buf += dec.decompress(chunk)
open(sys.argv[2], "wb").write(bytes(buf[SKIP:WANT]))
PYEOF
	mv "$DONOR.part" "$DONOR"
fi
# Hash-check bij ELKE build (16MB, verwaarloosbaar): vangt een truncated
# download én een per ongeluk vervangen cache — de les van deze bring-up is
# dat een payload die niet klopt een boot-cyclus kost.
GOT=$(shasum -a 256 "$DONOR" | cut -d' ' -f1)
if [ "$GOT" != "$DONOR_SHA" ]; then
	echo "FOUT: $DONOR heeft hash $GOT, verwacht $DONOR_SHA" >&2
	echo "  (verwijder het bestand en draai opnieuw voor een verse download," >&2
	echo "   of zet RADXA_DONOR naar een eigen donor-boot-blok)" >&2
	exit 1
fi

# Het complete, dd-bare kaart-image: onze MBR (één FAT16-partitie, type 0x0C,
# vanaf LBA 32768 = 16MiB), de donor-bytes raw op hun gemeten plek, en onze
# drie bestanden in de FAT. Alles onder de partitie is exact de donor-kaart;
# de partitie zelf is de verse-FAT-les van 05-08 (data altijd vooraan,
# deterministisch, verifieerbaar).
CARD="metal/out/hopos-radxa-zero3.img"
if [ "${PROBE:-0}" = 1 ] || [ "${EMBED:-0}" = 1 ]; then
	CARD="metal/out/$NAME-card.img"
fi
go run "$DIR/image/mkcard/main.go" -o "$CARD" -size 64 -start 32768 \
	-label hopos -vollabel -cfgwindow 1048576 -raw "$DONOR@32768" \
	"metal/out/$NAME.img" metal/out/hopos.cfg \
	metal/out/extlinux.conf=extlinux/extlinux.conf >&2

echo "" >&2
echo "$CARD ($(du -h "$CARD" | cut -f1), dd-baar) klaar — kernel $NAME.img + extlinux.conf + hopos.cfg." >&2
echo "flash: diskutil unmountDisk /dev/diskN && sudo dd if=$CARD of=/dev/rdiskN bs=4m" >&2
echo "daarna: de partitie 'hopos' mount gewoon — hopos.cfg bewerken of een nieuwe kernel erop is een cp." >&2
echo "console: 1500000 8N1 op de 40-pins header (pin 8/10/6)" >&2
