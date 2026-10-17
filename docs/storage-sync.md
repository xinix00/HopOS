# Confirmed storage barrier for apps

`applib::sys::Client::sync(path)` sends the additive `OP_SYNC` (20) over the
regular system API. An existing file or directory name drives the usual
slot, generation and mount check. `off`, `n` and data are zero/empty; a reply
contains only the confirmed HopFS tree generation. After deleting a rollback
journal the caller uses the parent directory.

The HopFS owner has calls from several apps on the disk at once, but one at
a time per slot and in order of arrival (only reads may run side by side, a
read changes nothing): so the barrier only starts once every earlier call of
that app is back from the device. At most one commit runs at a time; writes
from other apps run alongside and go with the next one. If there are changes
it runs the existing commit: data flush, metadata in the other place (body
before header), metadata flush. Only then does it confirm. Without changes it
still does a device flush. A volatile FS refuses with `VolatileStorage`. An
error in a flush or metadata write comes back via the system API, so an app
cannot mistake it for a durable commit.

The client does not repeat a lost confirmation. An error can mean that the
barrier is on disk after all: SQLite/Replica must then follow its own journal
and recovery protocol. An old kernel refuses the new opcode. This API does
not replace an exclusive database owner, a persistent volume, or safe
parking of the SQLite C stack during async I/O.

## Stateless by default

A confirmed barrier is durable on the disk, but whether the next cold boot
loads it depends on the node's config. The default is
`hopos.storage=stateless` (`image/cfg/default.cfg`): at a cold boot the
kernel ignores the index file and the tree starts empty
(`hopfs: stateless (hopos.storage), the index is ignored and the tree starts empty HOPOS_FS_STATELESS`),
so an app's volume, and everything it synced, is gone after a power cut or
reboot. A node that must remember sets `hopos.storage=stateful` in its own
config layer; then the last committed tree is loaded and the volume survives
a restart. A kernel flip landing (a found handoff, warm or cold;
`flip::landed` in `hopos/src/flip.rs`) always keeps the tree of the previous
kernel, whatever the setting. A firmware boot that finds a stale handoff pair
in memory (`HOPOS_FLIP_STALE`) is not a landing and so stays stateless. The
decision is in `hopos/src/storage.rs`.

## Tests

Host tests use a device cache in which only `flush` updates the durable
copy. They cover cold remount after writing, overwriting with the same size,
shrinking and journal deletion; every failure phase of the commit; a retry
after an error; generation/path refusals and no automatic retry.

The full test is `python3 tools/qemu-test-sync.py`. It only starts a
temporary QEMU disk, boots with `hopos.storage=stateful` (`BOOTARGS`), places
`apps/syncprobe` via Hop with a volume of its own, writes and syncs a journal
and 8192 database bytes, deletes the journal with a directory barrier and
stops QEMU with SIGKILL. The next boot must read the same volume, check the
bytes and miss the journal. The console and checks go to
`target/qemu-sync/`. This is a virtio test; it does not replace a power-cut
test with the physical NVMe controller.
