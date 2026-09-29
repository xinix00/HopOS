//go:build media

package slots

// /devices: de namespace van de NODE, naast het volume.
//
// Alles wat een node fysiek heeft en wat een taak kan lezen, hangt hier onder
// een naam: vandaag de optische drive als `/devices/disc0`, morgen een tweede
// drive als `disc1` en later misschien iets heel anders. Een taak mount eruit
// wat hij nodig heeft, net als elk ander volume:
//
//	"volumes": {"/devices/disc0": "/bluray"}
//
// en leest daarna met doodgewone stat- en read-opdrachten. Omdat system calls
// sinds ABI 6 over het slot-LAN naar HOP lopen, werkt dat vanaf elke core, elk
// slot en straks elke node — precies zoals de NVMe. Een apparaat dat bytes
// heeft ís lezen, en dat kan deze ABI al. Alleen wat geen lezen is (een drive
// die een SCSI-opdracht moet krijgen) heeft een eigen opdracht:
// OpDeviceCommand, één uitwisseling per call en nooit herhaald.
//
// Waarom een eigen prefix en niet een naam op de wortel: dit deel is van de
// node. Een pad onder /devices raakt het volume NOOIT — een onbekende naam
// hier is "geen zo'n apparaat" en niet een bestand dat vanzelf aangemaakt
// wordt. Zo kan er ook nooit een echte map /devices op het volume ontstaan die
// een apparaat verstopt.
//
// De namen zijn genummerd en niet de identiteit van het apparaat (geen
// vendor:product, geen poort): een jobspec moet leesbaar blijven, en welke
// drive `disc0` is vertelt de console (`printf 'disc\n' | nc node 5555`) met
// model, poort en al. Een nummer blijft van hetzelfde apparaat zolang de node
// draait; wie zijn drive uittrekt en terugsteekt krijgt hetzelfde nummer terug
// zolang er geen ander in dat gat sprong.

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

// DevicePath geeft het gedeelde pad van een apparaatnaam, zoals een jobspec
// het in zijn volumes zet.
func DevicePath(name string) string { return DevicesDir + "/" + name }

// Device is wat een apparaat aan de kern levert: een leesbaar oppervlak met
// een maat die kan veranderen. media/driver/optical vult dit in voor een optische
// drive; deze laag kent dat pakket niet, zodat een tweede soort drive of een
// image-bestand er net zo goed in past.
type Device interface {
	io.ReaderAt
	// Size is de maat van wat er NU in zit, in bytes. Nul plus een fout als er
	// niets in ligt: een disc wisselt zonder dat iemand het meldt, dus dit
	// wordt bij elke stat opnieuw gevraagd.
	Size() (int64, error)
}

var (
	devMu   sync.Mutex
	devices = map[string]Device{}
)

// errDeviceReadOnly zegt waarom schrijven niet kan, in plaats van een
// bestandsfout die naar het volume wijst.
var errDeviceReadOnly = errors.New("devices are read-only")

// AddDevice hangt een apparaat onder een naam in /devices. Een naam die al
// bestaat wordt vervangen — dat is wat een herplug is.
func AddDevice(name string, d Device) {
	devMu.Lock()
	defer devMu.Unlock()
	if d == nil {
		delete(devices, name)
		return
	}
	devices[name] = d
}

// RemoveDevice haalt een apparaat weg (uittrekken).
func RemoveDevice(name string) { AddDevice(name, nil) }

// DeviceNames geeft de namen die er nu hangen, gesorteerd.
func DeviceNames() []string {
	devMu.Lock()
	defer devMu.Unlock()
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// NextDeviceName geeft de laagste vrije naam met dit voorvoegsel (disc0,
// disc1, ...). Zo houdt een apparaat zijn nummer zolang het blijft hangen, en
// vult een nieuwe het laagste gat.
func NextDeviceName(prefix string) string {
	devMu.Lock()
	defer devMu.Unlock()
	for i := 0; ; i++ {
		name := prefix + itoa(i)
		if _, taken := devices[name]; !taken {
			return name
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	n := len(b)
	for i > 0 && n > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}

func deviceByName(name string) (Device, bool) {
	devMu.Lock()
	defer devMu.Unlock()
	d, ok := devices[name]
	return d, ok
}

// deviceServe beantwoordt de opdrachten die in /devices landen. Lezen,
// opsommen en een apparaatopdracht (OpDeviceCommand); de rest zegt waarom
// niet, in plaats van een fout die naar het volume wijst.
//
// Let op de duur: een read op een optische drive kan seconden kosten als hij
// nog opspint, en zolang wacht de servicer van dít slot. Dat is per slot, dus
// een trage disc houdt alleen zijn eigen lezer op — dezelfde afspraak als bij
// elke andere blokkerende system call.
func (s *servicer) deviceServe(req hopabi.Req, path string, maxChunk int, work *[]byte) []byte {
	if path == DevicesDir {
		if req.Op == hopabi.OpList || req.Op == hopabi.OpStat {
			if req.Op == hopabi.OpStat {
				return ok(req, 0, nil) // een map: maat 0, zoals het volume doet
			}
			return listRespLimit(req, DeviceNames(), maxChunk)
		}
		return fail(req, errDeviceReadOnly)
	}
	name := strings.TrimPrefix(path, DevicesDir+"/")
	d, have := deviceByName(name)
	if !have {
		return fail(req, ErrNoDevice)
	}
	switch req.Op {
	case hopabi.OpDeviceCommand:
		if req.Off != 0 || req.N != 0 {
			return fail(req, errors.New("device command: unexpected offset or length"))
		}
		command, err := hopabi.DecodeDeviceCommand(req.Data, maxChunk)
		if err != nil {
			return fail(req, err)
		}
		device, supportsCommands := d.(interface {
			ExecuteCommand(context.Context, []byte, []byte, []byte) (int, byte, []byte, error)
		})
		if !supportsCommands {
			return fail(req, errors.New("device does not support commands"))
		}
		in := make([]byte, command.InLen)
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(command.TimeoutMS)*time.Millisecond)
		n, status, sense, err := device.ExecuteCommand(ctx, command.CDB, command.DataOut, in)
		cancel()
		if err != nil {
			return fail(req, err)
		}
		if n < 0 || n > max(len(in), len(command.DataOut)) {
			return fail(req, errors.New("device returned an invalid transfer count"))
		}
		if len(in) > 0 {
			in = in[:n]
		}
		result, err := hopabi.EncodeDeviceResult(hopabi.DeviceResult{Status: status, Transferred: uint32(n), Sense: sense, Data: in})
		if err != nil {
			return fail(req, err)
		}
		return ok(req, uint64(n), result)

	case hopabi.OpStat:
		size, err := d.Size()
		if err != nil {
			return fail(req, err)
		}
		return ok(req, uint64(size), nil)

	case hopabi.OpRead:
		n := req.N
		if n > uint64(maxChunk) {
			n = uint64(maxChunk)
		}
		need := hopabi.HdrLen + int(n)
		if cap(*work) < need {
			*work = make([]byte, need)
		}
		buf := (*work)[:need]
		read, err := d.ReadAt(buf[hopabi.HdrLen:hopabi.HdrLen+int(n)], int64(req.Off))
		if err != nil && read == 0 {
			return fail(req, err)
		}
		return hopabi.EncodeRespInto(buf, hopabi.Resp{Op: req.Op, Seq: req.Seq, Size: uint64(read)}, read)

	default:
		return fail(req, errDeviceReadOnly)
	}
}
