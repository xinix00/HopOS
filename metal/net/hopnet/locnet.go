// locnet: het lokale-verkeer-schilletje om de uplink. De node-stack (lneto,
// één NIC) heeft drie soorten frames die de draad nooit op mogen, en die
// vangen we allemaal op déze naad — het NetworkDevice tussen stack en
// uplink — zodat de stack zelf nergens van hoeft te weten:
//
//  1. self-dial: dst-MAC == onze eigen MAC → terug de RX-wachtrij in. De
//     agent belt de leader op het eigen externe IP (de S3-lock adverteert
//     dat adres); gvisor deed dit met HandleLocal, dit is hetzelfde idee op
//     de device-naad.
//  2. ARP naar het eigen IP: zelf beantwoorden. Voor een self-dial moet de
//     stack eerst zijn eigen adres resolven, en niemand op het LAN gaat
//     onze vraag naar ons eigen IP beantwoorden.
//  3. intern subnet (10.100.0.0/24): niet de draad op maar de switch in,
//     via de statische 1:1-gateway-vertaling (hopswitch.GwFromHost) — de
//     MAC's komen uit het deterministische slot-plan, dus ook hier geen ARP.
//
// Inbound spiegelbeeldig: eerst de eigen wachtrij (loopback-frames plus de
// door hopswitch.GwToHost vertaalde app→node-frames), dan pas de echte NIC.
package hopnet

import (
	"encoding/binary"
	"sync"

	gnet "github.com/xinix00/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// locQueueMax begrenst de lokale wachtrij: dit is node-intern verkeer
// (agent/leader/apps), een handvol frames volstaat. Vol = drop (TCP herstelt)
// — nooit ongebonden groeien op een app-gedreven pad.
const locQueueMax = 64

type locdev struct {
	nic gnet.NetworkDevice
	mac [6]byte
	ip  uint32 // het externe stack-IP, als getal (layout-conventie)

	mu sync.Mutex
	q  [][]byte
}

// enqueue zet een kopie van het frame in de RX-wachtrij (de aanroepers
// hergebruiken hun buffers).
func (d *locdev) enqueue(p []byte) {
	d.mu.Lock()
	if len(d.q) < locQueueMax {
		d.q = append(d.q, append([]byte(nil), p...))
	}
	d.mu.Unlock()
}

// Receive (gnet.NetworkDevice): eerst de lokale wachtrij, dan de NIC.
func (d *locdev) Receive(buf []byte) (int, error) {
	d.mu.Lock()
	if len(d.q) > 0 {
		p := d.q[0]
		d.q = d.q[1:]
		d.mu.Unlock()
		return copy(buf, p), nil
	}
	d.mu.Unlock()
	return d.nic.Receive(buf)
}

// Transmit (gnet.NetworkDevice): de drie lokale gevallen, anders de draad op.
func (d *locdev) Transmit(p []byte) error {
	if len(p) >= 14 {
		if [6]byte(p[0:6]) == d.mac {
			d.enqueue(p) // self-dial: nooit de draad op
			return nil
		}
		if r := d.arpSelfReply(p); r != nil {
			d.enqueue(r)
			return nil
		}
		if binary.BigEndian.Uint16(p[12:]) == 0x0800 && len(p) >= 14+20 {
			if dst := binary.BigEndian.Uint32(p[14+16:]); dst>>8 == layout.HostIP4()>>8 {
				// Intern verkeer verlaat de node nooit — ook een frame dat de
				// vertaling weigert (fragment, vreemd slot) gaat niet de draad
				// op; dat zou het interne adresplan naar buiten lekken.
				if hopswitch.GwFromHost(p, d.ip) {
					hopswitch.FromGateway(p)
				}
				return nil
			}
		}
	}
	return d.nic.Transmit(p)
}

// arpSelfReply beantwoordt een ARP-request naar het eigen IP (RFC 826; zelfde
// frame-vorm als hopswitch.arpReplyGateway). nil = geen request voor ons.
func (d *locdev) arpSelfReply(p []byte) []byte {
	if len(p) < 14+28 || p[12] != 0x08 || p[13] != 0x06 {
		return nil
	}
	a := p[14:]
	// Ethernet/IPv4-request (oper=1)?
	if a[0] != 0x00 || a[1] != 0x01 || a[2] != 0x08 || a[3] != 0x00 || a[6] != 0x00 || a[7] != 0x01 {
		return nil
	}
	if binary.BigEndian.Uint32(a[24:]) != d.ip {
		return nil // niet ons adres: gewoon de draad op (echte buren)
	}
	var r [14 + 28]byte
	copy(r[0:6], a[8:14]) // dst = de vrager (wijzelf)
	copy(r[6:12], d.mac[:])
	r[12], r[13] = 0x08, 0x06
	b := r[14:]
	b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7] = 0x00, 0x01, 0x08, 0x00, 6, 4, 0x00, 0x02
	copy(b[8:14], d.mac[:])
	binary.BigEndian.PutUint32(b[14:], d.ip)
	copy(b[18:24], a[8:14])
	copy(b[24:28], a[14:18])
	return r[:]
}
