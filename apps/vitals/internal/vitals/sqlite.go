package vitals

// De databasetest: SQLite (ncruces/go-sqlite3, wasm2go — dezelfde build als
// Spin) op het volume-pad van de app, met de inrichting die op HopOS werkt:
//
//	rijen in een rowid-tabel   de blob staat in de rij, alleen de sleutel in
//	                           de index; een WITHOUT ROWID-tabel zet het hele
//	                           record in de b-tree-sleutel en laat SQLite bij
//	                           elke afdaling de buurrijen — mét hun overflow-
//	                           ketens — ophalen (gemeten: ~10 MiB lezen per
//	                           MiB schrijven, groeiend met de tabel)
//	page_size 64 KiB           1 MiB = 17 pagina's i.p.v. 257: 17 system calls
//	                           per MiB bij koud lezen of een cascade-delete
//	cache_size 64 MiB          elke pagina die SQLite leest is een system call;
//	                           een werkset die past wordt één keer gelezen. De
//	                           cache leeft in wasm-geheugen bínnen de Go-heap,
//	                           dus het slot moet hem dragen: hier hoogstens een
//	                           kwart van het slotgeheugen
//	journal DELETE, sync FULL  het journal is klein; Sync is in de VFS een flush
//
// Twee inrichtingen naast elkaar, want ze winnen op verschillende dingen:
// journal DELETE schrijft elke pagina twee keer (journal, dan database), WAL
// appendt één keer en checkpoint later. Voor kleine transacties scheelt WAL
// system calls, voor grote blobs betaalt hij de checkpoint alsnog. WAL vraagt
// normaal gedeeld geheugen voor zijn index; met locking_mode=EXCLUSIVE houdt
// SQLite die in zijn eigen heap, en dat mag hier omdat een slot één proces is.
//
// Per MiB één rij; ?batch= zet er zoveel in één transactie (default 1, het
// upload-patroon: elke chunk apart bevestigd). Dan alles koud terug — de
// database wordt eerst dichtgedaan, dus de page-cache is leeg en elke pagina
// is een system call — met inhoudscontrole, dan de cascade-delete. ?mb= kiest
// de omvang (default 32). ?trap=1 doet 8 MiB nog eens met de valkuil — WITHOUT
// ROWID, 4 KiB-pagina's, standaardcache — zodat het verschil op de pagina
// staat; dát was de inrichting die een database van 2,5 GB op 5 MB/s hield.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"
)

// sqlSetup is één inrichting van de database. Het onderscheid tussen dsn en
// pragmas is niet cosmetisch: alles wat per VERBINDING geldt (cache_size,
// locking_mode) hoort in de DSN, want een tweede verbinding krijgt de
// statements uit de eerste niet. Gemeten toen dit nog niet klopte: na
// heropenen viel de cache terug op de standaard 2 MiB en zakten puntlookups
// van tienduizenden naar 8358/s, en een WAL-database ging helemaal niet meer
// open ("unable to open database file") omdat locking_mode weg was en WAL dan
// om gedeeld geheugen vraagt dat deze VFS niet heeft.
type sqlSetup struct {
	name    string
	path    string
	dsn     []string // per verbinding, als _pragma= in de DSN
	pragmas []string // eenmalig bij het aanmaken (page_size, journal_mode)
	schema  string
}

const sqlSchemaRowid = `CREATE TABLE chunks (
	id INTEGER PRIMARY KEY,
	object_id INTEGER NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL,
	data BLOB NOT NULL,
	UNIQUE(object_id, sequence))`

const sqlSchemaTrap = `CREATE TABLE chunks (
	object_id INTEGER NOT NULL REFERENCES objects(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL,
	data BLOB NOT NULL,
	PRIMARY KEY(object_id, sequence)) WITHOUT ROWID`

var (
	sqlSetupWAL = sqlSetup{
		name: "rowid table, 64 KiB pages, 64 MiB cache, WAL",
		path: "/vitals-sqlite-wal.db",
		// locking_mode staat in de DSN en moet er de EERSTE zijn: staat er iets
		// vóór dat de database aanraakt (foreign_keys al), dan opent SQLite hem
		// nog in gedeeld-geheugenmodus en faalt WAL met "unable to open
		// database file". Zo geldt exclusive vanaf het openen, en dat is de
		// voorwaarde waaronder WAL zonder gedeeld geheugen werkt.
		// synchronous hoort óók in de DSN: hij is niet persistent, en SQLite
		// heeft voor WAL een ándere standaard (NORMAL) dan daarbuiten (FULL).
		// Zonder dit meet je WAL stilletjes op een lagere duurzaamheid dan het
		// journal waarmee je hem vergelijkt.
		dsn:     []string{"locking_mode(exclusive)", "cache_size(-65536)", "synchronous(full)", "foreign_keys(1)"},
		pragmas: []string{`PRAGMA page_size=65536`, `PRAGMA journal_mode=WAL`},
		schema:  sqlSchemaRowid,
	}
	// WAL met synchronous=NORMAL: de WAL wordt alleen bij een checkpoint
	// gesynct. Een stroomstoring kost dan de laatste transacties, maar
	// beschadigt de database niet — een andere afspraak, geen gratis winst.
	sqlSetupWALNormal = sqlSetup{
		name:    "rowid table, 64 KiB pages, 64 MiB cache, WAL + synchronous NORMAL",
		path:    "/vitals-sqlite-waln.db",
		dsn:     []string{"locking_mode(exclusive)", "cache_size(-65536)", "synchronous(normal)", "foreign_keys(1)"},
		pragmas: []string{`PRAGMA page_size=65536`, `PRAGMA journal_mode=WAL`},
		schema:  sqlSchemaRowid,
	}
	sqlSetupRowid = sqlSetup{
		name:    "rowid table, 64 KiB pages, 64 MiB cache",
		path:    "/vitals-sqlite.db",
		dsn:     []string{"cache_size(-65536)", "synchronous(full)", "foreign_keys(1)"},
		pragmas: []string{`PRAGMA page_size=65536`, `PRAGMA journal_mode=DELETE`},
		schema:  sqlSchemaRowid,
	}
	// Journal DELETE, maar mét een exclusief slot. De vraag die dit beantwoordt:
	// komt de leeswinst van WAL, of alleen van het feit dat een verbinding die
	// zijn slot vasthoudt zijn page-cache tussen statements mag houden? In
	// rollback-modus gooit SQLite die cache namelijk weg zodra hij het slot
	// loslaat, want een andere schrijver kan er dan geweest zijn.
	sqlSetupExcl = sqlSetup{
		name:    "rowid table, 64 KiB pages, 64 MiB cache, DELETE + exclusive lock",
		path:    "/vitals-sqlite-excl.db",
		dsn:     []string{"locking_mode(exclusive)", "cache_size(-65536)", "synchronous(full)", "foreign_keys(1)"},
		pragmas: []string{`PRAGMA page_size=65536`, `PRAGMA journal_mode=DELETE`},
		schema:  sqlSchemaRowid,
	}
	sqlSetupTrap = sqlSetup{
		name:    "WITHOUT ROWID, 4 KiB pages, default cache (the trap)",
		path:    "/vitals-sqlite-trap.db",
		dsn:     []string{"foreign_keys(1)"},
		pragmas: []string{`PRAGMA journal_mode=DELETE`, `PRAGMA synchronous=FULL`},
		schema:  sqlSchemaTrap,
	}
)

// sqlOutcome is de meting van één inrichting.
type sqlOutcome struct {
	mb                     int
	journal                string
	insertMBs, readMBs     float64
	insertP50, insertP99   float64 // ms
	deleteMs               float64
	insertCalls, readCalls int64
	deleteCalls            int64

	// De transactiemaat van een gewone app: kleine rijen.
	commitsPerSec  float64 // elke rij zijn eigen commit
	batchedPerSec  float64 // dezelfde rijen in één transactie
	lookupsPerSec  float64
	callsPerCommit float64
	syncsPerCommit float64
	synchronous    string
}

func (s *Server) runSQLite(res *Result, q url.Values) {
	if s.cfg.FS == nil {
		res.Err = "no file layer (not running as a HopOS app)"
		return
	}
	mb := qInt(q, "mb", 32, 1, 512)
	batch := qInt(q, "batch", 1, 1, 16)
	v := s.sqlVFS()

	rows := qInt(q, "rows", 2000, 10, 100000)
	cache := int64(64 << 20)
	if ram := int64(s.cfg.RAMSize); ram > 0 && ram/4 < cache {
		cache = ram / 4 // de page-cache leeft in de heap van het slot
	}
	setups := []sqlSetup{sqlSetupRowid, sqlSetupExcl, sqlSetupWAL, sqlSetupWALNormal}
	if only := q.Get("only"); only != "" {
		setups = nil
		for _, s := range setups2() {
			if strings.Contains(s.path, only) {
				setups = append(setups, s)
			}
		}
		if len(setups) == 0 {
			res.Err = "?only= must match part of a setup's file name: sqlite, wal, waln"
			return
		}
	}
	for k := range setups {
		setups[k].dsn = withCache(setups[k].dsn, cache)
	}

	for k, setup := range setups {
		out, err := runSQLSetup(v, setup, mb, batch, rows, s.setNote)
		if err != nil {
			res.Err = err.Error()
			return
		}
		prefix := []string{"", "excl ", "wal ", "wal-normal "}[min(k, 3)]
		res.add(prefix+"insert", out.insertMBs, "MB/s")
		res.add(prefix+"commits/s", out.commitsPerSec, "")
		if k == 0 {
			res.add("read", out.readMBs, "MB/s")
			res.add("calls/MiB insert", float64(out.insertCalls)/float64(mb), "")
		}
		res.linef("%s (journal %s, synchronous %s), %d MiB in rows of 1 MiB, %d per transaction:", setup.name, out.journal, out.synchronous, mb, batch)
		res.linef("  bulk: insert %.0f MB/s (p50 %.1f ms per row, %.0f calls per MiB); cold read back %.0f MB/s (%.0f calls per MiB); cascade delete %.0f ms",
			out.insertMBs, out.insertP50, float64(out.insertCalls)/float64(mb),
			out.readMBs, float64(out.readCalls)/float64(mb), out.deleteMs)
		res.linef("  small rows: %.0f commits/s each in its own transaction (%.1f system calls and %.1f syncs per commit), %.0f rows/s batched in one, %.0f point lookups/s",
			out.commitsPerSec, out.callsPerCommit, out.syncsPerCommit, out.batchedPerSec, out.lookupsPerSec)
	}
	res.linef("path: SQLite (wasm2go) → VFS → system calls → hopfs → NVMe; compare insert with the disk test's 1 MiB writes, everything below it is SQLite + the VFS")

	if q.Get("trap") == "1" {
		trapMB := min(mb, 8)
		trap, err := runSQLSetup(v, sqlSetupTrap, trapMB, batch, 0, s.setNote)
		if err != nil {
			res.Err = "trap: " + err.Error()
			return
		}
		res.add("trap insert", trap.insertMBs, "MB/s")
		res.linef("%s: %d rows — insert %.0f MB/s (%.0f calls per MiB), read %.0f MB/s, delete %.0f ms; the blob sits in the index key, so every b-tree descent reads the neighbouring rows' overflow chains",
			sqlSetupTrap.name, trapMB, trap.insertMBs, float64(trap.insertCalls)/float64(trapMB), trap.readMBs, trap.deleteMs)
	}
}

// sqlVFS registreert de VFS over de FS van de server, eenmalig.
func (s *Server) sqlVFS() *sqlVFS {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sqlvfs == nil {
		s.sqlvfs = newSQLVFS(s.cfg.FS)
	}
	return s.sqlvfs
}

// runSQLSetup draait de drie fasen op één inrichting en ruimt de database op.
// withCache zet de page-cache op n bytes; de cache leeft in wasm-geheugen
// binnen de Go-heap van het slot, dus hij moet in het slot passen.
// setups2 is de volle lijst voor ?only=.
func setups2() []sqlSetup {
	return []sqlSetup{sqlSetupRowid, sqlSetupExcl, sqlSetupWAL, sqlSetupWALNormal}
}

func withCache(dsn []string, n int64) []string {
	out := append([]string{}, dsn...)
	for i, p := range out {
		if strings.HasPrefix(p, "cache_size") {
			out[i] = fmt.Sprintf("cache_size(-%d)", n>>10)
		}
	}
	return out
}

func runSQLSetup(v *sqlVFS, setup sqlSetup, mb, batch, smallN int, note func(string, ...any)) (out sqlOutcome, err error) {
	out.mb = mb
	ctx := context.Background()
	defer v.fs.Remove(setup.path)
	_ = v.fs.Remove(setup.path) // een vorige, afgebroken run
	// Géén nolock=1: die vlag laat SQLite het locking-protocol overslaan en
	// dán weigert hij WAL (gemeten: journal_mode blijft "delete"). Onze
	// Lock/Unlock is in-process boekhouding, wat klopt zolang één verbinding
	// de database heeft — precies wat een slot doet.
	dsn := "file:" + setup.path + "?vfs=" + v.name
	for _, p := range setup.dsn {
		dsn += "&_pragma=" + p
	}
	open := func() (*sql.DB, error) {
		db, err := sql.Open("sqlite3", dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite open: %w", err)
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		return db, nil
	}
	db, err := open()
	if err != nil {
		return out, err
	}
	defer func() { db.Close() }()
	statements := append(append([]string{}, setup.pragmas...),
		`CREATE TABLE objects (id INTEGER PRIMARY KEY, kind TEXT NOT NULL)`, setup.schema)
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return out, fmt.Errorf("sqlite %.24s: %w", statement, err)
		}
	}
	result, err := db.ExecContext(ctx, `INSERT INTO objects(kind) VALUES('vitals')`)
	if err != nil {
		return out, fmt.Errorf("sqlite object: %w", err)
	}
	objectID, _ := result.LastInsertId()

	// De rij: één keer gevuld, per rij alleen een stempel vooraan en achteraan
	// — het vullen zelf hoort niet in de klok (1 MiB per rij kost op een
	// E-core ~1 ms, zoveel als de insert zelf).
	row := make([]byte, 1<<20)
	for i := range row {
		row[i] = byte(i * 7)
	}
	stamp := func(seq int) []byte {
		row[0], row[len(row)-1] = byte(seq), byte(seq>>8)
		return row
	}

	// Schrijven: batch rijen per transactie; 1 = elke chunk apart, zoals een
	// upload die per chunk bevestigt.
	v.calls.reset()
	lat := make([]float64, 0, mb)
	t0 := time.Now()
	for seq := 0; seq < mb; seq += batch {
		t := time.Now()
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return out, fmt.Errorf("sqlite begin: %w", err)
		}
		for k := seq; k < seq+batch && k < mb; k++ {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO chunks(object_id, sequence, data) VALUES(?, ?, ?)`, objectID, k, stamp(k)); err != nil {
				tx.Rollback()
				return out, fmt.Errorf("sqlite insert %d: %w", k, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return out, fmt.Errorf("sqlite commit: %w", err)
		}
		lat = append(lat, time.Since(t).Seconds()*1e3/float64(min(batch, mb-seq)))
		if seq%8 == 0 {
			note("sqlite %s: insert %d/%d MiB", setup.name, seq, mb)
		}
	}
	out.insertMBs = float64(mb<<20) / time.Since(t0).Seconds() / 1e6
	out.insertP50, out.insertP99 = pct(lat, 50), pct(lat, 99)
	out.insertCalls = v.calls.reads.Load() + v.calls.writes.Load() + v.calls.other.Load()

	// Koud teruglezen: dicht en opnieuw open, dus zonder page-cache — elke
	// pagina is dan een system call. Inhoud vergeleken.
	note("sqlite %s: read back", setup.name)
	if err := db.Close(); err != nil {
		return out, fmt.Errorf("sqlite close: %w", err)
	}
	if db, err = open(); err != nil {
		return out, err
	}
	v.calls.reset()
	t1 := time.Now()
	rows, err := db.QueryContext(ctx, `SELECT sequence, data FROM chunks WHERE object_id = ? ORDER BY sequence`, objectID)
	if err != nil {
		return out, fmt.Errorf("sqlite select: %w", err)
	}
	seen := 0
	for rows.Next() {
		var seq int
		var data []byte
		if err := rows.Scan(&seq, &data); err != nil {
			rows.Close()
			return out, fmt.Errorf("sqlite scan: %w", err)
		}
		if seq != seen || !bytes.Equal(data, stamp(seq)) {
			rows.Close()
			return out, fmt.Errorf("sqlite read back: row %d is wrong (sequence %d, %d bytes)", seen, seq, len(data))
		}
		seen++
	}
	rows.Close()
	if seen != mb {
		return out, fmt.Errorf("sqlite read back: %d rows, want %d", seen, mb)
	}
	out.readMBs = float64(mb<<20) / time.Since(t1).Seconds() / 1e6
	out.readCalls = v.calls.reads.Load() + v.calls.writes.Load() + v.calls.other.Load()

	// Weg: één DELETE op het object, de rijen gaan via de cascade.
	note("sqlite %s: delete", setup.name)
	v.calls.reset()
	t2 := time.Now()
	if _, err := db.ExecContext(ctx, `DELETE FROM objects WHERE id = ?`, objectID); err != nil {
		return out, fmt.Errorf("sqlite delete: %w", err)
	}
	out.deleteMs = time.Since(t2).Seconds() * 1e3
	out.deleteCalls = v.calls.reads.Load() + v.calls.writes.Load() + v.calls.other.Load()

	_ = db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&out.journal)
	_ = db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&out.synchronous)
	if smallN > 0 {
		if err := smallRows(ctx, db, v, smallN, &out, setup.name, note); err != nil {
			return out, err
		}
	}
	return out, nil
}

// smallRows meet de transactiemaat van een gewone app: rijen van een paar
// honderd bytes, elk in zijn eigen commit. Dat is het dure geval, want een
// commit kost een handvol system calls van elk ~26 µs — véél meer dan de
// schijf eronder. Daarna dezelfde rijen in één transactie (de bovengrens) en
// puntlookups (bijna alles uit de page-cache).
func smallRows(ctx context.Context, db *sql.DB, v *sqlVFS, rows int, out *sqlOutcome, label string, note func(string, ...any)) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE small (id INTEGER PRIMARY KEY, k TEXT UNIQUE, v INTEGER, payload BLOB)`); err != nil {
		return fmt.Errorf("sqlite small table: %w", err)
	}
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i)
	}
	key := func(i int) string { return fmt.Sprintf("key-%07d", i) }

	note("sqlite %s: %d single-row commits", label, rows)
	v.calls.reset()
	t := time.Now()
	for i := 0; i < rows; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO small(k, v, payload) VALUES(?, ?, ?)`, key(i), i, payload); err != nil {
			return fmt.Errorf("sqlite small insert %d: %w", i, err)
		}
	}
	elapsed := time.Since(t).Seconds()
	calls := v.calls.reads.Load() + v.calls.writes.Load() + v.calls.other.Load()
	out.commitsPerSec = float64(rows) / elapsed
	out.callsPerCommit = float64(calls) / float64(rows)
	out.syncsPerCommit = float64(v.calls.syncs.Load()) / float64(rows)

	note("sqlite %s: %d rows in one transaction", label, rows)
	t = time.Now()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite small begin: %w", err)
	}
	for i := rows; i < 2*rows; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO small(k, v, payload) VALUES(?, ?, ?)`, key(i), i, payload); err != nil {
			tx.Rollback()
			return fmt.Errorf("sqlite batched insert %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite small commit: %w", err)
	}
	out.batchedPerSec = float64(rows) / time.Since(t).Seconds()

	note("sqlite %s: %d point lookups", label, rows)
	t = time.Now()
	for i := 0; i < rows; i++ {
		var value int
		if err := db.QueryRowContext(ctx, `SELECT v FROM small WHERE k = ?`, key(i)).Scan(&value); err != nil {
			return fmt.Errorf("sqlite lookup %d: %w", i, err)
		}
		if value != i {
			return fmt.Errorf("sqlite lookup %d returned %d", i, value)
		}
	}
	out.lookupsPerSec = float64(rows) / time.Since(t).Seconds()
	return nil
}
