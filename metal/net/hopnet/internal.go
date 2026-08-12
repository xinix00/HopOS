// De interne gateway-naad: 10.100.0.1 — hetzelfde op élke node — is voor de
// apps "mijn node": de agent (:8080) en de leader (:9080) luisteren op de
// node-stack, dus een app die 10.100.0.1:9080 belt komt hier rechtstreeks
// uit, zonder proxy en zonder dat er een byte de fysieke NIC uit gaat
// (Dereks besluit 20-07: één vast intern adres i.p.v. {{host}}-hairpin).
//
// Tot de netstack-flip (09-08) was dit een twééde NIC op de gvisor-stack met
// statische buren (zie git history). Onze stack heeft één NIC, dus het
// interne adres is nu een statische 1:1-IP-vertaling op de gateway-naad:
// app→node-frames worden hier naar het echte stack-adres herschreven en
// komen als gewone RX de stack in; node→app-frames vertaalt locdev terug
// (hopswitch.GwFromHost) mét deterministische slot-MAC's. De
// anti-spoof-eigenschappen blijven: geen ARP het interne net op (de switch
// beantwoordt ARP voor 10.100.0.1 zelf, arpReplyGateway) en de MAC's komen
// uit het slot-plan, niet uit antwoorden van wie dan ook.
package hopnet

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// upInternal registreert de gateway-naad op de switch. Aanroepen ná
// iface.Init (de stack moet zijn adres kennen). Frames die geen vertaalbaar
// IPv4-frame voor het gateway-adres zijn (ARP-restjes, fragmenten) vervallen
// hier — de switch heeft de ARP's dan al beantwoord.
func upInternal(d *locdev) {
	hopswitch.SetGatewayRx(func(p []byte) {
		if hopswitch.GwToHost(p, d.ip) {
			// De app adresseerde de gateway-MAC (SlotMAC(0)); de stack-
			// ethernetlaag accepteert alleen de eigen MAC — dus die ook
			// omzetten, net als het IP.
			copy(p[0:6], d.mac[:])
			d.enqueue(p)
		}
	})
	fmt.Printf("net: internal gateway address %s via 1:1 rewrite (HOPOS_GWNIC_UP)\n", layout.IP4Str(layout.HostIP4()))
}
