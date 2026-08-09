// De gateway-poort: 10.100.0.1 is "mijn node" (Dereks besluit 20-07 — geen
// {{host}}-hairpin, gewoon één vast intern adres dat op élke node hetzelfde
// is). HOP hangt daarvoor zelf als poort 0 aan zijn eigen switch: een
// tweede, interne NIC op de node-stack (hopnet/internal.go). Frames van een
// slot naar het gateway-IP gaan die NIC in — geen NAT, de 4-tupel is
// vanzelf symmetrisch — en de antwoorden komen via FromGateway terug de
// switch in. Daarmee bereikt een app de agent/leader (:8080/:9080) op
// 10.100.0.1, zonder proxy en zonder dat er één byte de fysieke NIC uit gaat.
package hopswitch

import (
	"encoding/binary"
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// gatewayRx is de invoer van HOP's interne NIC (gezet door hopnet/internal):
// gateway-frames die geen NAT-route hebben gaan hierheen in plaats van "weg
// te vallen". Het frame is alleen geldig tijdens de aanroep (de switch-lus
// hergebruikt zijn buffer) — de ontvanger kopieert.
var gatewayRx func(p []byte)

// gwQueue is de wachtrij naar die naad. De aflevering mag NOOIT onder mu
// gebeuren: een netstack kan SYNCHROON binnen zijn ontvangst antwoorden (een
// SYN naar een gesloten node-poort levert direct een RST), en dat antwoord
// komt via de gateway-naad de switch weer in — op dezelfde goroutine, die dan
// mu opnieuw wil pakken. sync.Mutex is niet reentrant, dus
// dat is een self-deadlock met mu vast: de hele switch staat stil en élke app
// kan het uitlokken met één pakketje. Daarom onder mu alleen kopiëren en
// enqueuen (gwEnqueueLocked), en ná de unlock afleveren (drainGateway).
var gwQueue [][]byte

// gwQueueMax begrenst de wachtrij: dit pad is node-intern (agent/leader-
// verkeer), dus een handvol frames volstaat. Vol = drop, zoals elke andere
// volle ring hier — nooit ongebonden groeien op een app-gedreven pad.
const gwQueueMax = 64

// SetGatewayRx registreert de interne NIC-invoer (éénmalig bij hopnet-init).
func SetGatewayRx(f func(p []byte)) {
	mu.Lock()
	gatewayRx = f
	mu.Unlock()
}

// FromGateway voert één frame van HOP's interne NIC de switch in — bron 0
// (de gateway): bezorgen op dst-MAC, broadcasts (ARP-requests van de interne
// NIC naar slot-IP's) flooden naar alle slots. No-op zolang de switch niet
// Up() is (zelfde contract als Attach).
// Let op: dit kan (via de RST-teruglus) ónder een drainGateway lopen, dus de
// mu-sectie is strikt begrensd en de eigen drain volgt ná de unlock.
func FromGateway(p []byte) {
	mu.Lock()
	if up {
		forward(0, p)
	}
	mu.Unlock()
	drainGateway()
}

// gatewayClaimLocked (mu vast, vanuit forward): hoort dit gateway-frame bij
// HOP's interne NIC? Ja voor IPv4 naar het gateway-IP (10.100.0.1 — de
// agent/leader-poorten) en voor niet-IPv4-unicast naar de gateway-MAC (de
// ARP-replies op de eigen requests van de interne NIC). true = bezorgd.
func gatewayClaimLocked(p []byte) bool {
	if gatewayRx == nil {
		return false
	}
	if len(p) < ethLen+20 || binary.BigEndian.Uint16(p[12:]) != etIPv4 {
		gwEnqueueLocked(p) // ARP e.d.: alleen de interne NIC kan er iets mee
		return true
	}
	if binary.BigEndian.Uint32(p[ethLen+16:]) != layout.HostIP4() {
		return false // IPv4 naar elders: NAT-terrein (masquerade)
	}
	gwEnqueueLocked(p)
	return true
}

// gwEnqueueLocked zet een kopie van het frame in de wachtrij (mu vast). De
// kopie is nodig omdat de switch-lus zijn leesbuffer hergebruikt en de
// aflevering pas ná de unlock gebeurt.
func gwEnqueueLocked(p []byte) {
	if len(gwQueue) >= gwQueueMax {
		return // vol: drop (TCP herstelt)
	}
	gwQueue = append(gwQueue, append([]byte(nil), p...))
}

// drainGateway levert de gewachte frames af aan de interne NIC — ZONDER mu.
// Aanroepen direct na elke ronde die mu vasthield (switchPass, FromGateway).
//
// Eigen recover: de netstack krijgt hier app-gestuurde frame-inhoud te verwerken, en
// dat mag core 0 — en dus álle slots — niet vellen. Het lijstje wordt onder mu
// omgewisseld, zodat een aflevering die zélf weer frames aanlevert (de
// RST-teruglus via FromGateway) niet in dezelfde slice knoeit.
func drainGateway() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_GWNIC_PANIC: %v — frame dropped, switch keeps running\n", r)
		}
	}()
	for {
		mu.Lock()
		q, rx := gwQueue, gatewayRx
		gwQueue = nil
		mu.Unlock()
		if len(q) == 0 || rx == nil {
			return
		}
		for _, p := range q {
			rx(p)
		}
	}
}
