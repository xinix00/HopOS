//go:build lnetonet

package appnet

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/soypat/lneto"
	"github.com/soypat/lneto/x/xnet"

	"hop-os/metal/abi/layout"
	"hop-os/metal/abi/ring"
	"hop-os/metal/app/applib"
)

// Bewust niet gnet.MTU: dit bestand mag go-net niet importeren (zie de
// package-doc), dus de framematen staan hier zelf. Zelfde waarden.
const (
	mtu        = 1500
	ethMaxSize = 18 // Ethernet-header + VLAN-tag
	ethMinSize = 14
)

// stackBackoff is de retry-cadans voor blokkerende stack-protocollen
// (ARP-resolve e.d.): exponentieel 100µs → 20ms, zodat een idle job zijn
// core echt laat slapen (zie metal/cpu/idle) maar een handshake vlot blijft.
func stackBackoff(consecutive uint) time.Duration {
	sleep := 100 * time.Microsecond << min(consecutive, 8)
	if sleep > 20*time.Millisecond {
		sleep = 20 * time.Millisecond
	}
	return sleep
}

// tcpBackoff is de per-verbinding read/write-retry: korter bereik
// (10µs → 1ms) om interactieve streams responsief te houden.
func tcpBackoff() lneto.BackoffStrategy {
	return func(consecutive uint) time.Duration {
		sleep := 10 * time.Microsecond << min(consecutive, 6)
		if sleep > time.Millisecond {
			sleep = time.Millisecond
		}
		return sleep
	}
}

// Up brengt de eigen netstack (lneto) op en hangt hem in Go's net-package;
// geeft het eigen IP terug. Zelfde contract als de gVisor-variant: alle
// config komt uit het slotnummer via het gedeelde net-plan (layout).
//
// Nog zonder nette-dood-hook (de gVisor-variant abort bij de kill zijn
// verbindingen zodat peers direct een RST zien): xnet.StackAsync heeft geen
// close-all-API, alleen Reset. Peers van een lneto-app vallen dus terug op
// hun eigen read-deadline (de SURF-display: 30s). Toevoegen zodra lneto het
// kan — of zodra deze backend default wordt.
//
// De wiring spiegelt go-net's lneto.go, maar dan zonder go-net: stack
// resetten, StackGo in net.SocketFunc hangen, en twee pomp-lussen over de
// frame-ringen (RX ring→stack, TX stack→ring). De TX-lus pollt EgressEthernet
// met dezelfde 300µs-microslaap als de RX-lus — lneto heeft geen
// notify-callback voor uitgaande frames, en deze cadans houdt een idle core
// slapend terwijl sub-ms latency intern ruim volstaat.
func Up(a *applib.App) (string, error) {
	ip := layout.IP4Str(layout.SlotIP4(a.Slot))
	pfx, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", ip, layout.NetPrefix))
	if err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
	}
	m := layout.SlotMAC(a.Slot)
	gw, _ := netip.ParseAddr(layout.IP4Str(layout.HostIP4()))

	nd := &nic{
		tx: ring.Open(layout.NetRingTX(a.Slot)),
		rx: ring.Open(layout.NetRingRX(a.Slot)),
	}

	var stack xnet.StackAsync
	err = stack.Reset(xnet.StackConfig{
		// Geen wall-clock of TRNG nodig voor het zaad: het stuurt alleen
		// poort/seq-randomisatie. Slot+tellerstand is per stack-start uniek.
		RandSeed:          int64(uint64(a.Slot+1)*0x9E3779B97F4A7C15) ^ time.Now().UnixNano(),
		MaxActiveTCPPorts: 16,
		Hostname:          "hopapp",
		HardwareAddress:   m,
		MTU:               mtu,
		ICMPQueueLimit:    32,
		StaticAddress4:    pfx.Addr().As4(),
	})
	if err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
	}
	res := xnet.DHCPResults{Subnet: pfx}
	if err := stack.AssimilateDHCPResults(&res); err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
	}
	stack.EnableICMP(true)

	gostack := stack.StackGo(stackBackoff, xnet.StackGoConfig{
		ListenerPoolConfig: xnet.TCPPoolConfig{
			PoolSize:  16,
			QueueSize: 8,
			// 16×MSS i.p.v. lneto's 3×MSS-default: het window bepaalt de
			// doorvoer (window/RTT) en de apploader trekt hier complete
			// app-images doorheen. 2×23KB per verbinding is in een eigen
			// partitie verwaarloosbaar.
			TxBufSize:          16 * (mtu - 40),
			RxBufSize:          16 * (mtu - 40),
			EstablishedTimeout: 4 * time.Second,
			ClosingTimeout:     2 * time.Second,
			NewBackoff:         tcpBackoff,
		},
	})

	// In Go's standaard net-package hangen — zelfde hook als de gVisor-variant.
	net.SocketFunc = gostack.Socket
	if dns := a.Env("HOP_DNS"); dns != "" {
		net.SetDefaultNS([]string{dns})
	}

	// Gateway-MAC resolven (ARP) zodra de pomp-lussen lopen; tot die tijd
	// vallen off-subnet-frames op de vloer (TCP herstelt).
	go func() {
		hw, err := stack.StackBlocking(stackBackoff).DoResolveHardwareAddress6(gw, 4*time.Second)
		if err == nil {
			stack.SetGatewayHardwareAddr(hw)
		}
	}()

	// RX: ring → stack.
	go func() {
		buf := make([]byte, mtu+ethMaxSize)
		for {
			n, err := nd.Receive(buf)
			if n == 0 || err != nil {
				time.Sleep(300 * time.Microsecond)
				continue
			}
			stack.IngressEthernet(buf[:n])
		}
	}()
	// TX: stack → ring.
	go func() {
		buf := make([]byte, mtu+ethMaxSize)
		for {
			n, _ := stack.EgressEthernet(buf)
			if n < ethMinSize {
				time.Sleep(300 * time.Microsecond)
				continue
			}
			nd.Transmit(buf[:n])
		}
	}()
	return ip, nil
}
