# vitals — de board-dokter

Eén HopOS-app die de vitale functies van een node meet en benchmarkt. Voor
bring-up en diagnose van nieuwe boards: deploy vitals, open de pagina, druk op
**Run all**, en vergelijk het rapport (knop *Copy report*) met een gezond
board.

| test | meet | zegt iets over |
|---|---|---|
| idle *(passief, loopt altijd)* | idle-%, wakes/s, kosten per wake uit `CtrlIdle`/`CtrlWakes` | idle-governor, WFE-event-stream / CLINT-slaap |
| cpu | LCG-stappen/s op één core (mét Gosched-tax) | kloksnelheid van het hart |
| smp | serieel vs. parallel + gedeelde-heap-verificatie | komen alle harten echt op |
| burn | volgehouden last, per seconde tempo + temperatuur | thermal throttling, dvfs-terugklok, heatsink |
| membw | STREAM-achtig copy/triad | DRAM-controller-config |
| memlat | pointer-chase over oplopende working-sets | cache-hiërarchie, DRAM-latentie |
| rx | download van een externe bron (plain http) | doorvoer app-stack → gwnat → NIC → internet |
| tx | client-gedreven: `curl -o /dev/null http://node:8090/blob?mb=64` | zendkant van hetzelfde pad |
| storm | veel korte verbindingen naar de eigen gepubliceerde poort (hairpin) | verbindingsopbouw onder druk |
| rtt | kale TCP-handshakes naar de gateway | vloer van het interne pad |
| gc | allocatietempo, GC-cycli, langste pauze | GC-gezondheid binnen de partitie |
| timer | overslaap bij 1/5/20 ms | timerbron + scheduler-jitter |

Alles is ook zonder browser te bedienen: `GET /api/state` (alle cijfers als
JSON), `GET /api/run?test=<naam>` (of `test=all`), en de losse endpoints
`/ping` en `/blob?mb=N`. Eén test tegelijk; een tweede start geeft 409.

**Kanttekening:** onder QEMU-TCG is WFE een no-op — idle-cijfers zijn
ijzer-cijfers. En de storm-test stormt door zijn eigen slot heen (client én
server), dus die cijfers zijn een ondergrens; zuiver meten = van buitenaf naar
`/ping` stormen.

## Config (jobspec-env, alles heeft een default)

| env | default | betekenis |
|---|---|---|
| `ER_PORT_HTTP` | `8080` | de gepubliceerde poort (HOP zet hem via `ports:{http:…}`) |
| `HOP_ADDR` | `10.100.0.1:8080` | agent-API, alleen voor temperatuur |
| `HOP_KEY` | *(leeg)* | cluster-key; leeg = temperatuur n/a, de rest werkt |
| `VITALS_RX_URL` | cachefly 100MB | bron van de rx-test (plain http, mét Content-Length) |

## Jobspec

```
hopos.init[]={"name":"vitals","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop/releases/download/rolling-release/vitals-arm64-tamago.elf","match":{"node.arch":"arm64"}},{"url":"https://github.com/xinix00/hop/releases/download/rolling-release/vitals-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}],"memory_limit":134217728,"cpu_shares":2048,"ports":{"http":8090},"env":{"HOP_KEY":"..."}}
```

`cpu_shares` ≥ 2048 (2 cores) maakt de smp-test zinvol; met 1 core slaat hij
zichzelf over. Géén sharegroup: benchmarkcijfers horen van een dedicated core
te komen (de timer/idle-cijfers op een gedeelde core zijn juist wél
interessant — dat is een aparte deploy met `tags.sharegroup`). 128 MB
partitie geeft de geheugentests ruimte om boven de caches uit te meten.

## Bouwen

Via `easy/release.sh` (staat in `HOPOS_TARGETS` én `HOPOS_RISCV_TARGETS`), of
los:

```sh
GOWORK=off GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
  ~/tamago-go/bin/go build -tags "linkcpuinit nodefaultstack" -trimpath \
  -ldflags "-w -T 0x50010000 -R 0x1000" -o vitals-arm64-tamago.elf ./cmd/vitals
```

(riscv64: `-tags "linkramsize linkcpuinit nodefaultstack"`, `-T 0x88010000`.)

Het pakket `internal/vitals` is host-buildbaar (geen metal-imports; de
control-page-woorden komen als functie uit de tamago-main): `go test ./...`
draait de smoke-test gewoon op de Mac.

De go.mod pint metal **v1.11.1** — de nieuwste metal-tag zonder pad-replaces,
dus de nieuwste die met `GOWORK=off` reproduceerbaar bouwt, én de eerste met
`CtrlWakes`/`CtrlMemSys` in de ABI. Zodra de lneto-ronde een stabiele
metal-tag oplevert: pin bumpen en het image wordt een paar MB kleiner.
