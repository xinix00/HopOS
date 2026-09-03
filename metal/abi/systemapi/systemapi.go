// Package systemapi definieert het ene app→HOP-callcontract. Het transport is
// een blijvende TCP-verbinding naar 10.100.0.1 over het gewone, geïsoleerde
// slot-LAN. ARM64 en RISC-V spreken exact deze bytes; alleen hun ring-doorbell
// onder het netwerk verschilt.
package systemapi

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	Version = 1
	Port    = 10100
	Address = "10.100.0.1:10100"

	KindCall   = 1
	KindResult = 2
	KindLog    = 3

	// MaxIOChunk amortiseert de protocol- en opslagkosten zonder ooit een heel
	// backupbestand in HOP-geheugen te hoeven houden.
	MaxIOChunk = 1 << 20
	MaxPayload = MaxIOChunk + 64<<10
)

const (
	magic     = uint32(0x53504f48) // "HOPS" little-endian op de wire
	headerLen = 12
)

// WriteFrame schrijft één begrensd, zelfbeschrijvend bericht.
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("systemapi: payload %d > %d", len(payload), MaxPayload)
	}
	var h [headerLen]byte
	binary.LittleEndian.PutUint32(h[0:], magic)
	h[4], h[5] = Version, kind
	binary.LittleEndian.PutUint32(h[8:], uint32(len(payload)))
	if err := writeAll(w, h[:]); err != nil {
		return err
	}
	return writeAll(w, payload)
}

// ReadFrame leest één bericht en weigert een versie-, type- of groottefout
// vóór er payloadgeheugen wordt gealloceerd.
func ReadFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var h [headerLen]byte
	if _, err = io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if binary.LittleEndian.Uint32(h[0:]) != magic {
		return 0, nil, fmt.Errorf("systemapi: bad magic")
	}
	if h[4] != Version {
		return 0, nil, fmt.Errorf("systemapi: version %d, want %d", h[4], Version)
	}
	n := int(binary.LittleEndian.Uint32(h[8:]))
	if n > MaxPayload {
		return 0, nil, fmt.Errorf("systemapi: payload %d > %d", n, MaxPayload)
	}
	p := make([]byte, n)
	if _, err = io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	return h[5], p, nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
