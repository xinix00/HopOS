// Package netdev is het NIC-contract van HopOS: twee methodes en drie
// framemaat-constanten, en niets meer. Elke driver (dwmac, dwmac4, gem, genet,
// igb, virtionet), elke schil eromheen (hopnet's locdev, hopswitch' Uplink,
// appnet's ring-nic) en elke consument (de netstack, leandhcp) spreekt dit.
//
// Waarom een eigen pakketje in plaats van het interface van de netstack: zo
// hangt de hele ONDERKANT van de boom — boards, drivers, de switch — aan geen
// enkele netstack. Dat is precies de naad waarlangs we in 2026 twee keer van
// stack gewisseld zijn (gVisor → lneto → leannet, elk zonder een driver aan te
// raken), en de reden dat die wissels goedkoop waren. Go's interfaces zijn
// structureel, dus deze Device en die van de netstack zijn uitwisselbaar
// zonder conversie, adapter of import in één richting.
package netdev

import "github.com/xinix00/HopOS/metal/v2/abi/layout"

// Device is het rauwe-frame-contract. Het is bewust minimaal: alles wat een
// stack verder wil weten (MAC, link-status, statistiek) is board-kennis en
// loopt buiten dit interface om.
//
// Receive geeft (0, nil) als er niets ligt: het is een poll-model, geen
// blokkerende read — de aanroeper bepaalt zijn eigen tempo (HopOS pollt met
// een microslaap, zodat een idle core echt kan slapen). De buffer moet
// MTU+EthernetMaximumSize groot zijn.
//
// Transmit mag vanuit meerdere goroutines komen; een driver die dat niet kan
// serialiseert zelf (zo doet hopswitch.Uplink het). De buffer is na terugkeer
// weer van de aanroeper, dus wie hem langer nodig heeft kopieert.
type Device interface {
	Receive(buf []byte) (n int, err error)
	Transmit(buf []byte) (err error)
}

// Framematen. MTU is de payload, EthernetMaximumSize de kop plus marge —
// samen de buffergrootte die een RX-lus nodig heeft.
const (
	MTU                 = layout.NetMTU
	EthernetHeaderSize  = 14
	EthernetMaximumSize = 18
)

// Flusher is de optionele batching-kant van een NIC: een driver die hem
// draagt schrijft zijn doorbells (RX-producer, TX-tail) niet per frame maar
// pas bij Flush. De RX-pomp roept FlushRX aan na elke burst (en om de zoveel
// frames, zodat de NIC nooit zonder descriptors zit); wie zendt roept FlushTX
// aan na zijn burst. Per frame scheelt dat één tot drie PCIe-schrijf- of
// leesacties, en op een 1 Gbit-link zijn dat er 70.000 per seconde (L83, de
// 100 MB/s-jacht). Een device zonder Flusher gedraagt zich als vroeger.
// Batch(true) zet het uitstellen aan; tot dan (boot: DHCP, discovery — niemand
// die flusht) schrijft de driver zijn doorbells per frame zoals vroeger.
// Bundel 30/82 (19-09) leerde het: uitgesteld zonder flusher = geen lease,
// geen netwerk, watchdog.
// IRQRearmer: een device dat zijn RX-interrupt na de ack weer opent. De
// RX-pomp roept het aan op het moment dat hij gaat wachten (hopnet.rxLoop),
// niet eerder — zie daar.
type IRQRearmer interface {
	RearmIRQ()
}

type Flusher interface {
	FlushRX()
	FlushTX()
	Batch(on bool)
}
