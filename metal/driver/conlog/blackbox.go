package conlog

// De ZWARTE DOOS: dezelfde console-bytes, maar in DRAM buiten élke
// RAM-declaratie — dus van geen enkele kern eigendom, en daarmee het enige
// spoor dat een crash én de reboot erna overleeft.
//
// WAAROM (Derek, 06-09: "storage is ook een manier om te loggen... beter dan
// flaky UART"). Klopt, met één maar: opslag komt pas laat in de boot op, en
// juist de vroege boot is waar het misgaat. Een kern-flip die niet landt
// sterft vóór zijn netwerk, vóór zijn NVMe en soms vóór zijn console —
// gemeten 06-09 op de M4: "jumped into generation N and never came up", en op
// tcp/5555 stond niets omdat die kern nooit zo ver kwam. Een UART zou het
// zien, maar die hangt hier aan een USB-kabel die telkens opnieuw ingeplugd
// moet worden.
//
// Deze ring is dezelfde truc als de vluchtrecorder (kern/kernflip/stage.go),
// alleen met tekst in plaats van één woord. Hij ligt op een board-adres naast
// de recorder, is device-gemapt (dus elke byte staat meteen in DRAM, zonder
// cache-onderhoud) en wordt bij de volgende boot uitgelezen vóór er iets
// overheen gaat. Kost per byte één 8-bits device-write — naast de UART-poll
// die er toch al staat is dat niets.

import "github.com/xinix00/HopOS/metal/v2/dev"

// De kop: magic + hoeveel bytes er ooit geschreven zijn (monotoon, wrapt in de
// data). 16 bytes, daarna de data.
const (
	bbMagic  = 0x484F50_42425831 // "HOPBBX1"
	bbHdrLen = 16
)

var (
	bbBase uintptr
	bbCap  int
	bbHead uint64
	bbPrev []byte // wat de vórige boot achterliet
	bbMute bool   // tijdens het uitschrijven van bbPrev niet spiegelen
)

// UseBlackBox neemt de ring op pa in gebruik. De teller loopt DOOR over boots
// heen — bewust: de kern die je wilt lezen is juist degene die net gestorven
// is, en die schreef zijn laatste bytes vlak vóór de reset. Zou elke boot de
// ring legen, dan wist een kern die meteen omvalt precies het spoor dat je
// zocht (gemeten 06-09: de gecrashte kern haalde zijn main, veegde de doos en
// stierf, en de volgende boot vond niets). PreviousBoot geeft dus alles wat er
// vóór deze boot in stond; de staart daarvan is de stervende kern.
//
// Aanroepen zodra het board zijn plan heeft en vóór de eerste console-byte die
// je wilt bewaren; pa 0 of een te kleine size schakelt hem uit.
func UseBlackBox(pa uintptr, size int) {
	if pa == 0 || size <= bbHdrLen+64 {
		return
	}
	capacity := size - bbHdrLen
	head := uint64(0)
	if dev.Read64(pa) == bbMagic {
		head = dev.Read64(pa + 8)
		if head > 0 {
			n := int(head)
			if n > capacity {
				n = capacity
			}
			buf := make([]byte, n)
			start := head - uint64(n)
			for i := 0; i < n; i++ {
				buf[i] = dev.Read8(pa + bbHdrLen + uintptr((start+uint64(i))%uint64(capacity)))
			}
			bbPrev = buf
		}
	} else {
		dev.Write64(pa, bbMagic)
		dev.Write64(pa+8, 0)
		dev.MB()
	}
	bbBase, bbCap, bbHead = pa, capacity, head
}

// PreviousLen is hoeveel bytes er van vóór deze boot in de doos stonden. Nul
// betekent: de doos was leeg of iemand heeft hem onderweg gewist.
func PreviousLen() int { return len(bbPrev) }

// PreviousBoot geeft de console-bytes die de vorige boot achterliet (nil als
// er niets stond). Eén keer bruikbaar; daarna is de buffer vrijgegeven.
func PreviousBoot() []byte {
	b := bbPrev
	bbPrev = nil
	return b
}

// Mute zet het spiegelen tijdelijk uit — voor het uitschrijven van een vorige
// boot, dat anders zichzelf de nieuwe ring in schrijft.
func Mute(on bool) { bbMute = on }

// bbPut spiegelt één byte. Geen lock: net als de rest van deze console mag hij
// de node nooit ophouden, en een verminkte byte in een post-mortem is
// oneindig veel beter dan geen post-mortem.
func bbPut(c byte) {
	if bbBase == 0 || bbMute {
		return
	}
	dev.Write8(bbBase+bbHdrLen+uintptr(bbHead%uint64(bbCap)), c)
	bbHead++
	dev.Write64(bbBase+8, bbHead)
}
