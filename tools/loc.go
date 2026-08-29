// loc telt de regels van HopOS zoals de compiler ze ziet — per release-smaak
// een `go list -deps` met de échte GOOS/GOARCH/tags, zodat een file die maar
// voor één board of één ISA bouwt ook alleen dáár meetelt. Geen cloc-schatting
// over de hele boom: wat niet gelinkt wordt, telt niet.
//
//	go run ../tools/loc.go            (cwd = metal/; TAMAGO wijst de toolchain aan)
//
// De uitkomst voedt de regeltelling in docs/technical/isolation.md en op
// gethop.org — wijzigt de indeling of komt er een board bij, draai dit en
// neem de nieuwe nummers over. Telling: .go via go/scanner (commentaar en
// lege regels vallen weg), .s met een eigen //- en /* */-stripper.
//
// Emmers, in de volgorde van de ladder op de site:
//   - portable: in élke smaak gelinkt, uitgesplitst per laag (kern, runtime,
//     drivers, net, fw/boot)
//   - arch-gemeenschappelijk: in alle boards van één ISA, board-onafhankelijk
//   - per board: wat alleen déze smaak linkt — board-support plus zijn
//     drivers (igb hoort bij de Altra-stick, genet bij de Pi 4, dwmac4+PHY
//     bij de RK3566). Dít is de swappable buitenlaag.
package main

import (
	"encoding/json"
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const module = "github.com/xinix00/HopOS/metal"

// lean is de zelfgeschreven stdlib (netstack, http, elf) — eigen module,
// linkt in élke node. Hij telt mee, maar als eigen groep: zo blijft de
// metal-ladder over de versies vergelijkbaar én verstopt de telling geen
// eigen code die wél in het image zit. Files heten hier "lean/...".
const leanModule = "github.com/xinix00/lean"

// absPath: waar een lean/-file werkelijk woont (de module-cache) — de
// metal-files zijn relatief aan cwd en staan hier niet in.
var absPath = map[string]string{}

// flavour is één release-smaak: de node-build plus de apploader die erin
// gebakken wordt (fase 1 van elke job), elk met hun eigen tags — precies de
// twee `go build`s van het image-script. De KALE (headless) smaak is de basis
// van alle emmers hieronder; wat `-tags gui` erbij linkt (gui/, de
// surface-grant, de scanout-bedrading) staat apart in de gui-sectie — zo is
// "een headless node = X regels" een gemeten getal en geen voetnoot.
type flavour struct {
	name     string
	arch     string // GOARCH
	nodeTags string // cmd/hopos, kaal
	gui      bool   // heeft dit board een gui-smaak (dezelfde tags + gui)?
}

var flavours = []flavour{
	{"uefi", "arm64", "uefi linkcpuinit", true},
	{"rpi5", "arm64", "rpi5 linkcpuinit", true},
	{"rpi4", "arm64", "rpi4 linkcpuinit", true},
	{"rk3566", "arm64", "rk3566 linkcpuinit", true},
	{"licheerv", "riscv64", "licheerv embedcfg embedcagestub", false},
}

// qemuvirt is een dev-target, geen release-smaak: hij telt niet mee in de
// doorsnede die "portable" bepaalt en krijgt geen eigen board-emmer.

// extra: node-bronnen buiten de Go-builds om. De RISC-V-slotstub is een
// binutils-.S die als binary in kern/cagestub wordt gebakken.
var extra = map[string][]string{
	"licheerv": {"../image/licheerv/stub-slot/stub-slot.S"},
}

// layer deelt een portable file in bij zijn ladder-sport, op package-pad.
func layer(rel string) string {
	first, _, _ := strings.Cut(rel, "/")
	switch first {
	case "abi", "kern", "cpu":
		return "isolation core"
	case "app", "cmd":
		return "app runtime + node mains"
	case "driver", "dev":
		return "drivers"
	case "net":
		return "network stack"
	case "fw", "gui", "board": // board/*.go op de wortel = het board-contract
		return "firmware & boot config"
	}
	return "overig (" + first + ")"
}

type pkgJSON struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	SFiles     []string
}

// filesOf draait go list -deps voor één build en geeft de module-eigen
// bronbestanden terug, als pad relatief aan metal/. -e omdat go:embed-blobs
// (apploader.elf.gz, hopos.cfg) buiten een echte build niet bestaan — de
// filelijsten zijn dan alsnog compleet.
func filesOf(tamago, arch, tags, root string) map[string]bool {
	cmd := exec.Command(tamago, "list", "-deps", "-e", "-json=ImportPath,Dir,GoFiles,SFiles", "-tags", tags, root)
	cmd.Env = append(os.Environ(),
		"GOWORK=off", "GOTOOLCHAIN=local",
		"GOOS=tamago", "GOOSPKG=github.com/usbarmory/tamago",
		"GOARCH="+arch)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(os.Stderr, "go list %s %s: %s\n%s", arch, root, err, ee.Stderr)
		}
		os.Exit(1)
	}
	cwd, _ := os.Getwd()
	set := map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var p pkgJSON
		if err := dec.Decode(&p); err != nil {
			fmt.Fprintln(os.Stderr, "json:", err)
			os.Exit(1)
		}
		isMetal := p.ImportPath == module || strings.HasPrefix(p.ImportPath, module+"/")
		isLean := p.ImportPath == leanModule || strings.HasPrefix(p.ImportPath, leanModule+"/")
		if !isMetal && !isLean {
			continue
		}
		for _, f := range append(append([]string{}, p.GoFiles...), p.SFiles...) {
			if isLean {
				key := "lean" + strings.TrimPrefix(p.ImportPath, leanModule) + "/" + f
				absPath[key] = filepath.Join(p.Dir, f)
				set[key] = true
				continue
			}
			rel, err := filepath.Rel(cwd, filepath.Join(p.Dir, f))
			if err != nil || strings.HasPrefix(rel, "..") {
				continue // embed-blob of gegenereerd pad buiten de boom
			}
			set[rel] = true
		}
	}
	return set
}

// countGo telt regels met minstens één token; go/scanner slikt commentaar en
// lege regels in. Meerregelige (raw) strings tellen per regel mee.
func countGo(path string, src []byte) int {
	fset := token.NewFileSet()
	f := fset.AddFile(path, fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(f, src, func(token.Position, string) {}, 0)
	lines := map[int]bool{}
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		ln := f.Line(pos)
		lines[ln] = true
		for i := 0; i < strings.Count(lit, "\n"); i++ {
			lines[ln+i+1] = true
		}
	}
	return len(lines)
}

// countAsm strippt // en /* */ uit Go-assembly en telt wat overblijft.
// hashComments: bij binutils-.S is ook # commentaar (bij Go-.s juist een
// cpp-directive, die telt).
func countAsm(src []byte, hashComments bool) int {
	n, inBlock := 0, false
	for _, line := range strings.Split(string(src), "\n") {
		if hashComments && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		var b strings.Builder
		for i := 0; i < len(line); i++ {
			if inBlock {
				if strings.HasPrefix(line[i:], "*/") {
					inBlock, i = false, i+1
				}
				continue
			}
			if strings.HasPrefix(line[i:], "//") {
				break
			}
			if strings.HasPrefix(line[i:], "/*") {
				inBlock = true
				i++
				continue
			}
			b.WriteByte(line[i])
		}
		if strings.TrimSpace(b.String()) != "" {
			n++
		}
	}
	return n
}

func count(rel string) int {
	if a, ok := absPath[rel]; ok {
		rel = a
	}
	src, err := os.ReadFile(rel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	switch {
	case strings.HasSuffix(rel, ".S"):
		return countAsm(src, true)
	case strings.HasSuffix(rel, ".s"):
		return countAsm(src, false)
	}
	return countGo(rel, src)
}

func main() {
	tamago := os.Getenv("TAMAGO")
	if tamago == "" {
		tamago = filepath.Join(os.Getenv("HOME"), "tamago-go", "bin", "go")
	}

	sets := map[string]map[string]bool{}
	all := map[string]bool{}
	for _, fl := range flavours {
		set := filesOf(tamago, fl.arch, fl.nodeTags, "./cmd/hopos")
		for _, f := range extra[fl.name] {
			set[f] = true
		}
		sets[fl.name] = set
		for f := range set {
			all[f] = true
		}
	}

	// De gui-smaak, apart: per board het verschil tussen `-tags gui` en kaal.
	// Deze files tellen in GEEN enkele emmer hierboven mee — de kale node is de
	// basis, dit is de opt-in laag erbovenop.
	guiSets := map[string]map[string]bool{}
	guiAll := map[string]bool{}
	var guiBoards []string
	for _, fl := range flavours {
		if !fl.gui {
			continue
		}
		guiBoards = append(guiBoards, fl.name)
		diff := map[string]bool{}
		for f := range filesOf(tamago, fl.arch, fl.nodeTags+" gui", "./cmd/hopos") {
			if !sets[fl.name][f] {
				diff[f] = true
			}
		}
		guiSets[fl.name] = diff
		for f := range diff {
			guiAll[f] = true
		}
	}

	var arm64Boards []string
	for _, fl := range flavours {
		if fl.arch == "arm64" {
			arm64Boards = append(arm64Boards, fl.name)
		}
	}

	// Per file: in welke smaken zit hij? Daaruit volgt de emmer.
	portable := map[string]int{}         // laag → regels
	archCommon := map[string]int{}       // arch → regels (alle boards van die ISA)
	board := map[string]int{}            // smaak-handtekening → regels
	files := map[string]map[string]int{} // emmer → file → regels (voor -v)
	nodeTotal := map[string]int{}        // smaak → regels van die node
	lineCount := map[string]int{}
	for f := range all {
		lineCount[f] = count(f)
	}
	leanTotal := map[string]int{}  // smaak → lean-regels in die node
	leanBucket := map[string]int{} // handtekening → lean-regels
	for _, fl := range flavours {
		for f := range sets[fl.name] {
			if strings.HasPrefix(f, "lean/") {
				leanTotal[fl.name] += lineCount[f]
				continue
			}
			nodeTotal[fl.name] += lineCount[f]
		}
	}

	add := func(m map[string]map[string]int, bucket, f string, n int) {
		if m[bucket] == nil {
			m[bucket] = map[string]int{}
		}
		m[bucket][f] = n
	}
	for f := range all {
		var in []string
		for _, fl := range flavours {
			if sets[fl.name][f] {
				in = append(in, fl.name)
			}
		}
		n := lineCount[f]
		if strings.HasPrefix(f, "lean/") {
			sig := "alle smaken"
			if len(in) != len(flavours) {
				sig = strings.Join(in, "+")
			}
			leanBucket[sig] += n
			add(files, "lean · "+sig, f, n)
			continue
		}
		switch {
		case len(in) == len(flavours):
			portable[layer(f)] += n
			add(files, "portable · "+layer(f), f, n)
		case len(in) == len(arm64Boards) && !sets["licheerv"][f]:
			archCommon["arm64"] += n
			add(files, "arm64-common", f, n)
		case len(in) == 1 && in[0] == "licheerv" && !strings.HasPrefix(f, "board/") && !strings.HasPrefix(f, "driver/"):
			// Eén riscv64-board, dus arch en board vallen samen; het pad
			// splitst ze — cpu/kern/dev is de ISA-laag, board+driver het bord.
			archCommon["riscv64"] += n
			add(files, "riscv64-common", f, n)
		default:
			sig := strings.Join(in, "+")
			board[sig] += n
			add(files, "board "+sig, f, n)
		}
	}

	verbose := len(os.Args) > 1 && os.Args[1] == "-v"
	p := func(format string, a ...any) { fmt.Printf(format+"\n", a...) }

	p("== portable — telt voor élke node ==")
	total := 0
	for _, l := range []string{"isolation core", "app runtime + node mains", "drivers", "network stack", "firmware & boot config"} {
		p("  %-40s %6d", l, portable[l])
		total += portable[l]
	}
	for l, n := range portable {
		if strings.HasPrefix(l, "overig") {
			p("  %-40s %6d  (indelen!)", l, n)
			total += n
		}
	}
	p("  %-40s %6d", "— portable totaal", total)

	p("\n== per architectuur — board-onafhankelijk ==")
	p("  %-40s %6d", "arm64 (alle arm64-boards)", archCommon["arm64"])
	p("  %-40s %6d", "riscv64", archCommon["riscv64"])

	p("\n== per board — de swappable buitenlaag ==")
	var sigs []string
	for s := range board {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	for _, s := range sigs {
		p("  %-40s %6d", s, board[s])
	}

	// De gui-laag: wat `-tags gui` bovenop de kale node linkt. Gemeenschappelijk
	// (fbgrant, surface-grant, de stage-2-surface-kant) versus board-eigen
	// (gui/driver/rkscan + de scanout-bedrading is er alleen op de rk3566).
	guiTotal := map[string]int{}
	guiBuckets := map[string]int{}
	for f := range guiAll {
		n := count(f)
		var in []string
		for _, b := range guiBoards {
			if guiSets[b][f] {
				in = append(in, b)
				guiTotal[b] += n
			}
		}
		sig := strings.Join(in, "+")
		if len(in) == len(guiBoards) {
			sig = "alle gui-boards"
		}
		guiBuckets[sig] += n
		add(files, "gui · "+sig, f, n)
	}
	p("\n== lean — de zelfgeschreven stdlib (eigen module): wat de node includet ==")
	{
		var sigs []string
		for s := range leanBucket {
			sigs = append(sigs, s)
		}
		sort.Strings(sigs)
		for _, s := range sigs {
			p("  %-40s %6d", s, leanBucket[s])
		}
		// Per package: het antwoord op "is X eigenlijk wel een dependency
		// van de node, of bestaat hij alleen in de module (voor apps)?"
		pkg := map[string]int{}
		for f := range absPath {
			parts := strings.SplitN(f, "/", 3)
			if len(parts) >= 2 {
				pkg["lean/"+parts[1]] += lineCount[f]
			}
		}
		var names []string
		for n := range pkg {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p("    %-38s %6d", n, pkg[n])
		}
	}

	p("\n== gui — de opt-in smaak (-tags gui), telt in geen kale node mee ==")
	sigs = sigs[:0]
	for s := range guiBuckets {
		sigs = append(sigs, s)
	}
	sort.Strings(sigs)
	for _, s := range sigs {
		p("  %-40s %6d", s, guiBuckets[s])
	}

	// De ladder-view: een node = portable + zijn ISA-laag + zijn board-laag.
	// De board-laag hier is de rest van wat déze machine linkt — inclusief
	// zijn NIC/PHY/display-drivers en wat hij met een buurbord deelt. De
	// gui-kolom is de aparte vermelding: kaal is het getal, gui de optie.
	p("\n== een node = portable + ISA-laag + board-laag (gui apart) ==")
	for _, fl := range flavours {
		bs := nodeTotal[fl.name] - total - archCommon[fl.arch]
		g := ""
		if fl.gui {
			g = fmt.Sprintf("   (+%d met gui)", guiTotal[fl.name])
		}
		p("  %-16s %6d + %4d (%s) + %4d (board) = %6d metal + %4d lean%s", fl.name, total, archCommon[fl.arch], fl.arch, bs, nodeTotal[fl.name], leanTotal[fl.name], g)
	}

	if verbose {
		p("\n== files per emmer ==")
		var buckets []string
		for b := range files {
			buckets = append(buckets, b)
		}
		sort.Strings(buckets)
		for _, b := range buckets {
			p("%s", b)
			var fs []string
			for f := range files[b] {
				fs = append(fs, f)
			}
			sort.Strings(fs)
			for _, f := range fs {
				p("  %6d  %s", files[b][f], f)
			}
		}
	}
}
