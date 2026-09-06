package el2

import (
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// PrepareSMP publishes a node-owned start context. Only EL1 state is copied
// from the caller's control-page; all EL2 authority comes from HOP. src and
// dst may coincide for node cores, whose control-page is already trusted.
func PrepareSMP(dst, src uintptr, table, vmid, mailbox, vectors uint64) {
	for _, off := range [...]uintptr{
		layout.CtrlSMPSp, layout.CtrlSMPMp, layout.CtrlSMPG0,
		layout.CtrlSMPFn, layout.CtrlSMPTtbr0, layout.CtrlSMPStub,
		layout.CtrlSMPMair, layout.CtrlSMPTcr, layout.CtrlSMPVbar,
	} {
		dev.Pull(src+off, 8)
		dev.Write64(dst+off, dev.Read64(src+off))
	}
	dev.Write64(dst+layout.CtrlS2Table, table)
	dev.Write64(dst+layout.CtrlSlot, vmid)
	dev.Write64(dst+layout.CtrlSMPMbox, mailbox)
	dev.Write64(dst+layout.CtrlVecPA, vectors)
	dev.Push(dst, 256)
	dev.MB()
}
