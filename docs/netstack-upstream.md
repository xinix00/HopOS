# Netstack-upstream: openstaande PR's en wat wij ervan afhangen

HopOS draait sinds 09-08 op lneto (via go-net); de gaten die we daarvoor
dichtten zijn als PR's naar upstream gestuurd. Tot ze geland zijn bouwt de
boom tegen lokale clones (`metal/go.mod` heeft `replace` naar
`~/Git/lneto` en `~/Git/go-net`, beide branch `hopos`) — **dit bestand is de
checklist om die replaces weer kwijt te raken.**

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

Bij reviewvragen: de volledige onderbouwing (metingen, reproducties, de
afwegingen per PR) staat in `~/Git/netstack-prs.md` op de werk-Mac; de
regressietests in de PR's zelf zijn elk rood-bewezen op oude code.
#178 en #180 raken dezelfde test-file-staart — wie het laatst landt rebaset
(staat ook in de PR-tekst van #180).

## Ronde 2 — KLAAR OM TE VUREN (wacht op de reacties op ronde 1)

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
