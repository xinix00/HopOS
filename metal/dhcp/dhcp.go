// Package dhcp is HopOS' minimale DHCPv4-client (RFC 2131): de volledige
// DISCOVER→OFFER→REQUEST→ACK-handshake op rauwe ethernet-frames, vóór er een
// netstack bestaat — precies genoeg om bij boot een lease te halen waarmee
// hopnet de gVisor-stack configureert. Polled, één poging per timeout-window;
// de aanroeper bepaalt het geduld.
//
// De DISCOVER/OFFER-helft is boardvast bewezen op de Pi 5 (probe6 run 5,
// 2026-07-10: OFFER 192.168.178.33 van een FRITZ!Box door onze eigen
// PCIe→RP1→GEM-keten).
package dhcp

import (
	"fmt"
	"math/bits"
	"time"
)

// NIC is het rauwe-frame-contract (structureel gelijk aan go-net's
// NetworkDevice; gem.Net en virtionet.Net voldoen er beide aan).
type NIC interface {
	Receive(buf []byte) (int, error)
	Transmit(buf []byte) error
}

// Lease is het resultaat van een geslaagde handshake.
type Lease struct {
	IP     [4]byte
	Mask   [4]byte // optie 1
	GW     [4]byte // optie 3 (eerste router)
	DNS    [4]byte // optie 6 (eerste resolver; 0.0.0.0 = geen)
	Server [4]byte // optie 54 (de lessor)
}

func ipStr(a [4]byte) string { return fmt.Sprintf("%d.%d.%d.%d", a[0], a[1], a[2], a[3]) }

// IPString/GWString/DNSString geven de velden in tekstvorm (board.NetConfig).
func (l Lease) IPString() string  { return ipStr(l.IP) }
func (l Lease) GWString() string  { return ipStr(l.GW) }
func (l Lease) DNSString() string { return ipStr(l.DNS) }

// CIDR geeft "ip/prefix" — de vorm die de netstack (gnet.Interface) eet.
func (l Lease) CIDR() string {
	m := uint32(l.Mask[0])<<24 | uint32(l.Mask[1])<<16 | uint32(l.Mask[2])<<8 | uint32(l.Mask[3])
	return fmt.Sprintf("%s/%d", ipStr(l.IP), bits.OnesCount32(m))
}

// Acquire draait de handshake op de NIC en geeft de lease. Retries binnen de
// timeout (per ronde 3s wachten op het antwoord); xid per ronde vers zodat
// een laat OFFER van een vorige ronde ons niet in de war brengt.
func Acquire(nic NIC, mac [6]byte, timeout time.Duration) (Lease, error) {
	deadline := time.Now().Add(timeout)
	for ronde := uint32(1); ; ronde++ {
		if !time.Now().Before(deadline) {
			return Lease{}, fmt.Errorf("dhcp: geen lease binnen %v", timeout)
		}
		xid := 0x484F5000 | ronde // "HOP" + ronde

		if err := nic.Transmit(packet(mac, xid, 1, nil)); err != nil { // DISCOVER
			return Lease{}, fmt.Errorf("dhcp: TX: %w", err)
		}
		offer, ok := await(nic, mac, xid, 2, deadline) // OFFER
		if !ok {
			continue
		}

		// REQUEST bevestigt het aanbod (optie 50 = het IP, 54 = de server).
		req := []byte{
			50, 4, offer.IP[0], offer.IP[1], offer.IP[2], offer.IP[3],
			54, 4, offer.Server[0], offer.Server[1], offer.Server[2], offer.Server[3],
		}
		if err := nic.Transmit(packet(mac, xid, 3, req)); err != nil {
			return Lease{}, fmt.Errorf("dhcp: TX: %w", err)
		}
		if ack, ok := await(nic, mac, xid, 5, deadline); ok { // ACK
			return ack, nil
		}
	}
}

// await polt tot msgtype (2=OFFER, 5=ACK) voor onze xid binnenkomt, of tot
// het ronde-window (3s, begrensd door de totale deadline) sluit.
func await(nic NIC, mac [6]byte, xid uint32, msgtype byte, deadline time.Time) (Lease, bool) {
	window := time.Now().Add(3 * time.Second)
	if window.After(deadline) {
		window = deadline
	}
	buf := make([]byte, 1536)
	for time.Now().Before(window) {
		n, _ := nic.Receive(buf)
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		if l, ok := parse(buf[:n], mac, xid, msgtype); ok {
			return l, true
		}
	}
	return Lease{}, false
}

// packet bouwt één DHCP-frame: ethernet-broadcast, IPv4 0.0.0.0 →
// 255.255.255.255, UDP 68→67 (checksum 0 = uit, mag bij IPv4), BOOTP met
// broadcast-flag (het antwoord komt dan als broadcast — onafhankelijk van
// het RX-unicast-filter), DHCP-magic + optie 53 (msgtype) + extra + 255.
func packet(mac [6]byte, xid uint32, msgtype byte, extra []byte) []byte {
	f := make([]byte, 14+20+8+300)
	for i := range 6 {
		f[i] = 0xff
	}
	copy(f[6:12], mac[:])
	f[12], f[13] = 0x08, 0x00

	ip := f[14:34]
	ip[0], ip[8], ip[9] = 0x45, 64, 17 // IHL 5, TTL, UDP
	tot := len(f) - 14
	ip[2], ip[3] = byte(tot>>8), byte(tot)
	ip[16], ip[17], ip[18], ip[19] = 255, 255, 255, 255
	cs := checksum(ip)
	ip[10], ip[11] = byte(cs>>8), byte(cs)

	udp := f[34:42]
	udp[1], udp[3] = 68, 67
	ul := tot - 20
	udp[4], udp[5] = byte(ul>>8), byte(ul)

	bp := f[42:]
	bp[0], bp[1], bp[2] = 1, 1, 6 // BOOTREQUEST, ethernet, hlen
	bp[4], bp[5], bp[6], bp[7] = byte(xid>>24), byte(xid>>16), byte(xid>>8), byte(xid)
	bp[10] = 0x80 // broadcast-flag
	copy(bp[28:34], mac[:])
	copy(bp[236:240], []byte{99, 130, 83, 99}) // DHCP-magic
	o := append([]byte{53, 1, msgtype, 55, 3, 1, 3, 6}, extra...)
	copy(bp[240:], append(o, 255))
	return f
}

// checksum is de standaard 16-bit one's-complement over de IP-header.
func checksum(h []byte) uint16 {
	var s uint32
	for i := 0; i < len(h); i += 2 {
		s += uint32(h[i])<<8 | uint32(h[i+1])
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// parse valideert een frame als DHCP-antwoord (BOOTREPLY, onze xid en MAC,
// optie 53 = msgtype) en licht de lease-velden eruit.
func parse(f []byte, mac [6]byte, xid uint32, msgtype byte) (Lease, bool) {
	if len(f) < 14+20+8+240 || f[12] != 0x08 || f[13] != 0 || f[23] != 17 {
		return Lease{}, false
	}
	ihl := int(f[14]&0xf) * 4
	udp := f[14+ihl:]
	if len(udp) < 8+240 || udp[2] != 0 || udp[3] != 68 { // dst-poort 68
		return Lease{}, false
	}
	bp := udp[8:]
	if bp[0] != 2 { // BOOTREPLY
		return Lease{}, false
	}
	if uint32(bp[4])<<24|uint32(bp[5])<<16|uint32(bp[6])<<8|uint32(bp[7]) != xid {
		return Lease{}, false
	}
	for i := range 6 {
		if bp[28+i] != mac[i] {
			return Lease{}, false
		}
	}

	var l Lease
	copy(l.IP[:], bp[16:20]) // yiaddr

	// Opties: [code len data...], 0 = pad, 255 = einde.
	opts := bp[240:]
	typeOK := false
	for i := 0; i+1 < len(opts); {
		code := opts[i]
		if code == 0 {
			i++
			continue
		}
		if code == 255 {
			break
		}
		ln := int(opts[i+1])
		if i+2+ln > len(opts) {
			break
		}
		d := opts[i+2 : i+2+ln]
		switch code {
		case 53:
			typeOK = ln == 1 && d[0] == msgtype
		case 1:
			if ln >= 4 {
				copy(l.Mask[:], d)
			}
		case 3:
			if ln >= 4 {
				copy(l.GW[:], d)
			}
		case 6:
			if ln >= 4 {
				copy(l.DNS[:], d)
			}
		case 54:
			if ln >= 4 {
				copy(l.Server[:], d)
			}
		}
		i += 2 + ln
	}
	return l, typeOK
}
