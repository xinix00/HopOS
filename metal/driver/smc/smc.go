// Package smc leest de sensoren van Apple's System Management Controller.
//
// Temperatuur is op elk ander HopOS-bord een register (Radxa: TSADC) of een
// firmware-mailbox (Pi: vcmail). Op Apple-silicium is het een coprocessor: de
// SMC hangt aan dezelfde RTKit-bus als de opslag-coprocessor, praat op endpoint
// 0x20, en beantwoordt vragen over vier-letter-sleutels — hetzelfde model als
// de SMC in Intel-Macs, waar "TC0P" de CPU-temperatuur was.
//
// Klein te houden, en dat is hier geen ambitie maar een feit: de zware laag
// (RTKit: opstartgesprek, syslog, crashlog, geheugenverzoeken) hebben we al
// staan voor de NVMe. Wat hier bijkomt is één endpoint en vier opdrachten.
//
// GEEN DART, GEEN SART — de SMC deelt geheugen via een adres dat hij ZELF in
// zijn eerste bericht noemt. Dat is meteen de opstartvolgorde: endpoint openen,
// INITIALIZE sturen, en wachten tot dat adres binnenkomt.
package smc

import (
	"fmt"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/rtkit"
)

// Het protocol (m1n1 src/smc.c).
const (
	endpoint = 0x20

	cmdReadKey       = 0x10
	cmdWriteKey      = 0x11
	cmdGetKeyByIndex = 0x12
	cmdGetKeyInfo    = 0x13
	cmdInitialize    = 0x17
	cmdNotification  = 0x18
)

// Dev is een geopende SMC.
type Dev struct {
	rt    *rtkit.Dev
	shmem uintptr // door de SMC zelf opgegeven; hier landen waarden > 4 bytes
	msgid uint64

	pending map[uint8]bool
	result  map[uint8]uint64
}

// Open praat de SMC wakker. base is zijn ASC-blok uit de device tree
// (/arm-io/smc, reg[0]); alloc levert DMA-buffers voor de RTKit-laag.
func Open(base uintptr, alloc func(uint64) uintptr) (*Dev, error) {
	d := &Dev{
		pending: map[uint8]bool{},
		result:  map[uint8]uint64{},
	}
	d.rt = &rtkit.Dev{Name: "smc", Base: base, Alloc: alloc, App: d.handle}
	if err := d.rt.Boot(); err != nil {
		return nil, err
	}
	// Vanaf hier LOOPT de coprocessor, en elke uitgang moet hem weer in slaap
	// praten. Dat is geen netheid maar noodzaak, en het is dezelfde les die de
	// ANS ons al leerde: een half opgestarte RTKit-coprocessor die niemand meer
	// pollt loopt vol — zijn syslog wil elke regel bevestigd zien, en zonder
	// bevestiging houdt hij op met werken (zie driver/rtkit, epSyslog).
	//
	// Bij de ANS is dat gemeten en kostte het de opslag tot de volgende
	// power-reset. Wat het bij DIT blok kost weten we niet, en dat is precies
	// de reden om het niet uit te proberen: dit is de System Management
	// Controller. (We dachten 31-08 dat we het wisten — een node die na ~2
	// minuten ophield — maar dat was de watchdog van de firmware, die toen élke
	// boot omlegde rond 1:43, met of zonder SMC.)
	fail := func(err error) (*Dev, error) {
		_ = d.rt.Sleep() // beste inspanning; de fout hieronder is het verhaal
		return nil, err
	}
	if err := d.rt.StartEP(endpoint); err != nil {
		return fail(err)
	}
	// INITIALIZE, en dan wachten op zijn eerste bericht: dát bericht ís het
	// adres van het gedeelde geheugen. Zonder die stap is elke sleutel die
	// meer dan vier bytes teruggeeft onleesbaar.
	if err := d.send(cmdInitialize, 0, 0); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for d.shmem == 0 {
		if err := d.rt.Poll(); err != nil {
			return fail(err)
		}
		if time.Now().After(deadline) {
			return fail(fmt.Errorf("smc: no shared-memory address within 2s"))
		}
		time.Sleep(time.Millisecond)
	}
	return d, nil
}

// handle verwerkt één bericht op ons endpoint.
func (d *Dev) handle(msg uint64, ep uint32) {
	if ep != endpoint {
		return
	}
	if d.shmem == 0 {
		// Het eerste bericht is het adres, niet een antwoord.
		d.shmem = uintptr(msg)
		return
	}
	if msg&0xff == cmdNotification {
		return // sensor-melding; wij vragen zelf wel
	}
	id := uint8(msg >> 12 & 0xf)
	d.result[id] = msg
	d.pending[id] = false
}

func (d *Dev) send(cmd uint8, size uint8, key uint32) error {
	id := uint8(d.msgid&0xf) | 0
	d.msgid++
	d.pending[id] = true
	msg := uint64(cmd) | uint64(id)<<12 | uint64(size)<<16 | uint64(key)<<32
	return d.rt.Send(msg, endpoint)
}

// cmd stuurt een opdracht en wacht op het antwoord met hetzelfde id.
func (d *Dev) cmd(c uint8, size uint8, key uint32) (uint64, error) {
	id := uint8(d.msgid & 0xf)
	if err := d.send(c, size, key); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(time.Second)
	for d.pending[id] {
		if err := d.rt.Poll(); err != nil {
			return 0, err
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("smc: command %#x (key %s) got no answer", c, KeyString(key))
		}
	}
	r := d.result[id]
	if code := r & 0xff; code != 0 {
		return 0, fmt.Errorf("smc: command %#x (key %s) failed with %d", c, KeyString(key), code)
	}
	return r, nil
}

// Key maakt van vier tekens de sleutel zoals de SMC hem verwacht.
func Key(s string) uint32 {
	var k uint32
	for i := 0; i < 4 && i < len(s); i++ {
		k = k<<8 | uint32(s[i])
	}
	return k
}

// KeyString is de omgekeerde weg, voor foutmeldingen en dumps.
func KeyString(k uint32) string {
	return string([]byte{byte(k >> 24), byte(k >> 16), byte(k >> 8), byte(k)})
}

// Read leest een sleutel. Waarden tot vier bytes komen in het antwoord zelf
// mee; grotere staan in het gedeelde geheugen.
func (d *Dev) Read(key uint32) ([]byte, error) {
	r, err := d.cmd(cmdReadKey, 0, key)
	if err != nil {
		return nil, err
	}
	size := int(r >> 16 & 0xffff)
	if size == 0 || size > 4096 {
		return nil, fmt.Errorf("smc: key %s reports %d bytes", KeyString(key), size)
	}
	out := make([]byte, size)
	if size <= 4 {
		v := uint32(r >> 32)
		for i := 0; i < size; i++ {
			out[i] = byte(v >> (8 * i))
		}
		return out, nil
	}
	dev.CopyOut(out, d.shmem)
	return out, nil
}

// KeyAt geeft de sleutel op index i — zo is de sleutellijst van dit silicium te
// doorlopen zonder hem te kennen. Welke sensoren een machine heeft staat
// nergens gedocumenteerd; dit is de enige eerlijke manier om erachter te komen.
func (d *Dev) KeyAt(i uint32) (uint32, error) {
	r, err := d.cmd(cmdGetKeyByIndex, 0, i)
	if err != nil {
		return 0, err
	}
	return uint32(r >> 32), nil
}

// Count is hoeveel sleutels deze SMC kent (sleutel "#KEY").
func (d *Dev) Count() (uint32, error) {
	b, err := d.Read(Key("#KEY"))
	if err != nil {
		return 0, err
	}
	if len(b) < 4 {
		return 0, fmt.Errorf("smc: #KEY is %d bytes", len(b))
	}
	// Big-endian, zoals alle SMC-tellers.
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]), nil
}

// Float leest een sleutel als IEEE-754 float32 (het "flt "-type dat Apple voor
// temperaturen gebruikt) en geeft hem als graden.
func (d *Dev) Float(key uint32) (float32, error) {
	b, err := d.Read(key)
	if err != nil {
		return 0, err
	}
	if len(b) != 4 {
		return 0, fmt.Errorf("smc: key %s is %d bytes, not a float", KeyString(key), len(b))
	}
	bits := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	return float32FromBits(bits), nil
}

// float32FromBits is math.Float32frombits zonder math te importeren: dit pakket
// draait op een node waar elke geïmporteerde boom meetelt in de telling van
// wat er in een image zit.
func float32FromBits(b uint32) float32 {
	return *(*float32)(unsafe.Pointer(&b))
}

// Sensor is één gevonden sleutel met zijn waarde.
type Sensor struct {
	Key uint32
	C   float32
}

// Sensors loopt de sleutellijst van deze SMC langs en geeft alles terug wat met
// 'T' begint en als float leesbaar is.
//
// Dit bestaat omdat de sensoren van een machine nergens staan opgeschreven:
// Apple kiest per generatie andere namen, en de enige eerlijke manier om te
// weten wat er ís, is het de coprocessor zelf vragen. Voor de meetbank, niet
// voor elke boot — het is één ronde over honderden sleutels.
func Sensors(d *Dev) []Sensor {
	n, err := d.Count()
	if err != nil || n == 0 || n > 4096 {
		return nil
	}
	var out []Sensor
	for i := uint32(0); i < n; i++ {
		k, err := d.KeyAt(i)
		if err != nil || byte(k>>24) != 'T' {
			continue
		}
		if c, err := d.Float(k); err == nil {
			out = append(out, Sensor{Key: k, C: c})
		}
	}
	return out
}

// Hottest geeft de warmste temperatuursleutel van deze machine. De terugval
// voor als geen van de bekende namen bestaat: een node die zijn hitte niet kent
// kan er ook niet op ingrijpen, en dan is de warmste sensor een beter antwoord
// dan géén antwoord.
func Hottest(d *Dev) (float32, uint32) {
	var best float32
	var key uint32
	for _, s := range Sensors(d) {
		if s.C > best {
			best, key = s.C, s.Key
		}
	}
	return best, key
}
