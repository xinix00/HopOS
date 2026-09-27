package hopabi

import (
	"bytes"
	"testing"
)

func TestDeviceCommandBounds(t *testing.T) {
	b, err := EncodeDeviceCommand(DeviceCommand{CDB: []byte{0x12, 0, 0, 0, 96, 0}, TimeoutMS: 30000, InLen: 96})
	if err != nil {
		t.Fatal(err)
	}
	c, err := DecodeDeviceCommand(b, 4096)
	if err != nil || c.InLen != 96 || c.TimeoutMS != 30000 || c.CDB[0] != 0x12 {
		t.Fatalf("decoded command: %+v %v", c, err)
	}
	for _, change := range []func([]byte){func(b []byte) { b[1] = 17 }, func(b []byte) { le.PutUint32(b[4:], 0) }, func(b []byte) { le.PutUint32(b[4:], 300001) }, func(b []byte) { le.PutUint32(b[8:], 0xffffffff) }, func(b []byte) { le.PutUint32(b[12:], 1) }, func(b []byte) { b[2] = 1 }} {
		bad := bytes.Clone(b)
		change(bad)
		if _, err := DecodeDeviceCommand(bad, 4096); err == nil {
			t.Fatal("accepted malformed command")
		}
	}
	for i := 0; i < len(b); i++ {
		if _, err := DecodeDeviceCommand(b[:i], 4096); err == nil {
			t.Fatalf("accepted truncation %d", i)
		}
	}
	if _, err := EncodeDeviceCommand(DeviceCommand{CDB: []byte{1}, TimeoutMS: 1, InLen: 1, DataOut: []byte{1}}); err == nil {
		t.Fatal("accepted bidirectional exchange")
	}
}

func TestDeviceResultSenseAndShortTransfers(t *testing.T) {
	b, err := EncodeDeviceResult(DeviceResult{Status: 2, Transferred: 3, Sense: []byte{0x72, 5, 0x20, 0, 0, 0, 0, 0}, Data: []byte{1, 2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := DecodeDeviceResult(b, 10, 0)
	if err != nil || r.Status != 2 || r.Transferred != 3 || len(r.Sense) != 8 || len(r.Data) != 3 {
		t.Fatalf("result: %+v %v", r, err)
	}
	if _, err = DecodeDeviceResult(b, 2, 0); err == nil {
		t.Fatal("accepted impossible transfer")
	}
	if _, err = DecodeDeviceResult(b, 0, 10); err == nil {
		t.Fatal("accepted input data for output command")
	}
	b[1] = 65
	if _, err = DecodeDeviceResult(b, 10, 0); err == nil {
		t.Fatal("accepted oversized sense")
	}
}
