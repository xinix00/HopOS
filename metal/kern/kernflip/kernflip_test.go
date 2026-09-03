package kernflip

import (
	"encoding/binary"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/kern/slots"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
)

// Het handoff-blob is het enige dat een kern-flip van de vorige kern erft
// (docs/kern-flip.md). Gaat er één veld verloren, dan erft de nieuwe kern een
// verkeerde partitie of een verkeerde core — dus: exact terug wat erin ging.
func TestHandoffRoundTrip(t *testing.T) {
	in := Handoff{
		OldBase: 0x40000000, OldSize: 0x0F000000,
		Window: 0xA0E00000, Total: 0x0F200000,
		Gen: 2,
		Slots: []slots.SlotState{
			{Slot: 1, PartBase: 0xBC000000, PartSize: 64 << 20, Core: 1, Cores: 1,
				Job: "welcome", Ports: []uint16{8080, 443},
				Mounts: [][2]string{{"/data", "/data"}, {"/shared/logs", "/logs"}}},
			{Slot: 7, PartBase: 0x90000000, PartSize: 48 << 20, Core: 3, Cores: 1},
		},
	}
	b, err := encodeHandoff(in, handoffTail)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeHandoff(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OldBase != in.OldBase || out.OldSize != in.OldSize ||
		out.Window != in.Window || out.Total != in.Total || out.Gen != in.Gen {
		t.Fatalf("kop verschilt: %+v vs %+v", out, in)
	}
	if len(out.Slots) != len(in.Slots) {
		t.Fatalf("%d slots terug, %d erin", len(out.Slots), len(in.Slots))
	}
	for i, want := range in.Slots {
		got := out.Slots[i]
		if got.Slot != want.Slot || got.PartBase != want.PartBase || got.PartSize != want.PartSize ||
			got.Core != want.Core || got.Job != want.Job || len(got.Ports) != len(want.Ports) {
			t.Errorf("slot %d: %+v, wil %+v", i, got, want)
		}
		for k := range want.Ports {
			if got.Ports[k] != want.Ports[k] {
				t.Errorf("slot %d poort %d: %d, wil %d", i, k, got.Ports[k], want.Ports[k])
			}
		}
	}
}

// Een blob dat niet klopt, moet een FOUT zijn en geen halve adoptie: de
// aanroeper behandelt elke fout als "gewone boot", en dat is altijd veilig.
func TestHandoffWeigertOnzin(t *testing.T) {
	goed, err := encodeHandoff(Handoff{Gen: 1, Slots: []slots.SlotState{
		{Slot: 2, PartBase: 0x90000000, PartSize: 32 << 20, Core: 2},
	}}, handoffTail)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	cases := map[string]func(b []byte){
		"magic kapot":          func(b []byte) { b[0] ^= 0xFF },
		"onbekende versie":     func(b []byte) { binary.LittleEndian.PutUint64(b[8:], 99) },
		"absurde slot-telling": func(b []byte) { binary.LittleEndian.PutUint64(b[48:], 1<<40) },
		"slot-record afgekapt": func(b []byte) { binary.LittleEndian.PutUint64(b[48:], 4) },
	}
	for naam, breek := range cases {
		b := append([]byte(nil), goed...)
		breek(b)
		if _, err := decodeHandoff(b); err == nil {
			t.Errorf("%s werd geaccepteerd", naam)
		}
	}
	if _, err := decodeHandoff(goed[:32]); err == nil {
		t.Error("afgekapt blob werd geaccepteerd")
	}
}

// Een flip-bundel is een kern-ELF met een HOPRELO1-staart. ParseBundle mag
// alleen een compleet geldige bundel teruggeven — hij bepaalt straks wáár
// bytes geschreven worden en waar de node in springt.
func TestParseBundleWeigertOnzin(t *testing.T) {
	if _, err := ParseBundle(make([]byte, 200)); err == nil {
		t.Error("een blok nullen werd als bundel geaccepteerd")
	}
	// Een staart die een entry buiten de payload beschrijft: dat is een sprong
	// het niets in.
	b := buildBundle(t, 0x40000000, 0x100000, 0x40000000+0x200000, nil)
	if _, err := ParseBundle(b); err == nil {
		t.Error("entry buiten de payload werd geaccepteerd")
	}
	// Een reloc-offset voorbij het platte beeld zou bij het herbaseren buiten
	// het geleende venster schrijven.
	b = buildBundle(t, 0x40000000, 0x100000, 0x40001000, []uint32{0x100000})
	if _, err := ParseBundle(b); err == nil {
		t.Error("reloc-offset buiten het platte beeld werd geaccepteerd")
	}
	// En de goede vorm moet er wél doorheen.
	b = buildBundle(t, 0x40000000, 0x100000, 0x40001000, []uint32{0x40, 0x800})
	bun, err := ParseBundle(b)
	if err != nil {
		t.Fatalf("geldige bundel geweigerd: %v", err)
	}
	if bun.RelocCount() != 2 || bun.Entry != 0x40001000 || bun.LinkLoad != 0x40000000 {
		t.Errorf("bundel verkeerd gelezen: %+v", bun)
	}
}

// buildBundle maakt een bundel met een verzonnen (niet-ELF) payload: alles wat
// ParseBundle toetst zit in de staart, dus dit is genoeg om die grenzen te
// bewijzen zonder een echte kern te bouwen.
func buildBundle(t *testing.T, load, flat, entry uint64, relocs []uint32) []byte {
	t.Helper()
	elf := make([]byte, 4096)
	hdrOff := uint64(len(elf))
	out := append([]byte(nil), elf...)
	var hdr [56]byte
	binary.LittleEndian.PutUint64(hdr[0:], relocMagic)
	binary.LittleEndian.PutUint32(hdr[8:], 1)
	binary.LittleEndian.PutUint32(hdr[12:], ABI)
	binary.LittleEndian.PutUint64(hdr[16:], uint64(len(elf)))
	binary.LittleEndian.PutUint64(hdr[24:], load)
	binary.LittleEndian.PutUint64(hdr[32:], flat)
	binary.LittleEndian.PutUint64(hdr[40:], entry)
	binary.LittleEndian.PutUint64(hdr[48:], uint64(len(relocs)))
	out = append(out, hdr[:]...)
	for _, r := range relocs {
		out = binary.LittleEndian.AppendUint32(out, r)
	}
	if pad := (8 - uint64(len(out))&7) & 7; pad != 0 {
		out = append(out, make([]byte, pad)...)
	}
	out = binary.LittleEndian.AppendUint64(out, hdrOff)
	out = binary.LittleEndian.AppendUint64(out, relocMagic)
	return out
}

// De conntrack gaat als eigen blok door het blob (docs/kern-flip.md). Gaat er
// één veld verloren, dan komt het antwoord van een levende verbinding na de
// flip bij het verkeerde slot terecht — of nergens. Dus: exact terug.
func TestHandoffNATRoundTrip(t *testing.T) {
	in := Handoff{
		Gen: 1,
		NAT: hopswitch.NATState{
			MasqNext: 20345,
			GwMAC:    [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56},
			GwKnown:  true,
			Flows: []hopswitch.FlowState{
				{Proto: 6, Slot: 3, Fins: 2, SlotPort: 49152, DstPort: 443,
					NodePort: 20001, SlotIP: 0x0A640004, DstIP: 0x08080808},
				{Proto: 17, Slot: 1, SlotPort: 5353, DstPort: 53,
					NodePort: 20002, SlotIP: 0x0A640002, DstIP: 0x0A000203},
			},
		},
	}
	b, err := encodeHandoff(in, handoffTail)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decodeHandoff(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.NAT.MasqNext != in.NAT.MasqNext || out.NAT.GwMAC != in.NAT.GwMAC || !out.NAT.GwKnown {
		t.Errorf("NAT-kop verschilt: %+v vs %+v", out.NAT, in.NAT)
	}
	if len(out.NAT.Flows) != len(in.NAT.Flows) {
		t.Fatalf("%d flows terug, %d erin", len(out.NAT.Flows), len(in.NAT.Flows))
	}
	for i, want := range in.NAT.Flows {
		if out.NAT.Flows[i] != want {
			t.Errorf("flow %d: %+v, wil %+v", i, out.NAT.Flows[i], want)
		}
	}
}

// De volle conntrack moet in de staart passen — anders weigert de flip precies
// op de node die de overdracht het hardst nodig heeft (een drukke node).
func TestHandoffFullConntrackFits(t *testing.T) {
	h := Handoff{Gen: 1}
	for i := 0; i < hopswitch.MaxFlows; i++ {
		h.NAT.Flows = append(h.NAT.Flows, hopswitch.FlowState{
			Proto: 6, Slot: uint8(i%64 + 1), SlotPort: uint16(30000 + i%1000),
			DstPort: 443, NodePort: uint16(20000 + i%9000),
			SlotIP: 0x0A640002, DstIP: 0x08080808,
		})
	}
	for s := 1; s <= 16; s++ { // en een realistisch aantal bewoners erbij
		h.Slots = append(h.Slots, slots.SlotState{
			Slot: s, PartBase: uint64(s) << 24, PartSize: 64 << 20, Core: s,
			Job: "some-job-name", Ports: []uint16{8080, 443},
		})
	}
	b, err := encodeHandoff(h, handoffTail)
	if err != nil {
		t.Fatalf("volle conntrack past niet in de staart: %v", err)
	}
	if out, err := decodeHandoff(b); err != nil || len(out.NAT.Flows) != hopswitch.MaxFlows {
		t.Fatalf("teruglezen: %d flows, %v", len(out.NAT.Flows), err)
	}
	t.Logf("blob met volle conntrack (%d flows) + 16 slots = %d bytes van %d",
		hopswitch.MaxFlows, len(b), handoffTail)
}
