// Package rtkit praat met de co-processors van Apple silicon.
//
// Een aantal randapparaten op deze SoC's is geen registerblok maar een eigen
// processortje met firmware: de opslag (ANS), het beeld (DCP), de sensoren.
// Ze delen één protocol — RTKit — over een mailbox van twee registers. Wie de
// SSD wil aanspreken moet eerst dit gesprek voeren, en daarna pas het gewone
// NVMe praten dat erachter zit.
//
// Het gesprek is kort en vast: wek de coprocessor, ontvang HELLO met een
// versiebereik, antwoord met de versie die je kiest, ontvang de kaart van zijn
// endpoints, start de systeem-endpoints, en wacht tot hij "aan" meldt. Onderweg
// vraagt hij om geheugen — voor zijn syslog, zijn crashlog en zijn
// io-rapportage — en dat moeten wij geven, want zonder antwoord komt hij zijn
// opstart niet door.
//
// Wat we NIET doen: firmware laden (die zit al in de coprocessor), zijn syslog
// lezen (we bevestigen de regels en gooien ze weg), en DART-vertaling (op de
// ANS staat een SART, een simpel adresfilter, en daar is een DMA-adres gewoon
// een fysiek adres). Referentie: m1n1 src/rtkit.c en src/asc.c.
package rtkit

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De mailbox (m1n1 src/asc.c), relatief aan de ASC-basis; het CPU-controlwoord
// zit in het blok daarvóór.
const (
	cpuControl = 0x44
	cpuStart   = 0x10
	mboxOff    = 0x8000

	a2iControl = 0x110 // wij → coprocessor
	a2iSend0   = 0x800
	a2iSend1   = 0x808
	i2aControl = 0x114 // coprocessor → wij
	i2aRecv0   = 0x830
	i2aRecv1   = 0x838

	mboxFull  = 1 << 16
	mboxEmpty = 1 << 17
)

// Endpoints en berichttypes (m1n1 src/rtkit.c).
const (
	epMgmt     = 0
	epCrashlog = 1
	epSyslog   = 2
	epDebug    = 3
	epIOReport = 4
	epOSLog    = 8
	epSystem   = 0x20 // daarboven zijn het berichten van het apparaat zelf

	msgHello       = 1
	msgHelloAck    = 2
	msgStartEP     = 5
	msgIOPPwrState = 6
	msgIOPPwrAck   = 7
	msgEPMap       = 8
	msgAPPwrState  = 0xb

	msgBufferRequest = 1
	msgSyslogInit    = 8
	msgSyslogLog     = 5

	powerSleep = 0x01
	powerOn    = 0x20
	powerInit  = 0x220

	minVersion = 11
	maxVersion = 12
)

// Dev is één coprocessor.
type Dev struct {
	// Base is het ASC-blok uit de ADT (voor de ANS: de ans-node, reg[0]).
	Base uintptr
	// Name komt in foutmeldingen; er hangen er meerdere aan deze bus.
	Name string
	// Alloc levert een 16KB-uitgelijnde DMA-buffer uit het geheugenplan van het
	// board. 0 = geen ruimte meer; de opstart faalt dan met een duidelijke fout.
	Alloc func(size uint64) uintptr
	// Allow opent zo nodig een venster in het adresfilter (de SART). nil als het
	// board het hele DMA-gebied al in één keer heeft opengezet.
	Allow func(paddr, size uint64) bool

	// App krijgt elk bericht op een APPLICATIE-endpoint (0x20 en hoger). Dat
	// is het domein van een driver — de SMC praat op 0x20 — en dit pakket weet
	// van die protocollen niets. nil = zulke berichten vallen weg.
	App func(msg uint64, ep uint32)

	iopPower uint64
	apPower  uint64
	bufs     [epOSLog + 1]uint64 // per systeem-endpoint het afgegeven adres
	appEP    map[uint32]bool     // applicatie-endpoints die "aan" gemeld hebben
}

func (d *Dev) mb() uintptr { return d.Base + mboxOff }

func (d *Dev) send(msg uint64, ep uint32) error {
	deadline := time.Now().Add(200 * time.Millisecond)
	for dev.Read32(d.mb()+a2iControl)&mboxFull != 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("rtkit(%s): outgoing mailbox full for 200ms — coprocessor stuck?", d.Name)
		}
	}
	dev.MB()
	dev.Write64(d.mb()+a2iSend0, msg)
	dev.Write64(d.mb()+a2iSend1, uint64(ep))
	return nil
}

// recv haalt één bericht op. ok=false = niets klaar.
func (d *Dev) recv() (msg uint64, ep uint32, ok bool) {
	if dev.Read32(d.mb()+i2aControl)&mboxEmpty != 0 {
		return 0, 0, false
	}
	msg = dev.Read64(d.mb() + i2aRecv0)
	ep = uint32(dev.Read64(d.mb() + i2aRecv1))
	dev.MB()
	return msg, ep, true
}

func (d *Dev) recvTimeout(timeout time.Duration) (uint64, uint32, error) {
	deadline := time.Now().Add(timeout)
	for {
		if msg, ep, ok := d.recv(); ok {
			return msg, ep, nil
		}
		if time.Now().After(deadline) {
			return 0, 0, fmt.Errorf("rtkit(%s): no message within %s", d.Name, timeout)
		}
	}
}

// mgmtType is het berichttype: bits 59:52 van msg0.
func mgmtType(msg uint64) uint64 { return msg >> 52 & 0xff }
func typed(t uint64) uint64      { return t << 52 }

// handle verwerkt één systeembericht. Berichten voor het apparaat zelf
// (endpoint ≥ 0x20) horen hier niet thuis; die zijn voor de driver erboven.
func (d *Dev) handle(msg uint64, ep uint32) error {
	t := mgmtType(msg)
	switch ep {
	case epMgmt:
		switch t {
		case msgIOPPwrAck:
			d.iopPower = msg & 0xffff
		case msgAPPwrState:
			d.apPower = msg & 0xffff
		}
	case epSyslog:
		switch t {
		case msgBufferRequest:
			return d.giveBuffer(msg, ep)
		case msgSyslogInit:
			// afmetingen van zijn ringbuffer; wij lezen hem niet
		case msgSyslogLog:
			// Elke regel moet bevestigd worden — precies hetzelfde bericht
			// terug — anders houdt hij op met loggen en uiteindelijk met
			// werken. De inhoud laten we liggen.
			return d.send(msg, ep)
		}
	case epCrashlog:
		if t == msgBufferRequest {
			// Een tweede verzoek om een crashlog-buffer is hoe de coprocessor
			// meldt dat hij omviel: de eerste gaf hij bij zijn opstart, de
			// tweede komt met de melding erin.
			if d.bufs[epCrashlog] != 0 {
				return fmt.Errorf("rtkit(%s): coprocessor crashed — %s", d.Name, d.Crashlog())
			}
			return d.giveBuffer(msg, ep)
		}
	case epIOReport:
		switch t {
		case msgBufferRequest:
			return d.giveBuffer(msg, ep)
		case 0x8, 0xc:
			// Onbekend maar moet bevestigd worden (m1n1 doet hetzelfde).
			return d.send(msg, ep)
		}
	default:
		// Alles boven de systeem-endpoints is van een DRIVER: de SMC praat op
		// 0x20, de NVMe-kant heeft er zelf geen. Zonder haak vielen die
		// berichten stil op de grond — en een coprocessor die op antwoord
		// wacht, houdt op met werken.
		if d.appEP == nil {
			d.appEP = map[uint32]bool{}
		}
		d.appEP[ep] = true
		if d.App != nil {
			d.App(msg, ep)
		}
	}
	return nil
}

// giveBuffer beantwoordt een geheugenverzoek. Het verzoek draagt een maat in
// 4KB-pagina's en soms een adres: dat laatste betekent "ik heb er zelf al een,
// gebruik dit" en dan hoeven wij niets te geven.
func (d *Dev) giveBuffer(msg uint64, ep uint32) error {
	pages := msg >> 44 & 0xff
	iova := msg & (1<<42 - 1)
	if iova != 0 {
		d.bufs[ep] = iova
		return nil
	}
	size := pages << 12
	if size == 0 {
		size = 1 << 14
	}
	if d.Alloc == nil {
		return fmt.Errorf("rtkit(%s): endpoint %d asks for %d KB and there is no allocator", d.Name, ep, size>>10)
	}
	// 16KB is de paginamaat van dit silicium; de coprocessor en het adresfilter
	// rekenen er allebei mee.
	size = (size + 0x3fff) &^ 0x3fff
	p := d.Alloc(size)
	if p == 0 {
		return fmt.Errorf("rtkit(%s): no room for a %d KB buffer for endpoint %d", d.Name, size>>10, ep)
	}
	if d.Allow != nil && !d.Allow(uint64(p), size) {
		return fmt.Errorf("rtkit(%s): address filter refused %#x+%#x", d.Name, uint64(p), size)
	}
	dev.Clear(p, size)
	d.bufs[ep] = uint64(p)
	return d.send(typed(msgBufferRequest)|pages<<44|uint64(p), ep)
}

// Poll verwerkt wat er klaarstaat. De driver erboven roept dit in zijn
// wachtlussen aan: een coprocessor met een volle uitgaande mailbox wacht, en
// een wachtende coprocessor doet geen DMA meer.
func (d *Dev) Poll() error {
	for i := 0; i < 32; i++ {
		msg, ep, ok := d.recv()
		if !ok {
			return nil
		}
		if ep >= epSystem {
			continue // van het apparaat zelf; wij spreken alleen de systeemkant
		}
		if err := d.handle(msg, ep); err != nil {
			return err
		}
	}
	return nil
}

// Send zet één bericht op een endpoint. Voor drivers die zelf een
// applicatie-endpoint bedienen (zie App); de systeem-endpoints doet dit pakket.
func (d *Dev) Send(msg uint64, ep uint32) error { return d.send(msg, ep) }

// StartEP vraagt de coprocessor een applicatie-endpoint te openen en wacht tot
// hij dat bevestigt. Nodig vóór het eerste bericht erop: een endpoint dat niet
// gestart is, slikt alles zonder te antwoorden.
func (d *Dev) StartEP(ep uint32) error {
	if d.appEP == nil {
		d.appEP = map[uint32]bool{}
	}
	if err := d.send(typed(msgStartEP)|uint64(ep)<<32|1<<1, epMgmt); err != nil {
		return err
	}
	// De coprocessor bevestigt niet apart dat een applicatie-endpoint openging;
	// wat je krijgt is zijn EERSTE bericht erop. Dus: even pollen zodat een
	// vroeg bericht niet verloren gaat, en dan doorgaan — de aanroeper wacht
	// toch op zijn eigen antwoord.
	for i := 0; i < 20 && !d.appEP[ep]; i++ {
		if err := d.Poll(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	return nil
}

// Boot voert het opstartgesprek. Mag ook op een coprocessor die al draait: het
// wekbericht start het gesprek hoe dan ook opnieuw.
func (d *Dev) Boot() error {
	dev.Write32(d.Base+cpuControl, dev.Read32(d.Base+cpuControl)|cpuStart)
	if err := d.send(typed(msgIOPPwrState)|powerInit, epMgmt); err != nil {
		return err
	}

	msg, ep, err := d.recvTimeout(time.Second)
	if err != nil {
		return fmt.Errorf("%w — expected HELLO", err)
	}
	if ep != epMgmt || mgmtType(msg) != msgHello {
		return fmt.Errorf("rtkit(%s): expected HELLO, got type %#x on endpoint %d", d.Name, mgmtType(msg), ep)
	}
	minVer, maxVer := msg&0xffff, msg>>16&0xffff
	want := uint64(maxVersion)
	if maxVer < want {
		want = maxVer
	}
	if minVer > maxVersion || maxVer < minVersion {
		return fmt.Errorf("rtkit(%s): coprocessor speaks versions [%d,%d], we speak [%d,%d]",
			d.Name, minVer, maxVer, minVersion, maxVersion)
	}
	if err := d.send(typed(msgHelloAck)|want<<16|want, epMgmt); err != nil {
		return err
	}

	// De kaart van zijn endpoints komt in stukken van 32 bits; elk stuk moet
	// bevestigd worden en het laatste draagt een klaar-vlag.
	var have [epOSLog + 1]bool
	for done := false; !done; {
		msg, ep, err = d.recvTimeout(time.Second)
		if err != nil {
			return fmt.Errorf("%w — expected endpoint map", err)
		}
		if ep != epMgmt || mgmtType(msg) != msgEPMap {
			return fmt.Errorf("rtkit(%s): expected endpoint map, got type %#x on endpoint %d",
				d.Name, mgmtType(msg), ep)
		}
		bitmap, base := msg&0xffffffff, msg>>32&0x7
		for i := uint64(0); i < 32; i++ {
			if bitmap&(1<<i) == 0 {
				continue
			}
			if idx := 32*base + i; idx <= epOSLog {
				have[idx] = true
			}
		}
		done = msg&(1<<51) != 0
		reply := typed(msgEPMap) | base<<32
		if done {
			reply |= 1 << 51
		} else {
			reply |= 1
		}
		if err := d.send(reply, epMgmt); err != nil {
			return err
		}
	}

	for _, e := range []uint64{epDebug, epCrashlog, epSyslog, epIOReport, epOSLog} {
		if !have[e] {
			continue
		}
		if err := d.send(typed(msgStartEP)|e<<32|1<<1, epMgmt); err != nil {
			return err
		}
	}

	// Wachten tot hij "aan" meldt. Onderweg komen zijn geheugenverzoeken binnen;
	// die beantwoordt Poll.
	deadline := time.Now().Add(5 * time.Second)
	for d.iopPower != powerOn {
		if err := d.Poll(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rtkit(%s): coprocessor did not reach power state ON (now %#x)", d.Name, d.iopPower)
		}
	}

	// En melden dat wij er ook zijn — dit zet zijn syslog aan.
	return d.send(typed(msgAPPwrState)|powerOn, epMgmt)
}

// Crashlog leest de melding die de coprocessor in zijn crashbuffer achterliet.
// De vorm is een header met vaste magic en daarachter entries; alleen de
// tekstentries ('Cstr') zeggen iets tegen een mens. Dit is het enige kanaal
// waarlangs firmware die wij niet hebben ons vertelt wat er misging.
func (d *Dev) Crashlog() string {
	const (
		magicCLHE = 0x434c4845 // 'CLHE'
		magicCstr = 0x43737472 // 'Cstr'
		hdrSize   = 32
		entryHdr  = 16
	)
	b := uintptr(d.bufs[epCrashlog])
	if b == 0 {
		return "no crashlog buffer"
	}
	if m := dev.Read32(b); m != magicCLHE {
		return fmt.Sprintf("crashlog header %#x (expected %#x)", m, magicCLHE)
	}
	out := ""
	p := b + hdrSize
	for i := 0; i < 32; i++ {
		t, l := dev.Read32(p), dev.Read32(p+12)
		if t == magicCLHE || l < entryHdr || l > 1<<16 {
			break
		}
		if t == magicCstr {
			var s []byte
			for j := uintptr(entryHdr + 4); j < uintptr(l) && len(s) < 400; j++ {
				c := dev.Read8(p + j)
				if c == 0 {
					break
				}
				s = append(s, c)
			}
			if len(s) > 0 {
				if out != "" {
					out += " | "
				}
				out += string(s)
			}
		}
		p += uintptr(l)
	}
	if out == "" {
		return "crashlog without a text entry"
	}
	return out
}

// Sleep zet de coprocessor terug zoals we hem aantroffen: eerst melden dat wij
// weggaan, dan hem in slaap, dan zijn kern stilzetten. Dit is geen netheid maar
// noodzaak — laat je hem draaien, dan kan de vólgende boot hem niet meer
// overnemen (gemeten 29-08: de NVMe-controller wordt dan nooit meer ready) en
// helpt alleen nog een power-reset van zijn hele domein.
//
// De AP-kant moet naar QUIESCED en niet verder; m1n1 zegt erbij dat herstarten
// anders niet werkt.
func (d *Dev) Sleep() error {
	const quiesced = 0x10
	if err := d.send(typed(msgAPPwrState)|quiesced, epMgmt); err != nil {
		return err
	}
	if err := d.await(&d.apPower, quiesced, "AP quiesced"); err != nil {
		return err
	}
	if err := d.send(typed(msgIOPPwrState)|powerSleep, epMgmt); err != nil {
		return err
	}
	if err := d.await(&d.iopPower, powerSleep, "coprocessor asleep"); err != nil {
		return err
	}
	dev.Write32(d.Base+cpuControl, dev.Read32(d.Base+cpuControl)&^uint32(cpuStart))
	return nil
}

// await pollt tot een van de power-velden de gewenste waarde heeft; onderweg
// verwerkt het de rest van de mailbox.
func (d *Dev) await(field *uint64, want uint64, what string) error {
	deadline := time.Now().Add(5 * time.Second)
	for *field != want {
		if err := d.Poll(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rtkit(%s): no %s within 5s (state %#x)", d.Name, what, *field)
		}
	}
	return nil
}
