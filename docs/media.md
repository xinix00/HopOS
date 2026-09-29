# Het media-vlak: de videocodec van de O6N

De hardwaredecoder van de Orion O6N (Arm China Linlon V8, "mve") en de
codec-dienst waarmee een app een stream door de decoder haalt, in v3. De
Go-bron is `OLD/metal/media/driver/vpu/mve`, `OLD/metal/driver/codec`,
`OLD/metal/kern/slots/codec*.go` en `OLD/metal/board/o6n/hop/vpu*.go`; de
stand van de Go-generatie en de lessen staan in `OLD/docs/media-o6n.md` en
`OLD/docs/v1/technical/video-codec.md`.

Alles zit achter de feature `media` (op `hopos`, `kern`, `applib` en
`board-o6n`). Kaal kent de kern de codec-ops niet en antwoordt hij
"onbekende op".

## De vorm

| Crate / bestand | Wat het bezit |
| --- | --- |
| `driver/codec` (`driver-codec`) | Het contract: `Engine::open(&Config) -> Session`, `feed`, `offer`, `next_event`, `close`, `reap`; `Buffer { pa, size }`; `Event` met `Kind::{Consumed, Produced, Format, Done, Fault}`; de `Firmware`-bron. Een `Session` is een handvat zonder `Clone`; wie hem laat vallen, legt zijn nummer in het `Graveyard` van de engine en de engine sluit de sessie bij zijn volgende beurt. |
| `media/mve` (`media-mve`) | De Linlon V8: registers (getypeerd, `const`-asserties op elke offset), de arena (bitmap, first-fit, uitlijning), de page tables van de codec-MMU, het laden van de firmware, de ringen met de lopende checksum, de pomp (RPC-geheugen, berichten, buffers, de output-flush-handdruk, EOS op het laatste beeld), de vaste plekken per buffer. Eén `Device` per VPU, van één taak; geen slot. |
| `kern/src/codecabi.rs` | De system-functies (open, feed, offer, poll, close), de sessietabel per levensduur van een slot, `codec_grant` met de drie toetsen, en het cache-onderhoud (de VPU is niet coherent). |
| `abi/src/hopabi.rs`, `hopabi::codec` | De payloads op de draad: `OpenArgs` (8), `FeedArgs` (24), `BufArgs` (4), `Event` (64 bytes), byte voor byte die van Go. |
| `applib/src/codec.rs` | De client: `Session::{open, feed, offer, poll, close}` over `sys::Client::call_once` (nooit herhaald: twee keer dezelfde feed is twee happen bitstream). |
| `board/o6n/src/codec.rs` | De VPU aanzetten: vensters, interrupt en `_CCA` uit de DSDT (`scan_dsdt`), arena ongecached, stroomdomeinen via SCMI naar de TF-A, klokken en perf-domein via de SCP, reset, en de stroomcyclus als het blok vastzit (`needs_recovery`, `cycle_domains`). |
| `media/optical` (`media-optical`) | De bulk-only-transportlaag van een optische drive over een `Transport`-trait (zie hieronder). |
| `hopos/src/codec.rs` | `codec::up` ná de opslag: de dienst aanmelden bij de system-API, op de O6N de VPU aan en de driver erin, en de opruimtaak die eens per seconde de sessies van gestopte bewoners sluit. |

### Grote data loopt niet door de kern

Een 4K-beeld in P010 is 3840 x 2160 x 1,5 x 2 = 24.883.200 bytes; bij 24 fps
is dat 597 MB/s. Het slot-LAN piekt op 550 MB/s. De beelden kunnen dus niet
over de system-calls, en ze horen er ook niet: een buffer is een stuk van de
eigen partitie van de app (`off`, `n` vanaf `RamStart`, hele pagina's). De
kern toetst het (`codec_grant`: binnen de partitie, geen omloop,
pagina-precies), de driver hangt het in de page tables van de codec, en dat
is de grant én de isolatie: de VPU kan per constructie niets aanraken wat
niet van deze taak is.

Het cache-onderhoud doet de kern: `feed` schrijft uit (`dc cvac`), `offer`
schrijft uit en gooit weg (`dc civac`), een gevuld resultaat wordt weggegooid
vóór de app het leest, een teruggegeven invoer kost niets.

### Levensduur en evict

De sessietabel hoort bij één levensduur van een slot (de generatie van de
servicer). Elke call draagt de generatie waarvoor zijn verbinding werd
toegelaten; een tabel van een andere generatie geeft `None` (Revocable). Een
verzoek van de vorige huurder landt zo nooit in de tabel van de opvolger. Bij
evict valt de tabel, vallen de handvatten, en sluit de engine de sessies:
`Drop` is de vrijgave, er is geen `ReleaseCodecs` meer.

## Tests (Go naar Rust)

| Go | Rust | Toetsen |
| --- | --- | --- |
| `mve/session_test.go` (975 regels, 15 toetsen) | `media/mve/src/tests.rs` | alle 15 (37 toetsen in het crate), met dezelfde nep-VPU (een echte page walk door het geheugen, een nep-firmware die de checksum narekent), plus: een gevallen handvat sluit de sessie, een vreemd handvat is dicht, een vast slot is een fout zonder lek, describe/state dragen de getallen |
| `mve/mve_test.go` (11) | idem | alle 11 (arena, MMU, firmwarekop) |
| `mve/queue_test.go` (7) | idem | alle 7 (het ringprotocol) |
| `kern/slots/codecabi_test.go` (5) | `kern/src/codecabi/tests.rs` | alle 5 (de race van de onderbroken Open is in Rust ondeelbaar; de toets bewijst nu de weigering vóór het ijzer), plus het cache-onderhoud per richting, de weigering buiten de partitie en de weigering zonder ijzer |
| `board/o6n/hop/vpu_recovery_test.go` (3) | `board/o6n/src/codec.rs` | alle 3, plus de DSDT-lezer |
| `abi/hopabi/codec.go` | `abi/src/hopabi.rs` | de offsets van `EncodeEvent` byte voor byte, de roundtrips |
| `optical/optical_test.go` (19) | `media/optical/src/tests.rs` | de BOT-toetsen: de CBW van de spec, grenzen en richtingen, het residu, een gefaalde datafase die niet verstopt wordt, plus een verloren spoor (tag, phase error, CBW) dat reset, de herkansing op de status en beide sense-formaten; de MMC-toetsen volgen met de MMC-laag |
| (nieuw) | `driver/codec`, `applib/src/codec.rs` | nummering gelijk aan Go, het graveyard; de client spreekt de draad van de kern en herhaalt nooit |

## Wat nog van andere sporen komt

Drie haken zitten buiten de media-bestanden; de eerste is er. Zolang de
andere twee er niet zijn, meldt `codec::up` op de O6N luid welke ontbreekt en
blijft de codec uit; op QEMU merk je er niets van.

1. **De partitie van een levende bewoner** (`kern/src/slots.rs`): er.
   `Servicers::partition(slot) -> Option<Region>` geeft de basis en maat uit
   `ServicerCtl` zolang de servicer leeft, en `hopos::codec::NodeLives`
   gebruikt hem; na de stop weigert elke grant met `STATUS_DENIED`.
2. **Een arena buiten de partitie-pool** (Go: `slots.ReserveDevice`): een
   blok van `hopos.codec` MB (standaard 768) dat de lifecycle-actor nooit aan
   een partitie geeft. Zonder: `HOPOS_CODEC_OFF`.
3. **Een kern-lezing van hopfs** (`kern/src/rpc.rs`): de actor leest voor de
   kern zelf `/firmware/<naam>.fwb` (zestien blobs van zo'n 300 KB, door
   Lumen van Sky1-Linux/sky1-firmware gehaald). Zonder: `HOPOS_CODEC_NOFW`,
   en elke open weigert met "no firmware for this codec".

Niet geport: het meetinstrument `hopos.codecdemo` (het vraagt dezelfde twee
haken, arena en hopfs, plus een tweede blok voor zijn buffers).

De optische drive (`media/optical`, Go `OLD/metal/media/driver/optical`) is
voor de helft geport: de bulk-only-transportlaag (CBW, datafase, CSW met de
toetsen op signature, tag, residu en status, reset-recovery, de ene
herkansing op een gestalde status, REQUEST SENSE) tegen een
`Transport`-trait, met zeven toetsen tegen een nep-drive. De MMC-laag erboven
(INQUIRY-identiteit, GET CONFIGURATION, READ CAPACITY, READ(10), SET
STREAMING, `read_at` over sectoren van 2048 bytes) en de device-command-ABI
(`OP_DEVICE_COMMAND`) volgen met de USB-stack van het gui-spoor.

## Checklist: de O6N-mediatest

Bouwen: `cargo build --release` met `--features board-o6n,media` als PIE
(de gate doet het in `target/uefi-media`), of `BOARD=o6n sh image/uefi-run.sh`
zodra dat script een media-knop heeft. Config: `image/hopos-media-o6n.cfg`
uit de Go-boom geldt nog (`hopos.codec=768`, `hopos.storage=stateful`).

| Stap | Marker of regel | Wat het bewijst | Afwijking |
| --- | --- | --- | --- |
| 1 | `vpu: TF-A SCMI channel alive, power protocol vX.Y` | het SMC-kanaal naar de TF-A antwoordt | geen regel: het kanaal op 0x84380000 is niet gemapt of de SMC-functie klopt niet |
| 2 | `vpu: power domains 4 5 11-15 on (confirmed by the firmware)` | hub, top en vier cores aan, teruggevraagd | `power domain N not on`: de TF-A weigert; niet verder, de eerste registerlees zou een SError zijn |
| 3 | `vpu: mm ni700 clock on ...`, `vpu: vpu apb clock on ...` | de klokken van interconnect en blok | "no SCMI channel offers the clock protocol": registers lezen dan nul |
| 4 | `HOPOS_VPU_RECOVER` (alleen na een vastgelopen blok) | de stroomcyclus 15..11 uit, 11..15 aan | na de cyclus moet PGCTRL `0x07cefffc` zijn en TERMINATE nul (gemeten 27-09) |
| 5 | `vpu: id 0x56648002 rcsu ... (windows 0x14240000/0x14230000, intid 358, cca Some(false))` | het blok leeft; vensters, interrupt en `_CCA = 0` uit de DSDT | id 0: geen klok of hub; 0xffffffff: geen bus; `cca Some(true)`: de firmware noemt het blok coherent, de arena mag dan gecached |
| 6 | `codec: Linlon V8 (id 0x56648002 rev ...): 4 cores, N sessions, fuse ..., arena 768 MB (768 MB free) HOPOS_CODEC_UP` | de driver draait, de arena is heel | `HOPOS_CODEC_OFF` met de reden (arena, stroom, probe) |
| 7 | Lumen start een back-up | open, feed, offer, poll over de draad; de firmware vraagt zijn referentieframes (RPC) | een Fault met `firmware asked for N MB and arena ran out`: `hopos.codec` omhoog |
| 8 | fps in `/api/state` van Lumen | de meting: **24 fps 4K P010 = 24.883.200 bytes per beeld = 597 MB/s door de grant**, geen byte over de verbinding. de Go-kern haalde 27,25 fps met Lumen op GAMEOFTHRONES_S1_D1 (27-09, met de software-encoder erachter) | lager dan 24 op 4K: eerst de cache-ops (`dc civac` over 24 MB per beeld), dan de pomp (de opruimtaak en de poll van de app) |
| 9 | stop Lumen midden in een film; start hem opnieuw | evict sluit de sessies binnen één seconde (de opruimtaak); de nieuwe levensduur opent op hetzelfde LSID | "all hardware sessions in use": een levensduur die niet viel |
| 10 | twee films achter elkaar | na de eerste is de arena weer heel (`describe` na close) | minder vrij dan totaal: een lek in RPC-geheugen of tabellen |

Een stille sessie (geen events, geen Fault) is de eerste vraag voor `state`:
`enable`, `jobqueue`, per LSID `sched`, `irqhost`, `mmu` en de tellers
(`flushes`, `flushback`, `eos`, `rpc[allocs]`, `bufs[offered back held]`).
