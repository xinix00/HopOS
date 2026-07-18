package hop

import (
	"fmt"
	"time"

	"hop-os/metal/board/uefi"
	"hop-os/metal/driver/smpro"
)

// telemetr is het log-interval — zelfde cadans als de dvfs-telemetrie op de
// Pi, zodat één grep ("telemetry") beide platforms vangt.
const telemetr = 60 * time.Second

// StartTelemetry is het Altra-equivalent van de Pi's dvfs-telemetrieregel:
// elke minuut de SoC-temperatuur op de console. Alléén temperatuur — de klok
// is op servers firmware-domein, dus geen dvfs-beleid. De bron is de SMpro
// via zijn PCC-kanaal (metal/driver/smpro); de PCCT vertelt waar dat kanaal
// woont. Geen PCCT (QEMU virt) of geen antwoord: één melding en verder
// zonder — telemetrie is nooit een boot-blokker.
func StartTelemetry() {
	t := uefi.Tables()
	if t == nil {
		return // geen ACPI: dan was er ook geen boot; stil blijven
	}
	ss, ok := t.PCC(smpro.HwmonChannel)
	if !ok {
		fmt.Println("hwmon: no PCC hwmon channel in PCCT - temperature telemetry off")
		return
	}
	// Shmem ligt in gereserveerd DRAM (vlak bereikbaar), de doorbell kan in
	// hoge SoC-MMIO wonen — zelfde luik als de ECAM's.
	if !uefi.MapHigh(ss.ShmemBase, ss.ShmemLen) || !uefi.MapHigh(ss.Doorbell, 8) {
		fmt.Println("hwmon: PCC channel unreachable - temperature telemetry off")
		return
	}
	d := smpro.New(smpro.HwmonChannel, ss)
	mC, ok := d.SoCTemp()
	if !ok {
		fmt.Println("hwmon: SMpro not answering on PCC channel - temperature telemetry off")
		return
	}
	// ASCII-only: het fb-scherm degradeert multibyte (zoals de graden-ring)
	// naar '?'.
	fmt.Printf("hwmon: SoC %d.%dC (SMpro, PCC channel %d) - telemetry every 60s\n",
		mC/1000, mC%1000/100, smpro.HwmonChannel)
	go func() {
		for {
			time.Sleep(telemetr)
			if mC, ok := d.SoCTemp(); ok {
				fmt.Printf("hwmon: telemetry - SoC %d.%dC\n", mC/1000, mC%1000/100)
			}
		}
	}()
}
