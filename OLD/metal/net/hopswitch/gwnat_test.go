package hopswitch

import (
	"encoding/binary"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// De 1:1-gateway-vertaling: beide richtingen moeten na herschrijven geldige
// checksums dragen (de ontvangende stack verifieert vol), de juiste adressen
// hebben, en alles wat niet vertaalbaar is weigeren.

const gwTestHostIP = 0xC0A80264 // 192.168.2.100 — het "echte" stack-adres

func TestGwToHost(t *testing.T) {
	slotIP := layout.SlotIP4(3)
	for _, proto := range []byte{protoTCP, protoUDP} {
		f := mkFrame(proto, hostMAC, layout.SlotMAC(3), slotIP, layout.HostIP4(), 40000, 8080, []byte("hop"))
		if !GwToHost(f, gwTestHostIP) {
			t.Fatalf("proto %d: vertaalbaar frame geweigerd", proto)
		}
		ip := f[ethLen:]
		if got := binary.BigEndian.Uint32(ip[16:]); got != gwTestHostIP {
			t.Fatalf("proto %d: dst = %08x, wil %08x", proto, got, gwTestHostIP)
		}
		if got := binary.BigEndian.Uint32(ip[12:]); got != slotIP {
			t.Fatalf("proto %d: src aangeraakt: %08x", proto, got)
		}
		checkFrame(t, f, "GwToHost")
	}

	// Niet voor het gateway-IP: weigeren én onaangeraakt laten.
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(3), slotIP, layout.SlotIP4(5), 40000, 80, nil)
	orig := append([]byte(nil), f...)
	if GwToHost(f, gwTestHostIP) {
		t.Fatal("frame naar een slot-IP werd als gateway-frame vertaald")
	}
	if string(f) != string(orig) {
		t.Fatal("geweigerd frame is toch aangeraakt")
	}
}

func TestGwFromHost(t *testing.T) {
	for _, proto := range []byte{protoTCP, protoUDP} {
		// HOP's stack antwoordt vanaf zijn echte IP naar slot 3.
		f := mkFrame(proto, [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, [6]byte{1, 2, 3, 4, 5, 6},
			gwTestHostIP, layout.SlotIP4(3), 8080, 40000, []byte("antwoord"))
		if !GwFromHost(f, gwTestHostIP) {
			t.Fatalf("proto %d: vertaalbaar frame geweigerd", proto)
		}
		ip := f[ethLen:]
		if got := binary.BigEndian.Uint32(ip[12:]); got != layout.HostIP4() {
			t.Fatalf("proto %d: src = %08x, wil gateway", proto, got)
		}
		wantDst := layout.SlotMAC(3)
		if [6]byte(f[0:6]) != wantDst {
			t.Fatalf("proto %d: dst-MAC = %x, wil slot-MAC %x", proto, f[0:6], wantDst)
		}
		if [6]byte(f[6:12]) != hostMAC {
			t.Fatalf("proto %d: src-MAC = %x, wil gateway-MAC", proto, f[6:12])
		}
		checkFrame(t, f, "GwFromHost")
	}

	// Bestemming buiten het interne subnet: weigeren (dat is uplink-verkeer).
	f := mkFrame(protoTCP, [6]byte{}, [6]byte{}, gwTestHostIP, 0x08080808, 8080, 443, nil)
	if GwFromHost(f, gwTestHostIP) {
		t.Fatal("frame naar extern IP werd als intern vertaald")
	}
	// Bestemming 10.100.0.1 zelf (slot 0 bestaat niet als bezorgdoel): weigeren.
	f = mkFrame(protoTCP, [6]byte{}, [6]byte{}, gwTestHostIP, layout.HostIP4(), 8080, 443, nil)
	if GwFromHost(f, gwTestHostIP) {
		t.Fatal("frame naar het gateway-IP zelf werd bezorgd")
	}
	// Andere bron dan het stack-adres: weigeren.
	f = mkFrame(protoTCP, [6]byte{}, [6]byte{}, 0x01020304, layout.SlotIP4(3), 8080, 443, nil)
	if GwFromHost(f, gwTestHostIP) {
		t.Fatal("frame met vreemde bron werd vertaald")
	}
}

// TestGwICMP: ping naar 10.100.0.1 — alleen de IP-checksum verandert (ICMP
// heeft geen pseudo-header, de ICMP-checksum moet dus ongemoeid blijven).
func TestGwICMP(t *testing.T) {
	f := make([]byte, ethLen+20+8)
	copy(f[0:6], hostMAC[:])
	binary.BigEndian.PutUint16(f[12:], etIPv4)
	ip := f[ethLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], 28)
	ip[8], ip[9] = 64, protoICMP
	binary.BigEndian.PutUint32(ip[12:], layout.SlotIP4(2))
	binary.BigEndian.PutUint32(ip[16:], layout.HostIP4())
	binary.BigEndian.PutUint16(ip[10:], ^fold16(sumWords(ip[:20])))
	icmp := ip[20:]
	icmp[0] = 8 // echo request
	binary.BigEndian.PutUint16(icmp[2:], ^fold16(sumWords(icmp)))
	icmpSumBefore := binary.BigEndian.Uint16(icmp[2:])

	if !GwToHost(f, gwTestHostIP) {
		t.Fatal("ICMP naar het gateway-IP geweigerd")
	}
	if !ipValid(ip) {
		t.Fatal("IP-checksum klopt niet na ICMP-herschrijving")
	}
	if binary.BigEndian.Uint16(icmp[2:]) != icmpSumBefore {
		t.Fatal("ICMP-checksum aangeraakt — die kent geen pseudo-header")
	}
}

// TestGwFragmentRefused: fragmenten kunnen hun L4-checksum niet dragen; het
// pad moet ze weigeren in plaats van half herschrijven.
func TestGwFragmentRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field uint16
	}{
		{"offset", 0x00B9},
		{"eerste fragment met MF", 0x2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(3), layout.SlotIP4(3), layout.HostIP4(), 40000, 8080, nil)
			ip := f[ethLen:]
			binary.BigEndian.PutUint16(ip[6:], tc.field)
			binary.BigEndian.PutUint16(ip[10:], 0)
			binary.BigEndian.PutUint16(ip[10:], ^fold16(sumWords(ip[:20])))
			if GwToHost(f, gwTestHostIP) {
				t.Fatal("fragment werd vertaald")
			}
		})
	}
}
