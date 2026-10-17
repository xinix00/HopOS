# Synchronous C code as a non-parking executor task

`applib::stacktask::Task` gives one synchronous function a stack of its own,
fully reserved up front. A callback uses `Suspender::wait(future)`: on
`Pending` the original executor returns and the C frames and their borrowed
buffers stay alive. So the executor keeps running the network pump that has
to deliver the system API replies for that callback. There is no nested
executor and no extra core is reserved.

The owner creates, polls and drops the task on the same core. The handle is
`!Send`/`!Sync`; state and stack keep their heap address when the handle
moves. Only the small module with the context switch and the stack
allocation contains unsafe. The Rust/C functions have ordinary ABI-conformant
stacks while running. ARM64 saves the callee-saved registers and, outside the
softfloat ABI, the FP state; RISC-V also saves the D registers for
rv64gc/lp64d. The x86_64 SysV variant exists, but has not been run on an
x86_64 host here yet.

`Task::new` is explicitly unsafe: the caller must show that the work fits in
the chosen stack of 64 KiB..16 MiB (outside that range it is `Error::Size`).
The slot heap has no guard pages. A callback must return after `Cancelled`;
no longjmp out of the task and no kept references to the private stack.
Panics across the extern-C entry abort. This is no licence for unbounded
recursion in external C code.

Drop passes on the cancellation and resumes the task until the function
returns. That way both local Rust values and C handles on its own stack are
cleaned up before release. The app's stop doorbell plugs into this without
extra code: `select(app.stopped(), task)` drops the task when the kernel asks
to stop, the C side sees `Cancelled` at its next `wait` (a long CPU query via
the progress callback with `wait(yield_now())`), and the app then does its
cleanup within the grace period (see [apps.md](apps.md), The stop). A future
whose I/O acknowledgement is unknown additionally needs a poisoned I/O owner:
only cleaning up the C stack does not make an unknown write outcome reliable
again.

The host tests cover deep local frames, repeated parking, integer/FP state,
cleanup of cancelled futures and tasks that never started. Replica's
`tools/qemu-persist.py` runs the real SQLite C engine through this bridge on
ARM64: commit, rollback, SIGKILL, cold restart, integrity check, cancelling a
long CPU query via SQLite's progress callback, and reopening with the same
SQLite heap. The complete RISC-V app builds; this test claims no RISC-V
runtime proof. Replica pins the SDK to a published tag (since
v3.0.0-alpha.16 with this module and `sys::Client::sync` in it); there is no
candidate copy or local patch any more. Since the default storage became
stateless (see [storage-sync.md](storage-sync.md)), a cold restart only finds
the database again on a node with `hopos.storage=stateful`.
