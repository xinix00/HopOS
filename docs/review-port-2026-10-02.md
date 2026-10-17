> Snapshot of 2026-10-02; later status is in [description.md](description.md) chapter 8 and [ALLES.md](../ALLES.md).

# Port review Go v2.2.7 next to Rust v3, the other boards (2026-10-02, evening)

Five reading agents (Apple, the Pis, Radxa plus QEMU virt, UEFI/O6N/Altra,
the arm64 kernel) with their partial reviews. Read only, nothing built. The
partial reports are not in the repo; this is their summary.

## Not dropped

- The arm64 switcher (cpu/src/el2/switch.rs) is Go line for line: WFE on
  the event stream instead of a spin pause, FP via hvcYield on the app
  side, stage-2 revoke with TLBI ALLE1IS and SEV. All the hardware lessons
  from the Go comments are in it.
- The Pis: everything with a hardware lesson behind it is ported, sometimes
  stricter.
- The Radxa board side (CRU, GRF, pinmux, PHY reset, dwmac4, mdio, TRNG,
  WDT, TSADC): complete, with the measured order of 2026-08-05/06 kept.
- hopfs, conport, irq, drbg, trng, memattr, pool, partmem, grants, flip ABI:
  ported, often better.

## Gaps that count on hardware (by impact)

1. **Scrub without clean+invalidate first** (hopos/src/cage.rs `ArmCage::clear`).
   Go: CleanInv, Clear, Push (A76 2026-07-10). Rust: Clear, Push. On boards
   where the pool is mapped Device (M4, Radxa), the `dc cvac` afterwards
   writes the dirty lines of the previous tenant back over the zeros: the
   heap and stack of the new app contain old data. A tamago app assumes
   zero. Three agents independently. Fix: one line `dev::pull` before
   `dev::clear`.
2. **M4: app cores without an alarm and with the wrong idle model**
   (hopos/src/cage.rs `APP_IDLE_MODE` = 0 outside QEMU; hopos/src/slots.rs
   `wake_all` kicks mpidr 0). An app sleeps on WFE at EL1, which does not
   sleep on the M4: every busy app core spins (Go 2026-09-02: 74 % and
   1.3 M rounds/s, with yield plus waker.go 0 % and 47 wakes/s). Whoever
   does yield (share group, SMP secondary) sleeps in WFI at EL2 and nobody
   kicks it at its wake time. Fix ~60 lines, copy from kern/slots/waker.go;
   first the alarm and the targeted kick, only then the mode on yield.
3. **FP state on the OS core** (cpu/src/el2/oscore.rs, and cpu/src/riscv/oscore.rs).
   A resident interrupted by IRQ, kick or CNTHP loses q0..q31, FPCR and
   FPSR to the next resident. Harmless now (kernel and Hop are softfloat),
   wrong as soon as a hardfloat resident (tamago app, since 2026-10-02)
   shares the OS core. The same class as today's riscv FP bug. ~40 lines of
   asm; the ctx blocks are Device, so via GP registers or a Normal buffer.
4. **Core reclaim is missing** (hopos/src/cage.rs `join`: 5 ms, then
   HOPOS_SHARE_PENDING and Ok). Go: after 2 s HOPOS_CORE_RECLAIM, revoke the
   holder's cage, another 2 s, otherwise ErrDispatch (2026-08-14/15: hours
   of hostage-taking). Now a computing resident holds a share group core
   forever and the start counts as successful. 30 to 40 lines.
5. **Watchdog pets on weaker evidence** (hopos/src/watchdog.rs `run`: uplink IP
   plus Hop's heartbeat). Go: a fresh TCP connection to its own :8080 (the
   deafness of 2026-08-02: new connections and ICMP dead, everything inside
   healthy). A deaf node does not reset now. Self-dial 50 to 80 lines, or a
   progress counter of the switch actor as a cheaper flavor (20 lines).
6. **Flip on Apple calls PSCI** (hopos/src/flip.rs `reset` SYSTEM_RESET, `cores_off`
   AFFINITY_INFO). Without EL3 that is an UNDEF: the way back parks the
   core (only the WDT brings it back after 30 s) and the cold flip crashes
   the kernel after freezing the storage. The cold flip on the M4 has never
   been done. ~15 lines: reset via the WDT, refuse cold without EL3.
7. **Altra, first boot** (board/uefi/src/boot.rs MAP_CAP 64 KB, slots.rs
   `_dropped` silent, one kernel window 0x8800_0000). Go: 256 KB (thousands
   of descriptors, 2026-07-14), six window candidates because 0x9000_0000
   turned out to be taken (2026-07-13). The Altra has never booted v3.
   ~20 lines plus candidates.
8. **No second attempt without link at boot** (all boards, `probe_nic`
   "no link"). Go retried forever (2026-09-19). ~20 lines.

## Performance, measure first

- Ring: pull/push on head and tail always, even with Coherence::Hardware;
  Go skipped them on coherent rings and cleaned the peeked RX head once per
  burst (T13 2026-09-03: half; bulk HOP to app up to 4x, 2026-09-04).
- NIC buffers Normal-NC instead of WB on M4 (tg3), O6N and Altra (Go
  2026-07-17 and 2026-09-03). Could be part of O6N v3 76 to 78 versus v2 111
  to 116 MB/s inbound. ~8 lines per board.
- An app that keeps computing (BURN, vitals cpu) hears RX only on the
  pump's poll timer, which grows up to 1 s. Six lines in applib
  (`after_deferrable(poll.lo)`), works on every board.
- igb (Altra): no 10 ms after CTRL.RST, RDT read back before
  RXDCTL.ENABLE.
- VBAR_EL2 not on a bare vector before the flip jump (diagnostics, Go
  2026-09-01).
- O6N: the measured _CPC class source is off (`highest: 0`), the MPIDR
  table decides small/big; the console on the SPCR UART where Go
  (2026-09-09) took the header UART. Works now, cost not measured.

## Small

Radxa: no serial-number fallback for the MAC (every Radxa without
hopos.node gets the same one), xHCI without barriers within a TRB on
Normal-NC, VOP2 clock tree outside the chain, UTMI width uncertain. Pi:
cmdline.txt without a length check (Go 2026-07-19), mailbox spins up to
500 ms on core 0, early fault dump gone, gui 16 bpp. Kernel: last words
after a stop get lost, conport does not report "full" to the client, app
lines go synchronously to the UART, docs/boards-radxa.md and the memattr
comment lag behind.
