// Package nodemac leidt het MAC-adres van een node af uit zijn config.
//
// Waarom dit een eigen pakket is en niet per board: het probleem is niet
// board-specifiek. Elk bordje zonder MAC in een leesbare fuse heeft precies
// dezelfde drie eisen, en die zijn met elkaar in spanning:
//
//   - STABIEL over reboots. Een willekeurig adres kost bij iedere herstart een
//     nieuwe DHCP-lease en een nieuwe hopdns-registratie.
//   - UNIEK per node. Eén vaste constante laat twee bordjes van hetzelfde type
//     op één LAN botsen — precies de gemengde fleet waar ze voor bedoeld zijn.
//   - ZONDER nieuwe bron. Adressen van efuse-blokken gokken is hoe je een board
//     stilzet, en de node-naam staat toch al in de config.
//
// Daarom: expliciete hopos.mac heeft voorrang, anders volgt het adres uit
// hopos.node. Dezelfde naam geeft hetzelfde adres, verschillende namen
// verschillende — en node-namen verschillen per definitie, want dat ís de
// identiteit van een node in het cluster.
//
// Het voorvoegsel is 02:48:4f:50 — locally administered (bit 1 van het eerste
// byte) met "HOP" in ASCII erachter, zodat een node van ons herkenbaar is in een
// ARP-tabel.
package nodemac

import "fmt"

// Prefix is de vaste kop van elk HopOS-node-adres.
var Prefix = [4]byte{0x02, 0x48, 0x4f, 0x50}

// Fallback is het adres als er géén mac én géén node in de config staat. De
// enige stand waarin twee bordjes elkaar in de weg kunnen zitten — Identity
// waarschuwt er dan ook over.
var Fallback = [6]byte{0x02, 0x48, 0x4f, 0x50, 0x00, 0x01}

// Identity geeft het MAC-adres voor deze node. mac ("aa:bb:cc:dd:ee:ff") heeft
// voorrang; anders wordt het uit node afgeleid. board komt alleen in de
// waarschuwing terecht, zodat die zegt wélk soort bordje gaat botsen.
func Identity(mac, node, board string) [6]byte {
	if m, ok := Parse(mac); ok {
		return m
	}
	if node == "" {
		fmt.Printf("net: WARNING — no hopos.mac and no hopos.node: falling back to the built-in MAC. A second %s on this LAN will collide. HOPOS_MAC_FIXED\n", board)
		return Fallback
	}
	// FNV-1a over de naam, de onderste twee bytes eruit. Geen crypto nodig: het
	// enige dat telt is dat verschillende namen verschillende adressen geven en
	// dezelfde naam hetzelfde.
	h := uint32(2166136261)
	for i := 0; i < len(node); i++ {
		h = (h ^ uint32(node[i])) * 16777619
	}
	m := Fallback
	copy(m[:4], Prefix[:])
	m[4] = byte(h >> 8)
	m[5] = byte(h)
	return m
}

// Parse leest "aa:bb:cc:dd:ee:ff". Faalt stil (ok=false) zodat een typefout in
// de config terugvalt op de naam-afleiding in plaats van de node zonder netwerk
// te zetten — met de naam erbij is dat nog altijd een uniek adres.
func Parse(s string) ([6]byte, bool) {
	var m [6]byte
	if len(s) != 17 {
		return m, false
	}
	for i := range m {
		hi, ok1 := hexNibble(s[i*3])
		lo, ok2 := hexNibble(s[i*3+1])
		if !ok1 || !ok2 || (i < 5 && s[i*3+2] != ':') {
			return m, false
		}
		m[i] = hi<<4 | lo
	}
	return m, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
