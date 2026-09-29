# Netstack-upstream: openstaande PR's en wat wij ervan afhangen

HopOS draait sinds 09-08 op lneto (via go-net); de gaten die we daarvoor
dichtten zijn als PR's naar upstream gestuurd.

**10-08: de pad-replaces zijn weg.** Ze zaten er zolang we rechtstreeks op de
clones werkten, maar een `replace` geldt alleen in de hoofdmodule — dus alles
wat metal importeert (hop-os-surf, de vitals/welcome-apps) loste `soypat/lneto`
op naar UPSTREAM terwijl de node de gepatchte versie draaide. Stil, want het
compileert. Nu zijn het echte forks met een eigen module-pad:

| | fork | tag |
|---|---|---|
| lneto | `github.com/xinix00/lneto` | `v0.4.0-hopos.1` |
| go-net | `github.com/xinix00/go-net` | `v0.1.0-hopos.1` |

Elke clone heeft twee branches: **`hopos`** met het UPSTREAM module-pad (daar
leven de fixes, en hiervandaan maken we PR-branches — upstream kan een diff met
een gewijzigd module-pad niet gebruiken) en **`fork`** = hopos plus één
mechanische pad-commit. `tools/refork-netstack.sh` regenereert `fork` uit
`hopos` en reproduceert de getagde bomen byte-identiek; taggen blijft handwerk
omdat een versie kiezen een beslissing is. **De tag moet HOGER zijn dan élke
upstream-tag**, anders pakt `@latest` upstream-code met het verkeerde
module-pad (meegemaakt: upstream stond al op v0.3.2 toen onze eerste poging
v0.2.1-hopos.1 heette).

Zodra een fix upstream landt kan hij uit onze fork: `hopos` rebasen op de
nieuwe upstream, reforken, tag bumpen. Zijn ze állemaal geland, dan kan de fork
zelf weg en requiren we upstream direct — dat is nog steeds het doel, alleen
niet langer een blokkade.

Snel de stand opvragen:

    gh pr status --repo soypat/lneto
    gh pr list --repo soypat/lneto --author xinix00 --state all
    gh pr list --repo usbarmory/go-net --author xinix00 --state all

## Ronde 1 — ingediend 09-08, wachten op review/merge

| PR | Wat | Status |
|---|---|---|
| [soypat/lneto#178](https://github.com/soypat/lneto/pull/178) | Blocking waits deadline-gedreven i.p.v. maxIter-gecapt (dials faalden na ~20ms op GOMAXPROCS=1) | open |
| [soypat/lneto#179](https://github.com/soypat/lneto/pull/179) | Window scaling (RFC 7323) — zonder dit is elk venster ≤64K én wrapt een >64K-buffer de SYN | open |
| [soypat/lneto#180](https://github.com/soypat/lneto/pull/180) | Efemere poorten sequentieel (birthday-hergebruik van 4-tuples → 30s dode dials) | open |
| [usbarmory/go-net#5](https://github.com/usbarmory/go-net/pull/5) | `nodefaultstack`-build-tag: gvisor uit lneto-only binaries (−21%) | open |

## Ronde 1b — ingediend 10-08, gevonden mét de LicheeRV-meetbank

Deze drie komen uit de RX-jacht op ijzer, en het zijn correctheidsfouten (geen
performance): zonder ze corrumpeert een download stil, en herstelt een
verbinding niet van één verloren pakket.

| PR | Wat | Status |
|---|---|---|
| [soypat/lneto#181](https://github.com/soypat/lneto/pull/181) | Ring spoelde zijn schrijfpositie terug bij leeglezen → gestaagde out-of-order segmenten kwamen van de verkeerde offset (volle lengte, verkeerde bytes) | open |
| [soypat/lneto#182](https://github.com/soypat/lneto/pull/182) | Onbevestigde data bleef liggen na een lokale close (FIN-WAIT-1 weigerde élk datasegment) — raakt write-then-close, dus elke response | open |
| [soypat/lneto#183](https://github.com/soypat/lneto/pull/183) | Geen enkele verbinding had een retransmissie-timer, terwijl `NanoTime` dat wél documenteert; leunt op #182 | open |

Bewijs op ijzer: TX was bit-perfect (3× 8MiB, 9,45 MB/s) terwijl RX 16MiB met
een ándere sha afleverde en TLS `bad record MAC` gaf. Na #181 haalde dezelfde
node 6,25MB over HTTPS binnen, byte-exact gelijk aan de GitHub-asset — TLS
controleert elke byte, dus dat is het bewijs. Verliesherstel: 512KiB-stroomtest
ging van 2/10 naar 8/10 complete runs.

**Belangrijk voor de review:** alle drie zijn ook zónder onze hardware te
reproduceren. Elke PR draagt een test die op onaangetast upstream-main rood is
en met de fix groen — `go test` op een gewone machine, geen board nodig. De
LicheeRV was alleen de vinder: zijn te ondiepe RX-ring (ons probleem, apart
gefixt met 64→128 descriptors) liet 3-41 frames per download vallen, en dát is
de toestand waarin deze drie fouten zichtbaar worden. Op QEMU met slirp valt
nooit een frame, dus daar blijven ze onzichtbaar.

## Review 11-08: positief, met werk

soypat heeft alle zes gelezen ("Really exciting to see so many interesting PRs
to lneto!") en is voorlopig OOO, dus mergen kan duren. Vijf opmerkingen, waarvan
drie over hetzelfde: **ons commentaar is te lang.** De huisstijl van beide
upstreams staat nu in `upstream-pr-stijl.md`.

| PR | Ask | Verwerkt |
|---|---|---|
| #178 | lus-levensduur in de `for`-header, niet via een return uit de body | ja — en de diff werd er 25 regels kleiner van |
| #179 | geen; hij kijkt volgende week grondig | commentaar gesnoeid |
| #180 | test op `internal/ltesto`'s `Sched`, niets mag slapen of blokkeren (issue #140) | churn-test verwijderd, zie hieronder |
| #181 | `strconv.Itoa`, en commentaar korter (code én test) | ja |
| #182 | doc van `txQueuedDataOpen` gaat over `TxDataOpen` i.p.v. over zichzelf | herschreven + unexported |
| #183 | wacht op #182 | gerebased op de nieuwe #182 |

**#180 kon niet zoals gevraagd.** De churn-test draaide drie goroutines (dialer,
accept-lus, pakket-pomp) op de echte klok, mét sleeps en een 3-strikes-retry —
flaky by construction, precies waar issue #140 over gaat. Een getrouwe port kán
niet: `Sched.Goro()` paniekt op de tweede goroutine ("only one goroutine
supported for now"). Dus de test is eruit en `TestEphemeralPortSequence` blijft
over, die de eigenschap zélf pint (geen poort hergebruikt binnen 16384
allocaties) zonder klok, sleep of goroutine. Aangeboden om hem te porten zodra
`Sched` meerdere goroutines kan. **Dezelfde vraag komt bij #183**, wiens test een
echte RTO uitzit; dat melden we daar alvast.

Bij reviewvragen: de volledige onderbouwing (metingen, reproducties, de
afwegingen per PR) staat in `~/Git/netstack-prs.md` op de werk-Mac; de
regressietests in de PR's zelf zijn elk rood-bewezen op oude code.
#178 en #180 raken dezelfde test-file-staart — wie het laatst landt rebaset
(staat ook in de PR-tekst van #180).

## Ronde 2 — AANGEKONDIGD, wacht op zijn keuze

11-08 gevraagd in [discussion #184](https://github.com/soypat/lneto/discussions/184)
("Some more fixes (Hi there!)") in plaats van drie losse PR's erbij te gooien:
de drie lneto-vondsten op een rij met de vraag of hij er PR's van wil, en of ze
los mogen of gecombineerd. DHCPv4-renewal staat erbij als heads-up, niet als
aanbod — onze renewal is `leandhcp.KeepAlive`, een eigen client, dus het ding dat
we zouden aanbieden bestaat nog niet. De broadcast-flag is weggelaten: geen
apparaat dat het nodig heeft, dus niet aan te tonen.

**Vóór het vuren nog werk aan de takken zelf:** ze zijn groen (één commit, test
rood op main) maar niet schoon volgens de stijl die uit de review van 11-08 kwam
— 7 em-dashes in de code, 7 in de commit-berichten, en `SeedNeighbor` heeft een
doc-comment van zeven regels.

Afgemaakt 09-08 avond: PR-branches standalone groen vanaf upstream-main,
elke fix met een rood-bewezen test; concept-teksten in `~/Git/netstack-prs.md`.

| Branch (repo) | Wat | Test |
|---|---|---|
| `arp-resolve-all-pending` (lneto) | ARP-reply lost álle wachtende cache-entries op | dubbele query, één reply → 2 callbacks (main: 1) |
| `listener-pool-maintenance` (lneto) | `CheckTimeouts` aangedreven vanuit de accept-lus | half-open-flood → herstel (main: slots nooit terug) |
| `seed-neighbor` (lneto) | `SeedNeighbor`: statische buren, nul ARP | SYN direct met geseede MAC, geen ARP-frame |
| `listener-pool-size` (go-net) | pools op `MaxListenerConns` i.p.v. `MaxActiveTCPPorts` | one-liner (16MB/listener-claim in de tekst) |
| `passive-peers` (go-net) | `PassivePeers` aan → listener-replies naar peer-MAC | gedekt door lneto's self-dial-test |
| — geblokkeerd — (go-net) | statische gw-MAC + SeedNeighbor-passthrough | wacht op lneto-release mét SeedNeighbor |

## Wanneer mogen de replaces weg?

1. Alle commits waar de boom op leunt (ronde 1 **én** ronde 2) zijn upstream
   geland — of wij besluiten tot eigen fork-tags voor wat niet landt.
2. go-net heeft een release/commit die de benodigde lneto-versie pint.
3. `metal/go.mod`: replaces eruit, versies bumpen, gate + QEMU-smoke draaien.

Tot die tijd: **de clones horen op branch `hopos` te blijven staan** — de
replace volgt de werkboom, en een uitgecheckte PR-branch bouwt een kern
zonder wscale (meegemaakt: alles "invalid window size").
