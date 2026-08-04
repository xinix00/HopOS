package hopnet

// SNTP-client (RFC 4330, minimaal): één UDP-pakket om de klok te zetten.
// Zonder RTC boot HopOS op 1 januari 1970; TLS-certificaatvalidatie (S3,
// https) vereist echte tijd. HOP's eigen HMAC-auth is bewust klok-vrij,
// dus een node zonder bereikbare NTP-server blijft functioneren — alleen
// TLS faalt dan zichtbaar.

import (
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/board"
)

// ntpEpochOffset: seconden tussen 1900-01-01 (NTP) en 1970-01-01 (Unix).
const ntpEpochOffset = 2208988800

// SyncTime haalt de tijd op bij een NTP-server en zet de systeemklok
// (tamago TimerOffset — de generieke teller is gedeeld over alle cores, dus
// slots geeft dezelfde offset door aan elke app). Alleen bij boot aanroepen:
// de klok springt vooruit.
func SyncTime(server string) error {
	var lastErr error
	for range 3 {
		if err := syncOnce(server); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return lastErr
}

func syncOnce(server string) error {
	conn, err := net.Dial("udp4", server)
	if err != nil {
		return fmt.Errorf("sntp: %w", err)
	}
	defer conn.Close()

	req := make([]byte, 48)
	req[0] = 0x23 // LI=0, VN=4, Mode=3 (client)
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("sntp: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return fmt.Errorf("sntp: %w", err)
	}
	resp := make([]byte, 48)
	if n, err := conn.Read(resp); err != nil || n < 48 {
		return fmt.Errorf("sntp: antwoord %d bytes, %v", n, err)
	}

	// Transmit-timestamp (bytes 40-47): 32.32 fixed-point sinds 1900.
	secs := uint64(resp[40])<<24 | uint64(resp[41])<<16 | uint64(resp[42])<<8 | uint64(resp[43])
	frac := uint64(resp[44])<<24 | uint64(resp[45])<<16 | uint64(resp[46])<<8 | uint64(resp[47])
	// Onder de NTP→Unix-epoch weigeren, niet alleen 0: `secs-ntpEpochOffset`
	// underflowt anders op uint64 en zet de wandklok absurd ver weg. De klok is
	// wat TLS-certificaten valideert (S3, artifact-downloads), dus een
	// onzin-antwoord van een gespoofte of kapotte server moet hier stoppen.
	if secs <= ntpEpochOffset {
		return fmt.Errorf("sntp: onzin-timestamp %d van %s (vóór de Unix-epoch)", secs, server)
	}
	ns := int64(secs-ntpEpochOffset)*int64(time.Second) + int64(frac*1e9>>32)

	board.Current().SetWallTime(ns)
	return nil
}
