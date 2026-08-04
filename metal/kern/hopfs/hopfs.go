// Package hopfs is HOP's minimale bestandslaag op de NVMe — de storage van
// het plan (§3, herzien 2026-07-07): shared dirs (volumes) en de lege
// per-task roots leven hier. Bewust géén ext4, géén persistentie: de
// metadata (boom, extents) leeft in HOP's RAM, alleen de bestandsdata staat
// in 4KB-blokken op de schijf, en bij boot is alles per definitie leeg.
// Alleen HOP raakt dit pakket aan; apps komen er uitsluitend bij via de
// hop-ABI (metal/kern/slots resolvet hun paden tegen de mount-tabel).
package hopfs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/xinix00/HopOS/metal/driver/nvme"
)

// BlockSize is de logische blokmaat (8 NVMe-LBA's van 512B).
const BlockSize = 4096

// holeBlock is de sentinel voor een niet-gealloceerd gat in een bestand: een
// blokindex die de payload (nog) niet raakte. Leest als nul en kost geen
// schijf. Zo is een schrijf op een grote offset O(payload) i.p.v. het hele
// gat 0..off vol te nullen onder f.mu — dat bevroor alle andere slots' fs-RPC's
// (één sparse write van 1 byte op schijf-4096 = seconden schijf-I/O onder lock).
// Geldige blokindexen zijn 0..max-1 met max ≤ 2^32-1, dus ^uint32(0) botst nooit.
const holeBlock = ^uint32(0)

// maxNodes begrenst het aantal nodes in de boom. Anders dan bestandsgrootte
// (die de schijf zelf begrenst) leeft de metadata volledig in HOP's RAM: een
// app die eindeloos kleine bestanden aanmaakt gebruikt ~0 schijf maar laat
// HOP's heap groeien tot de kern OOM't — en dan vallen álle slots, niet alleen
// de dader. Dit is dezelfde isolatiegrens als de overflow-guard in WriteAt: één
// task mag HOP nooit vellen. ~1M nodes is ruim maar begrensd.
const maxNodes = 1 << 20

// maxIndexBlocks begrenst het TOTAAL aantal blok-indexen in de boom — de
// tweede helft van dezelfde grens als maxNodes, en de reparatie van een gat
// daarin: ook de blok-index van een bestand is metadata in HOP's RAM (4 byte
// per 4KB-blok), en die groeit met de OFFSET waarop geschreven wordt, niet met
// de payload. De schijfgrens in WriteAt dekt dat niet: één `write(off=schijf-1,
// 1 byte)` van een willekeurige app vroeg ~0 schijf maar liet de index tot de
// hele schijf groeien (op een 256GB-NVMe ~256MB) → kern-OOM → álle slots dood.
//
// 4M indexen = 16MB HOP-RAM = 16GB totaal geadresseerde bestandsruimte. Dat is
// de eerlijke bovengrens van deze laag: hopfs indexeert op 4KB in HOP's heap,
// dus de praktische grens is dít budget, niet de schijfmaat. Voor de ontworpen
// rol (scratch/RAM-overloop; de bron is S3, niets is persistent) is dat ruim —
// en vol = een directe fout i.p.v. een node die omvalt.
const maxIndexBlocks = 1 << 22

type node struct {
	dir      bool
	children map[string]*node // dir
	blocks   []uint32         // file: blokindexen
	size     uint64           // file: lengte in bytes
}

// FS is één bestandslaag op één NVMe-namespace.
type FS struct {
	mu    sync.Mutex
	disk  *nvme.Controller
	root  *node
	free  []uint32 // teruggegeven blokken
	next  uint32   // bump-allocator
	max   uint32   // totaal aantal blokken
	nodes int      // aantal nodes in de boom (excl. root), tegen OOM
	index int      // totaal aantal blok-indexen in de boom, tegen OOM
}

// New maakt een lege bestandslaag op de (als leeg beschouwde) schijf.
func New(disk *nvme.Controller) *FS {
	return &FS{
		disk: disk,
		root: &node{dir: true, children: map[string]*node{}},
		max:  uint32(disk.Blocks * disk.BlockSize / BlockSize),
	}
}

// split maakt van een pad propere segmenten; ".."/"." zijn niet toegestaan
// (paden zijn hier al door de mount-resolutie heen — dit is de laatste grens).
func split(path string) ([]string, error) {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		switch s {
		case "", ".":
		case "..":
			return nil, fmt.Errorf("hopfs: '..' not allowed (%q)", path)
		default:
			segs = append(segs, s)
		}
	}
	return segs, nil
}

// walk zoekt een node; bij mkParents worden ontbrekende dirs aangemaakt.
func (f *FS) walk(segs []string, mkParents bool) (*node, error) {
	n := f.root
	for i, s := range segs {
		if !n.dir {
			return nil, fmt.Errorf("hopfs: %q is not a directory", strings.Join(segs[:i], "/"))
		}
		child, ok := n.children[s]
		if !ok {
			if !mkParents {
				return nil, errNoEnt
			}
			if f.nodes >= maxNodes {
				return nil, fmt.Errorf("hopfs: te veel bestanden/dirs (max %d)", maxNodes)
			}
			child = &node{dir: true, children: map[string]*node{}}
			n.children[s] = child
			f.nodes++
		}
		n = child
	}
	return n, nil
}

var errNoEnt = fmt.Errorf("hopfs: bestaat niet")

// IsNotExist meldt of err "bestaat niet" is (voor de status-mapping).
func IsNotExist(err error) bool { return err == errNoEnt }

func (f *FS) alloc() (uint32, error) {
	if n := len(f.free); n > 0 {
		b := f.free[n-1]
		f.free = f.free[:n-1]
		return b, nil
	}
	if f.next >= f.max {
		return 0, fmt.Errorf("hopfs: schijf vol (%d blokken)", f.max)
	}
	f.next++
	return f.next - 1, nil
}

func (f *FS) lba(block uint32) uint64 {
	return uint64(block) * (BlockSize / f.disk.BlockSize)
}

// Stat geeft (size, isDir).
func (f *FS) Stat(path string) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs, err := split(path)
	if err != nil {
		return 0, false, err
	}
	n, err := f.walk(segs, false)
	if err != nil {
		return 0, false, err
	}
	return n.size, n.dir, nil
}

// List geeft de namen in een dir, gesorteerd; dirs krijgen een "/"-suffix.
func (f *FS) List(path string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs, err := split(path)
	if err != nil {
		return nil, err
	}
	n, err := f.walk(segs, false)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, fmt.Errorf("hopfs: %q is not a directory", path)
	}
	var names []string
	for name, c := range n.children {
		if c.dir {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// MkdirAll maakt een dir (incl. ouders).
func (f *FS) MkdirAll(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs, err := split(path)
	if err != nil {
		return err
	}
	n, err := f.walk(segs, true)
	if err != nil {
		return err
	}
	if !n.dir {
		return fmt.Errorf("hopfs: %q exists as a file", path)
	}
	return nil
}

// ReadAt leest maximaal len(p) bytes vanaf off; geeft n terug (kort bij EOF).
func (f *FS) ReadAt(path string, off uint64, p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs, err := split(path)
	if err != nil {
		return 0, err
	}
	n, err := f.walk(segs, false)
	if err != nil {
		return 0, err
	}
	if n.dir {
		return 0, fmt.Errorf("hopfs: %q is a directory", path)
	}
	if off >= n.size {
		return 0, nil
	}
	want := uint64(len(p))
	if off+want > n.size {
		want = n.size - off
	}
	var buf [BlockSize]byte
	done := uint64(0)
	for done < want {
		bi := (off + done) / BlockSize
		bo := (off + done) % BlockSize
		chunk := BlockSize - bo
		if chunk > want-done {
			chunk = want - done
		}
		if n.blocks[bi] == holeBlock { // gat: leest als nul
			clear(p[done : done+chunk])
			done += chunk
			continue
		}
		if err := f.disk.Read(f.lba(n.blocks[bi]), buf[:]); err != nil {
			return int(done), err
		}
		copy(p[done:done+chunk], buf[bo:bo+chunk])
		done += chunk
	}
	return int(done), nil
}

// file zoekt (of maakt, mét ouder-dirs) het bestand op path — de gedeelde kop
// van WriteAt en Truncate. Aanroepen onder f.mu.
func (f *FS) file(path string) (*node, error) {
	segs, err := split(path)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("hopfs: leeg pad")
	}
	parent, err := f.walk(segs[:len(segs)-1], true)
	if err != nil {
		return nil, err
	}
	name := segs[len(segs)-1]
	n, ok := parent.children[name]
	if !ok {
		if f.nodes >= maxNodes {
			return nil, fmt.Errorf("hopfs: te veel bestanden/dirs (max %d)", maxNodes)
		}
		n = &node{}
		parent.children[name] = n
		f.nodes++
	} else if n.dir {
		return nil, fmt.Errorf("hopfs: %q is a directory", path)
	}
	return n, nil
}

// growTo groeit de blokkenlijst van n met GATEN tot need blokken, tegen het
// index-budget (zie maxIndexBlocks): weigeren VÓÓR de groei-lus, want de lus
// zelf is de schade (allocatie onder f.mu). what benoemt de vrager in de fout.
func (f *FS) growTo(n *node, need uint64, what string) error {
	if grow := int(need) - len(n.blocks); grow > 0 {
		if f.index+grow > maxIndexBlocks {
			return fmt.Errorf("hopfs: bestandsindex-budget vol (%d van %d blokken; %s vraagt %d bij)",
				f.index, maxIndexBlocks, what, grow)
		}
		f.index += grow
	}
	for uint64(len(n.blocks)) < need {
		n.blocks = append(n.blocks, holeBlock)
	}
	return nil
}

// WriteAt schrijft p op off; maakt het bestand (en ouder-dirs) zo nodig aan
// en groeit het bij schrijven voorbij het einde (gat = nulbytes).
func (f *FS) WriteAt(path string, off uint64, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	// off komt (via de hop-ABI) ongecontroleerd van de app: overflow-veilig
	// rekenen en tot de fysieke schijfcapaciteit begrenzen. De overflow-guard
	// is verplicht — off+len bij uint64-max wrapt en laat de schrijf-lus buiten
	// n.blocks indexeren (panic → hele EL2-kern valt). De capaciteitsgrens is
	// de natuurlijke grens (zoals Linux vollopen): een bestand mag zo groot als
	// de schijf, maar niet daarbuiten — dat zou toch alloc-falen, nu met een
	// directe fout i.p.v. een doemende groei-lus onder f.mu.
	end := off + uint64(len(p))
	if end < off {
		return fmt.Errorf("hopfs: offset %d + %d bytes overflowt", off, len(p))
	}
	if diskBytes := uint64(f.max) * BlockSize; end > diskBytes {
		return fmt.Errorf("hopfs: offset %d + %d bytes > schijf (%d)", off, len(p), diskBytes)
	}

	n, err := f.file(path)
	if err != nil {
		return err
	}

	// Foutsemantiek is POSIX-achtig: een write die halverwege faalt (schijf
	// vol, disk-I/O) laat een deels geschreven bestand achter. Onderweg
	// gealloceerde blokken blijven gewoon van het bestand (Remove geeft ze
	// terug, en bij boot is alles sowieso leeg) — geen lek, dus ook geen
	// terugdraai-administratie. Alleen size wordt pas bij succes bijgewerkt.

	// Groei tot het benodigde aantal blokken met GATEN: geen alloc, geen
	// disk-write. Een gat leest als nul en wordt pas een echt blok als de
	// payload het hieronder raakt — sparse, dus een schrijf op een grote
	// offset kost geen schijf-I/O voor het gat.
	if err := f.growTo(n, (end+BlockSize-1)/BlockSize, fmt.Sprintf("offset %d", off)); err != nil {
		return err
	}

	var buf [BlockSize]byte
	done := uint64(0)
	for done < uint64(len(p)) {
		bi := (off + done) / BlockSize
		bo := (off + done) % BlockSize
		chunk := BlockSize - bo
		if chunk > uint64(len(p))-done {
			chunk = uint64(len(p)) - done
		}
		// Raakt de payload een gat, dan nú pas een echt blok alloceren.
		fresh := n.blocks[bi] == holeBlock
		if fresh {
			b, err := f.alloc()
			if err != nil {
				return err
			}
			n.blocks[bi] = b
		}
		lba := f.lba(n.blocks[bi])
		if chunk < BlockSize { // deelblok: bestaande inhoud behouden
			if fresh {
				buf = [BlockSize]byte{} // vers gat leest als nul → geen disk-read
			} else if err := f.disk.Read(lba, buf[:]); err != nil {
				return err
			}
		}
		copy(buf[bo:bo+chunk], p[done:done+chunk])
		if err := f.disk.Write(lba, buf[:]); err != nil {
			return err
		}
		done += chunk
	}
	if end > n.size {
		n.size = end
	}
	return nil
}

// Truncate zet het bestand op precies size bytes en maakt het (met ouder-dirs)
// aan als het nog niet bestond. Krimpen geeft de blokken voorbij de nieuwe
// lengte terug; groeien voegt gaten toe (die lezen als nul, zoals bij WriteAt).
//
// Dit is de helft die "een bestand schrijven" nodig had en niet had: WriteAt
// alleen kan een bestand niet KORTER maken, dus een kortere nieuwe inhoud liet
// de oude staart staan en een lege inhoud liet het oude bestand volledig
// intact. De aanroeper (de hop-ABI-WriteFile) zet daarom eerst op 0.
func (f *FS) Truncate(path string, size uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if diskBytes := uint64(f.max) * BlockSize; size > diskBytes {
		return fmt.Errorf("hopfs: truncate %d > schijf (%d)", size, diskBytes)
	}
	n, err := f.file(path)
	if err != nil {
		return err
	}

	need := int((size + BlockSize - 1) / BlockSize)
	if need > len(n.blocks) {
		if err := f.growTo(n, uint64(need), fmt.Sprintf("truncate %d", size)); err != nil {
			return err
		}
	} else {
		for _, b := range n.blocks[need:] {
			if b != holeBlock { // gaten zijn nooit gealloceerd
				f.free = append(f.free, b)
			}
		}
		f.index -= len(n.blocks) - need
		n.blocks = n.blocks[:need]
	}
	n.size = size
	return nil
}

// Remove verwijdert een bestand of lege dir en geeft blokken terug.
func (f *FS) Remove(path string) error {
	return f.remove(path, false)
}

// RemoveAll verwijdert een boom (voor de verse per-task root bij een start).
func (f *FS) RemoveAll(path string) error {
	err := f.remove(path, true)
	if IsNotExist(err) {
		return nil
	}
	return err
}

func (f *FS) remove(path string, recursive bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	segs, err := split(path)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("hopfs: root verwijderen kan niet")
	}
	parent, err := f.walk(segs[:len(segs)-1], false)
	if err != nil {
		return err
	}
	name := segs[len(segs)-1]
	n, ok := parent.children[name]
	if !ok {
		return errNoEnt
	}
	if n.dir && len(n.children) > 0 && !recursive {
		return fmt.Errorf("hopfs: %q is not empty", path)
	}
	f.release(n)
	delete(parent.children, name)
	return nil
}

func (f *FS) release(n *node) {
	f.nodes--
	if n.dir {
		for _, c := range n.children {
			f.release(c)
		}
		return
	}
	f.index -= len(n.blocks) // index-budget terug (zie maxIndexBlocks)
	for _, b := range n.blocks {
		if b != holeBlock { // gaten zijn nooit gealloceerd
			f.free = append(f.free, b)
		}
	}
}
