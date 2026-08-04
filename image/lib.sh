# Gedeelde stukjes van de image-scripts. Sourcen, niet uitvoeren:
#
#	. "$(dirname "$0")/lib.sh"
#
# Verwacht dat $TAMAGO en $DIR (repo-root) gezet zijn en dat de cwd $DIR/metal is
# op het moment dat je bake_apploader aanroept.

# bake_apploader <goarch> <tags> <textstart> — bouwt de universele apploader en
# legt hem GECOMPRIMEERD op zijn go:embed-plek (kern/apploaderblob).
#
# Dit is fase 1 van élke job: de node bakt hem in (-tags embedloader) en laadt hem
# in élk slot, waarna de app zijn echte image op zijn eigen core en netstack
# ophaalt. Zonder ingebakken loader start geen enkele job — de twee-fase-lading is
# de enige route.
#
# Waarom gzip -9 -n: de blob zit 6× in de Altra-PE (één per venster-kandidaat) en
# 1× per Pi/LicheeRV-image (8,4→3,1MB); de node pakt hem één keer lazy uit. -n laat
# naam en tijdstempel uit de gzip-header, zodat twee builds van dezelfde bron
# byte-identiek zijn.
#
# Dit recept stond vijf keer los in image/ (vier boards + qemu-run) en moest bij
# elke wijziging vijf keer mee — nu één keer. De ELF verschilt per architectuur en
# per linkadres, dus dát zijn de argumenten.
bake_apploader() {
	_arch="$1"
	_tags="$2"
	_text="$3"
	_elf="$DIR/metal/kern/apploaderblob/apploader.elf"
	GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH="$_arch" \
		"$TAMAGO" build -tags "$_tags" -trimpath \
		-ldflags "-w -T $_text -R 0x1000" -o "$_elf" ./app/apploader
	gzip -9 -n -f "$_elf"
}

# clean_embeds verwijdert álle ingebakken build-artifacts uit de sourceboom.
#
# Ze horen daar niet te blijven liggen en dat is geen netheid: de config kan een
# echte apikey bevatten (hij is gitignored, dus je ziet hem niet in `git status`),
# en zolang de resten er staan bouwt een LATERE build ongemerkt mee met de blobs
# van een VORIGE — een oude apploader of een config van een ander board. Roep dit
# via een trap aan, zodat het ook gebeurt als de build halverwege faalt:
#
#	trap clean_embeds EXIT INT TERM
clean_embeds() {
	rm -f "$DIR/metal/kern/apploaderblob/apploader.elf" \
		"$DIR/metal/kern/apploaderblob/apploader.elf.gz" \
		"$DIR/metal/kern/cagestub/stub-slot.bin" \
		"$DIR/metal/cmd/hopos/cfgblob/hopos.cfg"
}
