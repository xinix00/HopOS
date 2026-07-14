// importcheck dwingt de importrichting van docs/indeling.md af — als
// buildfout, niet als reviewtaak (zelfde filosofie als de compile-time
// contract-assertie in kern/slotmgr). tools/test.sh draait het vóór de tests:
//
//	go run ../tools/importcheck.go   (cwd = metal/)
//
// Het leest ELKE .go-file (ook achter build-tags — juist die randen drijven
// af) en toetst alle hop-os/metal-imports tegen de laag-regels. De regels
// zijn hier datavorm; de uitleg en het waarom staan in docs/indeling.md —
// wijzigt de indeling, pas dan BEIDE aan.
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const module = "hop-os/metal/"

// category bepaalt de regelgroep van een package-pad (relatief aan metal/).
func category(pkg string) string {
	first, _, _ := strings.Cut(pkg, "/")
	switch {
	case pkg == "board":
		return "board-contract"
	case pkg == "board/appboard":
		return "appboard"
	case strings.HasSuffix(pkg, "/hop") && first == "board",
		pkg == "board/raspi/vcfb":
		return "board-hop" // HOP-bedrading: drivers toegestaan
	case first == "board":
		return "board-basis" // wat een app-image meelinkt
	default:
		return first // dev, abi, fw, cpu, driver, net, kern, app, cmd
	}
}

// allowed toetst één import (imp, relatief aan metal/) vanuit een categorie.
func allowed(cat, imp string) bool {
	ifirst, _, _ := strings.Cut(imp, "/")
	icat := category(imp)
	switch cat {
	case "dev":
		return false // dev importeert niets
	case "abi", "fw":
		return imp == "dev"
	case "cpu":
		return imp == "dev" || ifirst == "abi" || ifirst == "cpu"
	case "driver":
		// Drivers kennen het board-contract niet (pcie.Window woont sinds
		// 14-07 bij pcie zelf). cpu ligt ónder driver (lagenvolgorde
		// indeling.md): dvfs leest bijvoorbeeld de idle-tik-teller (cpu/idle).
		return imp == "dev" ||
			ifirst == "abi" || ifirst == "cpu" || ifirst == "fw" || ifirst == "driver"
	case "net":
		return imp == "dev" || imp == "board" || ifirst == "abi" || ifirst == "net"
	case "kern":
		return imp == "dev" || imp == "board" ||
			ifirst == "abi" || ifirst == "cpu" || ifirst == "fw" ||
			ifirst == "driver" || ifirst == "net" || ifirst == "kern"
	case "appboard":
		return false // het app-contract importeert niets
	case "board-contract":
		// Alleen de typen die het contract draagt (fb.Desc, pcie.Window,
		// dhcp.Lease) plus het ge-embedde app-contract.
		return imp == "board/appboard" || imp == "driver/fb" || imp == "driver/pcie" || imp == "net/dhcp"
	case "board-basis":
		// De basis-helft wordt in élk app-image gelinkt: geen contract, geen
		// net/kern, en uit driver/ uitsluitend de console-uitzondering
		// (pl011/fb — printk is een runtime-hook en kan niet init-geïnjecteerd
		// worden zonder vroege bootdiagnose te verliezen).
		return imp == "dev" || imp == "board/appboard" || imp == "board/raspi" ||
			imp == "driver/pl011" || imp == "driver/fb" ||
			ifirst == "abi" || ifirst == "cpu" || ifirst == "fw"
	case "board-hop":
		// De HOP-bedrading mag alles behalve kern/app/cmd.
		return icat != "kern" && icat != "app" && icat != "cmd"
	case "app":
		// De app-kant kent HOP uitsluitend via abi/ (+ dev/cpu/appboard/
		// board-basis om op te draaien) — indeling.md regel 2, hier hard.
		return imp == "dev" || imp == "board/appboard" || icat == "board-basis" ||
			ifirst == "abi" || ifirst == "cpu" || ifirst == "app"
	case "cmd":
		return icat != "app" // regel 3: niets importeert app/
	}
	return false
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	fset := token.NewFileSet()
	var violations []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// De root zelf ("." bij aanroep uit metal/) nooit skippen — alleen
			// subdirs als buildoutput en verborgen mappen.
			if name := d.Name(); path != root && (name == "out" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if pkg == "." {
			return nil // module-root: alleen go.mod/go.sum
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		cat := category(pkg)
		for _, im := range f.Imports {
			p := strings.Trim(im.Path.Value, `"`)
			if !strings.HasPrefix(p, module) {
				continue // std, externe modules, hop/: buiten deze regels
			}
			imp := strings.TrimPrefix(p, module)
			if !allowed(cat, imp) {
				violations = append(violations,
					fmt.Sprintf("%s: %s (%s) importeert %s — verboden per docs/indeling.md",
						rel, pkg, cat, imp))
			}
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "importcheck:", err)
		os.Exit(2)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "IMPORTREGEL:", v)
		}
		fmt.Fprintf(os.Stderr, "importcheck: %d overtreding(en)\n", len(violations))
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "importcheck: importrichting klopt met docs/indeling.md")
}
