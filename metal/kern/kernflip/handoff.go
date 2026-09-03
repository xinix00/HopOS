package kernflip

// Het handoff-blob: wat de vertrekkende kern aan de nieuwe doorgeeft
// (docs/kern-flip.md). Het ligt in de staart van het geleende venster, boven
// de RAM-declaratie van de nieuwe kern, en de boot-scratch draagt er een
// pointer naar.
//
// Alles hierin is BOEKHOUDING, geen inhoud: de app-werelden zelf blijven staan
// waar ze staan. Daarom is het klein, vast van vorm, en mag het bij twijfel
// weggegooid worden — dan degradeert de flip naar een gewone boot.

import (
	"encoding/binary"
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/kern/slots"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
)

// handMagic spelt "HOPHAND1" (little-endian).
const handMagic = 0x31444E4148504F48

// handoffTail is de ruimte die bóven de RAM-declaratie van de nieuwe kern
// wordt meegeleend voor het blob: buiten zijn heap, binnen zijn lening.
//
// 256KB, en de maat komt uit de conntrack: de volle tabel (hopswitch.MaxFlows
// = 4096) kost 24 bytes per flow, dus ~96KB, plus de kop en de slot-records.
// Dat is 0,1% van een kern-venster en het koopt dat élke verbinding door de
// switch een kernwissel overleeft — geen enkele reden om hier krap te zitten.
const handoffTail = 0x40000

// maxAgentState begrenst de JSON van de agent. Ruim: een node met tientallen
// taken zit op tientallen KB, en de staart draagt daarnaast de volle
// conntrack. Een grotere state is geen bedrijfsgeval maar een teken dat er
// iets anders mis is, en die hoort te stranden vóór de sprong.
const maxAgentState = 128 << 10

// Blob-indeling (alles little-endian, 8-uitgelijnd):
//
//	kop (128B): magic | versie | oudVenster.base | oudVenster.size |
//	            nieuwVenster.base | nieuwVenster.total | slotCount | generatie |
//	            bundelsom | bufferArena.base | bufferArena.size | netRingHalf |
//	            (rest gereserveerd)
//	per slot:  slot | partBase | partSize | core | nPorts | jobLen |
//	           cores | nMounts | nPorts×u64 (poort) | job-bytes |
//	           per mount: localLen | sharedLen | bytes (alles 8-uitgelijnd)
//	NAT-blok:  masqNext | gwMAC+gwKnown | flowCount | flowCount×24B
//	agent:     lengte | JSON-bytes (8-uitgelijnd) — de state van HOP zelf
const (
	handVersion = 7
	handHead    = 128
	slotHead    = 64
)

// Handoff is wat de vertrekkende kern achterliet.
type Handoff struct {
	OldBase, OldSize uint64 // het venster van de vórige kern (→ terug de pool in)
	Window, Total    uint64 // het eigen venster incl. handoff-staart
	Gen              uint64 // hoeveelste flip deze boot is (1 = de eerste)
	// NAT is de conntrack van de switch: zonder deze tabel breekt élke
	// verbinding die door de masquerade loopt (een cloudflared-tunnel, een
	// uitgaande API-call) bij een kernwissel, terwijl de app zelf gewoon
	// doorleeft. Zie net/hopswitch/handoff.go voor wat er wél en niet in zit.
	NAT hopswitch.NATState

	// Agent is de state van HOP zélf (jobs + taken), als JSON zoals de agent
	// hem opschrijft (agentboot/agent.Snapshot). Zonder dit start de nieuwe
	// agent met een lege administratie en wil hij zijn eigen doorlopende taken
	// plaatsen op slots die ze al bezetten — de apps overleven de flip dan
	// technisch, maar worden wezen die niemand meer beheert.
	//
	// Het is JSON en geen eigen codering omdat de agent dit formaat al draagt
	// (types.Job/types.Task hebben hun tags voor de HTTP-API en de gecommitte
	// staat). En het gaat mee in plaats van uit S3 te komen, omdat een
	// standalone node zonder lock-backend anders zijn hele taakadministratie
	// kwijt is terwijl de taken gewoon doordraaien.
	Agent []byte
	// BundleSum is de som van de bundel waar deze kern uit geplaatst is. Hij
	// bestaat om precies één reden: een boot-config die zegt "flip naar deze
	// URL" mag geen eeuwige lus worden. De volgende kern haalt de bundel op,
	// rekent dezelfde som, en springt alleen als hij verschilt.
	BundleSum   uint64
	BufferArena slots.BufferArenaState
	Slots       []slots.SlotState
}

// encodeHandoff bouwt het blob. max is de ruimte in de staart; past het niet,
// dan is dat een fout vóór de sprong (en dus geen flip) in plaats van een half
// blob dat de nieuwe kern moet zien te overleven.
func encodeHandoff(h Handoff, max int) ([]byte, error) {
	b := make([]byte, handHead)
	binary.LittleEndian.PutUint64(b[0:], handMagic)
	binary.LittleEndian.PutUint64(b[8:], handVersion)
	binary.LittleEndian.PutUint64(b[16:], h.OldBase)
	binary.LittleEndian.PutUint64(b[24:], h.OldSize)
	binary.LittleEndian.PutUint64(b[32:], h.Window)
	binary.LittleEndian.PutUint64(b[40:], h.Total)
	binary.LittleEndian.PutUint64(b[48:], uint64(len(h.Slots)))
	binary.LittleEndian.PutUint64(b[56:], h.Gen)
	binary.LittleEndian.PutUint64(b[64:], h.BundleSum)
	binary.LittleEndian.PutUint64(b[72:], h.BufferArena.Base)
	binary.LittleEndian.PutUint64(b[80:], h.BufferArena.Size)
	binary.LittleEndian.PutUint64(b[88:], h.BufferArena.RingHalf)

	for _, s := range h.Slots {
		var rec [slotHead]byte
		binary.LittleEndian.PutUint64(rec[0:], uint64(s.Slot))
		binary.LittleEndian.PutUint64(rec[8:], s.PartBase)
		binary.LittleEndian.PutUint64(rec[16:], s.PartSize)
		binary.LittleEndian.PutUint64(rec[24:], uint64(s.Core))
		binary.LittleEndian.PutUint64(rec[32:], uint64(len(s.Ports)))
		binary.LittleEndian.PutUint64(rec[40:], uint64(len(s.Job)))
		binary.LittleEndian.PutUint64(rec[48:], uint64(s.Cores))
		binary.LittleEndian.PutUint64(rec[56:], uint64(len(s.Mounts)))
		b = append(b, rec[:]...)
		for _, p := range s.Ports {
			b = binary.LittleEndian.AppendUint64(b, uint64(p))
		}
		b = append(b, s.Job...)
		if pad := (8 - len(b)&7) & 7; pad != 0 {
			b = append(b, make([]byte, pad)...)
		}
		for _, m := range s.Mounts {
			b = binary.LittleEndian.AppendUint64(b, uint64(len(m[0])))
			b = binary.LittleEndian.AppendUint64(b, uint64(len(m[1])))
			b = append(b, m[0]...)
			b = append(b, m[1]...)
			if pad := (8 - len(b)&7) & 7; pad != 0 {
				b = append(b, make([]byte, pad)...)
			}
		}
	}
	// NAT-blok: kop van 24 bytes, dan 24 bytes per flow.
	var mac uint64
	for i, v := range h.NAT.GwMAC {
		mac |= uint64(v) << (8 * i)
	}
	if h.NAT.GwKnown {
		mac |= 1 << 56
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(h.NAT.MasqNext))
	b = binary.LittleEndian.AppendUint64(b, mac)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(h.NAT.Flows)))
	for _, f := range h.NAT.Flows {
		b = binary.LittleEndian.AppendUint64(b, uint64(f.Proto)|uint64(f.Slot)<<8|
			uint64(f.Fins)<<16|uint64(f.SlotPort)<<32|uint64(f.DstPort)<<48)
		b = binary.LittleEndian.AppendUint64(b, uint64(f.SlotIP)|uint64(f.DstIP)<<32)
		b = binary.LittleEndian.AppendUint64(b, uint64(f.NodePort))
	}

	b = binary.LittleEndian.AppendUint64(b, uint64(len(h.Agent)))
	b = append(b, h.Agent...)
	if pad := (8 - len(b)&7) & 7; pad != 0 {
		b = append(b, make([]byte, pad)...)
	}

	if len(b) > max {
		return nil, fmt.Errorf("handoff blob is %d bytes, tail holds %d", len(b), max)
	}
	return b, nil
}

// decodeHandoff leest het blob terug. Elke afwijking geeft een fout: de
// aanroeper behandelt dat als "gewone boot", en dat is altijd veilig.
func decodeHandoff(b []byte) (Handoff, error) {
	var h Handoff
	if len(b) < handHead {
		return h, fmt.Errorf("blob te klein (%d)", len(b))
	}
	if binary.LittleEndian.Uint64(b[0:]) != handMagic {
		return h, fmt.Errorf("magic klopt niet")
	}
	if v := binary.LittleEndian.Uint64(b[8:]); v != handVersion {
		return h, fmt.Errorf("blob-versie %d, deze kern kent %d", v, handVersion)
	}
	h.OldBase = binary.LittleEndian.Uint64(b[16:])
	h.OldSize = binary.LittleEndian.Uint64(b[24:])
	h.Window = binary.LittleEndian.Uint64(b[32:])
	h.Total = binary.LittleEndian.Uint64(b[40:])
	n := binary.LittleEndian.Uint64(b[48:])
	h.Gen = binary.LittleEndian.Uint64(b[56:])
	h.BundleSum = binary.LittleEndian.Uint64(b[64:])
	h.BufferArena.Base = binary.LittleEndian.Uint64(b[72:])
	h.BufferArena.Size = binary.LittleEndian.Uint64(b[80:])
	h.BufferArena.RingHalf = binary.LittleEndian.Uint64(b[88:])
	if n > 1024 {
		return h, fmt.Errorf("slot-telling %d is onzin", n)
	}
	off := handHead
	for k := uint64(0); k < n; k++ {
		if off+slotHead > len(b) {
			return h, fmt.Errorf("slot-record %d buiten het blob", k)
		}
		var s slots.SlotState
		s.Slot = int(binary.LittleEndian.Uint64(b[off:]))
		s.PartBase = binary.LittleEndian.Uint64(b[off+8:])
		s.PartSize = binary.LittleEndian.Uint64(b[off+16:])
		s.Core = int(binary.LittleEndian.Uint64(b[off+24:]))
		nPorts := binary.LittleEndian.Uint64(b[off+32:])
		jobLen := binary.LittleEndian.Uint64(b[off+40:])
		s.Cores = int(binary.LittleEndian.Uint64(b[off+48:]))
		nMounts := binary.LittleEndian.Uint64(b[off+56:])
		if s.Cores < 1 {
			s.Cores = 1
		}
		off += slotHead
		if nPorts > 64 || jobLen > 256 {
			return h, fmt.Errorf("slot-record %d: %d poorten / %d job-bytes is onzin", k, nPorts, jobLen)
		}
		if uint64(off)+nPorts*8+jobLen > uint64(len(b)) {
			return h, fmt.Errorf("slot-record %d loopt buiten het blob", k)
		}
		for j := uint64(0); j < nPorts; j++ {
			s.Ports = append(s.Ports, uint16(binary.LittleEndian.Uint64(b[off:])))
			off += 8
		}
		s.Job = string(b[off : off+int(jobLen)])
		off += int(jobLen)
		off += (8 - off&7) & 7
		if nMounts > 64 {
			return h, fmt.Errorf("slot-record %d: %d mounts is onzin", k, nMounts)
		}
		for j := uint64(0); j < nMounts; j++ {
			if off+16 > len(b) {
				return h, fmt.Errorf("slot-record %d: mount %d buiten het blob", k, j)
			}
			ll := binary.LittleEndian.Uint64(b[off:])
			sl := binary.LittleEndian.Uint64(b[off+8:])
			off += 16
			if ll > 4096 || sl > 4096 || uint64(off)+ll+sl > uint64(len(b)) {
				return h, fmt.Errorf("slot-record %d: mount %d met onmogelijke padlengtes", k, j)
			}
			local := string(b[off : off+int(ll)])
			shared := string(b[off+int(ll) : off+int(ll)+int(sl)])
			s.Mounts = append(s.Mounts, [2]string{local, shared})
			off += int(ll) + int(sl)
			off += (8 - off&7) & 7
		}
		h.Slots = append(h.Slots, s)
	}

	// Het NAT-blok is OPTIONEEL bij het lezen: een blob zonder (of met een
	// afgekapte) conntrack levert een node zonder overgenomen flows op, en dat
	// is een degradatie — verbindingen breken — maar geen reden om de hele
	// adoptie weg te gooien en de apps te laten vallen.
	if off+24 > len(b) {
		return h, nil
	}
	h.NAT.MasqNext = uint16(binary.LittleEndian.Uint64(b[off:]))
	mac := binary.LittleEndian.Uint64(b[off+8:])
	for i := range h.NAT.GwMAC {
		h.NAT.GwMAC[i] = byte(mac >> (8 * i))
	}
	h.NAT.GwKnown = mac&(1<<56) != 0
	nf := binary.LittleEndian.Uint64(b[off+16:])
	off += 24
	if nf > uint64(hopswitch.MaxFlows) || off+int(nf)*24 > len(b) {
		fmt.Printf("kernflip: conntrack block claims %d flows and does not fit — continuing without it\n", nf)
		return h, nil
	}
	for k := uint64(0); k < nf; k++ {
		w0 := binary.LittleEndian.Uint64(b[off:])
		w1 := binary.LittleEndian.Uint64(b[off+8:])
		w2 := binary.LittleEndian.Uint64(b[off+16:])
		h.NAT.Flows = append(h.NAT.Flows, hopswitch.FlowState{
			Proto: byte(w0), Slot: uint8(w0 >> 8), Fins: uint8(w0 >> 16),
			SlotPort: uint16(w0 >> 32), DstPort: uint16(w0 >> 48),
			SlotIP: uint32(w1), DstIP: uint32(w1 >> 32),
			NodePort: uint16(w2),
		})
		off += 24
	}

	// Het agent-blok, net als het NAT-blok optioneel bij het lezen: zonder
	// agent-state draaien de apps gewoon door en corrigeert de leader de
	// administratie bij zijn eerste synchronisatie. Dat is een degradatie,
	// geen reden om de bewoners te laten vallen.
	if off+8 > len(b) {
		return h, nil
	}
	na := binary.LittleEndian.Uint64(b[off:])
	off += 8
	if na > uint64(maxAgentState) || off+int(na) > len(b) {
		fmt.Printf("kernflip: agent state block claims %d bytes and does not fit — continuing without it\n", na)
		return h, nil
	}
	h.Agent = append([]byte(nil), b[off:off+int(na)]...)
	return h, nil
}
