# Shared allocator, 2026-10-01

`heap` contains the allocator with free lists and boundary tags that used to
live in `applib::heap`. Apps supply their SMP core identity (the trait
`heap::CoreId`); `board::heap` uses the same implementation for the kernel.
Only the primary kernel executor allocates; ISRs and parked secondary cores
do not.

Cause: during the Lumen test on the O6N, OP_SYNC stopped with
`out of memory (483328 bytes)`. The old kernel allocator only took back the
most recently handed-out block. With interleaved metadata, network and I/O
allocations, freed memory stayed occupied. The TCP console then failed too.
Only giving the kernel more RAM does not solve that growth.

The shared allocator merges adjacent free blocks and respects the existing
RAM ceiling. Alignment is capped at 4096 bytes (`MAX_ALIGN`), as in the app
allocator. There is no second copy of the algorithm. The public
`applib::heap::Heap` contract stays as a type alias; the global allocator
stays owned by the binary in question.

Validation: ten allocator tests, including 80,000 deterministic mixed
operations, four concurrent threads and freeing from another thread. The
board regression test repeats 2,000 metadata/network cycles in 4 MiB; after
every cycle everything is back and all 6,000 allocations succeed. Board and
applib tests pass, as does Clippy. Lumen runs with this kernel and app in
QEMU: 43 checks for HTTP, WebDAV, management and persistence after recreate
pass. 187 kernel tests plus two doc tests also pass. The QEMU warm flip keeps
Hop, an existing outgoing TCP connection, all four NAT flows and the HopFS
generation; after the flip it can place an app again. The reboot test finds
the black box. That flip test used `HOP_PATCH=0 HOP_DIR=/tmp/lumen-hop-hardware`
to keep the known Hop ELF; the regular patch helper has a lean version
mismatch with this old Hop pin and was not used for this test.

On the physical O6N the old kernel had already exhausted its heap when the
fix was ready. After a physical restart the bundles `lumen-heap` and then
`lumen-heap-recover` were confirmed landed. Lumen candidate 15 resumes from a
verified checkpoint and writes new images to the NVMe again. The second flip
let the existing VPU recovery routine clean up a stuck session. Long-running
hardware validation of the memory fix is still in progress.

## Size classes, 2026-10-03

With only one free list per power of two (32 to 63 bytes in one list), a
request for 48 bytes walked past every free 32-byte block. JSON work leaves
thousands of those behind: Stulp measured 505 ns per free plus allocation
with 4547 free blocks on the host, and on the LicheeRV that became a 250 ms
bootstrap and half-second callbacks. Since 2026-10-03 every size up to and
including `SMALL_LIMIT` (1 KiB) has an exact class (a multiple of the 16-byte
grain), so the first block in its list always fits; above that there is one
class per bit position. `alloc` searches first-fit in the class of the
request and above it takes the first block that fits via a bitmap of
non-empty classes. That made the same case 53 ns
([apps.md](apps.md), "The allocator counts"). The heap crate now has eleven
tests.

Reproduce:

```sh
cargo test -p heap -p board -p applib
cargo clippy -p heap -p board -p applib --all-targets -- -D warnings
GUI=1 HOPOS_STAMP=lumen-heap FEATURES=media sh image/flip-bundle.sh o6n
```
