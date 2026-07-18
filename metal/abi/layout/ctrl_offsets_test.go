package layout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestCtrlOffsetsUniek bewaakt het control-page-contract: élke Ctrl*-offset is
// uniek, 8-byte-gealigneerd en valt vóór de env-regio. AST-gebaseerd (leest
// layout.go zelf), zodat een nieuwe constante automatisch meegetest wordt —
// een expliciete lijst zou driften. Aanleiding (Derek, 18-07): CtrlSMPTcr en
// CtrlIdle stonden allebei op 0xD8 — de idle-tik-teller van een SMP-primaire
// (elke ~1,2ms) kon het zojuist neergelegde vertaalregime van de secundaire
// overschrijven; timing-afhankelijk, de stille-freeze-klasse. Deze test had
// 'm gevangen en vangt de volgende.
func TestCtrlOffsetsUniek(t *testing.T) {
	// Geen page-offsets maar adressen/maten/afgeleiden — bewust buiten de check.
	exclude := map[string]bool{
		"CtrlBase":   true, // IPA-basis van de ctrl-regio
		"CtrlStride": true, // page-grootte
		"CtrlEnvMax": true, // afgeleide maat
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "layout.go", nil, 0)
	if err != nil {
		t.Fatalf("layout.go parsen: %v", err)
	}

	seen := map[uint64]string{}
	n := 0
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Ctrl") || exclude[name.Name] {
					continue
				}
				if i >= len(vs.Values) {
					t.Errorf("%s: geen expliciete waarde — Ctrl*-offsets horen literals te zijn", name.Name)
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Errorf("%s: geen int-literal — maak het er één, of voeg 'm toe aan de exclude-lijst als het geen offset is", name.Name)
					continue
				}
				v, err := strconv.ParseUint(lit.Value, 0, 64)
				if err != nil {
					t.Errorf("%s: %v", name.Name, err)
					continue
				}
				n++
				if prev, dup := seen[v]; dup {
					t.Errorf("OFFSET-COLLISIE: %s en %s staan allebei op %#x", prev, name.Name, v)
				}
				seen[v] = name.Name
				if v%8 != 0 {
					t.Errorf("%s = %#x: niet 8-byte-gealigneerd", name.Name, v)
				}
				if name.Name != "CtrlEnvData" && v+8 > CtrlEnvData {
					t.Errorf("%s = %#x: overlapt de env-regio (CtrlEnvData = %#x)", name.Name, v, uint64(CtrlEnvData))
				}
			}
		}
	}
	if n < 20 {
		t.Fatalf("maar %d Ctrl*-offsets gevonden — is de parse kapot of zijn ze verhuisd uit layout.go?", n)
	}
}
