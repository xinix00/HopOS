// De interne gateway-NIC: HOP als poort 0 aan zijn eigen switch. Hierdoor is
// 10.100.0.1 — hetzelfde op élke node — voor de apps "mijn node": de agent
// (:8080) en de leader (:9080) luisteren wildcard op de node-stack, dus een
// app die 10.100.0.1:9080 belt komt hier rechtstreeks uit, zonder NAT,
// zonder proxy en zonder dat er een byte de fysieke NIC uit gaat (Dereks
// besluit 20-07: één vast intern adres i.p.v. {{host}}-hairpin).
//
// De wiring spiegelt go-net's GVisorStack, maar dan voor een tweede NIC op
// dezélfde gvisor-stack: een channel-endpoint met de gateway-MAC, ethernet-
// kop handmatig (14 bytes: dst, src, ethertype — exact go-net's encoding).
// RX: hopswitch.SetGatewayRx → InjectInbound. TX: notificatie-drain →
// hopswitch.FromGateway. ARP loopt hier NIET: elke slot-buur staat statisch in
// de stack (zie upInternal) omdat de nummering deterministisch is — een
// geflood who-has zou het eerste antwoord geloven, van wie het ook kwam.
package hopnet

import (
	"encoding/binary"
	"fmt"

	gnet "github.com/usbarmory/go-net"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// internalNICID is de tweede NIC op de node-stack (1 = de externe uplink).
const internalNICID = 2

// upInternal hangt de interne gateway-NIC aan de gvisor-stack en verbindt
// hem met de switch. Aanroepen ná iface.Init (de route-tabel van de uplink
// staat dan; wij zetten de interne subnet-route ervóór).
func upInternal(gs *gnet.GVisorStack) error {
	m := layout.SlotMAC(0) // de gateway-MAC (..:00) — de switch kent 'm al
	link := channel.New(512, gnet.MTU, tcpip.LinkAddress(m[:]))
	link.LinkEPCapabilities |= stack.CapabilityResolutionRequired
	if err := gs.Stack.CreateNIC(internalNICID, link); err != nil {
		return fmt.Errorf("interne NIC: %v", err)
	}

	gw := layout.HostIP4()
	ipb := []byte{byte(gw >> 24), byte(gw >> 16), byte(gw >> 8), byte(gw)}
	addr := tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{
			Address:   tcpip.AddrFromSlice(ipb),
			PrefixLen: layout.NetPrefix,
		},
	}
	if err := gs.Stack.AddProtocolAddress(internalNICID, addr, stack.AddressProperties{}); err != nil {
		return fmt.Errorf("gateway-adres: %v", err)
	}
	// GEEN ARP naar binnen. De switch nummert deterministisch — slot i heeft
	// SlotIP4(i) en SlotMAC(i), en forward() bezorgt op het slotnummer ín de
	// MAC — dus HOP kent elk intern adres al vóór hij iets vraagt. Alles
	// vooraf vastzetten is daarmee geen optimalisatie maar een dichte deur.
	//
	// Wat er anders gebeurt: gvisor floodt een who-has via de switch naar ÁLLE
	// slots en neemt het eerste antwoord. Niets bindt dat antwoord aan het slot
	// dat het adres bezit, dus de snelste buurman krijgt het verkeer. Zolang
	// dat agent-verkeer was, was het al niet best; sinds HOP toetsaanslagen
	// naar de display stuurt (gui/usbin) zou het een keylogger zijn.
	//
	// De tweede helft van dat slot zit in de switch (forward: een slot mag
	// alleen zijn eigen bron-MAC gebruiken). Die twee samen maken intern
	// adres-vervalsing onmogelijk in plaats van onwaarschijnlijk.
	for i := 1; i <= layout.MaxSlots; i++ {
		si := layout.SlotIP4(i)
		sb := []byte{byte(si >> 24), byte(si >> 16), byte(si >> 8), byte(si)}
		sm := layout.SlotMAC(i)
		if err := gs.Stack.AddStaticNeighbor(internalNICID, ipv4.ProtocolNumber,
			tcpip.AddrFromSlice(sb), tcpip.LinkAddress(sm[:])); err != nil {
			return fmt.Errorf("statische buur slot %d: %v", i, err)
		}
	}

	// De interne subnet-route VÓÓR de uplink-routes: 10.100.0.0/24 hoort bij
	// deze NIC, al het andere blijft zoals het was (subnet + default → uplink).
	rt := gs.Stack.GetRouteTable()
	rt = append([]tcpip.Route{{
		Destination: addr.AddressWithPrefix.Subnet(),
		NIC:         internalNICID,
	}}, rt...)
	gs.Stack.SetRouteTable(rt)

	// Switch → stack: onbeclaimde gateway-frames de NIC in. Kopie van de
	// payload — het frame is de hergebruikte buffer van de switch-lus.
	hopswitch.SetGatewayRx(func(p []byte) {
		if len(p) < gnet.EthernetMinimumSize {
			return
		}
		proto := tcpip.NetworkProtocolNumber(binary.BigEndian.Uint16(p[12:14]))
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: gnet.EthernetMinimumSize,
			Payload:            buffer.MakeWithData(append([]byte(nil), p[gnet.EthernetMinimumSize:]...)),
		})
		copy(pkt.LinkHeader().Push(gnet.EthernetMinimumSize), p[:gnet.EthernetMinimumSize])
		link.InjectInbound(proto, pkt)
		// gvisor-ownership: wij maakten de PacketBuffer, dus wij geven hem terug.
		// Zonder DecRef gaat hij nooit naar de pool en lopen de release-callbacks
		// niet — pure allocatie-/GC-druk op core 0 bij node-API-verkeer.
		pkt.DecRef()
	})

	// Stack → switch: notificatie-gedreven drain (geen poll-lus — dit pad is
	// alleen actief als HOP zelf intern verkeer heeft).
	link.AddNotify(internalTx{link: link, mac: m})

	fmt.Printf("net: interne gateway-NIC op %s (HOPOS_GWNIC_UP)\n", layout.IP4Str(gw))
	return nil
}

// internalTx draint het channel-endpoint naar de switch bij elke notificatie.
type internalTx struct {
	link *channel.Endpoint
	mac  [6]byte
}

// WriteNotify implementeert channel.Notification: gvisor heeft een uitgaand
// pakket klaargezet. Frame opbouwen zoals go-net's WriteOutboundPacket (dst
// uit de egress-route, src = onze MAC, ethertype, payload) en de switch in.
func (t internalTx) WriteNotify() {
	buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
	for {
		pkt := t.link.Read()
		if pkt == nil {
			return
		}
		n := copy(buf, pkt.EgressRoute.RemoteLinkAddress)
		n += copy(buf[n:], t.mac[:])
		binary.BigEndian.PutUint16(buf[n:], uint16(pkt.NetworkProtocolNumber))
		n += 2
		ok := true
		for _, v := range pkt.AsSlices() {
			if n+len(v) > len(buf) {
				ok = false // te groot voor één frame: drop (TCP herstelt)
				break
			}
			n += copy(buf[n:], v)
		}
		// gvisor-ownership: link.Read() draagt de PacketBuffer aan ons over, dus
		// hij moet terug — óók op het drop-pad. De inhoud staat al in buf, dus
		// teruggeven kan vóór de aflevering aan de switch.
		pkt.DecRef()
		if ok {
			hopswitch.FromGateway(buf[:n])
		}
	}
}
