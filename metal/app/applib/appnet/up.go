package appnet

// up.go — de netstack van een app: leannet (xinix00/lean), sinds 12-08. Eén
// budget uit de slot-partitie dat de stack zelf indeelt — groeien bij gebruik,
// floor per verbinding, luid weigeren als de pot leeg is; geen buffer-maten
// per job meer.
//
// Up() is de hele app-API: de zes callsites (welcome, vitals, cloudflared,
// hello, appspike) zien nooit welke stack eronder zit. Dat is wat de wissels
// van 09-08 (gVisor → lneto) en 12-08 (→ leannet) goedkoop maakte.

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/lean/leannet"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/ring"
	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/cpu/idle"
)

// Up brengt de eigen netstack op en hangt hem in Go's net-package; geeft het
// eigen IP terug. Alle config is afgeleid uit het slotnummer via het gedeelde
// net-plan (layout) — HOP hoeft niets per slot door te geven.
func Up(a *applib.App) (string, error) {
	ip := layout.SlotIP4(a.Slot)
	host := layout.HostIP4()

	nd := &nic{
		tx: ring.Open(layout.NetRingTX(a.Slot)),
		rx: ring.Open(layout.NetRingRX(a.Slot)),
	}

	// Het budget: 1/8 van de partitie, geklemd. Een welcome-app van 16MB
	// buffert dan tot 2MB, een dikke server-partitie tot 16MB — zonder dat
	// iemand een getal per job kiest. De floor per verbinding is ~4KiB, dus
	// een idle app kost vrijwel niets meer (de oude preallocatie was
	// MaxListenerConns×2×32KiB = 512KB per listener, óók bij nul verkeer).
	budget := int(a.RAMSize) / 8
	if budget < 1<<20 {
		budget = 1 << 20
	}
	if budget > 16<<20 {
		budget = 16 << 20
	}

	cfg := leannet.Config{
		IP:     ip4bytes(ip),
		Prefix: layout.NetPrefix,
		MAC:    layout.SlotMAC(a.Slot),
		GW:     ip4bytes(host),
		Budget: budget,
	}
	cfg.AdvWS = wsShiftFor(budget / 4)

	st := leannet.NewStack(nd, cfg, uint32(time.Now().UnixNano())^uint32(a.Slot)<<16)

	// Het interne net is deterministisch (layout nummert IP's én MAC's per
	// slot), dus er wordt NIETS geresolved: de gateway (10.100.0.1 = de node)
	// als statische buur — dials naar de node en listener-antwoorden hebben
	// nul ARP nodig. ICMP-echo zit standaard in de stack.
	if err := st.SeedNeighbor(ip4bytes(host), layout.SlotMAC(0)); err != nil {
		return "", fmt.Errorf("seed gateway neighbor: %w", err)
	}

	// In Go's standaard net-package hangen: hierna werken net.Listen en
	// net.Dial voor deze app. Interne IP's zijn deterministisch (geen DNS
	// nodig); voor uitgaand verkeer krijgt de app de node-resolver via HOP_DNS
	// mee (queries lopen als UDP door HOP's masquerade).
	net.SocketFunc = st.Socket
	if dns := a.Env("HOP_DNS"); dns != "" {
		net.SetDefaultNS([]string{dns})
	}

	// RX-lus met microslaap i.p.v. een Gosched-spin: een idle job laat zo
	// zijn hele core slapen (zie metal/cpu/idle). Hoe lang die slaap is,
	// bepaalt rxPoll (default = de vaste 300µs die hier altijd stond) — maar
	// de slaap is sinds de doorbell een BODEM, geen latency meer: de
	// idle-governor wapent een wek-drempel op de control-page en wekt deze
	// goroutine (runtime.WakeSleeper) zodra de switcher of een eigen idle-ronde
	// verkeer ziet (idle/rxdoor.go). De cap mag dus groot.
	idle.WatchRXRing(
		layout.SlotControl(a.Slot)+layout.CtrlRXDoor,
		nd.rx.HeadPending)
	lo, hi, hold := rxPoll(a.Env("RXPOLL"))
	go func() {
		gp, _ := runtime.GetG()
		idle.RXPumpG(gp) // dit is de goroutine die de bel wekt
		buf := make([]byte, leannet.MTU+leannet.EthernetMaximumSize)
		d, empty := lo, 0
		corruptLogged := false
		for {
			n, err := nd.Receive(buf)
			if n == 0 || err != nil {
				// Een dode ring is stil: ReadInto geeft niets meer, HeadPending
				// blijft "ja". Eén regel met de reden, anders is dat een
				// app die "gewoon niet reageert" (SMP-jacht 03-09).
				if !corruptLogged {
					if why := nd.rx.CorruptWhy(); why != "" {
						a.Logf("appnet: RX ring corrupt — %s", why)
						corruptLogged = true
					}
				}
				time.Sleep(d)
				// Niets gevonden. De eerste `hold` lege rondes blijven op lo —
				// dát is het venster waarin het antwoord van een lopend gesprek
				// hoort te komen — en pas daarna zakken we terug naar hi. Bij
				// lo == hi is dit de vaste slaap van vroeger.
				if empty++; empty > hold {
					d *= 2
					if d > hi {
						d = hi
					}
				}
				continue
			}
			d, empty = lo, 0 // verkeer: meteen weer scherp staan
			st.RecvInboundPacket(buf[:n])
		}
	}()
	// De stack bewaren voor WatchStats: één per app, en de tellers zijn
	// precies wat een veld-jacht nodig heeft (zie de spin/stilte-jacht 15-08).
	current = st
	a.NetworkReady()

	return layout.IP4Str(ip), nil
}

// rxPoll leest de RX-slaapstand uit de job-env: hoe lang de RX-lus wacht als
// er niets binnenkwam. Twee vormen, en de eerste is de stand van vóór dit
// knopje:
//
//	""                 de default: "300us:1s:4"
//	"300us"            vaste slaap (lo == hi) — het gedrag van vóór 29-08
//	"300us:5ms"        NAPI-achtig: verdubbelen tot hi zolang het stil is,
//	                   terug naar lo zodra er een frame binnenkomt
//	"300us:5ms:8"      idem, maar de eerste 8 lege rondes blijven op lo
//
// De default is GEMETEN (schedbench, 29-08): de doorbell draagt de latency
// (koud p50 0,8ms bij een cap van 10 SECONDEN), dus de cap is alleen nog het
// vangnet voor een gedoofde bel. Waarom dan 1s en niet groter: de heartbeat
// wekt elke app toch al ~1×/s, dus een grotere cap levert nul minder wekken
// op — 1s hangt gratis onder die vloer en begrenst een bel-storing op 1s.
// De vaste 300µs van vroeger (3.333 rondes/s, op een gedeelde core elk een
// context-wissel) is de "300us"-stand: de ontsnappingsklep, geen default meer.
func rxPoll(s string) (lo, hi time.Duration, hold int) {
	lo, hi, hold = 300*time.Microsecond, time.Second, 4
	if s == "" {
		return lo, hi, hold
	}
	hi, hold = lo, 0
	f := strings.Split(s, ":")
	if d, err := time.ParseDuration(f[0]); err == nil && d > 0 {
		lo, hi = d, d
	}
	if len(f) > 1 {
		if d, err := time.ParseDuration(f[1]); err == nil && d >= lo {
			hi = d
		}
	}
	if len(f) > 2 {
		if n, err := strconv.Atoi(f[2]); err == nil && n > 0 {
			hold = n
		}
	}
	return lo, hi, hold
}

// current is de stack van deze app (één per slot); gezet door Up.
var current *leannet.Stack

// WatchStats logt de netstack-tellers periodiek via logf — het meetinstrument
// voor "Manage zwijgt maar de task leeft": een leeggelopen pot
// (RefusedNoBudget), een poorteloos-drop-lek (DropNoPort) of een
// reply-wachtrij die volstroomt (DropReplyFull) is anders van buiten
// onzichtbaar. Bewust via de log-ring (het task-log haalt hem op), en alleen
// als er iets veranderd is: een stille node blijft stil.
func WatchStats(logf func(string, ...any), every time.Duration) {
	st := current
	if st == nil || logf == nil {
		return
	}
	go func() {
		var last leannet.Stats
		for {
			time.Sleep(every)
			now := st.Stats()
			if now != last {
				logf("netstats: refusedNoBudget=%d dropNoPort=%d dropBadFrame=%d dropReplyFull=%d arp{gaveUp=%d learnDrop=%d fullDrop=%d}",
					now.RefusedNoBudget, now.DropNoPort, now.DropBadFrame, now.DropReplyFull,
					now.ARP.GaveUp, now.ARP.LearnDrop, now.ARP.FullDrop)
				last = now
			}
		}
	}()
}

// JoinMulticast abonneert de app-stack op een link-local IPv4- of IPv6-groep
// (mDNS, matter-discovery), voor de rest van het app-leven — er is bewust
// geen Leave (geKAMd 15-08: de enige gebruiker leaved nooit, en een
// refcount die niemand gebruikt is alleen maar reviewvlak). De socket-API
// kent geen groepen — tamago heeft geen syscalls om x/net's
// ipv4/ipv6-besturing op te bouwen — dus dit is de expliciete knop naast
// net.ListenUDP. Alleen 224.0.0.0/24 en ff02::/16; idempotent.
func JoinMulticast(ip net.IP) error {
	st := current
	if st == nil {
		return errors.New("appnet: netstack is not up")
	}
	if v4 := ip.To4(); v4 != nil {
		return st.JoinGroup([4]byte(v4))
	}
	v6 := ip.To16()
	if v6 == nil {
		return errors.New("appnet: multicast join needs an IP group")
	}
	return st.JoinGroup6([16]byte(v6))
}

// ip4bytes zet layout's uint32-adres om naar de [4]byte-vorm van leannet.
func ip4bytes(ip uint32) [4]byte {
	return [4]byte{byte(ip >> 24), byte(ip >> 16), byte(ip >> 8), byte(ip)}
}

// wsShiftFor geeft de kleinste window-scale-shift waarmee een venster van
// maxBuf bytes te adverteren is (RFC 7323, plafond shift 14).
func wsShiftFor(maxBuf int) uint8 {
	var shift uint8
	for shift < 14 && 0xffff<<shift < maxBuf {
		shift++
	}
	return shift
}
