package slots

// De system service is het control/data-kanaal van een app naar HOP. Hij
// draait bewust op het gewone interne LAN: netwerkisolatie bepaalt de peer,
// de peer bepaalt het slot en de bestaande servicer bepaalt diens root en
// mounts. Er is dus geen tweede identity-, storage- of architectuurpad.

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/systemapi"
)

// maxSystemConns begrenst de open system-callverbindingen per app-lifecycle.
// ReadFrame alloceert per frame tot MaxPayload (ruim 1MiB) op HOP's heap, en
// elke verbinding kost de node-stack zijn buffers; zonder cap laat één app met
// N verbindingen HOP's heap N MiB groeien tot de kern OOM't — en dan vallen
// álle slots. Dezelfde isolatiegrens als maxNodes in kern/hopfs. applib houdt
// er één open; de tweede is voor een herverbinding waarvan HOP de FIN van de
// oude nog niet zag.
const maxSystemConns = 2

// ServeSystem luistert op HOP's vaste interne servicepoort. Aanroepen nadat
// hopnet.Up het standaard net-package aan de node-stack heeft gehangen.
func ServeSystem() error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(systemapi.Port))
	if err != nil {
		return err
	}
	fmt.Printf("system: app calls on %s (HOPOS_SYSTEM_UP)\n", systemapi.Address)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		slot, ok := slotFromRemote(conn.RemoteAddr())
		if !ok {
			_ = conn.Close()
			continue
		}
		svcMu.Lock()
		s := servicers[slot]
		svcMu.Unlock()
		if s == nil {
			_ = conn.Close()
			continue
		}
		if s.sysConns.Add(1) > maxSystemConns {
			s.sysConns.Add(-1)
			_ = conn.Close()
			continue
		}
		go func() {
			defer s.sysConns.Add(-1)
			serveSystemConn(conn, s)
		}()
	}
}

func slotFromRemote(addr net.Addr) (int, bool) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return 0, false
	}
	ip := net.ParseIP(strings.Trim(host, "[]")).To4()
	if ip == nil {
		return 0, false
	}
	v := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	slot := int(v&0xff) - 1
	if slot < 1 || slot > layout.MaxSlots || layout.SlotIP4(slot) != v {
		return 0, false
	}
	return slot, true
}

func serveSystemConn(conn net.Conn, s *servicer) {
	defer conn.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-s.stop:
			_ = conn.Close()
		case <-done:
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_SYSTEM_PANIC slot %d: %v\n", s.slot, r)
		}
	}()
	for {
		kind, payload, err := systemapi.ReadFrame(conn)
		if err != nil {
			return
		}
		// Een verbinding hoort bij één lifecycle. Een oude app die na een
		// herstart wonderbaarlijk nog bytes kan sturen, krijgt nooit de nieuwe
		// eigenaar van hetzelfde IP cadeau.
		svcMu.Lock()
		current := servicers[s.slot] == s
		svcMu.Unlock()
		if !current {
			return
		}
		switch kind {
		case systemapi.KindCall:
			resp := s.handleWithLimit(payload, systemapi.MaxIOChunk)
			if err := systemapi.WriteFrame(conn, systemapi.KindResult, resp); err != nil {
				return
			}
		case systemapi.KindLog:
			s.recordLog(string(payload))
		default:
			return
		}
	}
}

func (s *servicer) recordLog(line string) {
	diagMu.Lock()
	lastLog[s.slot] = line
	diagMu.Unlock()
	select {
	case s.logs <- line:
	default:
	}
}
