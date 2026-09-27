package optical

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"
	"time"
)

type deadlineDrive struct {
	*fakeDrive
	deadlines []time.Time
}

func (f *deadlineDrive) SetCommandDeadline(d time.Time) { f.deadlines = append(f.deadlines, d) }

func TestExecuteCommandPreservesRawSenseAndDeadline(t *testing.T) {
	f := &deadlineDrive{fakeDrive: newFake()}
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	f.failNext, f.sense = true, Sense{Key: 5, ASC: 0x24, ASCQ: 1}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	n, status, sense, err := d.ExecuteCommand(ctx, []byte{opInquiry, 0, 0, 0, 36, 0}, nil, make([]byte, 36))
	if err != nil || n != 0 || status != 2 || len(sense) != 18 || sense[0] != 0x70 || sense[2] != 5 || sense[12] != 0x24 || sense[13] != 1 {
		t.Fatalf("raw result: %d %d %x %v", n, status, sense, err)
	}
	if len(f.deadlines) != 2 || f.deadlines[0].IsZero() || !f.deadlines[1].IsZero() {
		t.Fatal("command deadline was not applied and cleared")
	}
	before := f.tag
	cancel()
	if _, _, _, err = d.ExecuteCommand(ctx, []byte{opInquiry}, nil, nil); !errors.Is(err, context.Canceled) || f.tag != before {
		t.Fatal("cancelled command reached USB")
	}
}

func TestExecuteCommandCanCancelWhileQueued(t *testing.T) {
	d, err := Open(newFake())
	if err != nil {
		t.Fatal(err)
	}
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, _, _, err = d.ExecuteCommand(ctx, []byte{opInquiry}, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
}

func TestExecuteBoundsAndDirections(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	before := f.tag
	for _, tc := range []struct{ cdb, out, in []byte }{
		{nil, nil, nil}, {make([]byte, 17), nil, nil},
		{[]byte{opInquiry}, []byte{1}, []byte{1}},
		{[]byte{opInquiry}, nil, make([]byte, f.maxTransfer+1)},
		{[]byte{opSetStreaming}, make([]byte, f.maxTransfer+1), nil},
	} {
		if _, err := d.Execute(tc.cdb, tc.out, tc.in); err == nil {
			t.Fatal("invalid command accepted")
		}
	}
	if f.tag != before {
		t.Fatal("invalid command reached USB")
	}
	buf := make([]byte, 36)
	if n, err := d.Execute([]byte{opInquiry, 0, 0, 0, 36, 0}, nil, buf); err != nil || n != 36 || buf[0] != peripheralOptical {
		t.Fatalf("inquiry: n=%d err=%v", n, err)
	}
	payload := []byte{1, 2, 3, 4}
	if n, err := d.Execute([]byte{opSetStreaming, 0, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0}, payload, nil); err != nil || n != len(payload) || !bytes.Equal(f.streamDesc, payload) {
		t.Fatalf("data out: n=%d err=%v", n, err)
	}
	f.failNext, f.sense = true, Sense{Key: 5, ASC: 0x24}
	if _, err := d.Execute([]byte{opInquiry, 0, 0, 0, 36, 0}, nil, buf); !errors.Is(err, f.sense) {
		t.Fatalf("lost sense: %v", err)
	}
}

func TestExecuteSerializesWithSectorReads(t *testing.T) {
	d, err := Open(newFake())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				buf := make([]byte, 36)
				if _, err := d.Execute([]byte{opInquiry, 0, 0, 0, 36, 0}, nil, buf); err != nil {
					t.Error(err)
					return
				}
				if err := d.ReadSectors(0, 1, make([]byte, SectorSize)); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}

type residueDrive struct {
	*fakeDrive
	residue uint32
}

func (f residueDrive) In(b []byte) (int, error) {
	status := f.phase == 2
	n, err := f.fakeDrive.In(b)
	if status && n == cswLen {
		binary.LittleEndian.PutUint32(b[8:12], f.residue)
	}
	return n, err
}

func TestExecuteUsesDriveTransferResidue(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	d.t = residueDrive{f, 2}
	if n, err := d.Execute([]byte{opSetStreaming}, []byte{1, 2, 3, 4}, nil); n != 2 || err != nil {
		t.Fatalf("short data out: %d %v", n, err)
	}
	d.t = residueDrive{f, 5}
	if _, err := d.Execute([]byte{opSetStreaming}, []byte{1, 2, 3, 4}, nil); err == nil || f.resets != 1 {
		t.Fatal("invalid transfer residue accepted")
	}
}

type failedDataOut struct{ *fakeDrive }

func (f failedDataOut) Out(b []byte) error {
	data := f.phase == 1
	err := f.fakeDrive.Out(b)
	if data {
		return errors.New("data out failed")
	}
	return err
}

func TestExecuteDoesNotHideDataTransportFailure(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	d.t = failedDataOut{f}
	if _, err := d.Execute([]byte{opSetStreaming}, []byte{1, 2, 3, 4}, nil); err == nil {
		t.Fatal("failed data upload reported as success")
	}
	if _, err := d.Profile(); err != nil {
		t.Fatalf("status was not drained: %v", err)
	}
}

// fakeDrive is een optische drive van vijftig regels: hij spreekt bulk-only
// transport en antwoordt op de handvol commando's die deze driver stuurt. Zo
// is het hele gesprek te toetsen zonder USB, zonder ijzer en zonder disc.
type fakeDrive struct {
	profile  uint16
	sectors  uint32
	medium   bool
	spinning int // hoeveel keer hij nog "becoming ready" zegt

	sense    Sense
	failNext bool // de volgende opdracht faalt met de sense hierboven

	maxTransfer int
	resets      int
	reads       []uint32 // de LBA's die hij gelezen heeft, op volgorde

	noStreaming bool   // kent SET STREAMING niet (zoals een oudere drive)
	streamDesc  []byte // de descriptor die SET STREAMING meekreeg
	cdSpeed     uint16 // wat SET CD SPEED vroeg

	cdb    []byte
	length uint32
	tag    uint32
	in     bool
	data   []byte // wat de volgende In moet teruggeven
	stall  bool   // de datafase stalt: zo weigert een echte drive een commando
	status byte
	phase  int // 0 = wacht op commando, 1 = data, 2 = status
}

func newFake() *fakeDrive {
	return &fakeDrive{profile: ProfileBDROM, sectors: 12_219_392, medium: true, maxTransfer: 64 << 10}
}

func (f *fakeDrive) MaxTransfer() int { return f.maxTransfer }

func (f *fakeDrive) ResetRecovery() error {
	f.resets++
	f.phase = 0
	return nil
}

func (f *fakeDrive) Out(data []byte) error {
	if f.phase == 0 {
		if len(data) != cbwLen || binary.LittleEndian.Uint32(data[0:4]) != cbwSignature {
			return errors.New("not a command block wrapper")
		}
		f.tag = binary.LittleEndian.Uint32(data[4:8])
		f.length = binary.LittleEndian.Uint32(data[8:12])
		f.in = data[12]&cbwFlagIn != 0
		f.cdb = append([]byte(nil), data[15:15+data[14]]...)
		f.answer()
		return nil
	}
	if f.phase == 1 {
		// De datafase van een OUT-commando: onthouden wat er gestuurd werd,
		// want dát is bij SET STREAMING het hele commando.
		if f.cdb[0] == opSetStreaming {
			f.streamDesc = append([]byte(nil), data...)
		}
	}
	f.phase = 2
	return nil
}

func (f *fakeDrive) In(buf []byte) (int, error) {
	switch f.phase {
	case 1:
		if f.stall {
			// Een drive die nee zegt stalt de datafase; de transportlaag
			// maakt de endpoint weer vrij en de status komt daarna.
			f.stall, f.phase = false, 2
			return 0, errors.New("endpoint stalled")
		}
		n := copy(buf, f.data)
		f.phase = 2
		return n, nil
	case 2:
		if len(buf) < cswLen {
			return 0, errors.New("status buffer too small")
		}
		csw := make([]byte, cswLen)
		binary.LittleEndian.PutUint32(csw[0:4], cswSignature)
		binary.LittleEndian.PutUint32(csw[4:8], f.tag)
		csw[12] = f.status
		f.phase = 0
		return copy(buf, csw), nil
	}
	return 0, errors.New("nothing to read")
}

// answer speelt de drive: het antwoord op het commando dat net binnenkwam.
func (f *fakeDrive) answer() {
	f.status, f.data, f.phase = cswPassed, nil, 2
	if f.length > 0 {
		f.phase = 1
	}
	if f.failNext && f.cdb[0] != opRequestSense {
		f.failNext, f.status = false, cswFailed
		f.stall = f.length > 0
		return
	}
	switch f.cdb[0] {
	case opTestUnitReady:
		switch {
		case !f.medium:
			f.status, f.sense = cswFailed, Sense{Key: 2, ASC: 0x3A}
		case f.spinning > 0:
			f.spinning--
			f.status, f.sense = cswFailed, Sense{Key: 2, ASC: 0x04, ASCQ: 0x01}
		}
		f.stall = false
	case opRequestSense:
		b := make([]byte, 18)
		b[0], b[2], b[12], b[13] = 0x70, f.sense.Key, f.sense.ASC, f.sense.ASCQ
		f.data = b
	case opInquiry:
		b := make([]byte, 36)
		b[0] = 0x05 // CD/DVD device
		copy(b[8:16], []byte("PIONEER "))
		copy(b[16:32], []byte("BD-RW   BDR-212M"))
		copy(b[32:36], []byte("1.01"))
		f.data = b
	case opGetConfiguration:
		b := make([]byte, 8)
		binary.BigEndian.PutUint16(b[6:8], f.profile)
		f.data = b
	case opReadCapacity10:
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b[0:4], f.sectors-1)
		binary.BigEndian.PutUint32(b[4:8], SectorSize)
		f.data = b
	case opRead10:
		lba := binary.BigEndian.Uint32(f.cdb[2:6])
		count := int(binary.BigEndian.Uint16(f.cdb[7:9]))
		f.reads = append(f.reads, lba)
		b := make([]byte, count*SectorSize)
		for i := 0; i < count; i++ {
			// Elke sector draagt zijn eigen nummer, zodat een test ziet of de
			// juiste bytes op de juiste plek landen.
			binary.BigEndian.PutUint32(b[i*SectorSize:], lba+uint32(i))
			b[i*SectorSize+4] = 0xAA
		}
		f.data = b
	case opSetStreaming:
		if f.noStreaming {
			f.status, f.sense = cswFailed, Sense{Key: 5, ASC: 0x20}
			f.stall = false // een OUT-commando stalt de OUT-pijp niet in deze drive
		}
	case opSetCDSpeed:
		f.cdSpeed = binary.BigEndian.Uint16(f.cdb[2:4])
	case opStartStopUnit:
		f.medium = f.cdb[4]&0x01 != 0
	default:
		f.status, f.sense = cswFailed, Sense{Key: 5, ASC: 0x20}
	}
}

func TestOpenReadsTheDriveIdentity(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if d.Vendor != "PIONEER" || d.Model != "BD-RW   BDR-212M" || d.Rev != "1.01" {
		t.Fatalf("identity = %q / %q / %q", d.Vendor, d.Model, d.Rev)
	}
	if got := d.Name(); got != "PIONEER BD-RW   BDR-212M (1.01)" {
		t.Fatalf("name = %q", got)
	}
}

func TestProfileAndCapacityOfABluRay(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := d.Profile()
	if err != nil {
		t.Fatal(err)
	}
	if profile != ProfileBDROM || ProfileName(profile) != "BD-ROM" {
		t.Fatalf("profile = %#04x (%s)", profile, ProfileName(profile))
	}
	sectors, size, err := d.Capacity()
	if err != nil {
		t.Fatal(err)
	}
	if sectors != 12_219_392 || size != SectorSize {
		t.Fatalf("capacity = %d sectors of %d bytes", sectors, size)
	}
	// 23,3 GB: de maat van een enkellaags BD-ROM, en de reden dat dit getal
	// in een uint32 sectoren telt en niet in bytes.
	if bytes := int64(sectors) * int64(size); bytes>>30 != 23 {
		t.Fatalf("capacity = %d GB", bytes>>30)
	}
}

// Een drive die net een disc kreeg zegt eerst dat hij opspint. Dat is geen
// fout maar een toestand, en Ready hoort erop te wachten.
func TestReadyWaitsWhileTheDriveSpinsUp(t *testing.T) {
	f := newFake()
	f.spinning = 3
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Ready(5 * time.Second); err != nil {
		t.Fatalf("ready = %v", err)
	}
	if f.spinning != 0 {
		t.Fatalf("%d spin-up answers left", f.spinning)
	}
}

// Een lege lade is geen fout van de node: één herkenbare fout, geen wachtlus.
func TestReadyOnAnEmptyTrayIsItsOwnAnswer(t *testing.T) {
	f := newFake()
	f.medium = false
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := d.Ready(2 * time.Second); !errors.Is(err, ErrNoMedium) {
		t.Fatalf("ready on an empty tray = %v, want ErrNoMedium", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("an empty tray should answer at once, not after the wait")
	}
}

func TestReadSectorsLandsWhereItShould(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4*SectorSize)
	if err := d.ReadSectors(1000, 4, buf); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if got := binary.BigEndian.Uint32(buf[i*SectorSize:]); got != uint32(1000+i) {
			t.Fatalf("sector %d carries %d", i, got)
		}
	}
	if len(f.reads) != 1 || f.reads[0] != 1000 {
		t.Fatalf("reads = %v", f.reads)
	}
}

// ReadAt hoeft niet op sectorgrenzen te liggen: dat is precies wat een lezer
// boven ons (een demuxer, een UDF-laag) nodig heeft.
func TestReadAtCrossesSectorBoundaries(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 6)
	// Vier bytes voor het eind van sector 2 beginnen: twee uit sector 2, dan
	// het nummer van sector 3.
	n, err := d.ReadAt(p, 3*SectorSize-2)
	if err != nil || n != len(p) {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	want := []byte{0, 0, 0, 0, 0, 3}
	if !bytes.Equal(p, want) {
		t.Fatalf("bytes = %v, want %v", p, want)
	}
	if len(f.reads) == 0 {
		t.Fatal("nothing was read")
	}
}

// Een groot verzoek wordt geknipt op wat er in één transfer past, en de
// stukken komen op volgorde en aaneengesloten terug.
func TestReadAtSplitsOnTheTransferSize(t *testing.T) {
	f := newFake()
	f.maxTransfer = 4 * SectorSize
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 10*SectorSize)
	if _, err := d.ReadAt(p, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if got := binary.BigEndian.Uint32(p[i*SectorSize:]); got != uint32(i) {
			t.Fatalf("sector %d carries %d", i, got)
		}
	}
	if len(f.reads) != 3 || f.reads[0] != 0 || f.reads[1] != 4 || f.reads[2] != 8 {
		t.Fatalf("reads = %v, want 0, 4, 8", f.reads)
	}
}

// Een drive die nee zegt, zegt waarom in zijn sense. Die reden hoort de
// aanroeper te krijgen, niet een statusnummer.
func TestAFailedCommandCarriesItsSense(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	f.failNext, f.sense = true, Sense{Key: 3, ASC: 0x11, ASCQ: 0x00}
	err = d.ReadSectors(42, 1, make([]byte, SectorSize))
	var s Sense
	if !errors.As(err, &s) {
		t.Fatalf("error = %v, want a Sense", err)
	}
	if s.Key != 3 || s.ASC != 0x11 {
		t.Fatalf("sense = %+v", s)
	}
	if errors.Is(err, ErrNoMedium) {
		t.Fatal("a medium error is not an empty tray")
	}
}

func TestBuildCBWIsTheWrapperTheSpecDescribes(t *testing.T) {
	b := buildCBW(7, 2048, true, []byte{opRead10, 0, 0, 0, 0, 5, 0, 0, 1, 0})
	if len(b) != cbwLen {
		t.Fatalf("wrapper is %d bytes, want %d", len(b), cbwLen)
	}
	if binary.LittleEndian.Uint32(b[0:4]) != cbwSignature {
		t.Fatal("signature")
	}
	if binary.LittleEndian.Uint32(b[4:8]) != 7 || binary.LittleEndian.Uint32(b[8:12]) != 2048 {
		t.Fatal("tag or length")
	}
	if b[12] != cbwFlagIn || b[13] != 0 || b[14] != 10 {
		t.Fatalf("flags %#x, lun %d, cdb length %d", b[12], b[13], b[14])
	}
	if b[15] != opRead10 || b[20] != 5 {
		t.Fatal("the command block did not land in the wrapper")
	}
}

// Een optische drive start op zijn laagste snelheid en blijft daar tot de host
// om meer vraagt. SET STREAMING is de manier waarop MMC-6 dat doet: een venster
// over de hele disc, en een aantal kilobytes per seconde.
func TestSetSpeedAsksForStreaming(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetSpeed(SpeedMax); err != nil {
		t.Fatal(err)
	}
	if len(f.streamDesc) != 28 {
		t.Fatalf("descriptor is %d bytes, want 28", len(f.streamDesc))
	}
	if got := binary.BigEndian.Uint32(f.streamDesc[8:12]); got != f.sectors-1 {
		t.Fatalf("end lba = %d, want %d (the whole disc)", got, f.sectors-1)
	}
	if got := binary.BigEndian.Uint32(f.streamDesc[12:16]); got != SpeedMax {
		t.Fatalf("read size = %d, want %d", got, uint32(SpeedMax))
	}
	if got := binary.BigEndian.Uint32(f.streamDesc[16:20]); got != 1000 {
		t.Fatalf("read time = %d ms, want 1000", got)
	}
	if f.cdSpeed != 0 {
		t.Fatal("the old command was sent as well; the new one already worked")
	}
}

// Kent een drive SET STREAMING niet, dan is dat geen fout maar een reden om
// het oude commando te proberen. Alleen als dat óók faalt hoort de aanroeper
// het te weten.
func TestSetSpeedFallsBackToTheOldCommand(t *testing.T) {
	f := newFake()
	f.noStreaming = true
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetSpeed(SpeedMax); err != nil {
		t.Fatal(err)
	}
	if f.cdSpeed != 0xFFFF {
		t.Fatalf("set cd speed asked for %d, want 0xFFFF", f.cdSpeed)
	}
}

// Een lezer die op sectorgrenzen vraagt hoort zijn bytes rechtstreeks te
// krijgen: geen tussenbuffer, geen kopie. Dat is niet zichtbaar van buiten,
// dus dit toetst wat er wél zichtbaar is — dat het antwoord klopt, in één
// READ(10) per hap.
func TestReadAtAlignedUsesWholeTransfers(t *testing.T) {
	f := newFake()
	d, err := Open(f)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 4*SectorSize)
	n, err := d.ReadAt(p, 100*SectorSize)
	if err != nil || n != len(p) {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	for i := 0; i < 4; i++ {
		if got := binary.BigEndian.Uint32(p[i*SectorSize:]); got != uint32(100+i) {
			t.Fatalf("sector %d carries %d", 100+i, got)
		}
	}
	if len(f.reads) != 1 || f.reads[0] != 100 {
		t.Fatalf("reads = %v, want one read at 100", f.reads)
	}
}
