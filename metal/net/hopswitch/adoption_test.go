package hopswitch

import (
	"encoding/binary"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"testing"
	"time"
)

func TestAdoptionKeepsAppPortsAwayFromNodeStack(t *testing.T) {
	resetNAT()
	setUplink(t)
	HoldAdoptionPorts([]uint16{18090, MasqBase})
	t.Cleanup(FinishAdoption)
	for _, proto := range []byte{protoTCP, protoUDP} {
		for _, port := range []uint16{18090, MasqBase} {
			f := mkFrame(proto, nicMAC, lanMAC0, lanIP, nodeIP, 1234, port, nil)
			if !natInbound(f) {
				t.Fatalf("adopting app port %d reached the node stack", port)
			}
		}
	}
	management := mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, 5555, nil)
	if natInbound(management) {
		t.Fatal("adoption blocked the node console")
	}
	read := testSlotRing(t, 1)
	if err := Publish("tcp", 18090, 1, 18090); err != nil {
		t.Fatal(err)
	}
	FinishAdoption()
	f := mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, 18090, []byte("continued"))
	if !natInbound(f) || read() == nil {
		t.Fatal("restored app did not receive its traffic")
	}
	// A claim without a restored owner also ends with adoption.
	f = mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, MasqBase, nil)
	if natInbound(f) {
		t.Fatal("temporary claim survived adoption")
	}
}

func TestAdoptionDefersOutboundUntilOldMappingRestored(t *testing.T) {
	for _, proto := range []byte{protoTCP, protoUDP} {
		t.Run(map[byte]string{protoTCP: "tcp", protoUDP: "udp"}[proto], func(t *testing.T) {
			resetNAT()
			nic := setUplink(t)
			leerGateway(t)
			testSlotRing(t, 1)
			oldPort := uint16(MasqBase + 17)
			state := NATState{Flows: []FlowState{{Proto: proto, Slot: 1, SlotIP: layout.SlotIP4(1), SlotPort: 5555, DstIP: extIP, DstPort: 443, NodePort: oldPort}}}
			HoldAdoptionPorts([]uint16{oldPort})
			t.Cleanup(FinishAdoption)
			send := func(sport uint16) {
				frame := mkFrame(proto, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, sport, 443, []byte("continued"))
				mu.Lock()
				claimed := natOutbound(1, frame)
				mu.Unlock()
				if !claimed {
					t.Fatal("outbound packet not claimed")
				}
			}
			send(5555) // same forward key as the old connection
			masqNext = oldPort
			send(5556) // different forward key, competing for the old reverse key
			if len(nic.sent) != 0 || len(flowsFwd) != 0 {
				t.Fatal("outbound allocated/sent before conntrack restoration")
			}
			if got := RestoreNAT(state); got != 1 {
				t.Fatalf("restored %d flows, want 1", got)
			}
			send(5555)
			if len(nic.sent) != 0 {
				t.Fatal("adoption released before FinishAdoption")
			}
			FinishAdoption()
			send(5555)
			if len(nic.sent) != 1 {
				t.Fatalf("retransmit sent %d frames", len(nic.sent))
			}
			sent := nic.sent[0]
			ihl, _, _ := ipv4L4(sent)
			if got := binary.BigEndian.Uint16(sent[ethLen+ihl:]); got != oldPort {
				t.Fatalf("retransmit changed peer-visible port: %d want %d", got, oldPort)
			}
			checkFrame(t, sent, "adopted outbound")
		})
	}
}
func TestAdoptionWithoutPortsAlsoDefersNewFlows(t *testing.T) {
	resetNAT()
	HoldAdoptionPorts(nil)
	t.Cleanup(FinishAdoption)
	mu.Lock()
	fl := flowFor(protoUDP, 1, layout.SlotIP4(1), 5555, extIP, 443, time.Now())
	mu.Unlock()
	if fl != nil {
		t.Fatal("empty handoff port list opened outgoing allocation before FinishAdoption")
	}
	FinishAdoption()
	mu.Lock()
	fl = flowFor(protoUDP, 1, layout.SlotIP4(1), 5555, extIP, 443, time.Now())
	mu.Unlock()
	if fl == nil {
		t.Fatal("new flow remained blocked after FinishAdoption")
	}
}
