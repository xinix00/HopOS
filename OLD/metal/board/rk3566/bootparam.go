package rk3566

import (
	"sync"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/fw/bootcfg"
	"github.com/xinix00/HopOS/metal/v2/fw/fdt"
)

// De platform-config van dit board komt uit twee kanalen die BEIDE gemeten zijn
// op 05-08 (zie docs/archief/radxa-zero3.md) — precies het Pi-mechanisme, andere
// bron:
//
//   - het bestand dat U-Boot als INITRD laadde (extlinux `initrd /hopos.cfg`):
//     regels `key=waarde`, waarde mag spaties bevatten (volle JSON-jobspecs),
//     # = commentaar. Dit kanaal kent geen bootargs-lengteplafond;
//   - de bootargs uit de extlinux `append`-regel: korte sleutels en overrides.
//
// Sleutels zijn hopos.*-geprefixt, zodat restanten van een Linux-cmdline op
// dezelfde kaart onschadelijk zijn.

var (
	cfgOnce  sync.Once
	cfgCache string
)

// dtb dereferenceert het scratch-woord waarin cpuinit de x0 (DTB-pointer) legde.
func dtb() uintptr { return uintptr(dev.Read64(DTBPtr)) }

// configFile leest het initrd-bestand één keer uit RAM: die regio is
// firmware-eigendom, dus we kopiëren hem vóór er iets overheen kan.
func configFile() string {
	cfgOnce.Do(func() {
		start, end, ok := fdt.InitrdRegion(dtb())
		if !ok {
			return
		}
		b := make([]byte, 0, end-start)
		for p := start; p < end; p++ {
			b = append(b, dev.Read8(p))
		}
		cfgCache = string(b)
	})
	return cfgCache
}

// BootParamAll geeft ALLE waarden van een (mogelijk herhaalde) sleutel, in
// volgorde: eerst uit het initrd-configbestand, dan uit de bootargs.
func BootParamAll(key string) []string {
	out := bootcfg.All(configFile(), key)
	if args, ok := fdt.Bootargs(dtb()); ok {
		out = append(out, bootcfg.Cmdline(args, key)...)
	}
	return out
}

// BootParam geeft de eerste waarde van een sleutel (leeg = niet aanwezig).
func BootParam(key string) string { return bootcfg.First(BootParamAll(key)) }

// SerialSuffix geeft de laatste 8 hexcijfers van /serial-number uit de DTB — de
// stabiele node-identiteit. Leeg als het serial onleesbaar is; de aanroeper
// kiest dan zijn eigen terugval (twee nodes op één LAN mogen nooit dezelfde
// naam dragen).
func SerialSuffix() string {
	s, ok := fdt.RootString(dtb(), "serial-number")
	if !ok || len(s) < 8 {
		return ""
	}
	return s[len(s)-8:]
}
