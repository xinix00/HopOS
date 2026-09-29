package layout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestSchedOffsets bewaakt het sched-blok, de tweelingbroer van
// TestCtrlOffsetsUniek — en met een reden erbij die op de control-page niet
// bestaat: de SCHRIJVERSGRENS op cacheline 0.
//
// Op RISC-V zijn HOP's hart en het app-hart niet coherent (gemeten 30-07) en
// draait de switcher zonder MMU, dus met cachebare writes. Twee schrijvers in
// één 64B-regel is dan geen theoretisch risico maar dataverlies: wie zijn regel
// terugschrijft, schrijft de bytes van de ander terug zoals ze bij zíjn fetch
// stonden. Regel 0 is daarom van de arch-laag op de core zelf, regel 1..3 van
// HOP. Die verdeling staat in proza in layout.go; hier staat hij als toets.
//
// Aanleiding om dit nú op te schrijven (31-07): er kwamen drie velden bij voor
// de slaap-stand van de switcher (SchedClintPA/SleepCap/MsipPA), pal achter een
// bewonerslijst die met SlotCap meeschaalt. Precies de vorm waarin een volgende
// uitbreiding stil over iets heen gaat.
func TestSchedOffsets(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "layout.go", nil, 0)
	if err != nil {
		t.Fatalf("layout.go parsen: %v", err)
	}

	// hopOwned: geschreven door HOP vanaf zijn eigen hart. De rest is van de
	// arch-laag op de core zelf (cpu/el2, cpu/mmode).
	hopOwned := map[string]bool{
		"SchedCursor": true, "SchedCount": true, "SchedList": true,
		"SchedS2PA": true, "SchedClintPA": true, "SchedSleepCap": true,
		"SchedMsipPA": true, "SchedTickTicks": true,
	}
	const line = 64

	seen := map[uint64]string{}
	off := map[string]uint64{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Sched") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Errorf("%s: geen int-literal — sched-offsets horen literals te zijn (ze staan ook zo in switch.s)", name.Name)
					continue
				}
				v, err := strconv.ParseUint(lit.Value, 0, 64)
				if err != nil {
					t.Errorf("%s: %v", name.Name, err)
					continue
				}
				if prev, dup := seen[v]; dup {
					t.Errorf("OFFSET-COLLISIE: %s en %s staan allebei op %d", prev, name.Name, v)
				}
				seen[v], off[name.Name] = name.Name, v

				if v%8 != 0 {
					t.Errorf("%s = %d: niet 8-byte-gealigneerd", name.Name, v)
				}
				if v+8 > ParkMboxLen {
					t.Errorf("%s = %d: valt buiten het blok van %d bytes", name.Name, v, ParkMboxLen)
				}
				if inHopLine := v >= line; inHopLine != hopOwned[name.Name] {
					who := map[bool]string{true: "HOP", false: "de arch-laag"}
					t.Errorf("SCHRIJVERSGRENS: %s = %d ligt in de regels van %s, maar wordt geschreven door %s — twee schrijvers in één cacheline is dataverlies op een niet-coherent hartpaar",
						name.Name, v, who[inHopLine], who[hopOwned[name.Name]])
				}
			}
		}
	}
	if len(off) < 8 {
		t.Fatalf("maar %d Sched*-offsets gevonden — is de parse kapot?", len(off))
	}

	// De bewonerslijst schaalt met SlotCap en moet nog altijd vóór het eerste
	// veld erna eindigen. Dit is de check die de drie nieuwe velden nodig hadden.
	next := uint64(ParkMboxLen)
	for _, v := range off {
		if v > off["SchedList"] && v < next {
			next = v
		}
	}
	if end := off["SchedList"] + SlotCap; end > next {
		t.Errorf("SchedList (%d) + SlotCap (%d) loopt tot %d en botst op het veld op %d",
			off["SchedList"], SlotCap, end, next)
	}
}
