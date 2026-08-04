//go:build !linkprintk

package licheerv

import (
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/driver/conlog"
)

// DW APB UART (16550-compatibel, reg-shift=2 → registers op 4-byte stride).
// De FSBL heeft UART0 al op 115200 8N1 geconfigureerd; wij hoeven alleen
// te pollen en te schrijven.
const (
	UART_THR = 0x00      // transmit holding register
	UART_LSR = 0x05 << 2 // line status register
	LSR_THRE = 1 << 5    // transmit holding register empty
)

// Géén lock hier, en dat is een bewuste terugdraai (30-07). Er stond een
// spinlock omdat de console er gehakt uitzag, maar de échte oorzaak lag buiten
// HopOS: er stonden twéé cat-processen op dezelfde tty, die de bytestroom
// onderling verdeelden (Dereks vondst), plus byteverlies in de USB-serial-keten —
// zichtbaar óók in de FSBL-uitvoer, die vóór HopOS komt.
//
// En de "fix" was schadelijk: houdt één goroutine de lock vast (panic-pad,
// stop-the-world), dan spint élke andere schrijver zijn volle budget per BYTE.
// Dat zag eruit als een dode node — console stil, HTTP weg — terwijl hij nog
// gewoon op ping antwoordde. Een console mag de node nooit kunnen ophouden.
//
// Gaan twee harten ooit écht om deze UART vechten, dan hoort dat opgelost te
// worden waar de schrijvers zitten (één console-goroutine), niet met een slot in
// de byte-primitief.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	// Eerst in de ring, dán naar de lijn: zit er geen kabel aan de UART of hangt
	// de TX-poll, dan is de byte alsnog over het netwerk op te vragen. Dat is de
	// hele reden dat conlog bestaat (zie dat pakket).
	conlog.Put(c)

	for read32(UART0_BASE+UART_LSR)&LSR_THRE == 0 {
		// wait for TX ready
	}

	write32(UART0_BASE+UART_THR, uint32(c))
}
