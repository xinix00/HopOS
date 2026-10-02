# Port-review Go v2.2.7 naast Rust v3, de andere borden (02-10, avond)

Vijf leesagenten (Apple, Pi's, Radxa plus QEMU virt, UEFI/O6N/Altra, de
arm64-kern) met hun deelreviews. Alleen gelezen, niets gebouwd. De
rapporten: review-port-apple.md, review-port-raspi.md, review-port-rk3566.md,
review-port-uefi.md, review-port-arm64-kern.md in deze map.

## Niet gevallen

- De arm64-switcher (cpu/src/el2/switch.rs) is regel voor regel Go: WFE op
  de event stream in plaats van een spinpauze, FP via hvcYield aan de
  app-kant, stage-2-revoke met TLBI ALLE1IS en SEV. Alle ijzerlessen uit de
  Go-commentaren staan erin.
- De Pi's: alles met een ijzerles erachter is geport, soms strenger.
- De Radxa-bordkant (CRU, GRF, pinmux, PHY-reset, dwmac4, mdio, TRNG, WDT,
  TSADC): volledig, met de gemeten volgorde van 05/06-08 bewaard.
- hopfs, conport, irq, drbg, trng, memattr, pool, partmem, grants, flip-ABI:
  geport, vaak beter.

## Gaten die op ijzer tellen (op impact)

1. **Scrub zonder clean+invalidate vooraf** (hopos/src/cage.rs `ArmCage::clear`).
   Go: CleanInv, Clear, Push (A76 10-07). Rust: Clear, Push. Op boards waar
   de pool Device gemapt is (M4, Radxa) schrijft de `dc cvac` daarna de vuile
   regels van de vorige huurder over de nullen terug: de heap en stack van de
   nieuwe app bevatten oude data. Een tamago-app neemt nul aan. Drie agenten
   onafhankelijk. Fix: één regel `dev::pull` vóór `dev::clear`.
2. **M4: app-cores zonder wekker en met het verkeerde idle-model**
   (hopos/src/cage.rs `APP_IDLE_MODE` = 0 buiten QEMU; hopos/src/slots.rs
   `wake_all` kickt mpidr 0). Een app slaapt op WFE op EL1, die op de M4 niet
   slaapt: elke bezette app-core spint (Go 02-09: 74 % en 1,3 M rondes/s, met
   yield plus waker.go 0 % en 47 wekken/s). Wie wél yieldt (sharegroup,
   SMP-secundair) slaapt in WFI op EL2 en niemand kickt hem op zijn wektijd.
   Fix ~60 regels, afkijken van kern/slots/waker.go; eerst de wekker en de
   gerichte kick, dan pas de modus op yield.
3. **FP-staat op de OS-core** (cpu/src/el2/oscore.rs, en cpu/src/riscv/oscore.rs).
   Een bewoner die door IRQ, kick of CNTHP onderbroken wordt, verliest q0..q31,
   FPCR en FPSR aan de volgende bewoner. Nu onschadelijk (kern en Hop zijn
   softfloat), fout zodra een hardfloat-bewoner (tamago-app, sinds 02-10) de
   OS-core deelt. Dezelfde klasse als de riscv-FP-fout van vandaag. ~40 regels
   asm; de ctx-blokken zijn Device, dus via GP-registers of een Normal-buffer.
4. **Core-reclaim ontbreekt** (hopos/src/cage.rs `join`: 5 ms, dan
   HOPOS_SHARE_PENDING en Ok). Go: na 2 s HOPOS_CORE_RECLAIM, de kooi van de
   vasthouder intrekken, nog 2 s, anders ErrDispatch (14/15-08: uren
   gijzeling). Nu houdt een rekenende bewoner een sharegroup-core voor altijd
   vast en telt de start als gelukt. 30 tot 40 regels.
5. **Watchdog aait op een zwakker bewijs** (hopos/src/watchdog.rs:147: uplink-IP
   plus heartbeat van Hop). Go: een verse TCP-verbinding naar de eigen :8080
   (de doofheid van 02-08: nieuwe verbindingen en ICMP dood, alles binnen
   gezond). Een dove node reset nu niet. Self-dial 50 tot 80 regels, of een
   voortgangsteller van de switch-actor als goedkopere smaak (20 regels).
6. **Flip op Apple roept PSCI** (hopos/src/flip.rs:577 SYSTEM_RESET, :1289
   AFFINITY_INFO). Zonder EL3 is dat een UNDEF: de weg terug parkeert de core
   (alleen de WDT haalt hem na 30 s terug) en de koude flip crasht de kern ná
   het bevriezen van de opslag. De koude flip op de M4 is nog nooit gedaan.
   ~15 regels: reset via de WDT, koud weigeren zonder EL3.
7. **Altra, eerste boot** (board/uefi/src/boot.rs MAP_CAP 64 KB, slots.rs
   `_dropped` stil, één kernvenster 0x8800_0000). Go: 256 KB (duizenden
   descriptors, 14-07), zes vensterkandidaten omdat 0x9000_0000 bezet bleek
   (13-07). De Altra heeft v3 nog nooit geboot. ~20 regels plus kandidaten.
8. **Geen tweede poging zonder link bij de boot** (alle borden, `probe_nic`
   "no link"). Go probeerde eindeloos (19-09). ~20 regels.

## Prestatie, eerst meten

- Ring: pull/push op head en tail altijd, ook bij Coherence::Hardware; Go
  sloeg ze op coherente ringen over en cleande de gepeekte RX-kop één keer
  per burst (T13 03-09: de helft; bulk HOP naar app tot 4x, 04-09).
- NIC-buffers Normal-NC in plaats van WB op M4 (tg3), O6N en Altra (Go
  17-07 en 03-09). Kan een deel zijn van O6N v3 76 tot 78 tegen v2 111 tot
  116 MB/s inkomend. ~8 regels per board.
- Een app die blijft rekenen (BURN, vitals cpu) hoort RX alleen op de
  poll-timer van de pomp, die tot 1 s oploopt. Zes regels in applib
  (`after_deferrable(poll.lo)`), werkt op elk board.
- igb (Altra): geen 10 ms na CTRL.RST, RDT vóór RXDCTL.ENABLE teruggelezen.
- VBAR_EL2 niet op een kale vector vóór de flip-sprong (diagnose, Go 01-09).
- O6N: de gemeten _CPC-klassenbron staat uit (`highest: 0`), de MPIDR-tabel
  beslist small/big; de console op de SPCR-UART waar Go (09-09) de
  header-UART nam. Werkt nu, kosten niet gemeten.

## Klein

Radxa: geen serienummer-terugval voor de MAC (elke Radxa zonder hopos.node
dezelfde), xHCI zonder barrières binnen een TRB op Normal-NC, VOP2-klokboom
uit de keten, UTMI-breedte onzeker. Pi: cmdline.txt zonder lengtetoets (Go
19-07), mailbox spint tot 500 ms op core 0, vroege faultdump weg, gui 16 bpp.
Kern: laatste woorden na een stop gaan verloren, conport meldt "vol" niet aan
de client, appregels synchroon naar de UART, docs/boards-radxa.md en het
memattr-commentaar lopen achter.
