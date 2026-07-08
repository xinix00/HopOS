// Package hopfs is HOP's minimale bestandslaag op de NVMe — de storage van
// het plan (§3, herzien 2026-07-07): shared dirs (volumes) en de lege
// per-task roots leven hier. Bewust géén ext4, géén persistentie: de
// metadata (boom, extents) leeft in HOP's RAM, alleen de bestandsdata staat
// in 4KB-blokken op de schijf, en bij boot is alles per definitie leeg.
// Alleen HOP raakt dit pakket aan; apps komen er uitsluitend bij via de
// hop-ABI (metal/slots resolvet hun paden tegen de mount-tabel).
package hopfs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"hop-os/metal/nvme"
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
			return nil, fmt.Errorf("hopfs: '..' niet toegestaan (%q)", path)
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
			return nil, fmt.Errorf("hopfs: %q is geen directory", strings.Join(segs[:i], "/"))
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
		return nil, fmt.Errorf("hopfs: %q is geen directory", path)
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
		return fmt.Errorf("hopfs: %q bestaat als bestand", path)
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
		return 0, fmt.Errorf("hopfs: %q is een directory", path)
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

	segs, err := split(path)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("hopfs: leeg pad")
	}
	parent, err := f.walk(segs[:len(segs)-1], true)
	if err != nil {
		return err
	}
	name := segs[len(segs)-1]
	n, ok := parent.children[name]
	created := false
	if !ok {
		if f.nodes >= maxNodes {
			return fmt.Errorf("hopfs: te veel bestanden/dirs (max %d)", maxNodes)
		}
		n = &node{}
		parent.children[name] = n
		f.nodes++
		created = true
	} else if n.dir {
		return fmt.Errorf("hopfs: %q is een directory", path)
	}

	// Bij een fout (schijf vol, disk-I/O) de zojuist gealloceerde blokken én
	// een vers aangemaakte node terugdraaien — anders lekt een mislukte write
	// blijvend blokken en metadata.
	orig := len(n.blocks)
	// Terugdraaien bij een fout halverwege: nieuw gealloceerde blokken vrijgeven
	// en ingevulde gaten (index < orig) terugzetten. De payload kan een gat
	// vóór orig echt maken, dus n.blocks[orig:] afkappen volstaat niet.
	var newlyAlloc []uint32
	var filled []int
	fail := func(e error) error {
		f.free = append(f.free, newlyAlloc...)
		for _, idx := range filled {
			n.blocks[idx] = holeBlock
		}
		n.blocks = n.blocks[:orig]
		if created {
			delete(parent.children, name)
			f.nodes--
		}
		return e
	}

	// Groei tot het benodigde aantal blokken met GATEN: geen alloc, geen
	// disk-write. Een gat leest als nul en wordt pas een echt blok als de
	// payload het hieronder raakt — sparse, dus een schrijf op een grote
	// offset kost geen schijf-I/O voor het gat.
	need := (end + BlockSize - 1) / BlockSize
	for uint64(len(n.blocks)) < need {
		n.blocks = append(n.blocks, holeBlock)
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
				return fail(err)
			}
			newlyAlloc = append(newlyAlloc, b)
			if int(bi) < orig {
				filled = append(filled, int(bi))
			}
			n.blocks[bi] = b
		}
		lba := f.lba(n.blocks[bi])
		if chunk < BlockSize { // deelblok: bestaande inhoud behouden
			if fresh {
				buf = [BlockSize]byte{} // vers gat leest als nul → geen disk-read
			} else if err := f.disk.Read(lba, buf[:]); err != nil {
				return fail(err)
			}
		}
		copy(buf[bo:bo+chunk], p[done:done+chunk])
		if err := f.disk.Write(lba, buf[:]); err != nil {
			return fail(err)
		}
		done += chunk
	}
	if end > n.size {
		n.size = end
	}
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
		return fmt.Errorf("hopfs: %q is niet leeg", path)
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
	for _, b := range n.blocks {
		if b != holeBlock { // gaten zijn nooit gealloceerd
			f.free = append(f.free, b)
		}
	}
}
