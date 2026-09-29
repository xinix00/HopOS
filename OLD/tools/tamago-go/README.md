# TamaGo toolchain patches

These patches reproduce the local HopOS changes to
[tamago-go](https://github.com/usbarmory/tamago-go). They are retained here for
reproducible builds and a future upstream submission; they have not been
submitted or accepted upstream.

The tested base is commit `07ec1e5beb4b5ac27420a8969db4334746878df5`, recorded in
[BASE_REVISION](BASE_REVISION). Its `VERSION` reports `go1.26.4`. The earlier
reference to a `tamago1.26.4` toolchain tag was not verified; use the commit.
The TamaGo **module** version is separate from this compiler-source revision.

## Patch set

| Patch | Change | Reason |
| --- | --- | --- |
| [0001-runtime-idlehook.patch](0001-runtime-idlehook.patch) | Add `IdleMayReady`, `NextTimer` and `RunIdleTimers`; connect cooperative `preemptM` wakeups to `goos.Wake`; wake an idle M from `preemptone`. | A parked core must return to a scheduler safepoint for stop-the-world and timer work. Only an unblocked M with a running P and no runtime locks may service idle callbacks. Note waits, P-less threads and stopped processors remain waiters. |
| [0002-net-adopt-bound-addresses.patch](0002-net-adopt-bound-addresses.patch) | Adopt actual listener/connection addresses after `net.SocketFunc`. | Wildcard binds and ephemeral ports must report the address selected by the implementation. Nil addresses preserve the existing value. |
| [0003-runtime-findtimer-heap-bound.patch](0003-runtime-findtimer-heap-bound.patch) | Bound the `findTimer` walk at `heap[0]` (arm64, riscv64). | `WakeG`/`Relay` on a goroutine whose cached timer is not in the heap walked below the slice until address 0. Reached through `os/signal.Relay` from the arm64 interrupt handler: an app on a board with an interrupt doorbell (M4 vFIQ) faulted 16 bytes below its partition when the doorbell fired while the signal loop was not sleeping (L79). A miss now returns failure; a level interrupt fires again once unmasked. |

`RunIdleTimers` uses the same eligibility predicate as `IdleMayReady`.
`NextTimer` is called only from the eligible scheduler-idle path; it reads the
processor timer deadlines under that caller contract. Timer servicing uses the
existing timer machinery. These patches add no worker or timer. The wake hook
requests a cooperative safepoint; it does not interrupt arbitrary running code.

## Apply

From the HopOS checkout, with a clean TamaGo checkout at the recorded base:

```sh
TAMAGO_SRC=/path/to/tamago-go tools/tamago-go/apply.sh
```

The default source directory is `$HOME/tamago-go`. The script supports regular
checkouts and Git worktrees, skips patches already applied, and checks the
complete pending set before applying it. A conflict fails without partially
applying that set. It does not reset local changes or change the checkout's
revision. If an older version of patch 1 is installed, use a separate clean
checkout for this revised set; the script deliberately does not overwrite it.

For a fresh clone, check out the exact revision first:

```sh
git -C /path/to/tamago-go checkout --detach "$(cat tools/tamago-go/BASE_REVISION)"
TAMAGO_SRC=/path/to/tamago-go tools/tamago-go/apply.sh
```

The existing compiler rebuilds the changed runtime/net packages from its source
tree during application builds; these edits do not require rebuilding the
compiler executable with `make.bash`. Rebuild affected applications and kernel
images. Already linked binaries retain their original runtime and network code.

## Validation and submission scope

On 9 September 2026, applying both patches to pristine files from the recorded
base reproduces all four changed files in the tested local toolchain byte for
byte. Reapplication is a no-op. A conflicting second patch leaves the first
unapplied. The currently patched checkout is also recognized as already applied.

ARM64 and RISC-V application builds passed with these runtime sources. The
Pi4/Pi5 TCP acceptance includes SMP, normal GC and statistics collection,
repeated bulk transfers and complete SHA-256 verification; see
[release log L70](../../docs/v1/technical/release-logboek.md) and
[the recorded evidence](../../docs/v1/technical/release-evidence/2026-09-09-pi-tcp.json).
That acceptance also includes independent HopOS-ring and Lean-TCP fixes. It is
not an isolated proof that this runtime patch alone solves every transport issue.

Before upstream submission, retain these separate runtime and net patches and
include the caller contract, minimal reproductions and measured evidence. The
runtime API and cooperative-preemption integration remain subject to upstream
review. The `runtime/goos` hooks resolve to the TamaGo module's `goos` package;
module changes belong in that repository, not in this toolchain patch set.

When regenerating patch 1, include the new `src/runtime/idlehook_tamago.go`
explicitly: ordinary `git diff` omits untracked files. Validate against
`BASE_REVISION` and compare the patched result with the intended toolchain;
a successful build alone does not establish that the patch captures every edit.
