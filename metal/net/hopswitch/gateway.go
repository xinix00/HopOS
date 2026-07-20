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

	"hop-os/metal/abi/layout"
)

// gatewayRx is de invoer van HOP's interne NIC (gezet door hopnet/internal):
// gateway-frames die geen NAT-route hebben gaan hierheen in plaats van "weg
// te vallen". Het frame is alleen geldig tijdens de aanroep (de switch-lus
// hergebruikt zijn buffer) — de ontvanger kopieert.
var gatewayRx func(p []byte)

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
func FromGateway(p []byte) {
	mu.Lock()
	defer mu.Unlock()
	if !up {
		return
	}
	forward(0, p)
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
		gatewayRx(p) // ARP e.d.: alleen de interne NIC kan er iets mee
		return true
	}
	if binary.BigEndian.Uint32(p[ethLen+16:]) != layout.HostIP4() {
		return false // IPv4 naar elders: NAT-terrein (masquerade)
	}
	gatewayRx(p)
	return true
}
