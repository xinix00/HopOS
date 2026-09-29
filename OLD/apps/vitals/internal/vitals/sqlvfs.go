package vitals

// De SQLite-VFS van vitals: SQLite's bestand-I/O op het volume-pad van een
// HopOS-app (system calls over het slot-LAN naar hopfs). Overgenomen uit Spin
// en teruggebracht tot de FS-interface van disk.go, zodat de sqlite-test op de
// host tegen een nep-FS draait en de VFS zelf een test heeft.
//
// Twee dingen bepalen de prijs: elke pagina die SQLite leest is één system
// call, en elke write is er ook één. Schrijven wordt daarom samengevoegd tot
// runs van één ABI-chunk (writeCoalescer); voor lezen moet SQLite zelf zuinig
// zijn: 64 KiB-pagina's, een ruime page-cache en géén WITHOUT ROWID-tabel met
// grote rijen (zie sqlite.go). De VFS telt zijn calls, zodat de test kan
// zeggen hoeveel system calls een MiB kostte.

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync/atomic"

	"github.com/ncruces/go-sqlite3/vfs"
)

// sqlIOChunk is de ABI-grens per call (applib.MaxIOChunk), hier als constante
// omdat dit pakket host-buildbaar blijft.
const sqlIOChunk = 1 << 20

// sqlVFS is één geregistreerde VFS over één FS.
type sqlVFS struct {
	name  string
	fs    FS
	temp  atomic.Uint64
	calls sqlCalls
}

// sqlCalls telt wat SQLite aan de bestandslaag vraagt.
type sqlCalls struct {
	reads, writes, other  atomic.Int64
	readBytes, writeBytes atomic.Int64
	syncs                 atomic.Int64 // SQLite's duurzaamheidsgrens, apart geteld
}

func (c *sqlCalls) reset() {
	c.reads.Store(0)
	c.writes.Store(0)
	c.other.Store(0)
	c.syncs.Store(0)
	c.readBytes.Store(0)
	c.writeBytes.Store(0)
}

func (c *sqlCalls) String() string {
	return fmt.Sprintf("%d reads (%d KiB), %d writes (%d KiB), %d other",
		c.reads.Load(), c.readBytes.Load()>>10, c.writes.Load(), c.writeBytes.Load()>>10, c.other.Load())
}

var sqlVFSSeq atomic.Uint64

// newSQLVFS registreert een VFS over fs onder een unieke naam en geeft hem
// terug; de naam gaat in de DSN (?vfs=...).
func newSQLVFS(fs FS) *sqlVFS {
	v := &sqlVFS{name: fmt.Sprintf("hop%d", sqlVFSSeq.Add(1)), fs: fs}
	vfs.Register(v.name, v)
	return v
}

func (v *sqlVFS) Open(name string, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	if name == "" {
		name = fmt.Sprintf("/.vitals-sqlite-temp-%d", v.temp.Add(1))
		flags |= vfs.OPEN_DELETEONCLOSE
	}
	name, err := cleanSQLPath(name)
	if err != nil {
		return nil, flags, err
	}
	v.calls.other.Add(1)
	if _, err := v.fs.Stat(name); err != nil {
		if flags&vfs.OPEN_CREATE == 0 {
			return nil, flags, err
		}
		v.calls.other.Add(1)
		if err := v.fs.Truncate(name, 0); err != nil {
			return nil, flags, err
		}
	}
	f := &sqlFile{vfs: v, path: name, deleteOnClose: flags&vfs.OPEN_DELETEONCLOSE != 0}
	f.writes = newWriteCoalescer(sqlIOChunk, func(offset int64, data []byte) error {
		v.calls.writes.Add(1)
		v.calls.writeBytes.Add(int64(len(data)))
		_, err := v.fs.WriteAt(name, uint64(offset), data)
		return err
	})
	return f, flags, nil
}

func (v *sqlVFS) Delete(name string, _ bool) error {
	name, err := cleanSQLPath(name)
	if err != nil {
		return err
	}
	v.calls.other.Add(1)
	return v.fs.Remove(name)
}

func (v *sqlVFS) Access(name string, _ vfs.AccessFlag) (bool, error) {
	name, err := cleanSQLPath(name)
	if err != nil {
		return false, err
	}
	v.calls.other.Add(1)
	_, err = v.fs.Stat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (*sqlVFS) FullPathname(name string) (string, error) { return cleanSQLPath(name) }

// sqlFile is één SQLite-bestand. Writes worden samengevoegd tot runs van één
// ABI-chunk en bereiken de opslag bij Sync, bij unlock, vóór een read of
// size-vraag die ze zou zien, en bij Close.
type sqlFile struct {
	vfs           *sqlVFS
	path          string
	deleteOnClose bool
	lock          vfs.LockLevel
	writes        *writeCoalescer
}

func (f *sqlFile) Close() error {
	err := f.writes.Flush()
	if f.deleteOnClose {
		f.vfs.calls.other.Add(1)
		err = errors.Join(err, f.vfs.fs.Remove(f.path))
	}
	return err
}

func (f *sqlFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative SQLite read offset")
	}
	if f.writes.Overlaps(off, int64(len(p))) {
		if err := f.writes.Flush(); err != nil {
			return 0, err
		}
	}
	total := 0
	for total < len(p) {
		n := min(len(p)-total, sqlIOChunk)
		f.vfs.calls.reads.Add(1)
		read, err := f.vfs.fs.ReadInto(f.path, uint64(off)+uint64(total), p[total:total+n])
		f.vfs.calls.readBytes.Add(int64(read))
		total += read
		if err != nil {
			return total, err
		}
		if read < n {
			return total, io.EOF
		}
	}
	return total, nil
}

func (f *sqlFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative SQLite write offset")
	}
	if err := f.writes.Write(off, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *sqlFile) Truncate(size int64) error {
	if size < 0 {
		return errors.New("negative SQLite truncate size")
	}
	f.writes.Discard(size)
	if err := f.writes.Flush(); err != nil {
		return err
	}
	f.vfs.calls.other.Add(1)
	return f.vfs.fs.Truncate(f.path, uint64(size))
}

// Sync is de duurzaamheidsgrens van SQLite: hier bereiken wachtende writes
// het volume. De ABI zelf kent geen flush; een voltooide write staat op het
// volume.
func (f *sqlFile) Sync(vfs.SyncFlag) error {
	f.vfs.calls.syncs.Add(1)
	return f.writes.Flush()
}

func (f *sqlFile) Size() (int64, error) {
	if err := f.writes.Flush(); err != nil {
		return 0, err
	}
	f.vfs.calls.other.Add(1)
	size, err := f.vfs.fs.Stat(f.path)
	return int64(size), err
}

func (f *sqlFile) Lock(lock vfs.LockLevel) error {
	f.lock = lock
	return nil
}

func (f *sqlFile) Unlock(lock vfs.LockLevel) error {
	if err := f.writes.Flush(); err != nil {
		return err
	}
	f.lock = lock
	return nil
}

func (f *sqlFile) CheckReservedLock() (bool, error) { return f.lock >= vfs.LOCK_RESERVED, nil }

func (*sqlFile) SectorSize() int { return 4096 }

func (*sqlFile) DeviceCharacteristics() vfs.DeviceCharacteristic { return 0 }

func cleanSQLPath(name string) (string, error) {
	name = path.Clean("/" + strings.TrimSpace(name))
	if name == "/" {
		return "", errors.New("SQLite path is empty")
	}
	return name, nil
}

// writeCoalescer voegt opeenvolgende writes samen tot één run van hoogstens
// limit bytes; het bestand beslist wanneer die run naar de opslag moet.
type writeCoalescer struct {
	limit   int
	write   func(offset int64, data []byte) error
	pending []byte
	offset  int64
}

func newWriteCoalescer(limit int, write func(offset int64, data []byte) error) *writeCoalescer {
	if limit <= 0 {
		limit = 1
	}
	return &writeCoalescer{limit: limit, write: write}
}

// Write neemt p op offset op. Sluit p aan op de wachtende run, dan wordt hij
// aangeplakt; anders gaat de run eerst weg. Writes van minstens limit gaan
// meteen door, in stukken van limit.
func (c *writeCoalescer) Write(offset int64, p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if len(c.pending) > 0 && (offset != c.offset+int64(len(c.pending)) || len(c.pending)+len(p) > c.limit) {
		if err := c.Flush(); err != nil {
			return err
		}
	}
	if len(c.pending) == 0 && len(p) >= c.limit {
		for len(p) > 0 {
			n := min(len(p), c.limit)
			if err := c.write(offset, p[:n]); err != nil {
				return err
			}
			offset += int64(n)
			p = p[n:]
		}
		return nil
	}
	if len(c.pending) == 0 {
		c.offset = offset
		if cap(c.pending) < c.limit {
			c.pending = make([]byte, 0, c.limit)
		}
	}
	c.pending = append(c.pending, p...)
	return nil
}

// Flush stuurt de wachtende run naar de opslag.
func (c *writeCoalescer) Flush() error {
	if len(c.pending) == 0 {
		return nil
	}
	err := c.write(c.offset, c.pending)
	c.pending = c.pending[:0]
	return err
}

// Overlaps zegt of een read van length op offset wachtende bytes zou raken.
func (c *writeCoalescer) Overlaps(offset, length int64) bool {
	if len(c.pending) == 0 || length <= 0 {
		return false
	}
	return offset < c.offset+int64(len(c.pending)) && offset+length > c.offset
}

// Discard laat wachtende bytes op of voorbij size vallen; een truncate maakt
// ze zinloos.
func (c *writeCoalescer) Discard(size int64) {
	if len(c.pending) == 0 {
		return
	}
	if size <= c.offset {
		c.pending = c.pending[:0]
		return
	}
	if keep := size - c.offset; keep < int64(len(c.pending)) {
		c.pending = c.pending[:keep]
	}
}
