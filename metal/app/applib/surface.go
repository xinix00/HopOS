package applib

import (
	"fmt"
	"unsafe"

	"github.com/xinix00/HopOS/metal/abi/hopabi"
	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/cpu/memattr"
)

// De app-kant van de surface-grant (gui-ontwerp P3): een GUI-app tekent zijn
// venster in een buffer die de display RECHTSTREEKS leest, in plaats van de
// pixels elke frame over een socket te sturen.
//
// Het verschil in één zin: hiervoor stond elke pixel twee keer in DRAM — één
// keer bij de app en één keer in de back/front-buffer van de display — en ging
// hij er bovendien een keer doorheen. Dat schaalde met het aantal vensters, en
// op de Radxa (06-08) viel de display bij zes vensters om op 78 van zijn 96 MB.
//
// Wat de app ervoor terugkrijgt is een gewone []byte. Wat hij ervoor inlevert
// is uitlijning: de grant gaat per 2MB-blok (zie layout.SurfBlock), dus een
// 1920x1080x32-venster van 8,29 MB kost 10 MB plus maximaal 2 MB uitlijn-slack.
// Dat is RAM van de app zelf; wie een surface neemt, hoort zijn memory_limit
// erop te zetten.

// Surface is een vensterbuffer waar de display in mag kijken.
type Surface struct {
	// Pix is de buffer: hier tekent de app. 2MB-uitgelijnd en een geheel
	// aantal blokken groot, dus meestal iets ruimer dan gevraagd.
	Pix []byte

	// IPA is het adres waarop de DISPLAY deze buffer ziet. Dit getal stuurt de
	// app zelf over zijn eigen protocol door — HopOS zit niet in het
	// GUI-protocol, hij verleent alleen het zicht en zegt waar het uitkomt.
	IPA uint64

	raw []byte // de ruwe allocatie incl. uitlijn-slack; houdt Pix in leven
	app *App
}

// GrantSurface alloceert een venster van minstens n bytes en laat de
// display-houder er read-only in kijken.
//
// Fouten zijn hier normaal en horen NIET fataal te zijn: er is niet altijd een
// display op de node, en op een RISC-V-kooi kan het mechanisme principieel niet
// (PMP-entries zijn locked). Een GUI-app die dit ziet falen hoort terug te
// vallen op de pixels-over-de-socket-weg — die blijft bestaan, al was het maar
// omdat een app op een ándere node nooit een grant kan krijgen.
func (a *App) GrantSurface(n int) (*Surface, error) {
	if n <= 0 {
		return nil, fmt.Errorf("surface: %d bytes", n)
	}
	want := (uint64(n) + layout.SurfBlock - 1) &^ (layout.SurfBlock - 1)

	// Uitlijnen door over te vragen: Go's allocator kent geen uitlijning boven
	// 8 bytes, dus we nemen één blok extra en schuiven op naar de eerste
	// 2MB-grens. De ruwe slice blijft in Surface bewaard — niet uit netheid
	// maar omdat de GC hem anders opruimt terwijl HOP er een stage-2-map naar
	// heeft staan. Go's heap is non-moving, dus het adres blijft geldig.
	raw := make([]byte, want+layout.SurfBlock)
	start := uint64(uintptr(unsafe.Pointer(&raw[0])))
	aligned := (start + layout.SurfBlock - 1) &^ (layout.SurfBlock - 1)
	skip := aligned - start
	pix := raw[skip : skip+want]

	if aligned < a.RAMStart || aligned+want > a.RAMStart+a.RAMSize {
		return nil, fmt.Errorf("surface: buffer %#x+%#x valt buiten het eigen RAM", aligned, want)
	}
	resp, err := a.rpc(hopabi.Req{
		Op:  hopabi.OpSurfGrant,
		Off: aligned - a.RAMStart,
		N:   want,
	}, rpcTimeout)
	if err != nil {
		return nil, err
	}
	return &Surface{Pix: pix, IPA: resp.Size, raw: raw, app: a}, nil
}

// ViewSurface is de ANDERE kant: de display krijgt van een app een IPA
// doorgestuurd en maakt er een leesbare []byte van.
//
// Twee dingen gebeuren hier, en de tweede is de valkuil:
//
//  1. het adres wordt getoetst tegen het surface-venster. Een app die een
//     verzonnen IPA doorgeeft, kan de display zo niet in zijn eigen partitie of
//     in de ctrl-regio laten lezen — het protocol draagt hier een getal van een
//     andere app, en dat is invoer;
//  2. het venster wordt in de eigen stage-1 op Normal cacheable read-only gezet.
//     Zonder die stap is het Device-nGnRnE, want dat is wat tamago doet met
//     alles buiten de RAM-declaratie, en dan leest de compositor elk frame als
//     miljoenen losse transacties. Precies de val die de framebuffer had, maar
//     dan aan de leeskant (zie cpu/memattr).
//
// De slice is READ-ONLY in bedoeling én in hardware: stage-2 en stage-1 zeggen
// het allebei. Erin schrijven is een fault, geen stille corruptie bij de buurman.
func ViewSurface(ipa uint64, n int) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("surface view: %d bytes", n)
	}
	lo, hi := uint64(layout.SurfIPA), uint64(layout.SurfIPA)+(1<<30)
	if ipa < lo || ipa+uint64(n) > hi || ipa+uint64(n) < ipa {
		return nil, fmt.Errorf("surface view: %#x+%#x valt buiten het surface-venster", ipa, n)
	}
	if ipa%layout.SurfBlock != 0 {
		return nil, fmt.Errorf("surface view: %#x is niet blok-uitgelijnd", ipa)
	}
	if err := memattr.NormalRO(uintptr(ipa), uintptr(n)); err != nil {
		return nil, fmt.Errorf("surface view: %w", err)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ipa))), n), nil
}

// Revoke trekt de grant in. Na afloop is Pix nog steeds geldig geheugen van de
// app — alleen kijkt de display er niet meer in.
//
// Hoeft niet bij het stoppen van de app: HOP trekt elke grant van een slot in
// vóór hij de partitie teruggeeft aan de pool (kern/slots, releaseSlot). Dit is
// voor een app die zijn venster sluit maar zelf doorleeft.
func (s *Surface) Revoke() error {
	if s == nil || s.app == nil {
		return nil
	}
	_, err := s.app.rpc(hopabi.Req{Op: hopabi.OpSurfRevoke}, rpcTimeout)
	s.IPA = 0
	return err
}
