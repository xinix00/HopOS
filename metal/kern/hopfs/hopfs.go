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

	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
)

// BlockSize is de logische blokmaat (8 NVMe-LBA's van 512B).
const BlockSize = 4096

// maxNodes begrenst het aantal nodes in de boom. Anders dan bestandsgrootte
// (die de schijf zelf begrenst) leeft de metadata volledig in HOP's RAM: een
// app die eindeloos kleine bestanden aanmaakt gebruikt ~0 schijf maar laat
// HOP's heap groeien tot de kern OOM't — en dan vallen álle slots, niet alleen
// de dader. Dit is dezelfde isolatiegrens als de overflow-guard in WriteAt: één
// task mag HOP nooit vellen. ~1M nodes is ruim maar begrensd.
const maxNodes = 1 << 20

// maxIndexExtents bounds fragmentation metadata, not file lengths. A contiguous
// file needs one extent regardless of its size; sparse holes need none.
const maxIndexExtents = 1 << 18

type node struct {
	dir      bool
	children map[string]*node // dir
	extents  []extent         // file: sorted allocated runs; holes are implicit
	size     uint64           // file: lengte in bytes
}

// blockDevice is precies de grens die hopfs nodig heeft. De productie gebruikt
// nvme.Controller; de kleine interface maakt run-batching ook zonder MMIO
// testbaar.
type blockDevice interface {
	Read(lba uint64, p []byte) error
	Write(lba uint64, p []byte) error
}

// FS is één bestandslaag op één NVMe-namespace.
type FS struct {
	mu   sync.Mutex
	disk blockDevice
	// base is de eerste LBA van ons venster (zie NewRange); 0 = de hele schijf.
	base         uint64
	lbasPerBlock uint64 // fysieke LBA's per logisch hopfs-blok
	maxIOBlocks  uint64 // hopfs-blokken per diskcommand
	root         *node
	free         []diskRange // sorted, coalesced returned runs
	next         uint32      // bump-allocator
	max          uint32      // totaal aantal blokken
	nodes        int         // aantal nodes in de boom (excl. root), tegen OOM
	index        int         // total file extents, against unbounded fragmentation
}

// New maakt een lege bestandslaag op de (als leeg beschouwde) schijf.
func New(disk *nvme.Controller) *FS {
	return NewRange(disk, 0, disk.Blocks)
}

// NewRange legt hopfs op een VENSTER van de schijf: vanaf firstLBA, blocks
// LBA's lang. Blok 0 van dit bestandssysteem is dan firstLBA op het ijzer, en
// verder weet hopfs van niets — hij kan per constructie niet buiten zijn venster
// schrijven.
//
// Waarom dat moet bestaan: op elk bord tot nu toe was de NVMe van ons alleen, en
// dan is "de hele schijf" de juiste aanname. Op een Mac mini niet: daar staat
// macOS op dezelfde SSD, plus een Recovery-partitie, en is er alleen een gat
// dat de eigenaar zelf heeft vrijgemaakt (30-08: 414GB tussen de APFS-container
// en RecoveryOS). Zonder venster zou de eerste download van een app dwars door
// het bestandssysteem van de gebruiker heen schrijven — en dat is geen bug die
// je met een foutmelding oplost.
func NewRange(disk *nvme.Controller, firstLBA, blocks uint64) *FS {
	return newRange(disk, firstLBA, blocks, disk.BlockSize, disk.MaxTransfer)
}

func newRange(disk blockDevice, firstLBA, blocks, diskBlockSize, maxTransfer uint64) *FS {
	perBlock := uint64(BlockSize) / diskBlockSize
	if perBlock == 0 {
		perBlock = 1 // schijf met blokken > 4KB: één LBA per hopfs-blok
	}
	ioBlocks := maxTransfer / BlockSize
	if ioBlocks == 0 {
		ioBlocks = 1
	}
	return &FS{
		disk:         disk,
		base:         firstLBA,
		lbasPerBlock: perBlock,
		maxIOBlocks:  ioBlocks,
		root:         &node{dir: true, children: map[string]*node{}},
		max:          uint32(blocks / perBlock),
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

func (f *FS) lba(block uint32) uint64 {
	return f.base + uint64(block)*f.lbasPerBlock
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
	names, _, err := f.list(path, -1)
	return names, err
}

// ListN is List met een bovengrens op het aantal namen. truncated betekent dat
// de directory meer entries draagt; in dat geval wordt geen gedeeltelijke lijst
// gebouwd. De aanroeper bepaalt zelf hoe namen worden geserialiseerd en hoeveel
// bytes daarbij passen.
func (f *FS) ListN(path string, maxEntries int) (names []string, truncated bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.list(path, maxEntries)
}

// list voert de listing uit onder f.mu. maxEntries < 0 betekent onbegrensd.
func (f *FS) list(path string, maxEntries int) ([]string, bool, error) {
	segs, err := split(path)
	if err != nil {
		return nil, false, err
	}
	n, err := f.walk(segs, false)
	if err != nil {
		return nil, false, err
	}
	if !n.dir {
		return nil, false, fmt.Errorf("hopfs: %q is not a directory", path)
	}
	if maxEntries >= 0 && len(n.children) > maxEntries {
		return nil, true, nil
	}
	names := make([]string, 0, len(n.children))
	for name, c := range n.children {
		if c.dir {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, false, nil
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

// ReadAt reads at most len(p) bytes; implicit holes read as zero.
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
	want := min(uint64(len(p)), n.size-off)
	var buf [BlockSize]byte
	done := uint64(0)
	for done < want {
		bi, bo := (off+done)/BlockSize, (off+done)%BlockSize
		block, run, mapped := n.lookup(uint32(bi))
		chunk := min(uint64(BlockSize)-bo, want-done)
		if !mapped {
			chunk = min(uint64(run)*BlockSize-bo, want-done)
			clear(p[done : done+chunk])
		} else if bo == 0 && chunk == BlockSize {
			chunk = min(uint64(run), f.maxIOBlocks, (want-done)/BlockSize) * BlockSize
			if err := f.disk.Read(f.lba(block), p[done:done+chunk]); err != nil {
				return int(done), err
			}
		} else {
			if err := f.disk.Read(f.lba(block), buf[:]); err != nil {
				return int(done), err
			}
			copy(p[done:done+chunk], buf[bo:bo+chunk])
		}
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

// WriteAt retains successful earlier chunks on an I/O error. A fresh mapping
// is published only after its data has been written, never exposing old owners.
func (f *FS) WriteAt(path string, off uint64, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := off + uint64(len(p))
	if end < off {
		return fmt.Errorf("hopfs: offset overflow")
	}
	if end > uint64(f.max)*BlockSize {
		return fmt.Errorf("hopfs: write exceeds disk window")
	}
	n, err := f.file(path)
	if err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	var buf [BlockSize]byte
	done := uint64(0)
	for done < uint64(len(p)) {
		bi, bo := (off+done)/BlockSize, (off+done)%BlockSize
		block, available, mapped := n.lookup(uint32(bi))
		chunk := min(uint64(BlockSize)-bo, uint64(len(p))-done)
		run := uint32(1)
		if bo == 0 && chunk == BlockSize {
			run = uint32(min(uint64(available), f.maxIOBlocks, (uint64(len(p))-done)/BlockSize))
		}
		if !mapped {
			block, run, err = f.allocRun(run)
			if err != nil {
				return err
			}
			if f.index >= maxIndexExtents && !n.joins(uint32(bi), block, run) {
				f.freeRun(block, run)
				return fmt.Errorf("hopfs: fragmented file index full (%d extents)", f.index)
			}
		}
		var payload []byte
		if bo == 0 && chunk == BlockSize {
			chunk = uint64(run) * BlockSize
			payload = p[done : done+chunk]
		} else {
			clear(buf[:])
			if mapped {
				if err := f.disk.Read(f.lba(block), buf[:]); err != nil {
					return err
				}
			}
			copy(buf[bo:bo+chunk], p[done:done+chunk])
			payload = buf[:]
		}
		if err := f.disk.Write(f.lba(block), payload); err != nil {
			if !mapped {
				f.freeRun(block, run)
			}
			return err
		}
		if !mapped {
			f.mapRun(n, extent{uint32(bi), block, run})
		}
		done += chunk
		n.size = max(n.size, off+done)
	}
	return nil
}

// Truncate grows sparsely; shrinking returns runs and zeros the retained tail
// so a later extension cannot reveal discarded bytes.
func (f *FS) Truncate(path string, size uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if size > uint64(f.max)*BlockSize {
		return fmt.Errorf("hopfs: truncate exceeds disk window")
	}
	n, err := f.file(path)
	if err != nil {
		return err
	}
	if size < n.size {
		if tail := size % BlockSize; tail != 0 {
			block, _, mapped := n.lookup(uint32(size / BlockSize))
			if mapped {
				var buf [BlockSize]byte
				if err := f.disk.Read(f.lba(block), buf[:]); err != nil {
					return err
				}
				clear(buf[tail:])
				if err := f.disk.Write(f.lba(block), buf[:]); err != nil {
					return err
				}
			}
		}
		need := uint32((size + BlockSize - 1) / BlockSize)
		keep := 0
		for _, e := range n.extents {
			if e.logical >= need {
				f.freeRun(e.physical, e.count)
				f.index--
				continue
			}
			if uint64(e.logical)+uint64(e.count) > uint64(need) {
				retain := need - e.logical
				f.freeRun(e.physical+retain, e.count-retain)
				e.count = retain
			}
			n.extents[keep] = e
			keep++
		}
		n.extents = compact(n.extents[:keep])
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
	f.index -= len(n.extents)
	for _, e := range n.extents {
		f.freeRun(e.physical, e.count)
	}
	n.extents = nil
}
