//go:build media

package slots

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

type commandDevice struct {
	fakeDevice
	calls int
}

func (d *commandDevice) ExecuteCommand(ctx context.Context, cdb, out, in []byte) (int, byte, []byte, error) {
	d.calls++
	copy(in, []byte{1, 2, 3})
	return 3, 2, []byte{0x72, 5, 0x20, 0, 0, 0, 0, 0}, nil
}

func TestDeviceCommandMountAndPayloadBounds(t *testing.T) {
	d := &commandDevice{fakeDevice: fakeDevice{size: 2048}}
	AddDevice("disc0", d)
	t.Cleanup(func() { RemoveDevice("disc0") })
	s := &servicer{root: "/tasks/test", mounts: [][2]string{{"/mounts", "/devices"}}}
	data, err := hopabi.EncodeDeviceCommand(hopabi.DeviceCommand{CDB: []byte{0x12, 0, 0, 0, 8, 0}, TimeoutMS: 1000, InLen: 8})
	if err != nil {
		t.Fatal(err)
	}
	var work []byte
	request := hopabi.Req{Op: hopabi.OpDeviceCommand, Path: "/mounts/disc0", Data: data}
	response := decode(t, s.handleWithLimit(hopabi.EncodeReq(request), 4096, &work))
	if response.Status != hopabi.StatusOK || response.Size != 3 || d.calls != 1 {
		t.Fatalf("command: %+v, calls=%d", response, d.calls)
	}
	result, err := hopabi.DecodeDeviceResult(response.Data, 8, 0)
	if err != nil || result.Status != 2 || len(result.Sense) != 8 || !bytes.Equal(result.Data, []byte{1, 2, 3}) {
		t.Fatalf("SCSI result: %+v %v", result, err)
	}
	for _, path := range []string{"/devices/disc0", "/mounts/../disc0", "/mounts/disc0/extra"} {
		request.Path = path
		response = decode(t, s.handleWithLimit(hopabi.EncodeReq(request), 4096, &work))
		if response.Status == hopabi.StatusOK || d.calls != 1 {
			t.Fatalf("unmounted path %q reached command transport", path)
		}
	}
	request.Path = "/mounts/disc0"
	request.Data = data[:16]
	response = decode(t, s.handleWithLimit(hopabi.EncodeReq(request), 4096, &work))
	if response.Status == hopabi.StatusOK || d.calls != 1 {
		t.Fatal("truncated command reached drive")
	}
}

// fakeDevice is een schijfoppervlak van niets: elke byte draagt zijn eigen
// adres, zodat een test ziet of een lezing op de juiste plek landde.
type fakeDevice struct {
	size int64
	err  error
}

func (f fakeDevice) Size() (int64, error) { return f.size, f.err }

func (f fakeDevice) ReadAt(p []byte, off int64) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n := 0
	for i := range p {
		if off+int64(i) >= f.size {
			break
		}
		p[i] = byte((off + int64(i)) % 251)
		n++
	}
	return n, nil
}

func decode(t *testing.T, raw []byte) hopabi.Resp {
	t.Helper()
	resp, err := hopabi.DecodeResp(raw)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

// Een taak die de disc mount doet gewone stat- en read-opdrachten; de kern
// beantwoordt ze uit de drive in plaats van uit het volume.
func TestDeviceAnswersStatAndRead(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0") })
	AddDevice("disc0", fakeDevice{size: 23 << 30})
	if names := DeviceNames(); len(names) != 1 || names[0] != "disc0" {
		t.Fatalf("devices = %v", names)
	}
	s := &servicer{}
	work := make([]byte, 0)

	resp := decode(t, s.deviceServe(hopabi.Req{Op: hopabi.OpStat, Path: DevicePath("disc0")}, DevicePath("disc0"), 4096, &work))
	if resp.Status != hopabi.StatusOK || resp.Size != 23<<30 {
		t.Fatalf("stat = status %d, size %d", resp.Status, resp.Size)
	}

	raw := s.deviceServe(hopabi.Req{Op: hopabi.OpRead, Path: DevicePath("disc0"), Off: 2048, N: 8}, DevicePath("disc0"), 4096, &work)
	resp = decode(t, raw)
	if resp.Status != hopabi.StatusOK || resp.Size != 8 {
		t.Fatalf("read = status %d, size %d", resp.Status, resp.Size)
	}
	want := make([]byte, 8)
	for i := range want {
		want[i] = byte((2048 + i) % 251)
	}
	if !bytes.Equal(resp.Data, want) {
		t.Fatalf("read gave %v, want %v", resp.Data, want)
	}
}

// Een leesverzoek dat groter is dan de transportgrens wordt geknipt, niet
// geweigerd: dat is hoe elke andere read in deze ABI zich gedraagt.
func TestDeviceReadIsCutToTheTransportLimit(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0") })
	AddDevice("disc0", fakeDevice{size: 1 << 20})
	s := &servicer{}
	work := make([]byte, 0)
	resp := decode(t, s.deviceServe(hopabi.Req{Op: hopabi.OpRead, Path: DevicePath("disc0"), N: 100000}, DevicePath("disc0"), 4096, &work))
	if resp.Status != hopabi.StatusOK || resp.Size != 4096 {
		t.Fatalf("read = status %d, size %d, want 4096 bytes", resp.Status, resp.Size)
	}
}

// Zonder drive is het antwoord een eigen fout: de node is niet stuk, er hangt
// gewoon niets aan.
func TestDeviceThatIsNotThereSaysSo(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0") })
	RemoveDevice("disc0")
	s := &servicer{}
	work := make([]byte, 0)
	resp := decode(t, s.deviceServe(hopabi.Req{Op: hopabi.OpStat, Path: DevicePath("disc0")}, DevicePath("disc0"), 4096, &work))
	if resp.Status == hopabi.StatusOK {
		t.Fatal("stat without a drive should not succeed")
	}
	if !bytes.Contains(resp.Data, []byte("no such device")) {
		t.Fatalf("error = %q", resp.Data)
	}
}

// Schrijven naar een disc die alleen kan lezen hoort te zeggen waarom, en niet
// te klinken als een bestandsfout op het volume.
func TestDeviceRefusesToBeWritten(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0") })
	AddDevice("disc0", fakeDevice{size: 1 << 20})
	s := &servicer{}
	work := make([]byte, 0)
	for _, op := range []uint8{hopabi.OpWrite, hopabi.OpTruncate, hopabi.OpRemove} {
		resp := decode(t, s.deviceServe(hopabi.Req{Op: op, Path: DevicePath("disc0")}, DevicePath("disc0"), 4096, &work))
		if resp.Status == hopabi.StatusOK {
			t.Fatalf("op %d succeeded on a read-only disc", op)
		}
		if !bytes.Contains(resp.Data, []byte("read-only")) {
			t.Fatalf("op %d error = %q", op, resp.Data)
		}
	}
}

// Een lege lade komt als de fout van de drive terug, niet als een maat van nul:
// nul bytes lezen en "er ligt niets" zijn verschillende antwoorden.
func TestDeviceWithAnEmptyTrayPassesTheReason(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0") })
	empty := errors.New("optical: no medium in the drive")
	AddDevice("disc0", fakeDevice{err: empty})
	s := &servicer{}
	work := make([]byte, 0)
	resp := decode(t, s.deviceServe(hopabi.Req{Op: hopabi.OpStat, Path: DevicePath("disc0")}, DevicePath("disc0"), 4096, &work))
	if resp.Status == hopabi.StatusOK {
		t.Fatal("stat on an empty tray should not succeed")
	}
	if !bytes.Contains(resp.Data, []byte("no medium")) {
		t.Fatalf("error = %q", resp.Data)
	}
}

// De namespace is te listen: een app die niet weet wat eraan hangt, vraagt het
// gewoon. Dat is dezelfde opdracht als op een map op het volume.
func TestDevicesDirectoryListsWhatHangsThere(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0"); RemoveDevice("disc1") })
	AddDevice("disc0", fakeDevice{size: 1 << 20})
	AddDevice("disc1", fakeDevice{size: 2 << 20})
	s := &servicer{}
	work := make([]byte, 0)
	resp := decode(t, s.deviceServe(hopabi.Req{Op: hopabi.OpList, Path: DevicesDir}, DevicesDir, 4096, &work))
	if resp.Status != hopabi.StatusOK {
		t.Fatalf("list = status %d (%q)", resp.Status, resp.Data)
	}
	if got := string(resp.Data); got != "disc0\ndisc1" && got != "disc0\ndisc1\n" {
		t.Fatalf("list = %q", got)
	}
}

// Een apparaat houdt zijn nummer zolang het hangt, en een nieuwe vult het
// laagste gat. Zo betekent disc0 in een jobspec morgen nog hetzelfde.
func TestDeviceNamesFillTheLowestGap(t *testing.T) {
	t.Cleanup(func() { RemoveDevice("disc0"); RemoveDevice("disc1"); RemoveDevice("disc2") })
	if got := NextDeviceName("disc"); got != "disc0" {
		t.Fatalf("first name = %q", got)
	}
	AddDevice("disc0", fakeDevice{})
	if got := NextDeviceName("disc"); got != "disc1" {
		t.Fatalf("second name = %q", got)
	}
	AddDevice("disc1", fakeDevice{})
	RemoveDevice("disc0")
	if got := NextDeviceName("disc"); got != "disc0" {
		t.Fatalf("after unplugging disc0 the next name = %q, want the gap", got)
	}
}
