# PE-relocatie: één image + patch-tabel i.p.v. 6× dezelfde binary

Ontwerp 14-07 (avond), ingebouwd 15-07 (ochtend). Status: **GEBOUWD + BEWEZEN**:
PE 74 → 12,5MB (49.154 entries); QEMU-boots op drie vensters — delta 0
(0xB0000000), −0x28000000 (0x88000000, MEM=1536M) en −0x60000000 (0x50000000,
MEM=1024M) — allemaal agent-up + jobs running + 0 crash. De diff/verify draait
bij elke image-build en faalt hard bij een gebroken aanname.

## Het probleem

Een tamago/Go-binary linkt op een vast adres — geen PIC, geen relocatie. Het
RAM-venster op een UEFI-machine is pas bij boot bekend, dus `mkkernel -pe`
verpakt nu **zes volledig gelinkte varianten** (één per venster-kandidaat uit
`SLOTS`) en de stub kiest er bij boot één. Kosten: 6 × ~12,3MB ≈ 74MB PE voor
~12,3MB aan echte inhoud, en elke extra kandidaat kost 12MB.

## Het inzicht + de meting

Twee varianten verschillen uitsluitend doordat absolute adressen een vaste
delta verschuiven. Gemeten (14-07, `hopos-uefi-agent-0xB0000000.elf` vs
`-0xA0000000.elf`, PT_LOAD-inhoud plat gelegd, per 8-byte-woord):

- 1.548.840 woorden, **49.136 verschillend, waarvan 49.126 exact +delta**;
- de 10 uitzonderingen zijn ASCII op offset ~0xf88..0xfe8: het **Go build-ID**
  (hash over de input, verandert mee met -T) — géén semantiek;
- staart (niet-woord-uitgelijnd) identiek; geen enkel 4-byte/instructie-diff:
  arm64-Go materialiseert adressen PC-relatief (adrp+add), absolute pointers
  leven alleen in data/rodata/pclntab-structuren.

Conclusie: met `-ldflags "-buildid= ..."` (leeg build-ID op álle varianten) is
de diff **100% zuivere +delta-woorden** → een klassieke relocatietabel.

## Het ontwerp

**Build (image/uefi-run.sh):**
1. Link de canonieke variant (eerste `SLOTS`-kandidaat) + één schaduwvariant
   op een andere basis — beide met `-buildid=`. (Tijdens de inbouwfase: álle
   kandidaten blijven linken voor de verify hieronder; daarna volstaan 2.)

**mkkernel (nieuwe modus, naast de bestaande `-pe`):**
2. Leg beide ELF's plat (PT_LOAD → flat image, zoals nu) en diff per
   8-byte-woord. Elk verschil móét exact +delta zijn → offset in de tabel.
   Eén afwijkend woord of grootte-verschil = **harde fout** (geen stille
   terugval): dan is een toolchain-aanname gebroken en willen we het weten.
3. Verify (zolang we alle varianten nog linken): elke overige variant moet op
   precies de tabel-offsets +deltaK verschillen en verder byte-identiek zijn.
4. PE-inhoud: `[stub][header][payload (1×)][reloc-tabel]`.
   - header: payload-linkbasis, entry-offset, RamStart-offset (symbol-lookup,
     we bouwen zonder -s), kandidatenlijst (de huidige uefiSlots-tabel),
     reloc-count.
   - tabel: gesorteerde u32-offsets (payload < 4GB): 49k × 4B ≈ 192KB.

**Stub (boot, vóór de sprong):**
5. Kies venster-basis B zoals nu (AllocatePages over de kandidaten).
6. Kopieer de payload naar B (zoals nu), `delta = B − linkbasis`.
7. Reloc-lus (~15 regels asm): voor elke u32-offset o:
   `*(u64*)(B+o) += delta`. 49k woorden = <1ms.
8. Patch RamStart = B (deed mkkernel per variant statisch; wordt één
   runtime-write op het header-offset) en spring naar B+entry.

## Wat het oplevert

| | nu | met relocatie |
|---|---|---|
| PE (6 kandidaten) | 74MB | **~12,5MB** |
| extra kandidaat | +12,3MB | **+8 bytes** |
| linkstappen per build | 6 | 2 |

Kandidatenlijst kan daarna ruim (10+): robuustere boot, gratis.

## Risico's & antwoorden

- **Build-ID** → `-buildid=` op alle variant-links; diff-stap faalt hard als
  er tóch iets achterblijft.
- **Toekomstige toolchain breekt de aanname** (absolute immediates in code,
  4-byte-diffs): de diff/verify draait bij élke image-build en faalt dan
  luid — nooit een stille kapotte stick. Terugval: de oude `-pe`-modus blijft
  bestaan (probe gebruikt 'm sowieso).
- **Woord-toeval** (waarde die toevallig +delta verschilt maar geen pointer
  is): kan alleen bij waarden die tussen builds verschillen — en met gelijk
  build-ID verschilt er niets meer behalve adres-afgeleiden. De
  multi-variant-verify maakt de kans op een gemiste fout verwaarloosbaar
  (zelfde toeval moet dan voor élke deltaK gelden).
- **Cache-coherentie na patchen**: de stub schrijft vóór de MMU-overdracht en
  doet nu al cache-onderhoud na de kopie — de reloc-writes vallen daarbinnen
  (patchen vóór de bestaande clean).

## Inbouwstappen (15-07)

1. `-buildid=` toevoegen aan de variant-links + meetscript herdraaien → 0
   uitzonderingen bevestigen.
2. mkkernel: flat-diff + verify + nieuw PE-formaat (Go-kant, host-testbaar:
   unit-test met twee mini-ELFs).
3. Stub: reloc-lus + RamStart-runtime-patch (pe.go/init.s).
4. QEMU: boot + 7-storm op de reloc-PE; dan pas de multi-variant-verify
   terugschroeven naar 2 links.
5. Altra-stick.
