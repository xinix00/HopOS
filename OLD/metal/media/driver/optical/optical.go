// Package optical praat met een optische drive (Blu-ray, DVD, CD) die als USB
// mass storage aan de node hangt. Twee lagen in één pakket, omdat ze niets
// zonder elkaar betekenen:
//
//   - bulk-only transport (BOT 1.0): commando eruit, data heen of terug,
//     status terug. Drie bulk-transfers per opdracht, altijd in die volgorde;
//   - SCSI/MMC: de commando's zelf, en het lezen van de sense-gegevens die
//     zeggen waarom een drive nee zei.
//
// Wat hier NIET in zit is USB: dit pakket kent alleen een Transport met twee
// pijpen. Dat is geen laagvrees maar de laagregel van deze boom (driver/ mag
// niet van gui/ afhangen, waar de xHCI-controller woont) én het maakt de hele
// commandolaag testbaar zonder ijzer: de tests hieronder draaien tegen een
// drive van vijftig regels.
//
// De doelgebruiker is het Blu-ray-archiefproject: een disc uitlezen, de stream
// naar de decoder, en dat op een node zonder Linux. Daarom is ReadAt een
// io.ReaderAt over sectoren van 2048 bytes: een demuxer of een UDF-lezer kan
// er rechtstreeks op zitten.
package optical

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Transport is wat een USB-apparaat met bulk-only transport levert: twee
// pijpen en een uitweg. Structureel geïmplementeerd door usbin.Bulk (dat elk
// verzoek naar de goroutine van de bus brengt), zodat dit pakket niets van USB
// hoeft te importeren.
type Transport interface {
	// Out stuurt bytes naar de BULK-OUT endpoint.
	Out(data []byte) error
	// In haalt tot len(buf) bytes van de BULK-IN endpoint; korter mag.
	In(buf []byte) (int, error)
	// ResetRecovery gooit de commandostaat van het apparaat weg en haalt
	// beide endpoints uit halted.
	ResetRecovery() error
	// MaxTransfer is hoeveel bytes er in één keer door een pijp passen.
	MaxTransfer() int
}

// De wrappers van bulk-only transport (BOT 1.0 §5.1 en §5.2).
const (
	cbwSignature = 0x43425355 // "USBC"
	cswSignature = 0x53425355 // "USBS"
	cbwLen       = 31
	cswLen       = 13

	cbwFlagIn = 0x80 // data gaat naar de host

	cswPassed     = 0
	cswFailed     = 1
	cswPhaseError = 2
)

// SCSI/MMC-opcodes. Alleen wat een lezende drive nodig heeft; schrijven
// (branden) hoort bij een ander verhaal en staat hier bewust niet.
const (
	opTestUnitReady    = 0x00
	opRequestSense     = 0x03
	opInquiry          = 0x12
	opStartStopUnit    = 0x1B
	opReadCapacity10   = 0x25
	opRead10           = 0x28
	opGetConfiguration = 0x46
	opSetStreaming     = 0xB6
	opSetCDSpeed       = 0xBB
)

// SectorSize is de sectorgrootte van elke optische disc die wij lezen. CD-ROM
// mode 1, DVD en Blu-ray delen hem; alleen audio-CD wijkt af en dat is geen
// datadrager.
const SectorSize = 2048

// Bekende profielen uit GET CONFIGURATION (MMC-6 tabel 88). Het profiel is het
// eerlijkste antwoord op "wat ligt erin": de drive zelf zegt het, in plaats van
// dat wij het uit de inhoud raden.
const (
	ProfileNone      = 0x0000
	ProfileCDROM     = 0x0008
	ProfileCDR       = 0x0009
	ProfileCDRW      = 0x000A
	ProfileDVDROM    = 0x0010
	ProfileDVDR      = 0x0011
	ProfileDVDRAM    = 0x0012
	ProfileDVDRW     = 0x0014
	ProfileDVDPlusR  = 0x001B
	ProfileDVDPlusRW = 0x001A
	ProfileBDROM     = 0x0040
	ProfileBDR       = 0x0041
	ProfileBDRSeq    = 0x0042
	ProfileBDRE      = 0x0043
)

// ProfileName geeft de naam van een profiel voor de console.
func ProfileName(p uint16) string {
	switch p {
	case ProfileNone:
		return "no medium"
	case ProfileCDROM:
		return "CD-ROM"
	case ProfileCDR:
		return "CD-R"
	case ProfileCDRW:
		return "CD-RW"
	case ProfileDVDROM:
		return "DVD-ROM"
	case ProfileDVDR:
		return "DVD-R"
	case ProfileDVDRAM:
		return "DVD-RAM"
	case ProfileDVDRW:
		return "DVD-RW"
	case ProfileDVDPlusR:
		return "DVD+R"
	case ProfileDVDPlusRW:
		return "DVD+RW"
	case ProfileBDROM:
		return "BD-ROM"
	case ProfileBDR, ProfileBDRSeq:
		return "BD-R"
	case ProfileBDRE:
		return "BD-RE"
	default:
		return fmt.Sprintf("profile %#04x", p)
	}
}

// ErrNotOptical betekent: dit apparaat spreekt wel bulk-only transport, maar
// het is geen optische drive. Een USB-stick meldt zich met precies hetzelfde
// interface-drietal; het verschil staat pas in zijn INQUIRY (peripheral device
// type 0 = schijf, 5 = CD/DVD/BD). Een eigen fout, want een node die van een
// stick boot mag daar verder niet aan zitten.
var ErrNotOptical = errors.New("optical: this is not an optical drive")

// peripheralOptical is het apparaattype van een optische drive in INQUIRY
// byte 0 (SPC-4 tabel 130).
const peripheralOptical = 0x05

// ErrNoMedium betekent: de lade is leeg of nog niet dicht. Eigen fout omdat
// dit de enige "fout" is die geen fout is — een node zonder disc erin hoort
// gewoon door te draaien.
var ErrNoMedium = errors.New("optical: no medium in the drive")

// Sense is wat een drive antwoordt als hij nee zei: de drie getallen die in
// elke MMC-tabel staan.
type Sense struct {
	Key  byte // sense key (2 = not ready, 5 = illegal request, ...)
	ASC  byte // additional sense code
	ASCQ byte // additional sense code qualifier
}

func (s Sense) Error() string {
	return fmt.Sprintf("sense %d/%02x/%02x (%s)", s.Key, s.ASC, s.ASCQ, s.what())
}

// what vertaalt de handvol combinaties die een lezende drive echt geeft. De
// rest komt als nummers terug, want een verzonnen tekst helpt niemand.
func (s Sense) what() string {
	switch {
	case s.Key == 0:
		return "no sense"
	case s.Key == 2 && s.ASC == 0x3A:
		return "no medium"
	case s.Key == 2 && s.ASC == 0x04 && s.ASCQ == 0x01:
		return "becoming ready"
	case s.Key == 2 && s.ASC == 0x04:
		return "not ready"
	case s.Key == 6 && s.ASC == 0x28:
		return "medium changed"
	case s.Key == 6 && s.ASC == 0x29:
		return "device reset"
	case s.Key == 5 && s.ASC == 0x24:
		return "invalid field in command"
	case s.Key == 5 && s.ASC == 0x20:
		return "command not supported"
	case s.Key == 3:
		return "medium error"
	default:
		return "see MMC table"
	}
}

// Drive is één optische drive achter een bulk-only transport.
type Drive struct {
	commandMu sync.Mutex // één volledig BOT-commando, REQUEST SENSE inbegrepen
	t         Transport
	tag       uint32
	Vendor    string
	Model     string
	Rev       string
}

// Execute wisselt één SCSI-commando uit met de drive, met de sense als fout.
// Eén datarichting per commando. Hier wordt nooit herhaald: een
// vendor-commando kan de staat van de drive veranderen ook als zijn antwoord
// verloren gaat.
func (d *Drive) Execute(cdb, toDrive, fromDrive []byte) (int, error) {
	if len(cdb) == 0 || len(cdb) > 16 {
		return 0, errors.New("optical: invalid command length")
	}
	if len(toDrive) != 0 && len(fromDrive) != 0 {
		return 0, errors.New("optical: bidirectional commands are not supported")
	}
	if len(toDrive) > d.t.MaxTransfer() || len(fromDrive) > d.t.MaxTransfer() {
		return 0, errors.New("optical: command data exceeds transport limit")
	}
	if len(fromDrive) != 0 {
		return d.run(cdb, fromDrive, true)
	}
	return d.run(cdb, toDrive, false)
}

// ExecuteCommand is Execute voor de device-command-ABI: de echte SCSI-status
// en de ruwe sense-bytes komen terug in plaats van een Go-fout, zodat de app
// zelf beslist. Een transportfout staat los van CHECK CONDITION (status 2).
// Ook hier geen herhaling. Een transport dat deadlines kent past die van ctx
// toe op elke resterende fase van de BOT-uitwisseling; wachten op een ander
// commando stopt zodra ctx afloopt.
func (d *Drive) ExecuteCommand(ctx context.Context, cdb, toDrive, fromDrive []byte) (int, byte, []byte, error) {
	if len(cdb) == 0 || len(cdb) > 16 || len(toDrive) != 0 && len(fromDrive) != 0 {
		return 0, 0, nil, errors.New("optical: invalid command")
	}
	if len(toDrive) > d.t.MaxTransfer() || len(fromDrive) > d.t.MaxTransfer() {
		return 0, 0, nil, errors.New("optical: command data exceeds transport limit")
	}
	for !d.commandMu.TryLock() {
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, 0, nil, ctx.Err()
		case <-timer.C:
		}
	}
	defer d.commandMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, 0, nil, err
	}
	if t, ok := d.t.(interface{ SetCommandDeadline(time.Time) }); ok {
		deadline, _ := ctx.Deadline()
		t.SetCommandDeadline(deadline)
		defer t.SetCommandDeadline(time.Time{})
	}
	buf, in := toDrive, false
	if len(fromDrive) > 0 {
		buf, in = fromDrive, true
	}
	n, status, err := d.exchange(cdb, buf, in)
	if err != nil {
		return n, 0, nil, err
	}
	if err = ctx.Err(); err != nil {
		return n, 0, nil, err
	}
	switch status {
	case cswPassed:
		return n, 0, nil, nil
	case cswFailed:
		sense, err := d.rawSense()
		if err != nil {
			return n, 0, nil, fmt.Errorf("optical: REQUEST SENSE: %w", err)
		}
		return n, 2, sense, nil
	default:
		if err = d.t.ResetRecovery(); err != nil {
			return n, 0, nil, fmt.Errorf("optical: phase error, reset failed: %w", err)
		}
		return n, 0, nil, errors.New("optical: phase error; the drive was reset")
	}
}

// Open maakt de drive klaar: de commandostaat leeg, en dan vragen wie hij is.
// Een drive zonder disc erin komt hier gewoon uit — dat is geen fout.
func Open(t Transport) (*Drive, error) {
	if t == nil {
		return nil, errors.New("optical: no transport")
	}
	d := &Drive{t: t}
	if err := d.inquiry(); err != nil {
		if errors.Is(err, ErrNotOptical) {
			return nil, err // geen drive: hier houdt het op, zonder reset
		}
		// Eén keer opnieuw, ná een reset: een drive die nog van een vorige
		// boot in het midden van een gesprek staat, antwoordt pas daarna.
		if rerr := t.ResetRecovery(); rerr != nil {
			return nil, fmt.Errorf("%w (reset also failed: %v)", err, rerr)
		}
		if err := d.inquiry(); err != nil {
			return nil, err
		}
	}
	return d, nil
}

// Name is wat er op de drive staat, voor de console.
func (d *Drive) Name() string {
	name := d.Vendor
	if d.Model != "" {
		if name != "" {
			name += " "
		}
		name += d.Model
	}
	if d.Rev != "" {
		name += " (" + d.Rev + ")"
	}
	return name
}

func (d *Drive) inquiry() error {
	buf := make([]byte, 36)
	cdb := []byte{opInquiry, 0, 0, 0, byte(len(buf)), 0}
	n, err := d.in(cdb, buf)
	if err != nil {
		return fmt.Errorf("inquiry: %w", err)
	}
	if n < 36 {
		return fmt.Errorf("inquiry: %d bytes, want 36", n)
	}
	if buf[0]&0x1F != peripheralOptical {
		return fmt.Errorf("%w (peripheral device type %d)", ErrNotOptical, buf[0]&0x1F)
	}
	d.Vendor = trim(buf[8:16])
	d.Model = trim(buf[16:32])
	d.Rev = trim(buf[32:36])
	return nil
}

// Ready wacht tot de drive een disc heeft gelezen. Een drive die net een disc
// kreeg zegt eerst "becoming ready" en heeft seconden nodig om op toeren te
// komen; dat is normaal en geen fout, dus die ene toestand wordt herhaald.
// Ligt er niets in, dan komt ErrNoMedium terug.
func (d *Drive) Ready(wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		err := d.command([]byte{opTestUnitReady, 0, 0, 0, 0, 0})
		if err == nil {
			return nil
		}
		var s Sense
		if !errors.As(err, &s) {
			return err
		}
		switch {
		case s.Key == 2 && s.ASC == 0x3A:
			return ErrNoMedium
		case s.Key == 6, s.Key == 2 && s.ASC == 0x04:
			// Medium changed, device reset of aan het opspinnen: opnieuw
			// vragen tot de termijn om is.
		default:
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("drive not ready within %v: %w", wait, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Profile vraagt de drive wat erin ligt (GET CONFIGURATION, current profile).
func (d *Drive) Profile() (uint16, error) {
	buf := make([]byte, 8)
	// RT = 0 (alle features), starting feature 0: de kop van het antwoord
	// draagt al het huidige profiel, dus meer hoeven we niet op te halen.
	cdb := []byte{opGetConfiguration, 0, 0, 0, 0, 0, 0, 0, byte(len(buf)), 0}
	n, err := d.in(cdb, buf)
	if err != nil {
		return 0, fmt.Errorf("get configuration: %w", err)
	}
	if n < 8 {
		return 0, fmt.Errorf("get configuration: %d bytes, want 8", n)
	}
	return binary.BigEndian.Uint16(buf[6:8]), nil
}

// Capacity geeft het aantal sectoren en de sectorgrootte. READ CAPACITY geeft
// het ADRES van de laatste sector, dus er gaat één bij.
func (d *Drive) Capacity() (sectors uint32, sectorSize uint32, err error) {
	buf := make([]byte, 8)
	cdb := []byte{opReadCapacity10, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	n, err := d.in(cdb, buf)
	if err != nil {
		return 0, 0, fmt.Errorf("read capacity: %w", err)
	}
	if n < 8 {
		return 0, 0, fmt.Errorf("read capacity: %d bytes, want 8", n)
	}
	last := binary.BigEndian.Uint32(buf[0:4])
	size := binary.BigEndian.Uint32(buf[4:8])
	if size == 0 {
		size = SectorSize
	}
	return last + 1, size, nil
}

// ReadSectors leest count sectoren vanaf lba in p. p moet count*SectorSize
// groot zijn. Eén READ(10) per aanroep, dus de aanroeper bepaalt de haplengte;
// MaxSectors zegt wat er in één keer past.
func (d *Drive) ReadSectors(lba uint32, count int, p []byte) error {
	if count <= 0 {
		return nil
	}
	if len(p) < count*SectorSize {
		return fmt.Errorf("optical: buffer holds %d bytes for %d sectors", len(p), count)
	}
	if count > d.MaxSectors() {
		return fmt.Errorf("optical: %d sectors exceed the %d that fit in one transfer", count, d.MaxSectors())
	}
	cdb := []byte{opRead10, 0,
		byte(lba >> 24), byte(lba >> 16), byte(lba >> 8), byte(lba),
		0, byte(count >> 8), byte(count), 0}
	n, err := d.in(cdb, p[:count*SectorSize])
	if err != nil {
		return fmt.Errorf("read %d sectors at %d: %w", count, lba, err)
	}
	if n != count*SectorSize {
		return fmt.Errorf("read %d sectors at %d: %d bytes, want %d", count, lba, n, count*SectorSize)
	}
	return nil
}

// Size is de maat van wat er NU in de drive ligt, in bytes. Elke keer opnieuw
// gevraagd, want een disc wisselt zonder dat iemand het meldt; een lege lade
// geeft ErrNoMedium en geen nul-met-succes.
func (d *Drive) Size() (int64, error) {
	if err := d.Ready(15 * time.Second); err != nil {
		return 0, err
	}
	sectors, size, err := d.Capacity()
	if err != nil {
		return 0, err
	}
	return int64(sectors) * int64(size), nil
}

// MaxSectors is hoeveel sectoren er in één transfer passen.
func (d *Drive) MaxSectors() int {
	n := d.t.MaxTransfer() / SectorSize
	if n < 1 {
		return 1
	}
	return n
}

// SpeedMax vraagt de hoogst mogelijke leessnelheid. Een optische drive komt
// niet vanzelf op toeren: hij start op zijn laagste stand en blijft daar tot
// de host om meer vraagt.
const SpeedMax = 0xFFFF

// SetSpeed vraagt de drive om op kbps kilobyte per seconde te lezen; SpeedMax
// is "zo hard als je kunt".
//
// GEMETEN 22-09 op een BU40N: zonder dit leest hij een BD-ROM op 5,0 MB/s, en
// dat is exact 1x. De drive draait dus op zijn instapstand tot iemand het
// vraagt — en dat vragen kan op twee manieren, want SET CD SPEED is in MMC-6
// verouderd en SET STREAMING is de opvolger. Welke een drive nog aanneemt is
// per model anders, dus we proberen de nieuwe eerst en de oude erna. Nee is
// hier geen fout: een drive die op één vaste snelheid draait mag beide
// weigeren en gewoon blijven lezen.
func (d *Drive) SetSpeed(kbps uint32) error {
	// SET STREAMING (MMC-6 6.43): het venster is de hele disc, en de vraag is
	// "dit aantal kilobytes binnen deze duizend milliseconden". Exact=0 laat
	// de drive naar boven afwijken; RA=0 zegt dat dit streaming is en geen
	// willekeurige toegang, want juist dát is wat een drive laat versnellen.
	end := uint32(0xFFFFFFFF)
	if sectors, _, err := d.Capacity(); err == nil && sectors > 0 {
		end = sectors - 1
	}
	desc := make([]byte, 28)
	be32(desc[4:], 0)     // start-LBA
	be32(desc[8:], end)   // eind-LBA
	be32(desc[12:], kbps) // leesgrootte per venster (KB)
	be32(desc[16:], 1000) // leesvenster (ms)
	be32(desc[20:], kbps) // en hetzelfde voor schrijven: de drive eist het veld
	be32(desc[24:], 1000) //
	cdb := []byte{opSetStreaming, 0, 0, 0, 0, 0, 0, 0, 0,
		byte(len(desc) >> 8), byte(len(desc)), 0}
	if _, err := d.run(cdb, desc, false); err == nil {
		return nil
	}
	// SET CD SPEED (MMC-3 6.37): twee getallen in het commando zelf, geen
	// datafase. Oud, maar bijna elke drive kent het nog.
	speed := kbps
	if speed > 0xFFFF {
		speed = 0xFFFF
	}
	old := []byte{opSetCDSpeed, 0,
		byte(speed >> 8), byte(speed), 0xFF, 0xFF, 0, 0, 0, 0, 0, 0}
	return d.command(old)
}

func be32(p []byte, v uint32) {
	p[0], p[1], p[2], p[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// ReadAt maakt van de drive een io.ReaderAt over de disc. Off en len(p) hoeven
// niet op sectorgrenzen te liggen: wat ernaast valt wordt gelezen en
// weggesneden. Dat kost hooguit één sector extra aan elke kant en het scheelt
// elke lezer boven ons dezelfde rekensom.
//
// Ligt de vraag wél op sectorgrenzen, dan gaat de data rechtstreeks in p. Dat
// scheelt per hap een allocatie én een kopie van tienduizenden bytes, en die
// kopie is bij een drive op volle snelheid niet meer verwaarloosbaar.
func (d *Drive) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("optical: negative offset")
	}
	var scratch []byte
	done := 0
	for done < len(p) {
		lba := uint32((off + int64(done)) / SectorSize)
		skip := int((off + int64(done)) % SectorSize)
		want := len(p) - done + skip
		count := (want + SectorSize - 1) / SectorSize
		if count > d.MaxSectors() {
			count = d.MaxSectors()
		}
		if skip == 0 && (len(p)-done) >= count*SectorSize {
			if err := d.ReadSectors(lba, count, p[done:]); err != nil {
				return done, err
			}
			done += count * SectorSize
			continue
		}
		if len(scratch) < count*SectorSize {
			scratch = make([]byte, count*SectorSize)
		}
		buf := scratch[:count*SectorSize]
		if err := d.ReadSectors(lba, count, buf); err != nil {
			return done, err
		}
		n := copy(p[done:], buf[skip:])
		if n == 0 {
			return done, errors.New("optical: read made no progress")
		}
		done += n
	}
	return done, nil
}

// command voert een opdracht zonder datafase uit.
func (d *Drive) command(cdb []byte) error {
	_, err := d.run(cdb, nil, false)
	return err
}

// in voert een opdracht uit die data teruggeeft.
func (d *Drive) in(cdb []byte, buf []byte) (int, error) {
	return d.run(cdb, buf, true)
}

// run is bulk-only transport in zijn geheel: commando, data, status. Faalt de
// datafase, dan wordt de status alsnog opgehaald — een drive die een commando
// weigert stalt juist die fase en vertelt de reden pas in de status. Zegt de
// status "check condition", dan haalt dit de sense op en geeft die als fout
// terug, zodat de aanroeper een beslissing kan nemen in plaats van een nummer.
func (d *Drive) run(cdb []byte, buf []byte, in bool) (int, error) {
	d.commandMu.Lock()
	defer d.commandMu.Unlock()
	n, status, err := d.exchange(cdb, buf, in)
	if err != nil {
		return 0, err
	}
	switch status {
	case cswPassed:
		return n, nil
	case cswFailed:
		s, serr := d.sense()
		if serr != nil {
			return n, fmt.Errorf("command failed and its sense could not be read: %w", serr)
		}
		return n, s
	default:
		// Phase error: host en drive zijn het spoor bijster. De spec kent
		// hier één uitweg en dat is de reset.
		if err := d.t.ResetRecovery(); err != nil {
			return n, fmt.Errorf("phase error, and the reset failed: %w", err)
		}
		return n, errors.New("optical: phase error; the drive was reset")
	}
}

// sense haalt REQUEST SENSE op. Zonder recursie in run: een sense-opdracht die
// zelf faalt mag geen tweede sense uitlokken.
func (d *Drive) sense() (Sense, error) {
	buf, err := d.rawSense()
	if err != nil {
		return Sense{}, err
	}
	switch buf[0] & 0x7f {
	case 0x70, 0x71:
		if len(buf) < 14 {
			return Sense{}, fmt.Errorf("request sense: %d bytes, want at least 14", len(buf))
		}
		return Sense{Key: buf[2] & 15, ASC: buf[12], ASCQ: buf[13]}, nil
	case 0x72, 0x73:
		return Sense{Key: buf[1] & 15, ASC: buf[2], ASCQ: buf[3]}, nil
	default:
		return Sense{}, errors.New("optical: unknown sense format")
	}
}

func (d *Drive) rawSense() ([]byte, error) {
	buf := make([]byte, 64)
	cdb := []byte{opRequestSense, 0, 0, 0, byte(len(buf)), 0}
	n, status, err := d.exchange(cdb, buf, true)
	if err != nil {
		return nil, err
	}
	if status != cswPassed {
		return nil, fmt.Errorf("request sense returned status %d", status)
	}
	if n < 8 {
		return nil, fmt.Errorf("request sense: %d bytes, want at least 8", n)
	}
	return buf[:n], nil
}

// exchange doet de drie transfers en geeft de ruwe status terug.
func (d *Drive) exchange(cdb []byte, buf []byte, in bool) (int, byte, error) {
	if len(cdb) == 0 || len(cdb) > 16 {
		return 0, 0, fmt.Errorf("optical: command block of %d bytes", len(cdb))
	}
	d.tag++
	tag := d.tag
	if err := d.t.Out(buildCBW(tag, uint32(len(buf)), in, cdb)); err != nil {
		// Het commando kwam niet eens weg: opnieuw beginnen is het enige
		// eerlijke antwoord, anders wacht de drive op data die nooit komt.
		_ = d.t.ResetRecovery()
		return 0, 0, fmt.Errorf("command transport: %w", err)
	}
	n := 0
	var dataErr error
	if len(buf) > 0 {
		var err error
		if in {
			n, err = d.t.In(buf)
		} else {
			err = d.t.Out(buf)
			if err == nil {
				n = len(buf)
			}
		}
		if err != nil {
			// Niet meteen opgeven: een gestalde datafase is de manier waarop
			// een drive "dat commando ken ik niet" zegt, en de reden staat in
			// de status die hierna komt.
			n = 0
			dataErr = err
		}
	}
	status, residue, err := d.status(tag)
	if err != nil {
		if rerr := d.t.ResetRecovery(); rerr != nil {
			return 0, 0, fmt.Errorf("%w (reset also failed: %v)", err, rerr)
		}
		return 0, 0, err
	}
	if residue > uint32(len(buf)) {
		_ = d.t.ResetRecovery()
		return 0, 0, errors.New("optical: status residue exceeds transfer size")
	}
	n = min(n, len(buf)-int(residue))
	if dataErr != nil && status == cswPassed {
		return n, status, fmt.Errorf("optical: data transport failed despite successful status: %w", dataErr)
	}
	return n, status, nil
}

// status leest de CSW en toetst hem. Een CSW met een vreemde signature of een
// andere tag is geen status maar een spoor dat we kwijt zijn: dan is de reset
// de enige uitweg, en dat beslist de aanroeper.
func (d *Drive) status(tag uint32) (byte, uint32, error) {
	csw := make([]byte, cswLen)
	n, err := d.t.In(csw)
	if err != nil {
		// Eén herkansing: de spec schrijft voor dat een gestalde status-
		// endpoint vrijgemaakt wordt en de CSW daarna alsnog komt. Het
		// vrijmaken zelf deed de transport al.
		n, err = d.t.In(csw)
		if err != nil {
			return 0, 0, fmt.Errorf("status transport: %w", err)
		}
	}
	if n < cswLen {
		return 0, 0, fmt.Errorf("status: %d bytes, want %d", n, cswLen)
	}
	if binary.LittleEndian.Uint32(csw[0:4]) != cswSignature {
		return 0, 0, errors.New("status: not a command status wrapper")
	}
	if got := binary.LittleEndian.Uint32(csw[4:8]); got != tag {
		return 0, 0, fmt.Errorf("status: tag %d, want %d", got, tag)
	}
	if csw[12] > cswPhaseError {
		return 0, 0, fmt.Errorf("status: unknown status %d", csw[12])
	}
	return csw[12], binary.LittleEndian.Uint32(csw[8:12]), nil
}

// buildCBW zet het commandoblok in zijn wrapper (BOT 1.0 §5.1).
func buildCBW(tag, length uint32, in bool, cdb []byte) []byte {
	b := make([]byte, cbwLen)
	binary.LittleEndian.PutUint32(b[0:4], cbwSignature)
	binary.LittleEndian.PutUint32(b[4:8], tag)
	binary.LittleEndian.PutUint32(b[8:12], length)
	if in {
		b[12] = cbwFlagIn
	}
	b[13] = 0 // LUN 0: een optische drive heeft er precies één
	b[14] = byte(len(cdb))
	copy(b[15:], cdb)
	return b
}

// trim maakt van een met spaties opgevuld SCSI-veld een nette string.
func trim(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == ' ' || b[end-1] == 0) {
		end--
	}
	start := 0
	for start < end && b[start] == ' ' {
		start++
	}
	return string(b[start:end])
}
