// De gateway-poort: 10.100.0.1 is "mijn node" (Dereks besluit 20-07 — geen
// {{host}}-hairpin, gewoon één vast intern adres dat op élke node hetzelfde
// is). HOP hangt daarvoor zelf als poort 0 aan zijn eigen switch: een
// gewone poort 0 met dezelfde SPSC-ringen als alle apps. Frames van een slot
// naar het gateway-IP gaan die poort in — geen heapqueue — en de antwoorden
// gaan via poort 0 terug de switch in. Daarmee bereikt een app de diensten op
// 10.100.0.1, zonder proxy en zonder dat er één byte de fysieke NIC uit gaat.
package hopswitch

import (
	"encoding/binary"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// gatewayClaimLocked (mu vast, vanuit forward): hoort dit gateway-frame bij
// HOP's LAN-poort? Ja voor IPv4 naar het gateway-IP (10.100.0.1). true =
// bezorgd. ARP voor de gateway beantwoordt de switch zelf.
func gatewayClaimLocked(p []byte) bool {
	if len(ports) == 0 || ports[0] == nil {
		return false
	}
	if len(p) < ethLen+20 || binary.BigEndian.Uint16(p[12:]) != etIPv4 {
		return false
	}
	if binary.BigEndian.Uint32(p[ethLen+16:]) != layout.HostIP4() {
		return false // IPv4 naar elders: NAT-terrein (masquerade)
	}
	writeRXLocked(0, p)
	return true
}
