package dwmac

import "testing"

// De descriptorwoorden zijn bit-arithmetiek en horen dus op de host bewezen te
// worden, niet op een bordje waar één ronde een kaartwissel kost. Deze tests
// bestaan omdat precies dit fout ging op de eerste DMA-boot (30-07): de
// buffergrootte werd met een 11-bits masker naar nul geveegd en de MAC kreeg
// "elke buffer is 0 bytes" te horen — link stond, TX liep, RX gaf 128
// descriptors terug zonder één frame.

func TestRxCntlMeldtEenEchteBuffergrootte(t *testing.T) {
	c := rxCntl(false)
	if got := c & cntlSize1Mask; got != maxFrame {
		t.Errorf("RBS1 = %d, wil %d — een maat die door het masker valt betekent nul-byte buffers", got, maxFrame)
	}
	if c&ringEnd != 0 {
		t.Error("ring-end op een descriptor die niet de laatste is")
	}
}

func TestRxCntlZetRingEndOpDeLaatste(t *testing.T) {
	c := rxCntl(true)
	if c&ringEnd == 0 {
		t.Error("laatste descriptor zonder ring-end: de DMA loopt de ring uit")
	}
	if got := c & cntlSize1Mask; got != maxFrame {
		t.Errorf("RBS1 = %d, wil %d", got, maxFrame)
	}
}

// maxFrame moet in het veld passen én boven een volledig ethernetframe liggen
// (1518 = 1500 MTU + 14 header + 4 FCS), anders splitst de MAC frames over
// meerdere descriptors en keurt Receive ze allemaal af.
func TestMaxFramePastInHetVeldEnBovenDeMTU(t *testing.T) {
	if maxFrame&^cntlSize1Mask != 0 {
		t.Fatalf("maxFrame %d past niet in RBS1 (%#x)", maxFrame, cntlSize1Mask)
	}
	if maxFrame < 1518 {
		t.Errorf("maxFrame %d ligt onder een volledig ethernetframe (1518)", maxFrame)
	}
	if maxFrame > bufSize {
		t.Errorf("maxFrame %d boven de gereserveerde buffer %d: de DMA schrijft buiten zijn slot", maxFrame, bufSize)
	}
}

func TestTxCntlZetFirstLastEnLengte(t *testing.T) {
	c := txCntl(64, false)
	if c&txCntlFirst == 0 || c&txCntlLast == 0 {
		t.Error("TX zonder FS+LS: de MAC wacht op de rest van een frame dat niet komt")
	}
	if got := c & cntlSize1Mask; got != 64 {
		t.Errorf("TBS1 = %d, wil 64", got)
	}
	if c&ringEnd != 0 {
		t.Error("ring-end op een descriptor die niet de laatste is")
	}
	if c := txCntl(1518, true); c&ringEnd == 0 || c&cntlSize1Mask != 1518 {
		t.Errorf("txCntl(1518, true) = %#08x, wil ring-end + lengte 1518", c)
	}
}

func TestTxCntlWeigertWatNietInHetVeldPast(t *testing.T) {
	for _, n := range []int{0, maxFrame + 1, bufSize} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("txCntl(%d) gaf een woord terug i.p.v. te panieken", n)
				}
			}()
			txCntl(n, false)
		}()
	}
}

// De ringen en buffers moeten binnen de gereserveerde DMA-regio blijven; het
// board rekent NeedBytes na in zijn plan (board/licheerv/hop/plan.go).
func TestNeedBytesDektRingenEnBuffers(t *testing.T) {
	if want := bufOff + 2*numDesc*bufSize; NeedBytes != want {
		t.Errorf("NeedBytes = %d, wil %d", NeedBytes, want)
	}
	// descStride, niet descSize: de descriptors liggen een cacheline uit elkaar
	// (DSL), dus de ringen zijn 4× zo groot als hun inhoud. Deze test keek eerst
	// naar descSize en zag daardoor niet dat de TX-ring bij 64 descriptors bovenop
	// de eerste RX-buffers landde (gemeten 30-07).
	if descs := 2 * numDesc * descStride; descs > bufOff {
		t.Errorf("descriptors (%d bytes, stride %d) lopen de bufferregio (offset %d) in",
			descs, descStride, bufOff)
	}
	if NeedBytes != bufOff+2*numDesc*bufSize {
		t.Errorf("NeedBytes %d dekt de ringen+buffers niet", NeedBytes)
	}
}
