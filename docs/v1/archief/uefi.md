# UEFI-boot + ACPI-discovery — het servers-dossier (Ampere Altra)

Stand 2026-07-13. **Bewezen in QEMU** (-M virt + EDK2-firmware, -cpu
neoverse-n1 = het Altra-silicium): PE-stub → ExitBootServices → relocatie →
EL2 → tamago-runtime → ACPI-discovery (cores/RAM/ECAM/SPCR/PSCI), inclusief
een levende PSCI 1.1-SMC-call en PCIe-enumeratie door de MCFG-ECAM. De weg
is byte-voor-byte dezelfde als op de echte Altra: FAT-medium →
`EFI/BOOT/BOOTAA64.EFI`.

Code: `metal/board/uefi` (stub + goos-hooks), `metal/fw/acpi` (tabellen),
`metal/cmd/probeuefi` (de discovery-probe), `image/mkkernel -pe`
(PE/COFF-verpakking), `image/uefi-run.sh` (QEMU-proeftuin).

## De boot-keten

1. Firmware laadt de PE op een **willekeurig adres** en roept de entry als
   UEFI-app: x0=ImageHandle, x1=SystemTable, AAPCS64, MMU aan
   (identity-mapped, 4K-pages, TTBR0), **EL2** op servers.
2. De stub (`metal/board/uefi/init.s`, positie-onafhankelijk):
   firmware-banner via ConOut → `AllocatePages(AllocateAddress)` op
   `[KernelStart, KernelStart+KernelSize)` (bewijst dat het venster vrij is;
   bezet = "RAM WINDOW BUSY" + hang) → `GetMemoryMap` (snapshot in een
   Go-global) → `ExitBootServices` (lus: MapKey moet vers zijn) → kopie van
   de hele image naar het linkadres → dc civac + ic iallu → MMU uit →
   sprong naar de L-kant.
3. `bootKernel` (op het linkadres): boot-EL vastleggen, HCR_EL2 = RW
   (**wist ook E2H**, zie valkuilen), CNTHCTL/CNTVOFF, drop naar EL1,
   `_rt0_tamago_start`.
4. Go: `hwinit1` parseert EFI-configuratietabel → RSDP → XSDT → SPCR →
   console leeft; de main leest MADT/MCFG/FADT en de UEFI-memory-map.

SystemTable, memory-map en boot-EL overleven als Go-globals: de stub
schrijft ze ná de kopie op de L-kant. De slide komt uit PC-relatief (ADRP)
vs. absoluut (`DATA $·sym(SB)`), de imagegrootte uit `runtime.end`.

## Zelfkiezend RAM-venster (universeel, sinds 13-07 avond)

Een Go-image zit vol absolute adressen en kan niet verplaatst worden — maar
één PE kan meerdere identiek gecompileerde varianten dragen, elk gelinkt op
een eigen kandidaat-venster. `mkkernel -pe` accepteert daarom N `-elf`'s
(zelfde build, ander `-T`), leidt per variant het laadadres af, **patcht
`runtime/goos.RamStart`** per variant (de bron van waarheid voor "waar
draai ik" — `uefi.Base()`; er is géén KernelStart-constante meer) en vult de
kandidatentabel `uefiSlots` in variant 0. De stub probeert de kandidaten op
volgorde met `AllocatePages(AllocateAddress)` — de vraag "is dit venster
vrij op dít bord" — kopieert de wínnende variant naar zijn linkadres en
zet daarna ImageHandle/SystemTable/memory-map op de L-kant over. Diagnose
"RAM WINDOW BUSY" verschijnt alleen nog als álle kandidaten bezet zijn, en
print dan de vrije regio's (QEMU-bewezen, incl. fallback: eerste kandidaat
bezet → variant 2 gekozen → volledige boot + DHCP-lease).

Kandidaten (image/uefi-run.sh, `SLOTS=`): 0xB0000000 0xA0000000 0xC8000000
0x88000000 0xE8000000 0x50000000 — gespreid over het lage DRAM-venster van
de Altra (0x80000000..0xFFFFFFFF; 0x90000000 is daar bezet, gemeten) plus
een lage QEMU-terugvaller. Zes varianten ≈ 20MB BOOTAA64.EFI: irrelevant
op een stick, en één bestand boot overal.

## Gemeten valkuilen (13-07, QEMU + EDK2-debugbuild)

1. **EDK2 image-protection vs. één RWX-sectie.** Eén grote
   `.text`-sectie met W+X wordt door DXE read-only-executable gemapt → de
   stub kreeg een data abort op zijn éérste global-store. Fix: échte
   secties per ELF-PT_LOAD (RX/RO/RW, Linux-stijl) in `mkkernel -pe`.
   Strikte W^X-serverfirmware eist dit sowieso.
2. **ACPI-tabellen zijn device-gemapt → geen Go-memmove.** De tabellen
   liggen buiten onze RAM-declaratie (nGnRnE): unaligned access = alignment
   fault (EL1 exception in `slicebytetostring`). Fix: `metal/fw/acpi` kopieert
   eerst met uitgelijnde `dev.Read32`-reads naar eigen RAM en parseert de
   kopie. (Zelfde les als de DTB op de Pi.)
3. **HCR_EL2.E2H.** CPU's met VHE (Neoverse-N1 = de Altra!): EDK2 zet
   E2H=1. `bootKernel` schrijft HCR_EL2 vers (RW, E2H=0) vóór de drop.
4. **Stille vroege panics.** Vóór de SPCR-parse is printk een no-op; een
   panic tussen rt0 en hwinit1 is dus onzichtbaar. Debugrecept: zet
   `uartBase` in `metal/board/uefi/uefi.go` tijdelijk hard op het
   platform-UART (QEMU virt: 0x09000000) — zo is valkuil 2 gevonden. En:
   QEMU's monitor-socket + `info registers` wijst de hangende PC aan
   (`-monitor unix:...`); PC op `arm64.exit.abi0` = runtime-exit(2) = panic.

## Het PE/COFF-recept (mkkernel -pe)

Gemodelleerd naar de Linux-arm64-kernel (`arch/arm64/kernel/efi-header.S`)
— de PE die elke EDK2 aantoonbaar slikt — en geverifieerd tegen EDK2's
`BasePeCoff.c` (master, 13-07-2026):

- DOS-stub: alleen `MZ` + `e_lfanew`@0x3C. COFF: Machine 0xAA64,
  Characteristics **0x0206** (EXECUTABLE | LINE_NUMS_STRIPPED |
  DEBUG_STRIPPED) — **géén** RELOCS_STRIPPED (dat betekent "alleen op
  ImageBase laden" en de DXE-core heeft daarvoor geen fallback).
- Optional header PE32+ (magic 0x20B): ImageBase 0, Section- en
  FileAlignment 0x1000 (RVA == bestandsoffset), Subsystem 10
  (EFI_APPLICATION), SizeOfOptionalHeader **exact** 0x70+8·N.
- **NumberOfRvaAndSizes = 6, nooit 5**: EDK2 checkt `< 5` (off-by-one) en
  leest bij N=5 de sectietabel als reloc-directory.
- Relocatie-oplossing: een `.reloc`-sectie met één leeg blok (12 bytes:
  PageRVA + BlockSize + twee IMAGE_REL_BASED_ABSOLUTE-padding-entries), waar
  de Base-Relocation-datadirectory naar wijst. Effect = nul fixups, maar de
  image geldt als verplaatsbaar (RELOCS_STRIPPED uit); de entry-code is PIC
  en verhuist zichzelf. Een reloc-directory met Size=0 zou óók werken
  (EDK2: "no base relocs to apply"), maar het lege blok is de robuustere
  variant die loaders zonder die tolerantie ook accepteren.
- Secties: per PT_LOAD één sectie met de echte permissies; BSS-nullen
  zitten al in de raw data (mkkernel-conventie, geen zero-fill-afhankelijkheid).
- Entry-RVA = 0x1000 + (ELF-entry − load); de tamago-entryketen
  (`_rt0_arm64_tamago` → `goos.CPUInit` → `cpuinit`) is volledig
  PC-relatief, dus geldig op elk laadadres.

Offsets die de stub gebruikt (UEFI 2.x, 64-bit): SystemTable: ConOut 0x40,
BootServices 0x60, NumberOfTableEntries 0x68, ConfigurationTable 0x70
(entries: GUID 16B + VendorTable 8B, stride 24). BootServices: AllocatePages
0x28, GetMemoryMap 0x38, ExitBootServices 0xE8 (en SetWatchdogTimer 0x100 —
BDS wapent 5 min vóór StartImage; ExitBootServices ontwapent hem).
OutputString = SIMPLE_TEXT_OUTPUT+0x08 (UCS-2, `\r\n`).
EFI_ACPI_20_TABLE_GUID in geheugen: `71 E8 68 88 F1 E4 D3 11 BC 22 00 80
C7 3C 88 81`. EFI_MEMORY_DESCRIPTOR: Type@0 (u32), PhysicalStart@8,
NumberOfPages@0x18 — **itereer met de teruggegeven DescriptorSize** (0x30
bij EDK2, niet sizeof=0x28).

ACPI-offsets (metal/fw/acpi): RSDP: rev@15 (≥2), XSDT@24. SDT-header 36B.
MADT: entries@44; GICC=type 0x0B (len 76/80/82 per ACPI-versie — parse op
Length): UID@8, Flags@12 (bit0 Enabled), **MPIDR@68**; GICD=0x0C (24B):
base@8. MCFG: entries@44, 16B: base/segment/startbus/endbus. SPCR:
iface@36 (0x03=PL011, 0x0E=SBSA-subset), GAS-adres@44. FADT:
ARM_BOOT_ARCH@129: bit0=PSCI, bit1=HVC-conduit (0 = SMC).

## QEMU-proeftuin

`image/uefi-run.sh`: bouwt de probe, verpakt als BOOTAA64.EFI in
`uefi-esp/EFI/BOOT/`, en boot met de brew-EDK2:

- pflash unit 0 = `edk2-aarch64-code.fd` (readonly, al 64MB), unit 1 =
  `uefi-vars.fd` (64MB nullen, eenmalig).
- `-drive file=fat:rw:uefi-esp` (vvfat) achter `qemu-xhci`+`usb-storage`:
  dezelfde USB-semantiek als de Altra-stick. Vvfat-regels: geen
  host-writes terwijl de gast draait, geen -snapshot.
- `-M virt,virtualization=on` → firmware op EL2, PSCI-conduit = SMC
  (QEMU-regel: gast heeft EL2 ⇒ SMC) — matcht onze SMC-only-invariant.
- `-cpu neoverse-n1` = Altra-silicium mét VHE: test de E2H-normalisatie.
- `-boot menu=on,splash-time=0` slaat de BDS-wachttijd over.

## USB-recept voor de echte Altra

1. Stick met GPT + één FAT32-partitie (ESP-type is netjes, maar de
   removable-media-regel eist alleen FAT):
   `diskutil eraseDisk FAT32 HOPOS GPT /dev/diskN`
2. `image/uefi-run.sh` draaien (of alleen de eerste twee stappen ervan) en
   de boom kopiëren: `cp -r uefi-esp/EFI /Volumes/HOPOS/ && diskutil eject
   /Volumes/HOPOS`
3. Altra: seriële console erop (SPCR wijst hem straks zelf aan; de banner
   "HopOS: UEFI stub" komt al uit de firmware-console), booten; de
   BDS pakt `\EFI\BOOT\BOOTAA64.EFI` van removable media automatisch —
   eventueel eenmalig de USB-entry kiezen in het boot-menu.
4. **Secure Boot moet uit** (of de image-hash enrollen): ongesigneerde
   BOOTAA64 wordt anders door DxeImageVerificationLib geweigerd.
5. **Het scherm praat mee via GOP** (sinds 13-07 avond; daarvóór zweeg het
   ná de stub — Altra-boot #2 leek daardoor "stil" terwijl hij vermoedelijk
   draaide): de stub vraagt vóór ExitBootServices het Graphics Output
   Protocol op (LocateProtocol, BS+0x140; alleen PixelFormat 0/1 = 32bpp
   lineair) en de Go-kant doet fb.Init — beeld = firmware-buffer, geen
   driver. printk spiegelt naar UART én scherm. QEMU-pixelbewijs: ramfb +
   monitor-screendump toont de volledige probe-output. Kanttekening: een
   framebuffer boven de 512GB-VA-grens (tamago TCR) blijft uit; de seriële
   console blijft de primaire waarheid.
6. Faalmodi: "RAM WINDOW BUSY" + regiodump = álle zes kandidaten bezet
   (voeg een venster uit de dump toe aan SLOTS en herbouw). Stilte op de
   sériële console na de discovery-regels = vroege panic → valkuil 4.

## Node-config: hopos.cfg op de stick (sinds 17-07)

Naast `EFI/BOOT/BOOTAA64.EFI` leest de node zijn platform-config uit een
tekstbestandje **`hopos.cfg` op de ESP-root** — het cmdline.txt-model van de
Pi: de stub vraagt het vóór ExitBootServices via het firmware-SimpleFileSystem
op (géén FS-driver in HopOS; de firmware leest zijn eigen FAT, HopOS parseert
alleen — `uefi.BootConfig`). Beheer = het bestandje op elke computer bewerken;
geen rebuild, geen EFI-shell. Ontbreekt het bestand, dan draait alles op
defaults (1 HOP-core, vluchtige standalone-staat, geen auth).

Sleutels (whitespace/regel-gescheiden `key=value`, geen spaties in waarden;
zelfde namen als de Pi-cmdline):

```
hopos.cores=2                # cores voor de HOP-runtime (default 1); rest = app-slots
hopos.node=altra-1           # node-identiteit (default hopos-1)
hopos.cluster=hopos          # clusternaam
hopos.apikey=…               # HMAC-auth (X-Hop-Auth) op agent- én leader-API
hopos.s3.endpoint=https://…  # S3-compatibele store; endpoint+bucket zetten
hopos.s3.bucket=…            #   de S3-lease/staat-backend aan: de leader
hopos.s3.region=…            #   commit zijn gewenste staat (state/<cluster>)
hopos.s3.key=…               #   en laadt hem bij boot — jobs overleven een
hopos.s3.secret=…            #   reboot. Object weghalen = schoon booten.
hopos.s3.pathstyle=1         # optioneel: path-style S3-URL's
```

Kanttekening: de apikey/secret staan plaintext op de FAT-stick — hetzelfde
vertrouwensmodel als cmdline.txt op de Pi-kaart (fysiek bezit = de node).
De waarden komen nooit in logs of in de repo.

## De NIC: Intel I210 (igb-familie) — driver bewezen

Derek bevestigde: de Altra Dev Kit heeft de Intel I210 (Linux: igb).
`metal/driver/nic/igb` is de HopOS-driver — gem.go-vorm (polled, één RX/TX-queue,
go-net NetworkDevice), maar met de igb-registerfamilie en **advanced
descriptors** (het enige type dat Linux gebruikt én dat QEMU's igb-model
emuleert). **End-to-end bewezen (13-07) tegen QEMU's igb (82576,
8086:10c9)**: BAR0 uit de firmware-config (pcie.Device.BAR — read-only, wij
wijzen niets toe), reset + MAC-autoload uit RAL0/RAH0, MDIC → PHY
BMCR-autoneg-herstart (zónder die herstart komt de link niet op — gemeten;
Linux doet hem ook altijd), link 1000FD, ringen in een
memory-map-geverifieerd DMA-venster (uefi.IsUsableRAM) direct boven de
RAM-partitie, en een volledige DHCP-lease als TX+RX+DMA-bewijs in één.
I210-verschillen t.o.v. QEMU's 82576 die op het bord gemeten moeten worden:
device-ID 8086:1533, NVM-MAC-autoload, echte PHY-autoneg-tijd (QEMU is
instant). probeuefi herkent de hele familie (10c9/1533/1536-1539 I210's,
1539 I211) en draait de proef automatisch.

QEMU-testrecept met de NIC achter een root-port (zoals op de Altra):
`-device pcie-root-port,id=rp1,chassis=1 -device igb,bus=rp1,netdev=n1
-netdev user,id=n1`. LET OP: een gewijzigde PCI-topologie maakt opgeslagen
boot-entries in de varstore ongeldig → EDK2 valt in de Shell; verse
`uefi-vars.fd` (64MB nullen) lost het op.

## De volledige HOP-node (cmd/hopos) — QEMU-groen sinds 13-07 avond

`image/uefi-run.sh agent` bouwt en boot de échte node (agent + leader +
slots + stage-2 + NAT) op het uefi-board — zelfde script als de probe, één
mode-argument verschil: MADT→CPUOn/CoreID, MCFG→igb, GOP-scherm,
layout-plan in de "carve" (32MB tussen de 64MB Go-RAM en het einde van de
stub-claim — samen een 96MB-claim per venster; gedimensioneerd voor
SlotCap=128 slots, sinds de net-ringen naar de partitie-staart van elk slot
verhuisden en de Go-RAM op meting kromp — was 544MB; pariteit met
CARVE_SIZE/REVOKE_OFF in init.s), VBAR_EL2 in
bootKernel, SBSA-watchdog uit de GTDT, 48-bit-MMU via mmu48.go. Bewezen:
job gesubmit → artifact-fetch → slot-start → app RUNNING, restart_count 0.

Drie gemeten avond-valkuilen (naast de drie van 's middags):

5. **EFI_RNG_PROTOCOL kan eeuwig blokkeren** (EDK2 zonder werkende TRNG
   pollt oneindig in GetRNG — PC danste in firmware-code). De stub roept
   hem daarom NIET aan; de hash-DRBG seedt zwak (teller) en meldt dat via
   RNGSeeded. Echte entropie: backlog (begrensd/jitter).
6. **tamago's tekst-grens-L3 ligt op Base()+0x8000** (mmu.go:
   l3pageTableStart + l3pageTableSize*8) — het "vrije gat" onder de image
   is dus níét vrij. Onze L0/MapHigh-tabellen leven daarom in de carve.
7. **App-images delen het cpuinit-symbool met de PE-stub** (-tags uefi +
   linkcpuinit): een app-core onder stage-2 entreert dezelfde entry — de
   stub heeft daarom een EL-discriminator (EL1 = app-core → direct de
   runtime in; EL2 = firmware-entry → het volle UEFI-pad).

Dereks review van 14-07 (15 punten) is volledig verwerkt; de belangrijkste
structurele gevolgen:

- **MMU-tabellen (L0/hoge L1's) wonen op Base()+0x9000..0xFFFF** — binnen de
  Normal-WB-gemapte Go-RAM, dus walker-coherent (de carve is device-gemapt:
  daar schrijven gaf stale walks op echt silicium; en +0x8000 is tamago's
  eigen grens-L3).
- **slotHint**: slots.Start patcht het slotnummer in uefi-app-images
  (symbool board/uefi.slotHint) — MPIDR is op servers geen slotnummer
  (Altra: aff0=0), dus geen MPIDR-ijking meer nodig voor de slot-identiteit.
- **igb-TX bewaakt de ring-occupancy** (max nTx−1 uitstaand; TDT==TDH is
  "leeg" voor deze familie) naast de per-descriptor-DD.
- **ECAMWindow()** rekent bus-ranges in uint64 (bus 0-255 wrapte in uint8
  naar 0) en mapt vanaf StartBus.
- **RNG = jitter-geseede hash-DRBG** (CNTPCT-delta's over 512 hash-rondes);
  het EFI_RNG_PROTOCOL blijft bewust ongebruikt (kan eeuwig blokkeren).
  Hardware-TRNG (begrensd/SMCCC) blijft backlog vóór productie-TLS.
- **Slot-pool wordt tegen de UEFI-memory-map getrimd** vóór layout.UsePlan.
- **Echte asm/Go-pariteit**: bootKernel schrijft REVOKE_OFF/CARVE_SIZE/
  MEMMAP_CAP naar globals; board-init panict op drift.
- **SBSA-watchdog**: WOR = timeout/2 (tweetraps WS0→WS1) met ondergrens.
- GTDT: off-by-one + secure-frame-skip; ACPI-Length begrensd (36..4MB);
  EL1-entry zonder relocatie parkeert (WFE) i.p.v. wild doorstarten;
  MADT-Enabled-filter; tools/test.sh heeft een uefi-gate.

## Open punten richting HopOS-op-Altra

- **SMP fase 2**: MADT-MPIDR-lijst + PSCI CPU_ON (conduit uit FADT; op
  QEMU/Altra SMC — bewezen call-pad). CoreID moet van aff0 naar een
  MPIDR→slot-mapping (Altra nummert aff1/aff2).
- **tamago's TCR_EL1 = 39-bit VA (512GB)**: de Altra's DRAM boven de
  512GB-grens is daarmee onbereikbaar tot de MMU-laag 48-bit VA kan;
  de probe-memory-map vertelt hoeveel er werkelijk boven ligt.
- **GICv3 via MADT/GICD** (nu alleen gerapporteerd), NIC-drivers
  (i210/X550 achter de MCFG-ECAM — `pcie.Scan` werkt er al doorheen),
  layout-plan × 128 slots, RNDR-loze entropie (N1 heeft geen RNDR:
  firmware-RNG of jitter vóór TLS).
- QEMU-max is 512 cores TCG (`-smp 128` werkt maar traag) — de
  MADT-parse op 128 GICC's kan dus ook vooraf gesimuleerd worden.
