# Support-matrix: wie voldoet aan het contract

*(NL, intern) Stand 2026-09-03, uit de code gelezen (metal/board/board.go + de hop-helft per board). Bijwerken zodra een cel op ijzer bewezen of gebouwd is; een cel zonder datum is een aanname.*

Het generieke board/kooi-contract (`board.Board`, `board.Cores`, `idle.Sleeper`, `cpu/irq`) heeft
nil-poten: een board dat iets niet heeft vult niets in en de kern valt terug. Dat is precies hoe een
gat onzichtbaar wordt. Deze matrix zegt per board per poot of het contract volledig gedragen wordt,
of dat we op de terugval draaien.

Legenda: ✅ bedraad én bewezen op dit board · 🟡 fallback of gat dat het silicium wél kan dragen ·
⚪ nil-poot terecht, silicium heeft het niet · ❓ bedraad, niet of niet opnieuw gemeten op ijzer ·
❌ leeg waar het contract het wél verwacht

## Kern-contract: cores, idle, interrupts

| Poot | QEMU virt | M4 | UEFI / Altra | Radxa RK3566 | Pi 4 | Pi 5 | LicheeRV |
|---|---|---|---|---|---|---|---|
| Privilege | ✅ EL2 gemeten | ✅ | ✅ | ✅ | ✅ | ✅ | 🟡 `RequireMMode(3)` hardcoded, niets gemeten |
| Cores.App | ✅ PSCI-probe | ✅ lijst van 9 | ✅ MADT | ✅ PSCI aff1 | ✅ | ✅ | ✅ lijst |
| Cores.Start | ✅ | ✅ PMGR + brievenbus | ❓ Altra niet geboot sinds boot.h (02-09) | ❓ kaart-boot open sinds boot.h | ❓ idem | ❓ idem | ✅ |
| Cores.Reset | ⚪ parkeert zelf | 🟡 PMGR-stop stopt niet, gemeten 02-09 | ⚪ | ⚪ | ⚪ CPU_OFF one-way | ⚪ | 🟡 alleen C906L, en daar woont HOP nu zelf: app-hart loopt via kill-tick |
| Cores.State | ✅ PSCI | 🟡 eigen boekhouding, niet silicium | ✅ | ✅ | ✅ | ✅ | ✅ reset-blok |
| App-core idle | ⚪ WFE, TCG slaapt niet; hoort erbij | ✅ IdleYield, EL2-WFI, 0% | ❓ WFE default | ❓ WFE default | ❓ WFE default | ✅ WFE, P2b gemeten | ✅ ecall naar M-mode, wfi op CLINT |
| Kick | ⚪ event-stream wekt zelf | ✅ fast IPI | ⚪ | ⚪ | ⚪ | ⚪ | ⚪ CLINT core-lokaal, cap 2 ms |
| HOP-core idle | ⚪ WFESleep, warm by design | ✅ WFESleep (WFI was doof voor SEV, 04-09) | ❓ WFESleep default | ❓ | ❓ | ✅ | ✅ MSleep mits CLINT-probe |
| NIC-interrupt | ✅ GICv3 + virtio-SPI | 🟡 **poll**, AIC + MSI open | 🟡 **poll** | 🟡 **poll** | 🟡 **poll** | 🟡 **poll** | 🟡 **poll** |
| HartTimerer | ⚪ | ⚪ | ⚪ | ⚪ | ⚪ | ⚪ | ✅ probe per boot |
| SMP-app | ✅ 03-09 | ✅ 03-09 | ❓ | ❓ | ❓ | ❓ | ⚪ één app-hart |
| Kern-flip | ✅ | ✅ 02-09 | ❓ | ❓ | ❓ | ❓ | ⚪ geen riscv-flip |

**De NIC-rij.** Alleen QEMU implementeert `board.NICInterrupter`; hopnet pollt op elk ander board elke
300 µs (`net/hopnet/hopnet.go`, rxLoop). HOP's eigen core wordt op ijzer dus ~3.333 keer per seconde
wakker, hoe goed zijn Sleeper ook is. Op QEMU met interrupt zijn dat er 100 bij stilte (de vangrail van
10 ms). Wat het per board kost om de poll kwijt te raken:

- **Radxa**: GIC-600 is GICv3, `driver/gicv3` ligt er al; alleen de GMAC1-SPI uit de DTS bedraden.
  Goedkoopste winst.
- **UEFI / Altra**: ook GICv3, en de igb-lijn is testbaar op QEMU-EDK2 (`image/uefi-run.sh`) zonder ijzer.
- **M4**: AIC plus PCIe-MSI, eerder geschat op ~350 regels, alleen op ijzer te bewijzen.
- **Pi 4 / Pi 5**: GIC-400 is GICv2, dus een tweede driver. Pi 5 gaat bovendien via RP1 MSI-X (MIP0).
- **LicheeRV**: PLIC-driver.

## Board-facetten: net, geheugen, sensor, watchdog

| Facet | QEMU virt | M4 | UEFI / Altra | Radxa RK3566 | Pi 4 | Pi 5 | LicheeRV |
|---|---|---|---|---|---|---|---|
| NIC + IP | ✅ virtio, statisch | ✅ tg3, eigen PCIe, DHCP | ❓ igb, DHCP | ✅ dwmac4 | ✅ genet | ✅ gem via RP1 | ✅ dwmac |
| LeaseHolder | ⚪ statisch | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| MemTotal | ✅ DTB | ✅ boot-args | ✅ memmap | ✅ | ✅ DTB | ✅ DTB | 🟡 256 MB hardcoded |
| CoreClass | ⚪ homogeen | ✅ E/P uit MPIDR | ⚪ | ⚪ | ⚪ | ⚪ | ✅ small/big |
| Thermometer | ⚪ | ❌ SMC standaard uit, gesprek komt nooit af: 0 | 🟡 smpro logt wel, geen TempMilliC: heartbeat 0 | ❌ TSADC niet bedraad na de klokgok | ✅ vcmail | ✅ | ✅ TEMPSEN |
| Watchdog | ⚪ by design | ✅ van iBoot overgenomen | ❓ SBSA uit GTDT, QEMU heeft er geen | ✅ reset-scope gemeten 06-08 | ✅ BCM-PM | ✅ | ✅ probe-gated |
| RNG | ⚪ jitter, TCG heeft geen RNDR | ❓ RNDR als FEAT_RNG meldt, niet gezien | ✅ RNDR of SMCCC | ✅ jitter, TRNG ná boot | ✅ RNG200 | ✅ | 🟡 jitter, luide WARNING |
| PCIe + opslag | ✅ ECAM, NVMe | ✅ apcie, eigen venster | ❓ MCFG, NVMe | ⚪ | ⚪ | 🟡 RP1 wel, NVMe pending | ⚪ |
| Framebuffer | ⚪ | ⚪ buffer bestaat, niemand scant hem uit | ✅ GOP | ✅ VOP2 in gui-smaak | ✅ vcfb | ✅ vcfb | ⚪ |
| DVFS / klok | ⚪ | 🟡 vast plafond E5/P6, geen beleid | ⚪ firmware | 🟡 U-Boot-klok, geen beleid | ✅ vcmail | ✅ | ⚪ |

## Wat er sloppy is, op volgorde

1. **Interrupts alleen op de testmachine.** Het contract noemt de NIC-interrupt optioneel, en dat is
   precies waarom niemand het miste. Zie de lijst hierboven voor de goedkoopste route per board.
2. **Temperatuur is 0 in de heartbeat op drie boards.** M4 via de dode SMC, Radxa omdat de TSADC na
   de klokgok nooit meer bedraad is, en UEFI omdat de smpro-telemetrie wel logt maar niet in het
   contract zit. De leader plant dus zonder thermometer op precies de boards met de meeste cores.
3. **Ijzer-bewijs na de boot.h-refactor ontbreekt.** Pi 4, Pi 5, Radxa en Altra staan sinds 02-09 op
   "was bewezen". HOP's eigen idle is op Radxa, Pi 4 en Altra nooit gemeten.
4. **LicheeRV hardcodet wat het contract wil meten.** Privilege geeft altijd 3 terug, MemTotal is een
   constante, en de Reset-poot is sinds de hop dood omdat alleen de C906L resetbaar is en HOP daar nu
   zelf woont.
5. **Apple's State is boekhouding en Reset ontbreekt.** Het PMGR-stopbit stopt een core niet, dus een
   ingetrokken core parkeert zichzelf. Consistent met de andere ARM-boards, maar een tight loop op een
   dedicated core is daar het bewijs nog niet voor geleverd.

## Bijhouden

Een nieuw board krijgt een kolom; een nieuwe poot in `board.Board` of `board.Cores` krijgt een rij. Een
cel gaat pas van ❓ of 🟡 naar ✅ met een datum en een meting erbij, niet op een redenering: de
consoleregels `net: RX wakes on the NIC interrupt` / `net: RX polled every 300µs`, `idle: waker on`,
`watchdog: hardware reset armed` en de `hopos.idlestat=1`-regel zijn de bewijzen die erbij horen.
