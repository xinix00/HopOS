# Gedeelde stukjes van de image-scripts. Sourcen, niet uitvoeren:
#
#	. "$(dirname "$0")/lib.sh"
#
# Verwacht dat $TAMAGO en $DIR (repo-root) gezet zijn en dat de cwd $DIR/metal is
# op het moment dat je de bake-helpers aanroept.

# clean_embeds verwijdert álle ingebakken build-artifacts uit de sourceboom.
#
# Ze horen daar niet te blijven liggen en dat is geen netheid: de config kan een
# echte apikey bevatten (hij is gitignored, dus je ziet hem niet in `git status`),
# en zolang de resten er staan bouwt een LATERE build ongemerkt mee met de blobs
# van een VORIGE — een oude stub of een config van een ander board. Roep dit
# via een trap aan, zodat het ook gebeurt als de build halverwege faalt:
#
#	trap clean_embeds EXIT INT TERM
clean_embeds() {
	rm -f "$DIR/metal/kern/cagestub/stub-slot.bin" \
		"$DIR/metal/cmd/hopos/cfgblob/hopos.cfg"
}

# Dezelfde kooi-stub voor een koude LicheeRV-boot en een flip-bundel.
build_licheerv_cagestub() {
	riscv64-elf-as -march=rv64imac_zicsr -o "$DIR/metal/out/stub-slot.o" "$DIR/image/licheerv/stub-slot/stub-slot.S"
	riscv64-elf-ld -Ttext="$1" -o "$DIR/metal/out/stub-slot.elf" "$DIR/metal/out/stub-slot.o"
	riscv64-elf-objcopy -O binary "$DIR/metal/out/stub-slot.elf" "$DIR/metal/out/stub-slot.bin"
}
