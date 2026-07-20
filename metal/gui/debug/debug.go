// Package debug is het read-only metal-debug-endpoint van de node (:9091):
// hardware-momentopnames zonder UART-kabel of herflash. Eerste bewoner is
// de P4-HVS-dumptool (docs/gui-ontwerp.md §8: eerst read-only kijken hoe de
// firmware de display-pipeline opzette, dan pas muteren). Alles hier is
// strikt lezend; muterende debug-haakjes horen hier níet.
//
// Alleen gui-builds linken dit (cmd/hopos/gui.go, `-tags gui`): een kale
// node heeft geen extra listener.
package debug

import (
	"fmt"
	"net/http"
	"strconv"

	"hop-os/metal/board"
	"hop-os/metal/dev"
	"hop-os/metal/gui/hvs"
)

// Display is het optionele board-contract van deze debugtool: een board mét
// display-blok (rpi5/hop achter `-tags gui`) declareert de HVS-registerbasis
// en het MMIO-venster waarbinnen read-only kijken veilig is — buiten zo'n
// venster kan een verdwaalde read de bus laten hangen. Boards zonder
// implementeren dit simpelweg niet; de endpoints antwoorden dat dan gewoon
// (zo is de bedrading QEMU-testbaar zonder HVS).
type Display interface {
	// HVS geeft de registerbasis van de HVS (bcm2712.dtsi: hvs@107c580000).
	HVS() (uintptr, bool)
	// DisplayMMIO geeft [lo, hi) van het display-blok van de SoC
	// (pixelvalves/mop/moplet/disp_intr/HVS) — de harde grens van /mmio.
	DisplayMMIO() (lo, hi uintptr)
}

// Start start de debug-server zodra het externe netwerk er is.
func Start() {
	mux := http.NewServeMux()
	mux.HandleFunc("/hvs", func(w http.ResponseWriter, r *http.Request) {
		d, ok := board.Current().(Display)
		if !ok {
			http.Error(w, "no hvs on this board", http.StatusNotFound)
			return
		}
		base, ok := d.HVS()
		if !ok {
			http.Error(w, "no hvs on this board", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, hvs.Read(base).Text())
	})
	// /mmio?addr=0x107c501000&len=32 — willekeurige READ-ONLY woorden uit
	// het display-blok van de SoC. Hard begrensd op het venster dat het
	// board declareert, en muteren kan hier per ontwerp niet.
	mux.HandleFunc("/mmio", func(w http.ResponseWriter, r *http.Request) {
		d, ok := board.Current().(Display)
		if !ok {
			http.Error(w, "no display block on this board", http.StatusNotFound)
			return
		}
		mmioLo, mmioHi := d.DisplayMMIO()
		addr, err1 := strconv.ParseUint(r.URL.Query().Get("addr"), 0, 64)
		n, err2 := strconv.ParseUint(r.URL.Query().Get("len"), 0, 64)
		if err2 != nil || n == 0 {
			n = 16 // woorden
		}
		if n > 1024 {
			n = 1024
		}
		a := uintptr(addr)
		if err1 != nil || a%4 != 0 || a < mmioLo || a+uintptr(n)*4 > mmioHi {
			http.Error(w, fmt.Sprintf("addr must be a 4-aligned address in [%#x, %#x)", mmioLo, mmioHi), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		for i := uintptr(0); i < uintptr(n); i++ {
			if i%8 == 0 {
				fmt.Fprintf(w, "\n%#x:", a+i*4)
			}
			fmt.Fprintf(w, " %08x", dev.Read32(a+i*4))
		}
		fmt.Fprintln(w)
	})

	go func() {
		if err := http.ListenAndServe(":9091", mux); err != nil {
			fmt.Printf("debug: listener failed: %v\n", err)
		}
	}()
	fmt.Println("debug: read-only endpoints on :9091 (/hvs, /mmio)")
}
