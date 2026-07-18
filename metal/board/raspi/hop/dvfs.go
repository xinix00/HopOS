package hop

import (
	"hop-os/metal/board/raspi"
	"hop-os/metal/driver/dvfs"
	"hop-os/metal/driver/vcmail"
)

// StartDVFS start het Pi-klokbeleid (docs/archief/plan-p2b-soak.md): klok volgt idle
// via de firmware-mailbox. De config stond drie keer uitgeschreven
// (board_rpi4/board_rpi5 in cmd/hopos + pi5_main in cmd/hopos-embed) met de
// mailbox-basis als enige verschil — dat is nu de parameter. Verbose logt
// elke flank (soak-diagnose).
func StartDVFS(vcMailBase uintptr) {
	dvfs.Start(dvfs.Config{
		Mbox:    &vcmail.Mbox{Base: vcMailBase, Buf: uintptr(raspi.VCMailBuf)},
		LowHz:   600_000_000,
		Verbose: true,
	})
}
