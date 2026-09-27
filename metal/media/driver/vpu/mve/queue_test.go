package mve

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// fakeFW is de firmware-kant van een ringpaar: precies de regels die de echte
// firmware volgt, zodat deze test het protocol bewijst en niet onze eigen
// aannames. Hij leest wat de host stuurt (kop, checksumwoord, data), verifieert
// de lopende som zoals de firmware dat doet, en kan zelf antwoorden schrijven.
type fakeFW struct {
	r   *ring
	sum uint32 // wat de firmware aan de kant van de host verwacht
}

// take leest één bericht uit de host-ring, zoals de firmware dat zou doen.
func (f *fakeFW) take(t *testing.T) (uint16, []byte) {
	t.Helper()
	rpos := uint32(dev.Read16(f.r.mve + qInRPos))
	wpos := uint32(dev.Read16(f.r.host + qInWPos))
	if rpos == wpos {
		t.Fatal("firmware vond een lege ring")
	}
	word := func() uint32 {
		v := dev.Read32(f.r.host + qDataOff + uintptr(rpos*4))
		rpos = (rpos + 1) % qWords
		return v
	}
	hdr := word()
	code, size := uint16(hdr), int(hdr>>16)
	sum := hdr
	var csum uint32
	if f.r.csum {
		csum = word()
	}
	data := make([]byte, 0, size)
	var buf [4]byte
	for len(data) < size {
		w := word()
		sum += w
		binary.LittleEndian.PutUint32(buf[:], w)
		data = append(data, buf[:]...)
	}
	data = data[:size]
	if f.r.csum {
		f.sum += sum
		if csum != f.sum {
			t.Errorf("checksum %#x, firmware rekende %#x (code %d)", csum, f.sum, code)
		}
	}
	dev.Write16(f.r.mve+qInRPos, uint16(rpos))
	return code, data
}

// empty meldt of de host-ring niets te lezen heeft.
func (f *fakeFW) empty() bool {
	return dev.Read16(f.r.mve+qInRPos) == dev.Read16(f.r.host+qInWPos)
}

// give schrijft een antwoord in de firmware-ring.
func (f *fakeFW) give(code uint16, data []byte) {
	wpos := uint32(dev.Read16(f.r.mve + qOutWPos))
	put := func(v uint32) {
		dev.Write32(f.r.mve+qDataOff+uintptr(wpos*4), v)
		wpos = (wpos + 1) % qWords
	}
	put(uint32(code) | uint32(len(data))<<16)
	for i := 0; i < len(data); i += 4 {
		put(wordAt(data, i))
	}
	dev.Write16(f.r.mve+qOutWPos, uint16(wpos))
}

// testRing legt twee pagina's neer en geeft het paar plus de firmware-kant.
func testRing(t *testing.T, csum bool) (*ring, *fakeFW) {
	t.Helper()
	buf := make([]byte, 3*pageSize)
	base := (uintptr(unsafe.Pointer(&buf[0])) + pageSize - 1) &^ uintptr(pageSize-1)
	r := &ring{host: base, mve: base + pageSize, csum: csum}
	dev.Clear(base, 2*pageSize)
	return r, &fakeFW{r: r}
}

func TestRingBerichtKomtOngeschondenAan(t *testing.T) {
	r, fw := testRing(t, true)
	payload := []byte{1, 2, 3, 4, 5, 6, 7}
	if err := r.send(reqJob, payload); err != nil {
		t.Fatal(err)
	}
	code, got := fw.take(t)
	if code != reqJob {
		t.Errorf("code = %d", code)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("data = %x, verwacht %x", got, payload)
	}
}

func TestRingChecksumLooptDoorOverBerichtenHeen(t *testing.T) {
	// Dit is de val waar een nieuwe driver in trapt: de som is niet per
	// bericht maar cumulatief. Drie berichten achter elkaar moeten alle drie
	// kloppen, ook eentje zonder data.
	r, fw := testRing(t, true)
	for i, p := range [][]byte{{0xaa}, nil, {1, 2, 3, 4, 5}} {
		if err := r.send(reqIdleAck, p); err != nil {
			t.Fatalf("bericht %d: %v", i, err)
		}
		if _, got := fw.take(t); !bytes.Equal(got, p) {
			t.Errorf("bericht %d: %x", i, got)
		}
	}
	// En de laatste som moet ook in reserved[2] staan, want daar kijkt de
	// firmware naar als hij de ring controleert.
	if got := dev.Read32(r.host + qReserved + 8); got != fw.sum {
		t.Errorf("reserved[2] = %#x, firmware verwacht %#x", got, fw.sum)
	}
}

func TestRingLooptRondZonderBerichtTeBreken(t *testing.T) {
	// 1020 woorden, dus na een paar honderd berichten van vier woorden is de
	// ring meermalen omgelopen. Een bericht dat over de grens heen breekt moet
	// aan de andere kant heel aankomen.
	r, fw := testRing(t, true)
	payload := make([]byte, 9)
	for n := 0; n < 700; n++ {
		for i := range payload {
			payload[i] = byte(n + i)
		}
		if err := r.send(reqIdleAck, payload); err != nil {
			t.Fatalf("ronde %d: %v", n, err)
		}
		code, got := fw.take(t)
		if code != reqIdleAck || !bytes.Equal(got, payload) {
			t.Fatalf("ronde %d: code %d data %x", n, code, got)
		}
	}
}

func TestRingWeigertWatNietPast(t *testing.T) {
	r, _ := testRing(t, true)
	if err := r.send(reqIdleAck, make([]byte, qWords*4)); err != errMsgTooBig {
		t.Errorf("te groot bericht: %v", err)
	}
	// Zonder dat iemand leest moet de ring netjes vollopen en dan weigeren,
	// niet stilletjes overschrijven.
	var sent int
	for {
		if err := r.send(reqIdleAck, make([]byte, 400)); err != nil {
			if err != errQueueFull {
				t.Fatalf("na %d berichten: %v", sent, err)
			}
			break
		}
		sent++
		if sent > 2000 {
			t.Fatal("ring liep vol zonder ooit te weigeren")
		}
	}
	if sent == 0 {
		t.Fatal("lege ring nam geen enkel bericht aan")
	}
}

func TestRingLeestAntwoordenVanDeFirmware(t *testing.T) {
	r, fw := testRing(t, true)
	body := make([]byte, 12)
	for i := range body {
		body[i] = byte(0x40 + i)
	}
	fw.give(respSeqParams, body)
	fw.give(respOutput, nil)

	dst := make([]byte, 64)
	code, n, ok, err := r.recv(dst)
	if err != nil || !ok {
		t.Fatalf("eerste antwoord: ok=%v err=%v", ok, err)
	}
	if code != respSeqParams || n != len(body) || !bytes.Equal(dst[:n], body) {
		t.Errorf("code=%d n=%d data=%x", code, n, dst[:n])
	}
	if code, n, ok, err := r.recv(dst); !ok || err != nil || code != respOutput || n != 0 {
		t.Errorf("tweede antwoord: code=%d n=%d ok=%v err=%v", code, n, ok, err)
	}
	if _, _, ok, _ := r.recv(dst); ok {
		t.Error("lege ring gaf toch een bericht")
	}
}

func TestRingWeigertOnzinnigeCode(t *testing.T) {
	r, fw := testRing(t, true)
	fw.give(42, nil)
	if _, _, ok, err := r.recv(make([]byte, 8)); ok || err != errMsgUnknown {
		t.Errorf("ok=%v err=%v", ok, err)
	}
}

func TestRingWeigertPositieBuitenDeRing(t *testing.T) {
	// De posities komen uit gedeeld geheugen dat de firmware kan beschrijven.
	// Een woord achter de 1020 ligt op de volgende fysieke pagina; dat is een
	// protocolfout, geen lees.
	r, _ := testRing(t, true)
	dev.Write16(r.mve+qOutWPos, qWords+5)
	if _, _, ok, err := r.recv(make([]byte, 64)); ok || err != errQueuePos {
		t.Errorf("schrijfpositie buiten de ring: ok=%v err=%v", ok, err)
	}
	dev.Write16(r.mve+qInRPos, 0xffff)
	if err := r.send(reqJob, nil); err != errQueuePos {
		t.Errorf("leespositie buiten de ring: %v", err)
	}
}
