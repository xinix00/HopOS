// gwnat.go — de 1:1-vertaling tussen het interne gateway-adres (10.100.0.1,
// layout.HostIP4) en het échte stack-adres van HOP. Sinds de netstack-flip
// (09-08) draait HOP één enkele-NIC-stack (lneto) op zijn externe IP; het
// interne adres is geen tweede NIC meer (dat was gvisor-multi-NIC, zie git
// history van hopnet/internal.go) maar een statische herschrijving op de
// gateway-naad: apps blijven 10.100.0.1 zien, de stack ziet zijn eigen IP.
//
// Geen conntrack: de mapping is 1:1 op IP-niveau, poorten blijven onaangeraakt,
// dus beide richtingen zijn stateloos en het pad kan niet vollopen. De
// checksum-updates zijn incrementeel (RFC 1624) via dezelfde helpers als de
// masquerade-NAT. ICMP heeft geen pseudo-header, dus daar volstaat de
// IP-checksum; fragmenten weigeren we net als ipv4L4 (het interne net heeft
// één MTU, fragmenten horen er niet voor te komen).
package hopswitch

import (
	"encoding/binary"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

const protoICMP = 1

// gwFixL4 werkt de L4-checksum bij voor een gewijzigd IP in de pseudo-header
// (poorten wijzigen hier nooit). UDP-checksum 0 = "geen checksum" en blijft 0;
// een incrementele uitkomst van 0 wordt 0xFFFF (RFC 768).
func gwFixL4(l4 []byte, proto byte, oldIP, newIP uint32) {
	csumOff := 16 // TCP
	if proto == protoUDP {
		csumOff = 6
	}
	if proto == protoUDP && binary.BigEndian.Uint16(l4[csumOff:]) == 0 {
		return
	}
	fixCsum32(l4[csumOff:], oldIP, newIP)
	if proto == protoUDP && binary.BigEndian.Uint16(l4[csumOff:]) == 0 {
		binary.BigEndian.PutUint16(l4[csumOff:], 0xFFFF)
	}
}

// gwParse valideert een intern-vertaalbaar IPv4-frame: TCP/UDP met volledige
// L4-kop (via ipv4L4) óf ICMP zonder fragmentatie. ok=false voor de rest.
func gwParse(f []byte) (ihl int, proto byte, ok bool) {
	if ihl, proto, ok = ipv4L4(f); ok {
		return ihl, proto, true
	}
	// ICMP apart: ipv4L4 kent alleen TCP/UDP.
	if len(f) < ethLen+20 || binary.BigEndian.Uint16(f[12:]) != etIPv4 {
		return 0, 0, false
	}
	ip := f[ethLen:]
	ihl = int(ip[0]&0xf) * 4
	if ip[0]>>4 != 4 || ihl < 20 || ip[9] != protoICMP ||
		binary.BigEndian.Uint16(ip[6:])&0x1fff != 0 {
		return 0, 0, false
	}
	return ihl, protoICMP, true
}

// GwToHost herschrijft een frame van een app richting HOP (dst 10.100.0.1)
// naar HOP's stack-adres: dst-IP → hostIP plus IP- en L4-checksums. false =
// geen vertaalbaar frame voor het gateway-adres; de aanroeper dropt het dan.
func GwToHost(f []byte, hostIP uint32) bool {
	ihl, proto, ok := gwParse(f)
	if !ok {
		return false
	}
	ip := f[ethLen:]
	old := binary.BigEndian.Uint32(ip[16:])
	if old != layout.HostIP4() {
		return false
	}
	binary.BigEndian.PutUint32(ip[16:], hostIP)
	fixCsum32(ip[10:], old, hostIP)
	if proto != protoICMP {
		gwFixL4(ip[ihl:], proto, old, hostIP)
	}
	return true
}

// GwFromHost herschrijft een frame van HOP's stack richting het interne net:
// src hostIP → 10.100.0.1 (plus checksums) en de MAC's op basis van het
// deterministische slot-plan — geen ARP, net als de rest van de switch. false
// = geen vertaalbaar frame of geen geldig slot-adres; niet bezorgen.
func GwFromHost(f []byte, hostIP uint32) bool {
	ihl, proto, ok := gwParse(f)
	if !ok {
		return false
	}
	ip := f[ethLen:]
	if binary.BigEndian.Uint32(ip[12:]) != hostIP {
		return false
	}
	dst := binary.BigEndian.Uint32(ip[16:])
	slot := int(dst&0xff) - 1 // SlotIP4(i) eindigt op i+1
	if dst>>8 != layout.HostIP4()>>8 || slot < 1 || slot > layout.MaxSlots {
		return false
	}
	gw := layout.HostIP4()
	binary.BigEndian.PutUint32(ip[12:], gw)
	fixCsum32(ip[10:], hostIP, gw)
	if proto != protoICMP {
		gwFixL4(ip[ihl:], proto, hostIP, gw)
	}
	mac := layout.SlotMAC(slot)
	copy(f[0:6], mac[:])
	copy(f[6:12], hostMAC[:])
	return true
}
