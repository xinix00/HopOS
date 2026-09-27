package hopfs

// persist.go — de boom vastleggen, zodat een flip en een koude boot niet meer
// leeg beginnen (24-09; daarvoor was elke kernwissel voor de opslag een
// format: de bibliotheek op de O6N ging er zo twee keer aan onderdoor).
//
// Het begin van het venster is het metagebied: twee snapshot-plekken van elk
// `slot` blokken, om en om beschreven. Een plek is één kopblok plus de boom.
// De kop draagt een generatie en een SHA-256 over kop én boom; bij het laden
// wint de hoogste generatie die heel is. Een half geschreven plek (stroom weg
// midden in een commit) valt daarmee vanzelf af en de vorige boom geldt.
//
// Alleen de boom gaat de schijf op. De allocator (vrije lijst, bump-pointer)
// is er volledig uit af te leiden — alles wat geen extent van een bestand is,
// is vrij — en wordt bij het laden herbouwd. Zo kan hij nooit uit de pas
// lopen met de boom.
//
// Eén regel maakt het veilig bij stroomuitval: een vrijgegeven blok wordt pas
// herbruikbaar ná een commit die het niet meer noemt (pending, dropRun).
// Anders kan de boom op schijf naar een blok wijzen dat intussen van een
// ander bestand is. De data zelf wordt niet gejournald: wie een bestaand blok
// overschrijft en de stroom verliest, heeft half nieuw/half oud — gewone
// bestandssysteem-semantiek.
//
// Het blob (kop + boom) is zelfbeschrijvend: dezelfde bytes kunnen later als
// machine-state elders (S3) bewaard worden zonder ander formaat.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"time"

	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
)

const (
	metaMagic     = "HOPFSv01"
	metaVersion   = 1
	maxSlotBlocks = 16384 // 64 MiB per plek: ruim genoeg voor ~1M kleine bestanden
	hdrHashOff    = 48    // de kop tot hier gaat mee in de hash
	hdrLen        = hdrHashOff + sha256.Size
	maxDepth      = 4096 // tegen een kapotte of vijandige boom die de stack opblaast
)

// Mount is New met geheugen: de hele schijf, boom van de vorige kern terug.
func Mount(disk *nvme.Controller, fresh bool) (*FS, string) {
	return MountRange(disk, 0, disk.Blocks, fresh)
}

// MountRange is NewRange met geheugen. fresh = bewust leeg beginnen (een
// stateless koude boot, of de nvmewipe): beide plekken worden gewist, zodat
// ook een latere flip niets ouds meer vindt. De string is de consoleregel over
// wat er gevonden is.
func MountRange(disk *nvme.Controller, firstLBA, blocks uint64, fresh bool) (*FS, string) {
	return mount(disk, firstLBA, blocks, disk.BlockSize, disk.MaxTransfer, fresh)
}

func mount(disk blockDevice, firstLBA, blocks, diskBlockSize, maxTransfer uint64, fresh bool) (*FS, string) {
	f := newRange(disk, firstLBA, blocks, diskBlockSize, maxTransfer)
	slot := min(uint32(maxSlotBlocks), f.max/64)
	if slot < 2 {
		return f, fmt.Sprintf("window of %d blocks too small to keep the tree - volatile", f.max)
	}
	f.persist, f.slot, f.last = true, slot, -1
	f.next = 2 * slot
	if fresh {
		var zero [BlockSize]byte
		for p := 0; p < 2; p++ {
			if err := f.disk.Write(f.lba(uint32(p)*slot), zero[:]); err != nil {
				return f, fmt.Sprintf("fresh start, but clearing tree slot %d failed: %v", p, err)
			}
		}
		return f, "starting empty, both tree slots cleared"
	}
	best, bestGen, why := -1, uint64(0), ""
	var bestTree []byte
	for p := 0; p < 2; p++ {
		gen, tree, err := f.readSlot(p)
		if err != nil {
			why += fmt.Sprintf(" slot %d: %v;", p, err)
			continue
		}
		if best < 0 || gen > bestGen {
			best, bestGen, bestTree = p, gen, tree
		}
	}
	if best < 0 {
		return f, "no saved tree - starting empty (" + why[1:len(why)-1] + ")"
	}
	if err := f.decode(bestTree); err != nil {
		// De hash klopte, de inhoud niet: een fout van ons, niet van de schijf.
		// Leeg beginnen (decode zet pas iets als alles klopt) — maar de
		// generatie doortellen, zodat de eerste commit de plek met het bewijs
		// niet als "nieuwste" laat staan.
		f.gen = bestGen
		f.last = best
		f.dirty = true
		return f, fmt.Sprintf("saved tree of generation %d is inconsistent (%v) - starting empty", bestGen, err)
	}
	f.gen, f.last = bestGen, best
	used := uint64(0)
	f.walkFiles(f.root, func(n *node) {
		for _, e := range n.extents {
			used += uint64(e.count)
		}
	})
	return f, fmt.Sprintf("tree of generation %d restored from slot %d: %d files and dirs, %d MB in use",
		bestGen, best, f.nodes, used*BlockSize>>20)
}

// Commit legt de boom vast als hij veranderde. Veilig om vaak aan te roepen.
func (f *FS) Commit() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commitLocked()
}

// Freeze legt de boom vast en houdt het bestandssysteem daarna dicht: elke
// volgende bewerking wacht. Voor de flip — alles wat ná het vastleggen nog
// geschreven zou worden, kent de nieuwe kern niet. thaw heft het op, voor
// een flip die daarna alsnog mislukt. gen is de generatie die de nieuwe kern
// zal vinden (niet via Generation: die wacht op de lock die wij nu houden).
func (f *FS) Freeze() (thaw func(), gen uint64, err error) {
	f.mu.Lock()
	if err := f.commitLocked(); err != nil {
		f.mu.Unlock()
		return nil, 0, err
	}
	return f.mu.Unlock, f.gen, nil
}

// CommitEvery legt de boom elke d vast als hij veranderde: de grens van wat
// een harde stroomuitval kost. Draait als eigen goroutine; een fout wordt
// één keer gemeld tot hij verandert.
func (f *FS) CommitEvery(d time.Duration) {
	last := ""
	for {
		time.Sleep(d)
		msg := ""
		if err := f.Commit(); err != nil {
			msg = err.Error()
		}
		if msg != last {
			if msg != "" {
				fmt.Printf("storage: saving the tree failed: %s - retrying every %v\n", msg, d)
			} else {
				fmt.Printf("storage: saving the tree works again (generation %d)\n", f.Generation())
			}
			last = msg
		}
	}
}

// Generation is de generatie van de laatst vastgelegde boom (0 = nog geen).
func (f *FS) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gen
}

// Persistent meldt of deze boom een flip en een boot overleeft.
func (f *FS) Persistent() bool { return f.persist }

// dropRun geeft blokken terug. Met vastleggen pas na de volgende commit: tot
// dan kan de boom op schijf ze nog noemen.
func (f *FS) dropRun(start, count uint32) {
	if !f.persist {
		f.freeRun(start, count)
		return
	}
	if count > 0 {
		f.pending = append(f.pending, diskRange{start, count})
	}
}

func (f *FS) commitLocked() error {
	if !f.persist || (!f.dirty && f.last >= 0) {
		return nil
	}
	tree := f.encode()
	if room := uint64(f.slot-1) * BlockSize; uint64(len(tree)) > room {
		return fmt.Errorf("hopfs: tree is %d bytes, a slot holds %d", len(tree), room)
	}
	// Eerst de data duurzaam, dan pas een boom die ernaar wijst.
	if err := f.flush(); err != nil {
		return fmt.Errorf("hopfs: flush before commit: %w", err)
	}
	target := 0
	if f.last == 0 {
		target = 1
	}
	gen := f.gen + 1
	blob := f.blob(gen, tree)
	// Boom eerst, kop als laatste: een kop die er staat hoort bij een boom die
	// er al stond. (De hash dekt het ook, dit maakt het venster alleen kleiner.)
	start := uint32(target) * f.slot
	if err := f.writeBlocks(start+1, blob[BlockSize:]); err != nil {
		return fmt.Errorf("hopfs: writing tree: %w", err)
	}
	if err := f.writeBlocks(start, blob[:BlockSize]); err != nil {
		return fmt.Errorf("hopfs: writing tree header: %w", err)
	}
	if err := f.flush(); err != nil {
		return fmt.Errorf("hopfs: flush after commit: %w", err)
	}
	f.gen, f.last, f.dirty = gen, target, false
	f.commitAt = time.Now().UnixNano()
	for _, r := range f.pending {
		f.freeRun(r.start, r.count)
	}
	f.pending = nil
	return nil
}

func (f *FS) flush() error {
	if fl, ok := f.disk.(interface{ Flush() error }); ok {
		return fl.Flush()
	}
	return nil
}

// writeBlocks schrijft p (blokveelvoud) vanaf hopfs-blok b, in commando's van
// hoogstens maxIOBlocks.
func (f *FS) writeBlocks(b uint32, p []byte) error {
	step := int(f.maxIOBlocks) * BlockSize
	for off := 0; off < len(p); off += step {
		end := min(off+step, len(p))
		if err := f.disk.Write(f.lba(b+uint32(off/BlockSize)), p[off:end]); err != nil {
			return err
		}
	}
	return nil
}

// blob is kopblok + boom, opgevuld tot hele blokken.
func (f *FS) blob(gen uint64, tree []byte) []byte {
	n := BlockSize + (len(tree)+BlockSize-1)/BlockSize*BlockSize
	b := make([]byte, n)
	copy(b[0:8], metaMagic)
	le := binary.LittleEndian
	le.PutUint32(b[8:], metaVersion)
	le.PutUint32(b[12:], f.slot)
	le.PutUint64(b[16:], gen)
	le.PutUint64(b[24:], f.base)
	le.PutUint32(b[32:], f.max)
	le.PutUint32(b[36:], uint32(f.lbasPerBlock))
	le.PutUint64(b[40:], uint64(len(tree)))
	copy(b[BlockSize:], tree)
	sum := treeSum(b[:hdrHashOff], tree)
	copy(b[hdrHashOff:hdrLen], sum[:])
	return b
}

func treeSum(hdr, tree []byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write(hdr)
	h.Write(tree)
	var s [sha256.Size]byte
	copy(s[:], h.Sum(nil))
	return s
}

// readSlot leest plek p en geeft (generatie, boom) als hij heel is en bij dít
// venster hoort. Een andere geometrie (ander venster, andere blokmaat) is
// geen boom van ons: de extents zouden naar andermans blokken wijzen.
func (f *FS) readSlot(p int) (uint64, []byte, error) {
	var hdr [BlockSize]byte
	start := uint32(p) * f.slot
	if err := f.disk.Read(f.lba(start), hdr[:]); err != nil {
		return 0, nil, err
	}
	if string(hdr[0:8]) != metaMagic {
		return 0, nil, fmt.Errorf("empty")
	}
	le := binary.LittleEndian
	if v := le.Uint32(hdr[8:]); v != metaVersion {
		return 0, nil, fmt.Errorf("version %d", v)
	}
	if le.Uint32(hdr[12:]) != f.slot || le.Uint64(hdr[24:]) != f.base ||
		le.Uint32(hdr[32:]) != f.max || le.Uint32(hdr[36:]) != uint32(f.lbasPerBlock) {
		return 0, nil, fmt.Errorf("written for another window")
	}
	gen, size := le.Uint64(hdr[16:]), le.Uint64(hdr[40:])
	if size > uint64(f.slot-1)*BlockSize {
		return 0, nil, fmt.Errorf("tree size %d does not fit a slot", size)
	}
	tree := make([]byte, (size+BlockSize-1)/BlockSize*BlockSize)
	step := int(f.maxIOBlocks) * BlockSize
	for off := 0; off < len(tree); off += step {
		end := min(off+step, len(tree))
		if err := f.disk.Read(f.lba(start+1+uint32(off/BlockSize)), tree[off:end]); err != nil {
			return 0, nil, err
		}
	}
	tree = tree[:size]
	if sum := treeSum(hdr[:hdrHashOff], tree); !bytes.Equal(sum[:], hdr[hdrHashOff:hdrLen]) {
		return 0, nil, fmt.Errorf("checksum mismatch (torn write)")
	}
	return gen, tree, nil
}

// encode schrijft de boom pre-order weg: per node een soortbyte, dan voor een
// dir het aantal kinderen en per kind naam + node (op naam gesorteerd, dus
// dezelfde boom = dezelfde bytes), voor een bestand de grootte en de extents.
func (f *FS) encode() []byte {
	var b []byte
	le := binary.LittleEndian
	var put func(n *node)
	put = func(n *node) {
		if n.dir {
			b = append(b, 1)
			b = le.AppendUint32(b, uint32(len(n.children)))
			names := make([]string, 0, len(n.children))
			for name := range n.children {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				b = le.AppendUint16(b, uint16(len(name)))
				b = append(b, name...)
				put(n.children[name])
			}
			return
		}
		b = append(b, 0)
		b = le.AppendUint64(b, n.size)
		b = le.AppendUint32(b, uint32(len(n.extents)))
		for _, e := range n.extents {
			b = le.AppendUint32(b, e.logical)
			b = le.AppendUint32(b, e.physical)
			b = le.AppendUint32(b, e.count)
		}
	}
	put(f.root)
	return b
}

// decode bouwt de boom uit encode's bytes, controleert elke grens die de
// levende code ook bewaakt, en herbouwt dan de allocator uit de extents.
func (f *FS) decode(b []byte) error {
	le := binary.LittleEndian
	pos := 0
	take := func(n int) ([]byte, error) {
		if n < 0 || pos+n > len(b) {
			return nil, fmt.Errorf("truncated at byte %d", pos)
		}
		p := b[pos : pos+n]
		pos += n
		return p, nil
	}
	meta := 2 * f.slot
	var runs []diskRange
	nodes, index := 0, 0
	var get func(depth int) (*node, error)
	get = func(depth int) (*node, error) {
		if depth > maxDepth {
			return nil, fmt.Errorf("tree deeper than %d", maxDepth)
		}
		k, err := take(1)
		if err != nil {
			return nil, err
		}
		switch k[0] {
		case 1:
			c, err := take(4)
			if err != nil {
				return nil, err
			}
			n := &node{dir: true, children: map[string]*node{}}
			for i := uint32(0); i < le.Uint32(c); i++ {
				l, err := take(2)
				if err != nil {
					return nil, err
				}
				nb, err := take(int(le.Uint16(l)))
				if err != nil {
					return nil, err
				}
				name := string(nb)
				if name == "" || name == "." || name == ".." || bytes.IndexByte(nb, '/') >= 0 {
					return nil, fmt.Errorf("bad name %q", name)
				}
				if _, dup := n.children[name]; dup {
					return nil, fmt.Errorf("duplicate name %q", name)
				}
				if nodes++; nodes > maxNodes {
					return nil, fmt.Errorf("more than %d nodes", maxNodes)
				}
				child, err := get(depth + 1)
				if err != nil {
					return nil, err
				}
				n.children[name] = child
			}
			return n, nil
		case 0:
			h, err := take(12)
			if err != nil {
				return nil, err
			}
			n := &node{size: le.Uint64(h)}
			cnt := int(le.Uint32(h[8:]))
			if index += cnt; index > maxIndexExtents {
				return nil, fmt.Errorf("more than %d extents", maxIndexExtents)
			}
			if n.size > uint64(f.max)*BlockSize {
				return nil, fmt.Errorf("file of %d bytes beyond the window", n.size)
			}
			need := (n.size + BlockSize - 1) / BlockSize
			prevEnd := uint64(0)
			for i := 0; i < cnt; i++ {
				r, err := take(12)
				if err != nil {
					return nil, err
				}
				e := extent{le.Uint32(r), le.Uint32(r[4:]), le.Uint32(r[8:])}
				lend := uint64(e.logical) + uint64(e.count)
				pend := uint64(e.physical) + uint64(e.count)
				switch {
				case e.count == 0:
					return nil, fmt.Errorf("empty extent")
				case i > 0 && uint64(e.logical) < prevEnd:
					return nil, fmt.Errorf("extents out of order")
				case lend > need:
					return nil, fmt.Errorf("extent beyond the file size")
				case e.physical < meta || pend > uint64(f.max):
					return nil, fmt.Errorf("extent %d+%d outside the data area", e.physical, e.count)
				}
				prevEnd = lend
				n.extents = append(n.extents, e)
				runs = append(runs, diskRange{e.physical, e.count})
			}
			return n, nil
		}
		return nil, fmt.Errorf("unknown node kind %d", k[0])
	}
	root, err := get(0)
	if err != nil {
		return err
	}
	if !root.dir {
		return fmt.Errorf("root is not a directory")
	}
	if pos != len(b) {
		return fmt.Errorf("%d trailing bytes", len(b)-pos)
	}
	// De allocator: wat geen extent is, is vrij. Twee bestanden op hetzelfde
	// blok is een kapotte boom, geen tweede eigenaar.
	sort.Slice(runs, func(i, j int) bool { return runs[i].start < runs[j].start })
	var free []diskRange
	next := meta
	for _, r := range runs {
		if r.start < next {
			return fmt.Errorf("block %d owned twice", r.start)
		}
		if r.start > next {
			free = append(free, diskRange{next, r.start - next})
		}
		next = r.start + r.count
	}
	f.root, f.nodes, f.index, f.free, f.next = root, nodes, index, free, next
	f.dirty, f.pending = false, nil
	return nil
}

// walkFiles bezoekt elk bestand in de boom.
func (f *FS) walkFiles(n *node, fn func(*node)) {
	if !n.dir {
		fn(n)
		return
	}
	for _, c := range n.children {
		f.walkFiles(c, fn)
	}
}
