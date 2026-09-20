package hop

import (
	"bytes"
	"os"
	"testing"
)

func TestCIXUSBActualFirmware(t *testing.T) {
	b, e := os.ReadFile("testdata/o6n-dsdt.aml")
	if e != nil {
		t.Fatal(e)
	}
	f, e := parseUSBFirmware(b)
	if e != nil {
		t.Fatal(e)
	}
	if f.base != 0xfffe0000 || f.size != 0x4d {
		t.Fatal(f)
	}
	bases := [10]uint64{0x9018000, 0x9088000, 0x90f8000, 0x9168000, 0x91d8000, 0x91e8000, 0x9268000, 0x9298000, 0x92c8000, 0x92f8000}
	enables := [10]byte{0x12, 0x13, 0x16, 0x15, 0x17, 0x18, 0x14, 0x1b, 0x1a, 0x19}
	roles := [10]byte{0x1c, 0, 0, 0, 0x1d, 0x1e, 0, 0, 0, 0}
	for i, h := range f.hosts {
		if h.Base != bases[i] || h.Size != 0x8000 || h.EnableOffset != enables[i] || h.RoleOffset != roles[i] {
			t.Fatalf("host%d: %+v", i, h)
		}
	}
	for _, key := range [][]byte{[]byte("GNVA"), []byte("GNVL"), []byte("XHC2"), []byte("PNP0D10"), {'G', 'E', 'T', 'V', 9, 0xa0}} {
		damaged := append([]byte(nil), b...)
		at := bytes.Index(damaged, key)
		if at < 0 {
			t.Fatal("missing key")
		}
		damaged[at] ^= 0x20
		if _, e := parseUSBFirmware(damaged); e == nil {
			t.Fatalf("accepted changed %q", key)
		}
	}
	if _, e := parseUSBFirmware(b[:len(b)/2]); e == nil {
		t.Fatal("accepted truncated firmware")
	}
	duplicate := append(append([]byte(nil), b...), []byte{8, 'G', 'N', 'V', 'A', 0}...)
	if _, e := parseUSBFirmware(duplicate); e == nil {
		t.Fatal("accepted ambiguous GNVA")
	}
}

func TestCIXUSBStableSlotsAcrossEnableMasks(t *testing.T) {
	b, err := os.ReadFile("testdata/o6n-dsdt.aml")
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseUSBFirmware(b)
	if err != nil {
		t.Fatal(err)
	}
	for mask := 0; mask < 1<<usbHostCount; mask++ {
		values := make([]byte, f.size)
		for i, h := range f.hosts {
			if mask&(1<<i) != 0 {
				values[h.EnableOffset] = 1
			}
		}
		hosts := f.enabledHosts(values)
		for i, h := range hosts {
			if h.Name != f.hosts[i].Name || h.Base != f.hosts[i].Base || h.Enabled != (mask&(1<<i) != 0) {
				t.Fatalf("mask %d slot %d changed ownership/status: %+v", mask, i, h)
			}
		}
		for _, h := range f.hosts {
			if h.RoleOffset != 0 {
				values[h.RoleOffset] = 1
			}
		}
		hosts = f.enabledHosts(values)
		for i, h := range hosts {
			if f.hosts[i].RoleOffset != 0 && h.Enabled {
				t.Fatalf("device role enabled host %d", i)
			}
		}
	}
	for i, h := range f.enabledHosts(nil) {
		if h.Enabled {
			t.Fatalf("missing state enabled host %d", i)
		}
	}
}

func TestCIXUSBRejectsAmbiguousRoleZero(t *testing.T) {
	b, err := os.ReadFile("testdata/o6n-dsdt.aml")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte{0x93, 'G', 'E', 'T', 'V', 0x0a, 0x1c, 0}
	at := bytes.Index(b, key)
	if at < 0 {
		t.Fatal("role predicate missing")
	}
	b[at+6] = 0
	if _, err := parseUSBFirmware(b); err == nil {
		t.Fatal("dual-role offset zero accepted as host-only")
	}
}
