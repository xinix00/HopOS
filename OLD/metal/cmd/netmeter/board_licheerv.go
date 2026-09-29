//go:build licheerv

// board_licheerv.go — de LicheeRV Nano-kant van de bank (Sophgo SG2002,
// XuanTie C906): board-registratie en de RAM-declaratie, gespiegeld aan
// cmd/hopos/board_licheerv.go. De bank draait als monitor-payload in de FIP
// (PAYLOAD=netmeter image/licheerv-agent.sh) — de hele node is dan het
// meetinstrument voor de RX-jacht, er staat geen agent tussen de meting en
// het silicium.
package main

import (
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/v2/board/licheerv"
	_ "github.com/xinix00/HopOS/metal/v2/board/licheerv/hop" // registreert het board (init) + basis-hooks
)

// Warn vertelt wat dit board niet kan verzwijgen: jitter-DRBG i.p.v. TRNG
// (de pull-github-fase doet TLS), de CLINT-uitslag, en de temperatuurlog per
// minuut — op een fanless board is die trace deel van elke duurmeting.
func init() { boardWarn = licheerv.Warn }

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = licheerv.HopBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = licheerv.HopSize

// serveSize: het HOP-venster is hier 64MB totaal — een blob van 8MiB laat de
// runtime en de stack-buffers ruim ademen, en is zat om TX van buitenaf te
// klokken op 100Mbit.
const serveSize = 8 << 20
