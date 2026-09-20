// Package scmi is een minimale SCMI-client (Arm System Control and Management
// Interface, DEN 0056) over het shared-memory-transport: één kanaal = een
// geheugenblok met de standaard SCMI-shmem-layout plus een doorbell-woord.
// Dat is precies wat de Cix P1 (Orion O6N) zijn SCP aanbiedt — de ACPI-DSDT
// van het bord doet zijn temperatuurlezingen (_TMP) via dezelfde bytes
// (device PMMX: OperationRegion op 0x065d0000, doorbell BEEL op +0x80), dus
// dit is het bewezen pad dat Linux-onder-ACPI ook loopt, alleen zonder
// AML-interpreter ertussen.
//
// Alleen wat HopOS nodig heeft: het generieke Call, en de sensor-berichten
// (beschrijving + lezing) voor de thermometer. DVFS loopt op de O6N niet
// hierlangs maar via de SCMI-fastchannels uit de _CPC (een MMIO-woord per
// domein, fw/acpi cpc.go).
package scmi

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Shmem-layout (SCMI spec 5.1.2 "Shared memory based transport"): alles
// relatief aan Channel.Base.
const (
	offStatus  = 0x04 // channel status: bit 0 = free, bit 1 = error
	offSign    = 0x0c // (gereserveerd) — Cix' AML schrijft hier een signatuur
	offFlags   = 0x10 // bit 0 = interrupt-completion; wij pollen: 0
	offLength  = 0x14 // header + payload in bytes
	offHeader  = 0x18 // message header
	offPayload = 0x1c
	offBell    = 0x80 // Cix-mailbox doorbell: bit 0 (AML: BEEL)

	statusFree  = 1 << 0
	statusError = 1 << 1

	cixSignature = 0x50434303 // wat Cix' AML in +0x0c zet (MAILBOX_SCMI_BEGIN)

	maxPayload = 0x60 // AML werkt met 96-byte buffers; ruim voor onze berichten
)

// Protocol-ID's en berichten (DEN 0056 / Cix AcpiScmi.h).
const (
	ProtoBase   = 0x10
	ProtoPerf   = 0x13
	ProtoSensor = 0x15

	MsgVersion    = 0x000
	MsgAttributes = 0x001

	SensorDescriptionGet = 0x003
	SensorReadingGet     = 0x006

	ProtoClock     = 0x14
	ClockRateGet   = 0x006
	ClockConfigSet = 0x007 // payload: clock_id, attributes (bit 0 = enable)
)

// Statuscodes (SCMI 4.1.4): 0 = succes, negatief = fout.
const (
	StatusSuccess      = 0
	StatusNotSupported = -1
	StatusInvalidParam = -2
	StatusDenied       = -3
	StatusNotFound     = -4
	StatusOutOfRange   = -5
	StatusBusy         = -6
)

// Channel is één SCMI-shmem-kanaal met een doorbell op Base+0x80.
type Channel struct {
	Base    uintptr
	Timeout time.Duration // per bericht; 0 = 400ms (de AML-waarde)
	// NoSleep: wachten door te spinnen i.p.v. time.Sleep — voor gebruik
	// vóór de scheduler draait (hwinit1). Begrensd op ~1M polls.
	NoSleep bool
}

func (c *Channel) rd(off uintptr) uint32    { return dev.Read32(c.Base + off) }
func (c *Channel) wr(off uintptr, v uint32) { dev.Write32(c.Base+off, v) }

// header bouwt de SCMI-message-header: msg[7:0], type[9:8]=0 (command),
// protocol[17:10], token[27:18].
func header(proto, msg, token uint32) uint32 {
	return msg&0xff | (proto&0xff)<<10 | (token&0x3ff)<<18
}

// Call stuurt één commando en wacht op het antwoord. req/resp zijn de
// payload-woorden ná de header; resp[0] is de SCMI-status (als int32).
// Sequentie = de AML MAILBOX_SCMI_BEGIN/PROCESS-macro's: wachten tot het
// kanaal vrij is, signatuur + flags + payload + lengte + header schrijven,
// kanaal bezet markeren, doorbell, wachten tot vrij, payload teruglezen.
func (c *Channel) Call(proto, msg uint32, req []uint32) ([]uint32, error) {
	if c.Base == 0 {
		return nil, fmt.Errorf("scmi: no channel")
	}
	if 4*len(req) > maxPayload {
		return nil, fmt.Errorf("scmi: request too large (%d words)", len(req))
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 400 * time.Millisecond
	}
	if err := c.waitFree(timeout); err != nil {
		return nil, fmt.Errorf("scmi: channel busy before %#x/%#x: %w", proto, msg, err)
	}
	c.wr(offSign, cixSignature)
	c.wr(offFlags, 0)
	for i, w := range req {
		c.wr(offPayload+uintptr(i)*4, w)
	}
	c.wr(offLength, uint32(4+4*len(req)))
	c.wr(offHeader, header(proto, msg, c.token()))
	c.wr(offStatus, c.rd(offStatus)&^statusFree) // kanaal bezet
	dev.MB()
	c.wr(offBell, 1)
	dev.MB()
	if err := c.waitFree(timeout); err != nil {
		return nil, fmt.Errorf("scmi: no reply to %#x/%#x: %w", proto, msg, err)
	}
	if c.rd(offStatus)&statusError != 0 {
		return nil, fmt.Errorf("scmi: channel error on %#x/%#x", proto, msg)
	}
	n := int(c.rd(offLength))
	if n < 8 || n > 4+maxPayload {
		return nil, fmt.Errorf("scmi: reply length %d out of range", n)
	}
	words := (n - 4) / 4
	resp := make([]uint32, words)
	for i := range resp {
		resp[i] = c.rd(offPayload + uintptr(i)*4)
	}
	if st := int32(resp[0]); st != StatusSuccess {
		return resp, fmt.Errorf("scmi: %#x/%#x status %d", proto, msg, st)
	}
	return resp, nil
}

// waitFree pollt de free-bit, begrensd.
func (c *Channel) waitFree(timeout time.Duration) error {
	if c.NoSleep {
		for i := 0; c.rd(offStatus)&statusFree == 0; i++ {
			if i > 1<<20 {
				return fmt.Errorf("timeout (status=%#x)", c.rd(offStatus))
			}
		}
		return nil
	}
	deadline := time.Now().Add(timeout)
	for c.rd(offStatus)&statusFree == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout (status=%#x)", c.rd(offStatus))
		}
		time.Sleep(100 * time.Microsecond)
	}
	return nil
}

// tokenSeq: een oplopende token per bericht (spec: de agent kiest; het
// platform echoot hem). Niet essentieel bij één uitstaand bericht.
var tokenSeq uint32

func (c *Channel) token() uint32 {
	tokenSeq++
	return tokenSeq & 0x3ff
}

// Version geeft de protocolversie (major<<16|minor) van proto.
func (c *Channel) Version(proto uint32) (uint32, error) {
	resp, err := c.Call(proto, MsgVersion, nil)
	if err != nil {
		return 0, err
	}
	if len(resp) < 2 {
		return 0, fmt.Errorf("scmi: short version reply")
	}
	return resp[1], nil
}

// Sensor is één sensorbeschrijving (SENSOR_DESCRIPTION_GET).
type Sensor struct {
	ID       uint32
	Type     uint8  // attributes_high[7:0]: 2 = graden Celsius
	Exponent int    // attributes_high[15:11], 5-bit signed: waarde × 10^Exponent
	Name     string // 16 bytes, NUL-getermineerd
}

// Sensors somt de sensoren op: per aanroep vanaf index i één beschrijving
// (het platform mag er meer sturen; we nemen de eerste en lopen door op het
// "remaining"-veld). Onbekende extensies van de descriptor (SCMI 3.x) raken
// we zo niet aan.
func (c *Channel) Sensors() ([]Sensor, error) {
	var out []Sensor
	for i := uint32(0); i < 64; i++ {
		resp, err := c.Call(ProtoSensor, SensorDescriptionGet, []uint32{i})
		if err != nil {
			return out, err
		}
		// resp: status, num_flags (returned[15:0], remaining[31:16]),
		// dan per sensor: id, attr_low, attr_high, name[16].
		if len(resp) < 2+3+4 || resp[1]&0xffff == 0 {
			return out, nil
		}
		s := Sensor{ID: resp[2], Type: uint8(resp[4]), Exponent: int(int8(uint8(resp[4]>>11&0x1f)<<3) >> 3)}
		var name [16]byte
		for k := 0; k < 4; k++ {
			w := resp[5+k]
			name[4*k], name[4*k+1], name[4*k+2], name[4*k+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
		}
		n := 0
		for n < len(name) && name[n] != 0 {
			n++
		}
		s.Name = string(name[:n])
		out = append(out, s)
		if resp[1]>>16 == 0 {
			return out, nil
		}
	}
	return out, nil
}

// Reading leest de scalaire waarde van sensor id (SENSOR_READING_GET,
// synchroon): 64 bits, in de eenheid × 10^Exponent van de beschrijving.
func (c *Channel) Reading(id uint32) (int64, error) {
	resp, err := c.Call(ProtoSensor, SensorReadingGet, []uint32{id, 0})
	if err != nil {
		return 0, err
	}
	if len(resp) < 3 {
		return 0, fmt.Errorf("scmi: short sensor reply")
	}
	return int64(uint64(resp[1]) | uint64(resp[2])<<32), nil
}

// MilliC zet een lezing van een Celsius-sensor om naar milligraden.
func (s Sensor) MilliC(v int64) int {
	e := s.Exponent + 3 // ×1000
	for ; e > 0; e-- {
		v *= 10
	}
	for ; e < 0; e++ {
		v /= 10
	}
	return int(v)
}
