package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// partAdopt claimt een BESTAANDE partitie voor een geadopteerd slot (de
// kern-flip, docs/kern-flip.md). De eis is dezelfde als bij partAlloc, maar
// omgekeerd geformuleerd: na de claim mag dat bereik nooit meer aan iemand
// anders uitgedeeld worden.
func TestPartAdoptClaimtEnGeeftNietOpnieuwUit(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 0x10000000}}) // 256MB

	const base, size = 0x88000000, 0x4000000 // 64MB middenin
	if err := partAdopt(3, base, size); err != nil {
		t.Fatalf("partAdopt: %v", err)
	}
	if b, s, ok := partitionOf(3); !ok || b != base || s != size {
		t.Fatalf("slot 3 draagt %#x+%#x (ok=%v), wil %#x+%#x", b, s, ok, uint64(base), uint64(size))
	}
	// Alles wat er daarna uit de pool komt moet buiten de geadopteerde partitie
	// vallen — dát is de invariant die een dubbeluitgifte zou breken.
	for slot := 4; slot < 10; slot++ {
		b, s, err := partAlloc(slot, 32<<20)
		if err != nil {
			break // pool op: prima, we zoeken alleen overlap
		}
		if b < base+size && b+s > base {
			t.Fatalf("slot %d kreeg %#x+%#x — overlapt de geadopteerde partitie %#x+%#x",
				slot, b, s, uint64(base), uint64(size))
		}
	}
}

// Een blob dat een partitie beschrijft die deze kern niet vrij heeft, is een
// blob dat niet bij deze pool hoort. Dan is niet-adopteren het enige veilige
// antwoord: doorgaan zou het geheugen van een ánder slot claimen.
func TestPartAdoptWeigertBezetBereik(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 0x10000000}})
	if err := partAdopt(1, 0x88000000, 0x4000000); err != nil {
		t.Fatalf("eerste claim: %v", err)
	}
	if err := partAdopt(2, 0x88000000, 0x4000000); err == nil {
		t.Fatal("tweede claim op hetzelfde bereik werd geaccepteerd — dat is de dubbeluitgifte van 31-08")
	}
	if err := partAdopt(2, 0x89000000, 0x1000000); err == nil {
		t.Fatal("claim BINNEN een al geclaimde partitie werd geaccepteerd")
	}
}

// BorrowKernWindow is de lening waarin een nieuwe kern geplaatst wordt. Hij
// moet echte pool-grond pakken (en dus onbereikbaar maken voor apps), en na
// ReturnKernWindow — het faalpad van een flip die niet doorging — moet die
// grond weer beschikbaar zijn.
func TestBorrowKernWindow(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 0x10000000}}) // 256MB
	win, total, err := BorrowKernWindow(64 << 20)
	if err != nil {
		t.Fatalf("lenen: %v", err)
	}
	if total < 64<<20 || win%part2M != 0 {
		t.Fatalf("lening %#x+%#x is niet bruikbaar als kern-venster", win, total)
	}
	if _, _, err := BorrowKernWindow(64 << 20); err == nil {
		t.Error("tweede lening werd geaccepteerd — er kan er maar één tegelijk staan")
	}
	// Geen partitie mag in het geleende venster vallen zolang de lening staat.
	for slot := 1; slot < 8; slot++ {
		b, s, err := partAlloc(slot, 32<<20)
		if err != nil {
			break
		}
		if b < win+total && b+s > win {
			t.Fatalf("slot %d kreeg %#x+%#x in het geleende kern-venster %#x+%#x", slot, b, s, win, total)
		}
	}
	// En terug: de grond komt weer vrij (mislukte flip).
	before := PoolLargest()
	ReturnKernWindow()
	if after := PoolLargest(); after <= before {
		t.Errorf("na ReturnKernWindow is het grootste gat %d MB, was %d MB — de lening kwam niet terug",
			after>>20, before>>20)
	}
}

// takeRange knipt uit de vrije lijst, en een claim MIDDENIN een regio splitst
// die in tweeën. Dan schrijft de knip-lus méér elementen dan hij leest — en
// een in-place herbouw (partFree[:0]) overschrijft daarmee de eerstvolgende,
// nog ongelezen regio. GEMETEN 01-09 op de flip-regressie: het kern-venster
// lag middenin de grote QEMU-regio, de tweede pool-regio verdween, en de
// adoptie vond zijn partitie "niet vrij in de pool".
func TestTakeRangeMiddenInRegioRaaktBuurregioNiet(t *testing.T) {
	poolReset(t, []layout.Region{
		{Base: 0x50000000, Size: 0x60000000}, // groot, hier knippen we middenin
		{Base: 0xB1000000, Size: 0x0F000000}, // de buur die moet overleven
	})
	partMu.Lock()
	took := takeRange(0xA0E00000, 0xAFFF0000)
	partMu.Unlock()
	if took != 0xAFFF0000-0xA0E00000 {
		t.Errorf("takeRange nam %#x, wil %#x", took, uint64(0xAFFF0000-0xA0E00000))
	}
	// De buurregio moet er nog volledig zijn: een partitie daarin hoort te passen.
	base, _, err := partAlloc(1, 200<<20)
	if err != nil {
		t.Fatalf("200MB uit de tweede pool-regio: %v — buurregio verdwenen bij de knip", err)
	}
	if base < 0xB1000000 || base >= 0xC0000000 {
		t.Errorf("200MB landde op %#x, buiten de tweede pool-regio", base)
	}
	// En het geknipte bereik zelf is niet meer uit te delen.
	partMu.Lock()
	stillFree := freeSpan(0xA0E00000, 0xAFFF0000)
	partMu.Unlock()
	if stillFree {
		t.Error("het geknipte bereik staat nog als vrij te boek")
	}
}
