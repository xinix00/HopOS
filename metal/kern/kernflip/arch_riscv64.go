//go:build tamago && riscv64

package kernflip

import (
	"runtime"

	"github.com/xinix00/HopOS/metal/v2/cpu/mmode"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
)

// De RISC-V-helft van de flip-naad (docs/kern-flip.md) — zie arch_arm64.go
// voor het contract. Twee dingen zijn hier eenvoudiger, en allebei omdat HOP
// zelf in machine mode zonder MMU draait: de sprong is een kale JALR (geen
// exception-niveau om over te steken), en de nieuwe kern verwacht geen
// firmware-argument (de FSBL geeft er ook geen).

// De code die een app-hart uitvoert woont in cpu/mmode en wordt door
// kern/slots (cage_riscv64) naar de plan-regio gekopieerd.
func blobSymbols() [][2]string { return mmode.BlobSymbols }
func maxBlobSize() uint64      { return mmode.MaxBlobSize }
func switchCodeHash() uint64   { return slots.SwitchCodeHash() }
func setAdopting(v bool)       { slots.SetAdopting(v) }

// nodeCoresActive: dit board geeft HOP één hart (node_riscv64.go:
// smp.ConfigureNode is er een no-op), dus er zijn nooit extra node-cores om
// rekening mee te houden.
func nodeCoresActive() int { return 0 }

func firmwareArg() uint64 { return 0 }

// archPreflight: riscv64 geeft (nog) geen firmware-woord door — daar is x0=0
// het contract, geen fout.
func archPreflight() error { return nil }

func chainload(entry, x0arg uint64) { slots.ChainloadM(entry, x0arg) }

func ownRamEnd() uint64 {
	_, end := runtime.MemRegion()
	return uint64(end)
}
