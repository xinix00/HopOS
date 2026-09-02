package hopswitch

// De NAT-overdracht van de kern-flip (docs/kern-flip.md): de conntrack-tabel
// mee over een kernwissel, zodat verbindingen dóór de switch niet breken.
//
// Waarom dit kán, en lean: alle NAT-staat leeft onder één mutex en heeft één
// schrijver (de switch-lus op de HOP-core). Er is dus geen moment waarop de
// tabel half-gemuteerd is, en een snapshot is een gewone lees-ronde — geen
// quiesce, geen barrière, geen tweede kopie van de waarheid.
//
// En waarom het kleiner is dan het lijkt: een flow is volledig beschreven door
// zijn eigen velden, en BEIDE lookup-sleutels zijn daaruit af te leiden
// (fkey uit slot/dst, rkey uit nodePort/dst). Er hoeft dus geen index mee — de
// nieuwe kern bouwt hem terug uit dezelfde velden waarmee flowFor hem ooit
// aanlegde. 24 bytes per flow, en de volle conntrack (4096) past daarmee in
// ~96KB van de handoff-staart.
//
// Wat BEWUST niet meegaat:
//   - de neighbor-cache: die leert passief terug binnen één ARP-ronde, en een
//     verkeerd overgenomen L2-next-hop is erger dan een korte leerpauze;
//   - `seen` per flow: de nieuwe kern zet hem op "nu". De flip-duur telt zo
//     niet als idle-tijd, wat hooguit genereus is — een flow die tijdens de
//     wissel toch verliep, verloopt daarna alsnog;
//   - `masqNext`: de poort-allocator vindt zijn weg vanzelf, want allocPort
//     toetst elke kandidaat tegen flowsRev én de gepubliceerde poorten. Met de
//     herstelde flows erin kan hij dus per constructie geen bezette poort
//     uitdelen. Hij gaat tóch mee, want het is één woord en het scheelt de
//     eerste allocaties een scan over bezet terrein.

import "time"

// FlowState is één masquerade-flow in overdraagbare vorm: platte velden,
// geen pointers, vaste maten.
type FlowState struct {
	Proto    byte
	Slot     uint8
	Fins     uint8 // bit0 = FIN gezien richting peer, bit1 = richting client
	SlotPort uint16
	DstPort  uint16
	NodePort uint16
	SlotIP   uint32
	DstIP    uint32
}

// NATState is alles wat de NAT aan een volgende kern doorgeeft.
type NATState struct {
	Flows    []FlowState
	MasqNext uint16
	GwMAC    [6]byte
	GwKnown  bool
}

// SnapshotNAT beschrijft de levende conntrack voor het handoff-blob. Verlopen
// flows gaan niet mee: die zou de nieuwe kern alleen maar opnieuw moeten
// opruimen, en ze kosten wél ruimte in de staart.
func SnapshotNAT() NATState {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	s := NATState{MasqNext: masqNext, GwMAC: gwMAC, GwKnown: gwKnown}
	for _, fl := range flowsFwd {
		// Slot 1..255: het slotnummer moet in één byte passen (de switch-MAC
		// codeert hem ook zo, layout.SlotMAC), en SlotCap is 128.
		if flowExpiredAt(fl, now) || fl.slot < 1 || fl.slot > 255 {
			continue
		}
		var fins uint8
		if fl.finFwd {
			fins |= 1
		}
		if fl.finRev {
			fins |= 2
		}
		s.Flows = append(s.Flows, FlowState{
			Proto: fl.proto, Slot: uint8(fl.slot), Fins: fins,
			SlotPort: fl.slotPort, DstPort: fl.dstPort, NodePort: fl.nodePort,
			SlotIP: fl.slotIP, DstIP: fl.dstIP,
		})
	}
	return s
}

// RestoreNAT bouwt de conntrack terug en geeft het aantal herstelde flows.
// Aanroepen ná Up() en ná de slot-adoptie: een flow van een slot dat de flip
// niet overleefde hoort niet te blijven staan, en zijn poort hoort vrij te
// komen — dus dit pad slaat flows over waarvan het slot geen switch-poort
// (meer) heeft.
func RestoreNAT(s NATState) int {
	mu.Lock()
	defer mu.Unlock()
	if s.GwKnown {
		gwMAC, gwKnown = s.GwMAC, true
	}
	if s.MasqNext >= MasqBase && s.MasqNext < MasqEnd {
		masqNext = s.MasqNext
	}
	now := time.Now()
	n := 0
	for _, f := range s.Flows {
		slot := int(f.Slot)
		if slot < 1 || slot >= len(ports) || ports[slot] == nil {
			continue // het slot leeft niet meer: de flow ook niet
		}
		if len(flowsFwd) >= maxFlows || flowCountBySlot[slot] >= maxFlowsPerSlot {
			continue
		}
		fl := &flow{
			proto: f.Proto, slot: slot, slotIP: f.SlotIP, slotPort: f.SlotPort,
			dstIP: f.DstIP, dstPort: f.DstPort, nodePort: f.NodePort,
			seen:   now,
			finFwd: f.Fins&1 != 0, finRev: f.Fins&2 != 0,
		}
		fk := fkey{fl.proto, fl.slotIP, fl.dstIP, fl.slotPort, fl.dstPort}
		rk := rkey{fl.proto, fl.nodePort, fl.dstIP, fl.dstPort}
		if _, dup := flowsFwd[fk]; dup {
			continue
		}
		if _, dup := flowsRev[rk]; dup {
			continue
		}
		flowsFwd[fk] = fl
		flowsRev[rk] = fl
		flowCountBySlot[slot]++
		n++
	}
	if len(flowsFwd) > flowMapHighWater {
		flowMapHighWater = len(flowsFwd)
	}
	return n
}

// MaxFlows is de conntrack-cap, zodat de flip-laag zijn staart-budget kan
// uitrekenen zonder de interne constante te dupliceren.
const MaxFlows = maxFlows
