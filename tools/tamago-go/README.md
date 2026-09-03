# tamago-go: onze patches op de toolchain

HopOS bouwt met [tamago-go](https://github.com/usbarmory/tamago-go), tag
`tamago1.26.4`, plus de patches in deze map. De fork op de werkbank is een
gewone clone van upstream met deze patches erop; er zitten geen lokale commits
in. Wie de toolchain opnieuw kloont (docs/app.md) legt ze er zo weer op:

```
tools/tamago-go/apply.sh            # controleert eerst, past dan toe
```

De runtime wordt per build uit `$GOROOT/src` gecompileerd, dus na een patch
hoeft `make.bash` niet opnieuw.

| patch | wat | waarom |
|---|---|---|
| `0001-runtime-wakesleeper.patch` | `runtime.WakeSleeper` en `runtime.IdleMayReady` (nieuw bestand) en `preemptM` op de `goos.Wake`-haak | Een SMP-app: de RX-doorbell wekt de pomp-goroutine onder de timer-lock (tamago's `WakeG` herschrijft de heap zonder lock en sloopt hem op twee cores), en een stop-the-world bereikt een core die op EL2 slaapt via dezelfde reschedule-IPI als `semawakeup`. `IdleMayReady` zegt of de idle-hook goroutines runnable mag maken (alleen in de scheduler-idle; vanuit `semasleep` zou dat de scheduler slopen). Zie `metal/cpu/idle/rxdoor.go` en `metal/cpu/smp`. |
| `0002-net-adopt-bound-addresses.patch` | `net_tamago.go`: Addr/LocalAddr/RemoteAddr uit de implementatie | Een wildcard-bind (`":0"`) krijgt zijn IP en poort pas in `net.SocketFunc` (HopOS: leannet); zonder dit rapporteert een luisteraar `":0"`. |

Wat níét hier hoort: de `goos`-haken zelf (`Idle`, `Wake`, `ProcID`, `Task`)
wonen in de tamago-*module* (`github.com/usbarmory/tamago/goos`), niet in de
toolchain — voor tamago-builds is `runtime/goos` dát pakket. Een nieuwe haak
is dus een module-fork, geen patch hier.

Bijwerken van een patch: wijzig de fork, dan in `~/tamago-go`
`git diff -- <bestanden>` achter de kopregels van het patchbestand plakken
(zo zijn deze twee ook gemaakt).
