# Writing apps for HopOS

An app is one ELF in one slot: a cage with its own partition, a control page
to the kernel, a netstack (lean) on its own slot IP, and an executor that
runs the app's tasks. `applib::main!` sets that up; after that the app is a
set of `async fn` tasks on that executor (the Rust handbook, §2 and §4).
This document is about the one thing an app must do well to be a good
neighbour: **sleep on events, not on the clock.**

## Why this makes the difference

A slot gets its core from the kernel. When the executor idles (no task has
its bit set), applib's sleeper hands the core back with a wake time: the
earliest real timer. On a core of its own that is a `WFE`; on a shared core
(`sharegroup` in the jobspec, or a board that asks for yield-idle) it is a
yield to the kernel, and the switcher gives the core to the next resident.
At the wake time, or earlier on the doorbell (a frame in the RX ring), the
app comes back.

So the kernel shares time fairly between residents that sleep. What it
cannot do on arm64 is take time away from a resident that does not sleep:
there is no preemption, a turn lasts until the executor is idle. On riscv64
(the LicheeRV, all apps on one hart) the switcher has, since 2026-10-02,
taken the turn away after 10 ms when there are neighbours; that keeps the
node reachable, but an app that leans on it still burns its neighbours'
time. Two rules follow:

1. Every nap (`after(5 ms)` to see if something has arrived yet) is a wake
   of the slot, a full round of everything that is ready, and only then a
   release for at most those 5 ms. Ten tasks napping like that are two
   thousand wakes per second. On a core of its own that costs power; on a
   shared core it costs the neighbours their turn.
2. A long synchronous computation in one poll (crypto, serializing a large
   JSON document) holds the core until it is done. The neighbours see
   nothing in that time, not even their heartbeats.

What it looks like when this goes wrong, measured on the LicheeRV on
2026-10-02 with three Rust slots in one share group: an empty 404 from the
controller took 0.2 to 2 s over the LAN and once more than 20 s; every 8 to
11 minutes ten plugins lost their heartbeat (5 s) to the controller at the
same time. The Go predecessor on the same node and the same kernel never had
this problem. Not because Go is faster, but because Go blocked on the real
thing: a `Read`, a `select` on channels, a timer for the next deadline. The
Rust port looked every 1 to 5 ms whether something had arrived.

## Which core

Where Hop itself lives is set by the config (the board layer in
`image/cfg`, [boards.md](boards.md), "The config"): `hopos.hop.sharegroup=system`
(the OS core next to the kernel, the default where the kernel shares its
core) or another name, `hop` in the board layers (an app core of its own;
jobs with `"tags":{"sharegroup":"hop"}` share it and get their share of the
turns under load, as in any share group). Hop gets that name as
`HOPOS_HOP_GROUP` and counts it as free in its planning. With a group of its
own, `hopos.hop.core-class=small|mid|big` picks the kind of core: the first
core of the group comes from that class if one is free, otherwise from any
(`Placement::prefer` in `kern/src/pool.rs`; the same names as a job's
`tags.core-class`). On the M4 small is an E-core, on the O6N an A520; boards
without classes (the Pis, the Radxa, the Altra, the LicheeRV) take the first
free core, and on the OS core (`system`) `hopos.oscore` picks the core. On the
console: `HOPOS_HOP_GROUP`, `HOPOS_HOP_CLASS` and `class=` at the end of
`HOPOS_HOP_START`. A warm flip does not move Hop (`HOPOS_HOP_RESUMED`): the
new kernel takes over its group and core, the boot line says
`(carried over the flip)`, and if the new kernel's config asks for another
group, `HOPOS_HOP_GROUP_COLD` says that it only applies at the next cold
start.

Which core a slot gets is the kernel's choice (`kern/src/pool.rs`): without
`sharegroup` a core of its own (or `cores` contiguous ones), with a
`sharegroup` the cores of that group. The kernel's own core is shareable
too, as group `system`: the kernel is its fixed resident with priority, and
a resident gets the core in the kernel's idle time (PORT.md decision 2). A
job with `"tags":{"sharegroup":"system"}` goes there (one core, no SMP), and
so does, as a fallback, a single-core job without a group that finds no free
core of its own and does not ask for a `core-class` that excludes the OS
core, with one line:
`slot N: no free app core, joining the system group on the OS core
HOPOS_PLACE_SYSTEM`. A job that names another group stays with its group.
On the M4 the kernel does not share its core (Apple's EL2 flavor), so there
is no `system` there. Whether Hop sends out such a job is Hop's decision: it
plans against `HOPOS_CORES` (the app cores of its own that it hands out,
minus its own if it occupies one; the OS core does not count), and
`HOPOS_SYSTEM_CORE=1` tells it that the kernel shares its core. Then a job
in `system` costs it no core and always fits (memory does count), just like
a job in `hop` on Hop's own core (on any node); a single-core job without a
group and without `core-class` is passed through to the fallback on a full
node, and no longer counted once the kernel reports core 0. With
`HOPOS_SYSTEM_CORE`, `HOPOS_CORES` may be 0 (the LicheeRV: Hop on the only
app core); without it Hop makes it at least 1. A refusal is one line per job
on the console:
`hop: job NAME refused here: no capacity (cpu; ...) HOP_NO_CAPACITY`.

Besides the cores the kernel also says whether the board can flip cold:
`HOPOS_COLD_FLIP=yes|no|fresh` (`hopos/src/flip.rs`, `COLD_FLIP`, the same
value the kernel itself refuses with). `no` on a board without PSCI (the
M4): the app cores do not go off. `fresh` on a board where CPU_OFF does not
return (the Pi 5): cold only as long as no app core has ever run. Otherwise
`yes`. Hop decides with it before it stops a task: with `no`, and with
`fresh` once it has ever put something on an app core (or lives on an app
core itself), a cold flip is immediately a 502 with "ask warm", without any
task stopping.

## The rule

A task waits on exactly the things that can give it work, with one
`select`, and on nothing else:

```rust
loop {
    match select(self.stop.wait(), self.conn.readable()).await {
        Either::Left(()) => return,          // Drop cleans up.
        Either::Right(Err(e)) => return self.fail(e),
        Either::Right(Ok(())) => {}
    }
    while let Some(frame) = self.read_frame()? {   // reads until WouldBlock
        self.handle(frame).await?;
    }
}
```

What you have for that, all from applib and the `sync` crate:

| Waiting on | Building block |
| --- | --- |
| bytes on a TCP connection, without reading them yet | `TcpStream::readable()` (the read itself also blocks properly: `read()` waits via lean's waker) |
| a datagram | `UdpSocket::readable()`, or `recv_from()` |
| a new connection | `TcpListener::accept()` |
| a message from another task | `Channel<T, N>` and `Mailbox<T, N>` (`recv().await`) |
| the reply to a request to another task (an actor) | `Oneshot<T>` (`recv().await`), or `oneshot::call(inbox, reply, msg)`: send, wait, take the reply |
| a doorbell from another task | `Signal::wait()` (level-triggered: ten rings at once are one) |
| a real deadline: heartbeat, timeout, keepalive | `exec.after(d)` or `exec.until(at)` |
| a safety net that only counts while the core is awake anyway | `exec.after_deferrable(d)`: fires in the first round after its deadline, but does not wake a sleeping core |
| two or more of the above | `select(a, b)`, nested for more |
| the app's stop | `app.stopped()` or `app.stop().wait()`: the stop doorbell, see below |

### The stop

A `DELETE` of the job is not a kill: the kernel sets the kill flag with a
grace period in it (Hop asks for 3 s), and applib turns that into the app's
stop doorbell, `app.stop()` (a `sync::Stop`, the counterpart of `close(stop)`
and `context.WithCancel`). The app then has that grace period minus 500 ms
(`STOP_MARGIN`: the network goodbye and one heartbeat) to finish: the
database closed, the session ended, the tunnel closed, a last line. During
that grace period it still has its network and the system API. When `main`
returns, the main shell does the network goodbye (every connection closed,
every FIN acknowledged) and the exit. Two lines in the main loop are enough:

```rust
async fn app_main(app: &'static App) {
    let db = Db::open(app).await;
    // The main loop until the kernel asks to stop.
    select(app.stopped(), serve(app, &db)).await;
    db.close().await;                     // the cleanup, within the grace period
}                                         // return: applib does shutdown(0)
```

On the console: `HOPOS_APP_STOP grace_ms=2500` at the request,
`HOPOS_APP_SHUTDOWN` at the goodbye, and `HOPOS_SLOT_STOPPED` when the
kernel releases the partition. An app that ignores the doorbell gets
`HOPOS_APP_STOP_FORCED` after the grace period and the same goodbye; one that
still does not park is revoked after the grace period
(`HOPOS_PART_QUARANTINE` if even that does not take). A `stacktask` is
aborted by dropping it: `select(app.stopped(), task)` gives the C side
`Cancelled` at its next `wait`.

So `after(d)` is a deadline, not a poll. If you write `after(d)` in a loop
with a `d` smaller than the deadline you are really guarding, you are
looking around: a `readable()`, a `Signal` or a `Channel` is missing from the
`select`.

### Long computations

A handshake, a PBKDF2, a document of a megabyte: cut it up and put
`yield_now().await` between the pieces. A network round trip is already an
`.await` and so already a cut; the computation step in between should stay
short. The measure: one poll of one task stays under a millisecond on the
slowest board the app is meant for. Anything longer is work for a
`stacktask` (see [stacktask.md](stacktask.md)) or for more `yield_now`.

### Pools, not a task per connection

The number of tasks is fixed at compile time (the handbook, §2). A worker
waits behind its door; an acceptor hands out (`sync::Doors`, as in welcome,
bench and vitals). A pool that is full leaves the connection waiting in the
accept queue, and the acceptor waits for the first worker that reports
itself free (`Doors::place`); it does not look around to see whether a
worker is free yet.

### Many small reads: bundle them

Every call to the kernel is a round trip through the OS core and keeps one
request in flight; an SSD only reaches its speed with many at once. Whoever
does many small reads (a database, an index) bundles them, like io_uring:
`Client::read_many(path, &mut ops)` puts up to `sys::MAX_READS` (16) reads
from one file in one call (`OP_READ_MANY`), the kernel puts them on the
device in one batch and answers once, each read in its own buffer with its
own outcome (`ReadOp::got`; a block error in one leaves the others alone). A
bundle waits for its slowest read: to keep the disk full, keep two bundles
in flight with a second `Client` (a connection of its own; an app has two)
and wait for them together. MEASURED 2026-10-04 on the Altra (SN770, vitals
`rand` with `depth` and `bundles`): one app 18,800 reads per second one by
one, 100,000 with bundles of 16, 159,000 with two bundles; four apps with
bundles of 16 together 249,000, eight 305,000 (one by one: 54,000 and
85,000). On the M4 (ANS): one app 8,400, 56,000 and 80,500; six apps with
bundles of 16 together 171,000.

## The load gauge

Without numbers, "the app sleeps" cannot be told apart from "the app naps".
Every app publishes two counters on its control page, without doing anything
for it: `CTRL_IDLE` (the ticks of the architectural counter that the sleeper
was away, all of the app's cores together) and `CTRL_WAKES` (how often the
sleeper was called). The kernel reads them and puts one line on the console
for each live slot every thirty seconds:

```
slot 3: idle=94% wakes=41/s cores=1 HOPOS_SLOT_LOAD
```

Read it like this:

- **idle** is the time the app did not need the core, over the last thirty
  seconds. On hardware a quiet app sits above 95%; an app that works drops,
  and it should: the question is whether it rises again when the work is
  done. The norm only applies on hardware: on QEMU-TCG `WFE` is a no-op and
  a core of its own measures ~0%, so a QEMU run is never red on this number.
- **wakes** is how often the app woke up. A quiet app with one heartbeat of
  50 ms (applib's `watch` task) and a few real timers sits in the tens per
  second. Hundreds per second without traffic is napping; thousands is
  looking around.
- So on QEMU-TCG only `wakes` says something, except on a shared core:
  there the yield does count as idle, also under QEMU
  (`tools/qemu-test-share.sh`).
- Hop reads the same two counters for a task's cpu percentage in its API,
  over its own poll interval: that number is "100 minus idle" over that
  window, the console line is idle over thirty seconds. They come from one
  source and only differ in rhythm; do not compare them to the second.
- The kernel itself appears as slot 0: `SLOT_STATUS` of slot 0 reports it as
  a live app on the OS core with one core, with its executor's sleep time
  (the same as `busy_ms` of `HOPOS_TICK`) as idle, the heap in use as memory
  and the tick as heartbeat. That way Hop also shows the kernel per node.
  Hop's turns on the shared OS core count there as idle of the kernel; they
  are in slot 1.

Inside the app itself the same is available in finer detail:
`EXEC.get().stats` (`rounds`, `polls`, `sleeps`, `slept_ns`). An app with a
diagnostic line of its own puts those numbers in it.

## The Go equivalent, for those coming from there

| Go on tamago | Rust on applib |
| --- | --- |
| `conn.Read(buf)` blocks the goroutine | `stream.read(&mut buf).await`, or first `stream.readable().await` |
| `select { case <-ch: case <-time.After(d): }` | `select(rx.recv(), exec.after(d))` |
| `time.Sleep(d)` | `exec.after(d).await`, and only if `d` is a real deadline |
| a goroutine per connection | a worker from a fixed pool |
| `runtime.Gosched()` | `yield_now().await` |
| the runtime parks when everything blocks | the executor idles when no task has a bit; applib yields or does a `WFE` |
| `ctx.Done()` / `<-stop` | `app.stopped()` |

Go's runtime did the releasing for you: as long as you blocked, the app
slept. Here it is the same, with one difference: an `after(1 ms)` is also
possible in Go, but nobody writes it there because `Read` blocks. In Rust
`poll::once(read)` plus `after(1 ms)` is the obvious choice when you
translate a non-blocking loop. That is the trap, and `readable()` is the way
out.

## Lessons from the Stulp port (2026-10-03 and 2026-10-04)

Stulp (the home automation, ten plugins in one bundle slot plus the
controller in a second) went in one day from "everything is slow, 502s,
Matter falls over" to stable on the LicheeRV. Sleeping on events was the
first step. What still went wrong after that, and none of it was the
scheduler:

**A late heartbeat is not dead.** A plugin that closed the connection after
5 s without an answer paid ~70 inits and dozens of Matter handshakes on
reconnect; those costs made the next heartbeat late again. A missed
heartbeat is now a log line; only after 5 minutes of complete silence does
the connection close. The more expensive reconnecting is, the wider the
limit.

**Give the executor back, even when everything is ready.** A protocol loop
in which every `.await` is immediately `Ready` (a full inbox, a channel with
work) never yields by itself. Stulp's SDK does a mandatory `yield_now()`
after 32 turns (the cooperative budget), regardless of what is still
waiting.

**Measure per task, not per slot.** `HOPOS_SLOT_LOAD` said "slot 3 is busy";
only a measurement per plugin task (`busy_ms` per task and a line for every
poll above 200 ms, with the task's name) pointed at the Matter plugin with
polls of ~290 ms, hundreds of times. Also measure what happens before the
work: the time a connection waited in the queue for a worker was recorded
nowhere, so a full pool was invisible.

**Do not copy a whole state per event.** JSON of a plugin state to text and
back cost ~300 ms on the C906, and the pool did that twice per lamp command,
for every worker that lagged behind. Give workers the change (usually one
device), not the state. Watch the building blocks too: `json::set` rebuilt
the object and copied every other field with everything below it, so one
device update copied the whole state. Stulp now changes in place via
`Object::get_mut`, `insert` and `remove`, for now as an overlay on Hop's
`types`; those belong in Hop itself.

**The allocator counts.** A first-fit list with thousands of small free
blocks made every allocation a walk: 505 ns per free plus allocation with
4547 free blocks, and on the node bootstraps of 250 ms. Exact classes up to
1 KiB with a bitmap of non-empty classes made it 53 ns
([heap.md](heap.md), the commits of 2026-10-03).

**Handshakes are expensive; share them.** On the C906 a Matter CASE costs 0.7
to 1.7 s (P-256) and a TLS handshake ~100 ms (after the faster AES and GHASH
in leantls: seal from 10 to 232 MB/s). Three consequences:
- One session per device. With a session per worker the first command to a
  lamp on each worker first paid a full handshake, and the sessions pushed
  each other out on the device; the Go version shared one, and therefore
  felt instant. See "One owner per peer" below.
- Reuse connections. An HTTPS client that handshakes again on every poll
  (TaHoma: 134 times in a quarter of an hour) burns compute time.
- An unreachable device gets a backoff (Stulp: 1 minute, doubling up to 30
  minutes), not an attempt per minute with a timeout of 40 s.

**One owner per peer.** Work in parallel per peer (a device, a server), not
per worker. Give each peer one fixed task (for example its id modulo the
number of workers) that owns its connection or session and does all traffic
to it: subscription, reconnecting and requests. Requests to different peers
still run at the same time. Stulp first did a session per worker and so paid
a handshake on every first Matter command; with one owner per node no
command needed one any more.

**Pass on before you wait for an acknowledgement.** A reliable protocol
(MRP, an ACK layer of your own) only acknowledges your reply after a round
trip. Do not wait for it before processing the received data: send the
reply, pass the data on, and let the acknowledgement come in in the
background. Stulp waited for that ACK after every Matter report (550 to 600
ms on Thread), and every notification, including a motion event, lagged by
that much.

**More parallelism is not free.** Workers on one executor share the
waiting, not the compute time: what each worker costs per event, you pay as
many times as there are workers. In Stulp eight workers per report started a
task that began with a copy of the state, and the slot dropped from above
90% to 11 to 40% idle. Measure `HOPOS_SLOT_LOAD` again after every step
toward more concurrency; do short work directly and only give long,
cancellable work a task of its own.

**Measure a chain per link.** A delay the user feels is rarely in one place.
Give each link a timing line (waiting in a queue, processing, connecting,
the round trip, passing on), then every suspicion gets a number. At Stulp
("motion to lamp takes 5 s") the queue was ruled out that way with 0 ms, and
the handshake (0.7 to 1.7 s) and waiting for an ACK (~600 ms) turned out to
be the big items.

**Do not let lifecycle abort running work.** Matter's maintenance was
cancelled by every `device.init`, in the middle of a handshake (137 of 173
handshakes after a start). Wait until the start is quiet (Stulp: 3 s without
init), and do not make an init a barrier for commands.

**Every failure path a log line.** The camera image was a silent 502:
`leanhttp::get` requires a `Content-Length`, and a live stream or a source
that closes after the response does not have one (use `leanhttp::fetch` and
check the status yourself). Without a line the console only showed a
successful callback and then nothing. For outgoing connections,
`applib::tcp::Dialer::last_error` says why a dial failed (`DialError`:
resolve, or connect with address and port); put it in the line.

**A full pool makes callers wait, bounded.** Dropping a connection when all
workers are busy is a 502 for the browser. A queue in front is better, but
not longer than the client itself waits: the tunnel gives up after 95 s
without a response header, so Stulp closes what has been in the queue longer
than 90 s.

**Timers are scarce.** An executor has 32 (`applib::rt`, `TIMERS`). An
acceptor or worker that always has a timer open eats into that; only use one
while there is something to guard.

**A close race is not an error.** A write or close for a stream that is
already gone is normal after a reconnect. Stulp made that a fatal error, and
the UniFi plugin fell over every time.

**Pick the core with a measurement.** On the LicheeRV the big core is the
kernel's (`system`); the plugin bundle had half to spare there, moved to the
small core it sat at 100%. Look at `HOPOS_SLOT_LOAD` before and after a
move, not at what a hart's name suggests.

## Review checklist

- No `after(d)` in a loop with a `d` shorter than the deadline the loop
  guards. Every `after` names in its comment which deadline it is.
- Every loop that waits on I/O waits via `readable()`, `accept()`, `recv()`
  or `Signal::wait()`, not via a non-blocking attempt plus a nap.
- A synchronous step that costs more than a millisecond on the slowest board
  has a `yield_now` or a `stacktask`.
- The app's `HOPOS_SLOT_LOAD` line on a quiet node: idle above 95%, wakes in
  the tens. Put that line in the app's test log.
- On a shared core: the neighbour's 404 stays under 50 ms while your app
  works.
- A missed heartbeat closes nothing; a reconnect is more expensive than
  waiting.
- No full copy of a large document per event; workers get the change.
- Every task has a measurement of its own (busy time, long polls with name),
  and every queue measures its waiting time.
- Every error status the app returns has a console line with the reason.
- The main loop also waits on `app.stopped()`, and whatever is open (a
  database, a session, a tunnel) is closed before `main` returns.
- Sessions and connections to the same device are shared and reused; an
  unreachable device gets a backoff.
- Every device (peer) has one owner task; parallel per device, not per
  worker.
- Data is passed on as soon as it is in; a protocol ACK is not awaited
  synchronously.
- After more concurrency: `HOPOS_SLOT_LOAD` measured again.
- A chain with noticeable delay has a timing line per link.

See also: the Rust handbook (`rustdoc/README.md`, §2 Tasks and §4 The
executor), [stacktask.md](stacktask.md) for synchronous code on a stack of
its own, [go-apps.md](go-apps.md) for apps that stay in Go, and
`tools/qemu-test-share.sh` for the rotation on a shared core.
