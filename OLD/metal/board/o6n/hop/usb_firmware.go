package hop

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// The CIX firmware describes ten native xHCI windows in its DSDT. Read only
// its static resource buffers and the two exact GETV-based _STA forms; this
// is deliberately not an AML interpreter. Unknown firmware is unsupported.
// Sources: CIX Sky1 Dsdt-USB.asl and Dsdt-AcpiRam.asl (cix_p1_dev).
// Despite CIX's USB_EHCI_HOST* macro names, USB0..3 are xHCI too:
// Sky1's DT labels their windows "xhci" under cdns,usbssp. See commit
// 57e018a398248d7e5e4d798610df79a557c0629f in Sky1-Linux/linux-sky1,
// patches-latest/0001-arm64-dts-cix-Add-Sky1-SoC-and-Radxa-Orion-O6-device.patch.
// The captured O6N DSDT likewise describes all ten with HID PNP0D10.
const usbHostCount = 10

type USBHost struct {
	Enabled                  bool
	Name                     string
	Base, Size               uint64
	EnableOffset, RoleOffset byte // RoleOffset 0 means host-only controller
}
type usbFirmware struct {
	base, size uint64
	hosts      [usbHostCount]USBHost
}

func usbInteger(b []byte) (uint64, int, bool) {
	if len(b) == 0 {
		return 0, 0, false
	}
	switch b[0] {
	case 0:
		return 0, 1, true
	case 1:
		return 1, 1, true
	case 0x0a:
		if len(b) >= 2 {
			return uint64(b[1]), 2, true
		}
	case 0x0b:
		if len(b) >= 3 {
			return uint64(binary.LittleEndian.Uint16(b[1:])), 3, true
		}
	case 0x0c:
		if len(b) >= 5 {
			return uint64(binary.LittleEndian.Uint32(b[1:])), 5, true
		}
	case 0x0e:
		if len(b) >= 9 {
			return binary.LittleEndian.Uint64(b[1:]), 9, true
		}
	}
	return 0, 0, false
}
func usbNamedInteger(b []byte, name string) (uint64, bool) {
	key := append([]byte{8}, name...)
	i := bytes.Index(b, key)
	if i < 0 || bytes.Contains(b[i+len(key):], key) {
		return 0, false
	}
	v, _, ok := usbInteger(b[i+len(key):])
	return v, ok
}

// pkg returns a package body and its total length, including PkgLength.
func usbPkg(b []byte) ([]byte, int, bool) {
	if len(b) == 0 {
		return nil, 0, false
	}
	n := int(b[0]>>6) + 1
	if len(b) < n {
		return nil, 0, false
	}
	size := int(b[0] & 0x3f)
	if n > 1 {
		size = int(b[0] & 15)
		for i := 1; i < n; i++ {
			size |= int(b[i]) << uint(4+8*(i-1))
		}
	}
	if size < n || size > len(b) {
		return nil, 0, false
	}
	return b[n:size], size, true
}
func usbMethod(b []byte, name string) ([]byte, int, bool) {
	if len(b) < 2 || b[0] != 0x14 {
		return nil, 0, false
	}
	body, n, ok := usbPkg(b[1:])
	if !ok || len(body) < 5 || string(body[:4]) != name {
		return nil, 0, false
	}
	return body[4:], n + 1, true
}
func usbReadName(b []byte, name string) (uint64, int, bool) {
	if len(b) < 5 || b[0] != 8 || string(b[1:5]) != name {
		return 0, 0, false
	}
	v, n, ok := usbInteger(b[5:])
	return v, n + 5, ok
}
func parseUSBDevice(body []byte, index int) (USBHost, bool) {
	h := USBHost{Name: fmt.Sprintf("XHC%d", index)}
	if index >= 6 {
		h.Name = fmt.Sprintf("USB%d", index-6)
	}
	prefix := append([]byte(h.Name), []byte{8, '_', 'H', 'I', 'D', 0x0d, 'P', 'N', 'P', '0', 'D', '1', '0', 0}...)
	if !bytes.HasPrefix(body, prefix) {
		return h, false
	}
	body = body[len(prefix):]
	uid, n, ok := usbReadName(body, "_UID")
	if !ok || uid != uint64(index) {
		return h, false
	}
	body = body[n:]
	cca, n, ok := usbReadName(body, "_CCA")
	if !ok || cca != 0 {
		return h, false
	}
	body = body[n:]
	sta, n, ok := usbMethod(body, "_STA")
	if !ok {
		return h, false
	}
	body = body[n:]
	// Exact host-only / dual-role methods, with firmware-supplied GETV offsets.
	host := []byte{0, 0xa0, 0x0a, 'G', 'E', 'T', 'V', 0x0a, 0, 0xa4, 0x0a, 15, 0xa1, 3, 0xa4, 0}
	dual := []byte{0, 0xa0, 0x13, 0x90, 'G', 'E', 'T', 'V', 0x0a, 0, 0x93, 'G', 'E', 'T', 'V', 0x0a, 0, 0, 0xa4, 0x0a, 15, 0xa1, 3, 0xa4, 0}
	switch len(sta) {
	case len(host):
		host[8] = sta[8]
		if !bytes.Equal(sta, host) {
			return h, false
		}
		h.EnableOffset = sta[8]
	case len(dual):
		dual[9], dual[16] = sta[9], sta[16]
		if !bytes.Equal(sta, dual) {
			return h, false
		}
		h.EnableOffset, h.RoleOffset = sta[9], sta[16]
		if h.RoleOffset == 0 {
			return h, false
		}
	default:
		return h, false
	}
	crs, n, ok := usbMethod(body, "_CRS")
	if !ok || n != len(body) || len(crs) < 7 || !bytes.Equal(crs[:7], []byte{8, 8, 'R', 'B', 'U', 'F', 0x11}) {
		return h, false
	}
	buffer, k, ok := usbPkg(crs[7:])
	if !ok || !bytes.Equal(crs[7+k:], []byte{0xa4, 'R', 'B', 'U', 'F'}) {
		return h, false
	}
	size, k, ok := usbInteger(buffer)
	if !ok || size != uint64(len(buffer)-k) {
		return h, false
	}
	resource := buffer[k:]
	// Memory32Fixed, one Extended Interrupt and EndTag; no extra windows.
	if len(resource) != 23 || !bytes.Equal(resource[:4], []byte{0x86, 9, 0, 1}) || !bytes.Equal(resource[12:17], []byte{0x89, 6, 0, 1, 1}) || resource[21] != 0x79 || resource[22] != 0 {
		return h, false
	}
	h.Base = uint64(binary.LittleEndian.Uint32(resource[4:]))
	h.Size = uint64(binary.LittleEndian.Uint32(resource[8:]))
	return h, h.Base != 0 && h.Base%4096 == 0 && h.Size >= 0x1000 && h.Size <= 0x10000
}
func parseUSBFirmware(b []byte) (usbFirmware, error) {
	var f usbFirmware
	fail := func() (usbFirmware, error) {
		return usbFirmware{}, fmt.Errorf("unsupported CIX USB firmware description")
	}
	var ok bool
	f.base, ok = usbNamedInteger(b, "GNVA")
	if !ok || f.base == 0 || f.base == 0xffffffff {
		return fail()
	}
	f.size, ok = usbNamedInteger(b, "GNVL")
	if !ok || f.size < 0x1f || f.size > 4096 || f.base > ^uint64(0)-f.size {
		return fail()
	}
	// Verify GETV's exact byte-read implementation and its bounds limit.
	getvKey := []byte{'G', 'E', 'T', 'V', 9, 0xa0, 8, 0x92, 0x95, 0x68, 0x0a, byte(f.size), 0xa4, 0, 0x72, 0x68, 'G', 'N', 'V', 'A', 0x60, 0x5b, 0x80, 'G', 'P', 'N', 'V', 0, 0x60, 1, 0x5b, 0x81, 0x0b, 'G', 'P', 'N', 'V', 1, 'V', 'A', 'R', 'V', 8, 0x99, 'V', 'A', 'R', 'V', 0x60, 0xa4, 0x60}
	if f.size > 255 || bytes.Count(b, getvKey) != 1 {
		return fail()
	}
	seen := [usbHostCount]bool{}
	for i := 36; i+3 < len(b); i++ {
		if b[i] != 0x5b || b[i+1] != 0x82 {
			continue
		}
		body, _, valid := usbPkg(b[i+2:])
		if !valid || len(body) < 4 {
			continue
		}
		index := -1
		switch {
		case string(body[:3]) == "XHC" && body[3] >= '0' && body[3] <= '5':
			index = int(body[3] - '0')
		case string(body[:3]) == "USB" && body[3] >= '0' && body[3] <= '3':
			index = 6 + int(body[3]-'0')
		}
		if index < 0 {
			continue
		}
		// Ignore similarly named children (for example USB0 with only _ADR).
		if !bytes.HasPrefix(body[4:], []byte{8, '_', 'H', 'I', 'D', 0x0d, 'P', 'N', 'P', '0', 'D', '1', '0', 0}) {
			continue
		}
		if seen[index] {
			return fail()
		}
		h, valid := parseUSBDevice(body, index)
		if !valid || uint64(h.EnableOffset) >= f.size || uint64(h.RoleOffset) >= f.size {
			return fail()
		}
		for j := 0; j < usbHostCount; j++ {
			if seen[j] && h.Base < f.hosts[j].Base+f.hosts[j].Size && f.hosts[j].Base < h.Base+h.Size {
				return fail()
			}
		}
		f.hosts[index] = h
		seen[index] = true
	}
	for _, yes := range seen {
		if !yes {
			return fail()
		}
	}
	return f, nil
}

// Keep all ten positions, including disabled controllers: their DMA slices
// must retain the same owners across kernel flips.
func (f usbFirmware) enabledHosts(values []byte) [usbHostCount]USBHost {
	hosts := f.hosts
	for i, h := range hosts {
		hosts[i].Enabled = int(h.EnableOffset) < len(values) && values[h.EnableOffset] != 0 && (h.RoleOffset == 0 || int(h.RoleOffset) < len(values) && values[h.RoleOffset] == 0)
	}
	return hosts
}
