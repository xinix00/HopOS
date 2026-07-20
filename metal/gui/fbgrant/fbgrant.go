// Package fbgrant is de kleinste DeviceGrant (docs/gui-ontwerp.md §7/§8) —
// HOP mapt de firmware-framebuffer in de kooi van één app (de display-server
// van hop-os-surf), zodat dezelfde compositie die headless via /screen.png
// loopt ook op écht glas landt. Een lineaire pixelbuffer: geen registers,
// geen DMA, geen interrupts — daarom mag dit vooruitlopen op het volledige
// DeviceGrant-contract.
//
// Dit is gui-beleid en woont dus in metal/gui; kern/slots draagt alleen de
// lifecycle-haakjes (slots.RegisterGrant — de bedrading zit in cmd's
// gui-smaak). De mechaniek eronder (stage2.GrantWindow, layout.FbIPA) is
// basis: het generieke DeviceGrant-primitief.
//
// Aanvraag: de job zet FB=1 in zijn env (de jobspec is HOP-domein; zolang
// signing uitstaat is HOP zelf de enige bron van jobs, dus dit is geen
// escalatiepad — herzien bij publisher-signing). Eén houder tegelijk; de
// eerste aanvrager wint. HOP's eigen fb-console gaat van het glas zolang de
// grant leeft en komt terug wanneer het slot vrijkomt — een gecrashte
// display-app geeft je dus vanzelf de bootconsole terug.
package fbgrant

import (
	"fmt"
	"sync"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/driver/fb"
	"hop-os/metal/kern/stage2"
)

var (
	mu     sync.Mutex
	holder int    // slot met de grant (0 = niemand)
	base   uint64 // venster zoals gemapt (voor Arm/GrantWindow)
	size   uint64
)

// Env is de prepStart-hook (slots.GrantHooks.Env): kent bij env["FB"]=="1"
// de framebuffer exclusief aan slot i toe en geeft een env-kopie met de
// FB_*-beschrijving terug (het contract van cmd/display's fbblit.go in
// hop-os-surf). Zonder framebuffer of bij een andere houder blijft de env
// onaangeroerd — de app draait dan headless, dat is geen fout.
func Env(i int, env map[string]string) map[string]string {
	if env["FB"] != "1" {
		return env
	}
	d, ok := board.Current().Framebuffer()
	if !ok || d.Base == 0 || d.Height <= 0 || d.Stride <= 0 {
		fmt.Printf("slot %d: fb grant requested, board has no framebuffer\n", i)
		return env
	}
	mu.Lock()
	defer mu.Unlock()
	if holder != 0 && holder != i {
		fmt.Printf("slot %d: fb grant requested, already held by slot %d\n", i, holder)
		return env
	}
	holder = i
	base = uint64(d.Base)
	size = uint64(d.Stride) * uint64(d.Height)

	out := make(map[string]string, len(env)+6)
	for k, v := range env {
		out[k] = v
	}
	// De app ziet het venster op FbIPA (32-bit IPA-ruimte; de fysieke fb mag
	// boven de 4GB liggen): FbIPA + de offset van de fb in zijn 2MB-blok.
	ipa := uint64(layout.FbIPA) + (uint64(d.Base) - (uint64(d.Base) &^ ((2 << 20) - 1)))
	out["FB_BASE"] = fmt.Sprintf("%#x", ipa)
	out["FB_WIDTH"] = fmt.Sprintf("%d", d.Width)
	out["FB_HEIGHT"] = fmt.Sprintf("%d", d.Height)
	out["FB_STRIDE"] = fmt.Sprintf("%d", d.Stride)
	out["FB_BPP"] = fmt.Sprintf("%d", d.BPP)
	if d.SwapRB {
		out["FB_SWAP"] = "1"
	}

	// Het glas is nu van de app: HOP's log-console eraf (Putc wordt no-op).
	fb.Disable()
	fmt.Printf("slot %d: fb granted %dx%d stride %d @ %#x\n", i, d.Width, d.Height, d.Stride, d.Base)
	return out
}

// Arm is de armSlot-hook (slots.GrantHooks.Arm): mapt (ná stage2.Build) het
// venster in de kooi van de houder. Voor andere slots een no-op.
func Arm(i int) error {
	mu.Lock()
	h, b, s := holder, base, size
	mu.Unlock()
	if h != i {
		return nil
	}
	return stage2.GrantWindow(i, b, s)
}

// Release is de releaseSlot-hook (slots.GrantHooks.Release): geeft de grant
// terug bij het vrijkomen van het slot en zet HOP's console terug op het
// scherm (verse Init: schone lei, log loopt weer).
func Release(i int) {
	mu.Lock()
	held := holder == i
	if held {
		holder = 0
	}
	mu.Unlock()
	if !held {
		return
	}
	if d, ok := board.Current().Framebuffer(); ok {
		fb.Init(d)
	}
	fmt.Printf("slot %d: fb grant released, console back on glass\n", i)
}
