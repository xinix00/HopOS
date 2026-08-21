package slots

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/place"
	"github.com/xinix00/HopOS/metal/dev"
)

// Dit bestand is het one-phase startpad: de image stroomt van r rechtstreeks
// de partitie van het slot in — elke byte landt op het adres waar hij gaat
// draaien (abi/place.Stream). Geen apploader, geen gestagede kopie: een
// partitie draagt alleen de app zelf plus zijn heap, in plaats van image +
// staged kopie + een loader-runtime (cloudflared: 124MB voor een image van 30).
//
// De download loopt over HOP's eigen netstack op core 0. Dat was precies wat
// niet mocht — 127 hele images door de kern-heap was de OOM van 14-07 — maar
// streamend buffert een download alleen zijn leesblok, en dát maakt dit pad
// mogelijk. De aanroeper (hop's runner) begrenst hoeveel er tegelijk lopen.

// claimSlot is de claim-stap van élke slot-start: shared = het slot komt er
// als (mede)bewoner bij en een onbeheerd ctx-lijk wordt geruimd; dedicated =
// de cores moeten geparkeerd of cold zijn. Gedeeld door startImage en
// startStream — dit is boekhouding die nooit mag divergeren.
func claimSlot(i, cores int, shared bool) error {
	if !shared {
		return coresFree(i, cores, "not parked/cold")
	}
	if st := ctxState(i); ctxLive(st) {
		// De task-boekhouding is hier de autoriteit: wie plaatst, heeft
		// het slot vrij bevonden — een "levende" ctx zonder eigenaar is
		// dus een LIJK, geen bewoner. De klassieke bron is DRAM-residu:
		// DDR overleeft een warme herstart (snelle herprik, watchdog),
		// en gemeten 02-08 weigerde een verse boot zijn állereerste
		// plaatsing op het Saved-lijk van de vorige run — beide jobs
		// stormden tot give-up. Detecteren zonder opruimen is dan half
		// werk (Derek): ruim het lijk en ga door.
		//
		// De scheidsrechter is de BEWONERSLIJST, niet het hart: die lijst
		// is HOP-eigen RAM (vers bij elke boot, onder lock gemuteerd), en
		// de rotatie hervat uitsluitend wat erin staat. Een ctx-lijk
		// búiten de lijst kan dus nooit meer tot leven komen — draaiend
		// hart of niet — en dat is precies het gat dat "alleen op een
		// stilstaand hart" liet liggen: op één gedeeld hart start de
		// eerste plaatsing het hart, waarna het lijk van de twééde slot
		// onruimbaar werd (gemeten 02-08, cloudflared-storm na de
		// welcome-evict). Stáát hij wél in de lijst, dan is de
		// inconsistentie echt en faalt het luid.
		if residentListed(coreOf(i), i) {
			return fmt.Errorf("slot %d still live (ctx-state %d, scheduled on core %d) — stop it before StartShared", i, st, coreOf(i))
		}
		fmt.Printf("slot %d: unowned resident (ctx-state %d, in no rotation) — evicting the corpse, reusing the slot HOPOS_CTX_EVICT\n", i, st)
		ctxWrite(i, layout.CtxState, layout.CtxEmpty)
	}
	return nil
}

// StartStreamOn start slot i op een door HOPOS gekozen core (PlaceCage — die
// dekt dedicated én sharegroup) door de image streamend te plaatsen. Eén
// ingang, zoals StartLoaderOn dat voor het two-phase pad is; cores > 1 geeft
// de app SMP — dat claimt armSlot zelf, precies zoals bij StartStaged.
func StartStreamOn(core, i int, r io.Reader, imgSize int64, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int, job string) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	if core < 1 || core > layout.NumAppCores() {
		return fmt.Errorf("shared core %d out of range 1..%d", core, layout.NumAppCores())
	}
	partOnce.Do(poolInit)
	previousCore := hostCore[i]
	hostCore[i] = core
	err := startStream(i, r, imgSize, memLimit, cores, env, mounts, ports, job)
	rollbackHostCore(i, previousCore, err)
	return err
}

// startStream is startImage voor een stromende bron, altijd via de
// bewoners-route (hostCore is gezet — dedicated is één bewoner op een verse
// core). Het grote verschil zit in de vensters: de download duurt minuten, dus
// het lifecycle-venster wordt twéémaal kort gepakt — claim + partitie-allocatie
// vooraf, wapenen + dispatch achteraf — en de stroom zelf loopt erbuiten. Dat
// kan veilig: het slot is dan geclaimd en niemand anders schrijft in die
// partitie (de runner van HOP serialiseert Stop tegen een lopende download).
func startStream(i int, r io.Reader, imgSize int64, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int, job string) (err error) {
	if imgSize <= 0 {
		return fmt.Errorf("StartStream: onbekende image-grootte (Content-Length vereist)")
	}
	mtab, preparedEnv, cores, err := prepStart(i, memLimit, cores, env, mounts, ports)
	if err != nil {
		return err
	}
	var started bool

	// Venster 1 (kort): het slot claimen en de partitie alloceren.
	unlock := lifecycleWindow()
	vectorsOnce.Do(cageInit)
	if err = claimStart(i, cores, true); err != nil {
		unlock()
		return err
	}
	base, size, err := partAlloc(i, memLimit)
	if err != nil {
		unlock()
		return err
	}
	appRAM, err := appRAMSize(size)
	if err != nil {
		partRelease(i)
		unlock()
		return err
	}
	// De grant hoort bij het venster (grants.go muteert): aanvragen ná claimSlot,
	// en het venster gaat daarna dicht — de stroom zelf loopt erbuiten.
	var attemptGrant startGrant
	envBlob, err := attemptGrant.prepare(i, preparedEnv)
	if err != nil {
		partRelease(i)
		unlock()
		return err
	}
	// Ook hier pas ná de acquisitie wapenen; een mislukte claim raakt dus nooit
	// het grant-token van de al draaiende task.
	defer func() {
		if !started {
			attemptGrant.rollback(i, err)
		}
	}()
	unlock()
	fmt.Printf("slot %d: partition %d MB @ %#x — streaming %d MB image\n", i, size>>20, base, imgSize>>20)
	defer func() {
		if started {
			return
		}
		// Zelfde uitzondering als startImage: faalde het startschot zélf, dan
		// is onbekend of de core tóch aangaat — partitie in quarantaine.
		if errors.Is(err, ErrDispatch) {
			fmt.Printf("slot %d: partition quarantined — dispatch outcome unknown HOPOS_PART_QUARANTINE\n", i)
			return
		}
		partRelease(i)
	}()

	// Cache-hygiëne van de vorige huurder, vóór de ongecachte writes (zie
	// startImage). Coöperatief: dit yieldt tussendoor, en we houden hier
	// bewust geen venster vast.
	coopCleanInv(uintptr(base), uintptr(size))

	// De stroom: elke byte meteen op zijn eindadres. De plaatser valideert
	// alles (venster, volgorde, maat) en Finish draait dezelfde Build als het
	// staging-pad — één bron van waarheid.
	linkBase := cageLinkBase()
	st := place.NewStream(devSink{base: uintptr(base)}, imgSize, linkBase, appRAM, cageFloor, i, layout.ABIVersion)
	t0 := time.Now()
	if _, err = io.Copy(st, r); err != nil {
		return err
	}
	plan, err := st.Finish()
	if err != nil {
		return err
	}

	// Venster 2 (kort): wapenen en dispatchen — identiek aan de staart van
	// placeFromStaging.
	unlock = lifecycleWindow()
	err = armSlot(i, base, size, plan.Entry, memLimit, cores, envBlob, mtab, ports, job)
	unlock()
	if err != nil {
		return err
	}
	started = true
	fmt.Printf("slot %d: image streamed and placed in %s%s\n", i, time.Since(t0).Round(time.Millisecond), kernMemOnce())
	return nil
}

// devSink adresseert de partitie van één slot in linkBase-offsets: het
// Sink-contract van abi/place op dev.Copy/CopyOut/Clear. De plaatser heeft
// alle offsets al tegen het app-RAM gevalideerd.
type devSink struct{ base uintptr }

func (d devSink) Write(off uint64, p []byte) { dev.Copy(d.base+uintptr(off), p) }
func (d devSink) Read(p []byte, off uint64)  { dev.CopyOut(p, d.base+uintptr(off)) }
func (d devSink) Zero(off, n uint64)         { dev.Clear(d.base+uintptr(off), n) }
