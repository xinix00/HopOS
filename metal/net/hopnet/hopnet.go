// Package hopnet brengt het netwerk van de HOP-kern op: de NIC-driver van
// het board onder de netstack, gehookt in Go's standaard net-package. Daarna
// werken net.Listen, net.Dial en net/http gewoon — precies wat de HOP-agent
// nodig heeft. Dit is "geen video, wel een poort".
//
// De stack is leannet (xinix00/lean, sinds 12-08); de opbouw staat in
// stack.go, het device-contract in metal/net/netdev, en de afwegingen in
// lean/leannet/DESIGN.md.
//
// Historie van dit bestand, want het verklaart de vorm: tot 09-08 stond hier
// gVisor (~90k gelinkte regels), daarna lneto via go-net, en sinds 12-08 onze
// eigen stack. Elke wissel raakte precies twee bestanden (stack.go en de
// stack-import) omdat álles eromheen aan het twee-methode-device hangt:
// HandleLocal en de tweede interne NIC zitten op de device-naad (locnet.go +
// internal.go), niet in de stack.
package hopnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/v2/net/netdev"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
	"github.com/xinix00/lean/leandhcp"
)

// rxIdle telt de lege rondes van de RX-lus: hoe vaak HOP keek en niets vond.
// Gepold is dat ~3.333/s bij stilte, op de interrupt ~100/s (de vangrail).
// Dit is de meetlat van de RX-lus die óók op QEMU klopt — de governor-wekken
// (idle.Wakes) zijn daar ruis, want WFE is op TCG een no-op.
var rxIdle atomic.Uint64

// RXIdleRounds geeft de teller (hopos.idlestat in cmd/hopos leest hem).
func RXIdleRounds() uint64 { return rxIdle.Load() }

// ForcePoll dwingt de RX-lus in de poll-stand, ook op een board met een
// NIC-interrupt (hopos.rxpoll=1): de A/B-knop voor de meting én de veiligheids-
// klep als een interrupt-bedrading op een board niet deugt. Zetten vóór Up.
var ForcePoll bool

// Up initialiseert de NIC en de netstack en hangt ze in het net-package. Het
// IP-plan en de NIC-probe komen van het actieve board (op QEMU de slirp-
// defaults; op echt ijzer straks een board met DHCP/DT).
func Up() error {
	// Het board levert een kant-en-klaar netdev.Device (driver + init zijn
	// board-kennis); hopnet weet niet welke NIC dit is. ProbeNIC vóór Net():
	// op echt ijzer (Pi 5) haalt de probe zelf de DHCP-lease die Net() daarna
	// rapporteert — die volgorde is het contract.
	nic, hw, err := board.Current().ProbeNIC()
	if err != nil {
		return fmt.Errorf("nic: %w", err)
	}
	if nic == nil {
		return fmt.Errorf("no NIC found")
	}
	mac := hw.String()
	nc := board.Current().Net()

	// De NIC achter de NAT-shim van de switch: inbound frames voor
	// gepubliceerde poorten of lopende masquerade-flows worden vóór HOP's
	// stack afgevangen. De CIDR (niet alleen het IP) mee, zodat de NAT weet
	// wat "off-subnet" is (dan is de next-hop de gateway).
	uplink, err := hopswitch.WrapUplink(nic, nc.CIDR, hw)
	if err != nil {
		return err
	}

	// Het lokale-verkeer-schilletje om de uplink (locnet.go): self-dial op
	// het eigen IP (agent ↔ leader op dezelfde node — gvisor's HandleLocal,
	// nu op de device-naad), ARP naar het eigen adres, en de 1:1-vertaling
	// van het interne subnet. Het IP als getal is de sleutel van die naad.
	ip4 := net.ParseIP(nc.IP).To4()
	if ip4 == nil {
		return fmt.Errorf("node IP %q is not IPv4", nc.IP)
	}
	internal, err := hopswitch.HostDevice()
	if err != nil {
		return err
	}
	loc := &locdev{nic: uplink, internal: internal, ip: binary.BigEndian.Uint32(ip4)}
	copy(loc.mac[:], hw)

	// De stack zelf (stack.go): die hangt ook net.SocketFunc; wat terugkomt
	// is de RX-voeding voor rxLoop.
	recv, err := stackUp(loc, nc, mac)
	if err != nil {
		return err
	}

	// De interne gateway-naad: 10.100.0.1 = "mijn node" voor de apps — de
	// agent/leader zijn dan van binnenuit bereikbaar zonder proxy (zie
	// internal.go).
	upInternal(loc)

	net.SetDefaultNS([]string{nc.DNS})

	// RX-lus: op de NIC-interrupt waar het board die heeft (board.NICInterrupter
	// — de doorbell voor HOP's eigen core), anders pollen mét microslaap i.p.v.
	// een Gosched-spin, zodat de idle-governor (metal/cpu/idle) de core echt
	// kan laten slapen als het stil is; onder last wordt er nooit geslapen
	// (ring leeg = pas dan slapen). Over de locdev, zodat loopback- en
	// gateway-frames vóór de NIC gaan.
	waiter, _ := board.Current().(board.NICInterrupter)
	if ForcePoll {
		waiter = nil
	}
	if waiter != nil {
		fmt.Println("net: RX wakes on the NIC interrupt")
	} else {
		fmt.Println("net: RX polled every 300µs (no NIC interrupt on this board, or hopos.rxpoll=1)")
	}
	events := make(chan struct{}, 1)
	signal := func() {
		select {
		case events <- struct{}{}:
		default:
		}
	}
	hopswitch.SetHostWake(signal)
	go rxLoop(loc, recv, waiter, events, signal)

	// DHCP-lease levend houden: heeft dit board een verkregen lease (de Pi's),
	// dan vernieuwt KeepAlive hem op T1 via de netstack (UDP-RENEW) — dat kan
	// pas nú, want het leunt op net.SocketFunc hierboven en op rxLoop die de
	// stack voedt. Boards met statische config (qemuvirt) zijn geen LeaseHolder
	// en slaan dit over.
	if lh, ok := board.Current().(board.LeaseHolder); ok {
		if l, has := lh.DHCPLease(); has {
			var m [6]byte
			copy(m[:], hw)
			// Eigen recover: KeepAlive verwerkt DHCP-antwoorden van het LAN
			// (onvertrouwde inhoud) op een eigen goroutine — een panic daar zou
			// de hele node vellen. Zonder lease-vernieuwing blijft de node
			// werken tot de lease verloopt; dat is oneindig veel beter dan dood.
			go func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("HOPOS_DHCP_PANIC: %v — lease renewal stopped, node keeps running\n", r)
					}
				}()
				keepLease(m, l)
			}()
		}
	}

	fmt.Printf("net: %s (mac %s, gw %s) — HOPOS_NET_UP\n", nc.IP, mac, nc.GW)
	return nil
}

// rxLoop pompt frames van het device naar de stack; recv is de
// ingress-voeding die stack.go teruggaf.
//
// Met een waiter is de lus: ring leegpompen, wachten op de interrupt. De
// vangrail van 10ms is er omdat een verloren flank nooit een hang mag worden
// (cpu/irq) — hij kost bij stilte 100 wekmomenten per seconde in plaats van
// 3.333, en onder verkeer wordt hij nooit geraakt.
func rxLoop(nic netdev.Device, recv func([]byte) error, waiter board.NICInterrupter, events <-chan struct{}, signal func()) {
	buf := make([]byte, netdev.MTU+netdev.EthernetMaximumSize)
	if waiter != nil {
		go func() {
			for {
				waiter.WaitNIC(10 * time.Millisecond)
				signal()
			}
		}()
	}
	// Zonder interrupt (waiter == nil; Apple tot AIC/MSI er is) blijft de NIC
	// zelf gepold, in de praktijk op de klok van de idle-governor (~1ms event
	// stream); het events-kanaal wekt eerder voor LAN-poort 0. Eén hergebruikte
	// timer: time.After alloceerde er ~3.000 per seconde op de M4.
	const nicPoll = 300 * time.Microsecond
	var poll *time.Timer
	if waiter == nil {
		poll = time.NewTimer(nicPoll)
	}
	flusher, _ := nic.(netdev.Flusher)
	rearm, _ := nic.(netdev.IRQRearmer)
	if flusher != nil {
		flusher.Batch(true) // vanaf hier flusht deze pomp (RX) en de switch/locdev (TX)
	}
	burst := 0
	delivered := false
	for {
		if rxPass(nic, recv, buf) {
			delivered = true
			// Batching: de RX-doorbell (RDT/mailbox) niet per frame maar per
			// burst, en tussendoor om de 32 frames zodat de NIC nooit zonder
			// descriptors komt te zitten.
			if burst++; burst >= 32 && flusher != nil {
				flusher.FlushRX()
				burst = 0
			}
			continue
		}
		if flusher != nil {
			if burst > 0 {
				flusher.FlushRX()
				burst = 0
			}
			flusher.FlushTX() // wat een zender zonder eigen flush achterliet (dhcp-renew) gaat nu de draad op
		}
		if delivered {
			// De burst is bij de apps bezorgd: hun antwoorden (ACK's) liggen zo
			// in hun TX-ringen. De switch nú wekken, niet pas bij HOP's idle of
			// de failsafe — dat is de RTT van elke verbinding door deze node.
			hopswitch.Kick()
			delivered = false
		}
		rxIdle.Add(1)
		if waiter != nil {
			// De interrupt pas hier weer openen, ná een lege ronde — waar
			// Linux' NAPI-poll dat ook doet (tg3_int_reenable, napi_complete).
			// Niet in de waiter-goroutine: die liep vóór elke wacht "open +
			// werk-check + forceer" terwijl deze pomp nog aan het legen was,
			// en dat is een interrupt per frame bovenop de pomp, tot de
			// watchdog (M4, bundel 35, 20-09). Werk dat tijdens het gesloten
			// masker binnenkwam vangt de driver in zijn RearmIRQ.
			if rearm != nil {
				rearm.RearmIRQ()
				// Eén ronde ná het openen: een frame dat tussen de laatste lege
				// ronde en het openen viel maakt op een flank-chip geen
				// interrupt meer (rtl8126) — napi_complete → re-enable →
				// "nog werk? dan opnieuw", in die volgorde.
				if rxPass(nic, recv, buf) {
					delivered = true
					burst++
					continue
				}
			}
			<-events
			continue
		}
		poll.Reset(nicPoll)
		select {
		case <-events:
		case <-poll.C:
		}
	}
}

// rxPass doet één ontvangst-ronde, mét recover — het spiegelbeeld van
// hopswitch.switchPass. Dit is het meest blootgestelde pad van de node: ruwe
// frames van de fysieke NIC, remote en zonder authenticatie, en ze lopen via
// Uplink.Receive tot in de NAT (arpLearn, DNAT, checksum-herschrijving) en de
// netstack. Een panic op frame-inhoud zou hier de RX-goroutine velen, en dat
// is de hele node — álle slots. Frame gedropt, lus draait door; na een panic
// slaapt de lus zijn normale tikje, zodat een panic-storm de core niet opeet.
// Stack-fouten worden bewust niet geprint: dit is de broadcast-ruis van een
// echt LAN (mDNS, SSDP, router advertisements) en de console is een busy-wait
// per byte — GEMETEN 11-08: een LicheeRV ging er na ~250s aan dood. Meten doen
// we met netmeter/vitals, niet in het datapad.
func rxPass(nic netdev.Device, recv func([]byte) error, buf []byte) (worked bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_RX_PANIC: %v — frame dropped, RX keeps running\n", r)
		}
	}()
	n, err := nic.Receive(buf)
	if n == 0 || err != nil {
		return false
	}
	_ = recv(buf[:n])
	return true
}

// AddressLost: het board/de kern beslist wat er gebeurt als het adres van
// een draaiende node niet meer van ons is (DHCPNAK, ander adres aangeboden).
// De stack kan niet van adres wisselen; HopOS' antwoord is een herstart
// (cmd/hopos: de canary houdt zijn pets in). Nil = alleen melden.
var AddressLost func(reason string)

// keepLease houdt de lease in leven zolang de node leeft. leandhcp.KeepAlive
// geeft op als de lease verlopen is zonder antwoord ("a reboot acquires a new
// address") — dat was een dode node na elke netwerkonderbreking die langer
// duurde dan de lease (19-09, Derek). Hier: verlopen zonder antwoord → blijven
// rebinden per broadcast, met oplopende pauze tot een minuut, tot een server
// hetzelfde adres bevestigt (dan gewoon verder) of het weigert (dan AddressLost).
// Het adres blijft ondertussen in gebruik: een lease die niemand bevestigt is
// een netwerk zonder server, geen adresconflict.
func keepLease(mac [6]byte, l leandhcp.Lease) {
	for {
		leandhcp.KeepAlive(mac, l)
		// KeepAlive keerde terug: verlopen, geweigerd of verhuisd. Eén rebind
		// zegt welke van de drie het is — NAK is refused, een ander adres is
		// verhuisd, geen antwoord is "geen server".
		wait := 5 * time.Second
		for attempt := 1; ; attempt++ {
			fresh, err := leandhcp.Rebind(l, mac, 5*time.Second)
			if err == nil {
				if fresh.IP != l.IP {
					lost(fmt.Sprintf("dhcp: server now offers %s instead of %s", fresh.IPString(), l.IPString()))
					return
				}
				fmt.Printf("dhcp: lease on %s reacquired after %d attempt(s), %ds to go HOPOS_DHCP_REACQUIRED\n", l.IPString(), attempt, fresh.LeaseSecs)
				l = fresh
				break
			}
			// Een NAK herkennen we voorlopig aan de tekst: de gepubliceerde
			// lean (v1.1.1) exporteert leandhcp.ErrRefused nog niet. Zodra de
			// volgende lean-tag uit is: errors.Is(err, leandhcp.ErrRefused).
			if strings.Contains(err.Error(), "DHCPNAK") {
				lost(fmt.Sprintf("dhcp: %s refused by the server (%v)", l.IPString(), err))
				return
			}
			if attempt == 1 || attempt%12 == 0 {
				fmt.Printf("dhcp: no server confirms %s (%v) — keeping the address and retrying every %s (attempt %d) HOPOS_DHCP_RETRY\n", l.IPString(), err, wait, attempt)
			}
			time.Sleep(wait)
			if wait < time.Minute {
				wait += 5 * time.Second
			}
		}
	}
}

func lost(reason string) {
	if AddressLost != nil {
		AddressLost(reason)
		return
	}
	fmt.Printf("dhcp: %s — address lost, nothing wired to act on it HOPOS_DHCP_LOST\n", reason)
}
