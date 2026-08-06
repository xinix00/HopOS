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
# hebben we niet in deze repo — hij staat al op elke Radxa/Armbian-kaart.
# Eén keer een officieel Radxa-image naar SD schrijven levert de hele keten
# (TPL/SPL + TF-A + U-Boot); daarna is HopOS-flashen alleen nog bestanden
# vervangen op de bootpartitie:
#
#   1. dd een Radxa Zero 3 image naar de kaart (eenmalig, levert U-Boot)
#   2. mount de bootpartitie en zet daar:
#        proberk3566.img              (dit script bouwt hem)
#        extlinux/extlinux.conf       (dit script schrijft een voorbeeld)
#   3. seriële console: USB-UART op de 40-pins header (pin 8=TX, 10=RX, 6=GND),
#      1500000 8N1 — de Rockchip-default die U-Boot laat staan.
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
	TARGET=./cmd/hopos; NAME=hopos-radxa; TAGS="rk3566 linkcpuinit embedloader$GUITAG"
	# De universele apploader op zijn go:embed-plek. Dit is FASE 1 van élke job:
	# de node laadt hem in elk slot, waarna de app zijn echte image op zijn eigen
	# core en netstack ophaalt. Zonder deze stap start geen enkele job — GEMETEN
	# 06-08, toen de agent op dit bord voor het eerst een job kreeg en meldde
	# "apploader niet ingebakken of uitpakken faalde".
	bake_apploader arm64 linkcpuinit 0x50010000
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

echo "" >&2
echo "metal/out/$NAME.img ($(du -h "metal/out/$NAME.img" | cut -f1)) + extlinux.conf + hopos.cfg klaar." >&2
echo "op de bootpartitie van een Radxa/Armbian-kaart:" >&2
echo "  cp metal/out/$NAME.img metal/out/hopos.cfg <boot>/" >&2
echo "  mkdir -p <boot>/extlinux && cp metal/out/extlinux.conf <boot>/extlinux/" >&2
echo "console: 1500000 8N1 op de 40-pins header (pin 8/10/6)" >&2
