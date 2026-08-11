# Configure a node

One set of keys, every board. Only the file differs:

| board | file | format |
|---|---|---|
| UEFI (USB stick) | `hopos.cfg` in the stick's root | one `key=value` per line, `#` comments |
| Raspberry Pi | `hopos.cfg` on the SD bootfs | one `key=value` per line, `#` comments |
| Raspberry Pi (alternative) | `cmdline.txt` on the SD bootfs | same keys as whitespace-separated tokens on the single cmdline |
| Radxa Zero 3E | `hopos.cfg` on the boot partition (U-Boot hands it to HopOS via the extlinux `initrd` line) | one `key=value` per line, `#` comments |
| LicheeRV Nano | baked into the kernel — that board has no SD driver, so there is no file on the card to edit. Patchable in place all the same (see below), or rebuilt with `CFG=… image/licheerv-agent.sh`; the released image bakes the headless default ([`hopos-headless.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-headless.cfg)) | one `key=value` per line, `#` comments |

Every board that reads a **file** reads it the same way: one key per line, a
value may contain spaces, and a line starting with `#` is a comment — with or
without a space after the `#`. The Pi's `cmdline.txt` is the one exception, and
only because it is a kernel command line: there the whole config is one line of
whitespace-separated tokens, so a value cannot contain a space.

Editing the file **is** node management — no shell, no rebuild, no agent.

## Editing it without a mount — the config window

Every card image carries `hopos.cfg` as a fixed-size **config window**: a
magic first line (`#HOPCFG1 window=… len=…`), the config itself, and
`#`-comment padding (1 MiB on the cards, 64 KiB inside the LicheeRV kernel).
To every parser it is just a config file — but it makes the bytes patchable
**in place**, so the config can be edited anywhere, before or after writing
the card, without mounting anything and regardless of whether the OS can
mount the partition at all.

Two tools use that window. The [imager](https://github.com/xinix00/hop-imager)'s
**CONFIGURE** is the no-terminal road: pick the card and the config appears —
no load button, coloured while you type, with secrets highlighted so you can
see where the keys are. Save, put the card back in the node. From the command
line it is `image/hopcfg`:

    go run image/hopcfg/main.go show     hopos-radxa-zero3.img
    go run image/hopcfg/main.go replace  hopos-radxa-zero3.img my-node.cfg
    sudo go run image/hopcfg/main.go replace /dev/rdisk4 my-node.cfg

Both cover every board — including the LicheeRV, where the config is baked
into the kernel: the window sits inside the Sophgo FIP, and writing it updates
the checksums that guard it (`MONITOR_CKSUM`, `PARAM2_CKSUM`) so the FSBL
keeps accepting the image. On boards whose partition does mount (Pi, UEFI
stick) editing the file directly keeps working exactly as before — the
window is only comments.

## The keys

| key | meaning | default |
|---|---|---|
| `hopos.node` | node name (shows up in `hop agents`) | generated |
| `hopos.mac` | MAC address, `aa:bb:cc:dd:ee:ff`. Only needed on boards without one in hardware — the LicheeRV has no MAC fuse, so it derives one from `hopos.node` and this key overrides that. Two such boards on one LAN both need a distinct node name, or this. | derived from `hopos.node` |
| `hopos.cluster` | cluster name — nodes with the same name form one cluster | — |
| `hopos.cores` | cores reserved for the node runtime itself (clamped to the board's physical cores) | `1` |
| `hopos.apikey` | HMAC key for the HTTP API — requests must be signed with it. Set this before the node leaves a LAN you trust; it and `hopos.insecure` are mutually exclusive, and with neither one set the node refuses to start the API (see below) | — |
| `hopos.insecure` | `1` = start the API with **no authentication** on purpose. **The shipped configs set this**, so a freshly written card boots into a usable node; the node warns on every boot until you swap it for a key | set in the shipped configs |
| `hopos.console` | TCP port that serves the node console — connect and you get the retained history, then live output (`nc <node-ip> 5555`). Every reader gets everything the node prints, jobs included, so this is a bench convenience and a leak anywhere else. **All shipped default configs set it to 5555** — a node without it is diagnosable only with a cable at the board, and a headless node has no other channel at all. Set it to `0` on a hostile network | off without a config; `5555` in the shipped configs |
| `hopos.wd` | `off` = don't arm the hardware watchdog (Raspberry Pi and Radxa Zero 3E). The watchdog reset-cycles a node until a boot succeeds; turn it off to keep a frozen node standing for a post-mortem | on |
| `hopos.s3.endpoint` | S3 endpoint for cluster state + leader election | state off |
| `hopos.s3.bucket` | bucket name | — |
| `hopos.s3.region` | region | — |
| `hopos.s3.key` / `hopos.s3.secret` | credentials | — |
| `hopos.s3.pathstyle` | `1` = path-style URLs (required for e.g. BunnyCDN) | virtual-host |
| `hopos.init[]` | a job to seed on a clean boot — one compact-JSON job per entry, repeatable | none |
| `hopos.apps[]` | an *available* (not auto-started) job for the launcher's catalog — same format as `hopos.init[]`, repeatable | none |

Addresses in a jobspec are literal — there is no template magic. The model is
one port number per host: a job's `ports` are always published on the node's
LAN address, DNS gets you to the right host, and when that host happens to be
your own node the switch takes the shortcut (hairpin — the frame never leaves
the machine). So a published service is reachable on the node address from
everywhere, inside and out. Every slot gets its own node's address handed in
as `HOPOS_HOST`; the node's own services (agent `:8080`, leader `:9080`) live
on the fixed internal address **`10.100.0.1`**, the same on every node. For
name-based discovery across nodes — `display.hop.local` instead of an IP —
run [hopdns](https://github.com/xinix00/hopdns): every app gets the node's
resolver (from the DHCP lease) handed in as `HOP_DNS`, and a jobspec can
override `HOP_DNS` per job to point straight at a hopdns instance.

## The API and its key

The agent (`:8080`) and leader (`:9080`) APIs accept job dispatch — that is
remote code execution on a trusted node. They listen on the LAN, so without
`hopos.apikey` **any host on that network can start jobs**.

**The shipped configs boot with the API open** (`hopos.insecure=1`), and the
node logs `HOPOS_API_INSECURE` every single boot. That is on purpose: the API
is how you talk to a node, so a key you have not set yet is a node you cannot
use — on a headless board, a node you cannot reach at all. A first boot that
works and complains beats one that refuses and explains.

Set a key before the node leaves a network you trust:

```
openssl rand -hex 24     # generate a key; the same key for every node in a cluster
```

Put it in `hopos.apikey` and remove the `hopos.insecure` line. The two are
mutually exclusive, and with **neither** set the node refuses to start its API:
it stays alive (switch, clock, storage and dvfs all run) and prints
`HOPOS_API_NO_AUTH`. That case is someone who deleted the insecure line meaning
to close the door — so the door closes, rather than staying open on a config
that no longer says it should be.

### Why HMAC and not TLS

This is a deliberate choice, not a shortcut. The HMAC signs method + path +
body, so the key never travels over the wire and a tampered request is
rejected — reading the traffic gets an attacker neither the key nor the
ability to forge a *new* call. What remains is **replay**: someone who can
already see your management traffic can repeat a request they captured.

We accept that. Anyone sitting on the management VLAN is inside the perimeter
already, and a second lock on the same door buys little. Full mTLS would mean
issuing, distributing and rotating client certificates across the whole fleet
— real operational weight, for a threat that starts with "the attacker is
already on your management network". Protection has to stop somewhere.

So: **the management network is the trust boundary.** Keep the API on a
network you trust, the same way you'd treat the boot stick.

## Example

```
hopos.node=altra-1 hopos.cluster=prod hopos.cores=2
hopos.apikey=<random-hex>
hopos.s3.endpoint=https://s3.example.com hopos.s3.bucket=hop-prod
hopos.s3.region=eu hopos.s3.key=AK... hopos.s3.secret=... hopos.s3.pathstyle=1
```

With S3 configured the node commits its cluster state there and **reloads
its own jobs after any reboot or power cut** — see
[Stateless](technical/stateless.md).

## Init jobs — a baseline on the stick

`hopos.init[]` seeds jobs on a **clean boot** so a node comes out of the box
already running something. Each entry is one job as **compact JSON** (same
schema as `POST /v1/jobs` / `hop apply`, so it's copy-pastable) — repeat the
key for more jobs:

```
hopos.init[]={"name":"dashboard","driver":"hop","artifacts":[{"url":"http://10.0.0.5/dash.elf"}],"memory_limit":100663296,"ports":{"http":80}}
hopos.init[]={"name":"worker","driver":"hop","artifacts":[{"url":"http://10.0.0.5/worker.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"svc"},"cpu_shares":2048}
```

- **One entry per line.** In a `hopos.cfg` the value is the rest of the line, so
  spaces inside the JSON are fine (keep it on one line — no pretty-printing). On
  the Pi's `cmdline.txt` the whole config is one whitespace-tokenised line, so
  there the JSON must be compact.
- **Standalone, without S3:** there is no committed state, so *every* boot is
  clean — the node always comes up with exactly these jobs. This is the way to
  ship a self-contained node.
- **With S3:** they seed only on the *first* clean boot; after that the
  committed state is the truth. A seeded job you later delete stays deleted
  (init jobs are a baseline, not a continuously enforced set).
- Order sets priority; an init job whose name already exists is skipped (a seed
  never overwrites operator state). A malformed entry is logged and skipped.

## App catalog — a desktop that starts itself

`hopos.apps[]` entries are **not** started; they are the catalog the SURF
launcher app shows. HopOS bundles them (as a JSON array) into the env var
`HOPOS_APPS` of any slot whose jobspec declares that key *empty* — opt-in,
because the per-slot env blob is small. Every slot also gets `HOPOS_HOST`
(this node's LAN IP) for free.

The desktop is two init jobs — the **display** (the surface plus its web-KVM)
and the **launcher** (the start-menu the taskbar's *hop* button toggles;
without it that button has nothing to open) — plus the catalog of everything
else in `hopos.apps[]`. All of it pulls straight from the SURF
[rolling-release](https://github.com/xinix00/hop-os-surf/releases/tag/rolling-release)
— the same URL on every node, no http server of your own.

**Two sharegroups is the whole trick.** A desktop is not compute — it is a pile
of mostly-idle UI that must stay *clickable*, and a node has few cores. So the
default splits it in two pools and lets each pool stack as deep as you like:

- `desktop` — the display + launcher on **one** core. Always-on chrome.
- `apps` — every window app on the **remaining** cores. Because they cooperate
  on a shared pool, you can open more apps than the node has cores; the eighth
  window is just another cage on the same pool, not a rejection.

```
hopos.init[]={"name":"display","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/display.elf"}],"memory_limit":134217728,"ports":{"surf":7878,"http":80},"tags":{"sharegroup":"desktop"},"cpu_shares":1024,"env":{"FB":"1"}}
hopos.init[]={"name":"launcher","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/launcher.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"desktop"},"cpu_shares":1024,"env":{"HOPOS_APPS":""}}
hopos.apps[]={"name":"clock","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/clock.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"apps"},"cpu_shares":2048}
hopos.apps[]={"name":"calc","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/calc.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"apps"},"cpu_shares":2048}
hopos.apps[]={"name":"browser","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/browser.elf"}],"memory_limit":134217728,"tags":{"sharegroup":"apps"},"cpu_shares":2048}
hopos.apps[]={"name":"dash","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/dash.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"apps"},"cpu_shares":2048}
hopos.apps[]={"name":"taskman","driver":"hop","artifacts":[{"url":"https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/taskman.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"apps"},"cpu_shares":2048}
```

Note what is *not* there: no addresses. Every SURF app defaults to the display
on its own node (`HOPOS_HOST:7878` — published port, hairpinned internally)
and to the agent on `10.100.0.1:8080`. Set `SURF_ADDR`/`HOP_ADDR` only to
point at *another* node — a hopdns name like `display.hop.local:7878`, or an
explicit address. The display's `"FB":"1"` asks for the framebuffer grant
(render to the real screen); without a framebuffer the desktop is still
served over HTTP. A USB keyboard and mouse plugged into the node arrive with
that same grant — screen and input are one seat, so there is nothing extra to
configure. This whole block ships as the default config in every GUI
release asset ([`image/hopos-gui.cfg`](../image/hopos-gui.cfg) in the tree,
[`hopos.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos.cfg)
in the newest release).

`cpu_shares` is the **pool size**, not a per-app quota: `2048` = the `apps` pool
owns 2 whole cores, however many apps you open on it (the first job of a group
sets the size; the rest join for free). Tune it to the board — a 4-core Pi
leaves 2 app cores after HOP and the desktop; on a big server give the pool
more, or drop the tag from a heavy app so it gets whole cores to itself.

Memory is *not* pooled: every app still gets its own partition, so RAM stays
the honest ceiling on how many windows fit.

Boot the node and the display comes up with the launcher's *hop* button live;
every catalog app is one click — click starts it, click again stops it. The
launcher POSTs the catalog entry to the agent verbatim (`hopos.apikey` set →
it must also get `"HOP_KEY":"<key>"` in its env).

**Headful vs headless.** This whole block is what the **GUI (headful)** release
images are built for — the URLs are live, so it works as-is. The `*-headless`
images link *zero* GUI code: drop the `display`/`launcher` init and the
`hopos.apps[]` catalog and just list the `hopos.init[]` jobs the node should
run — no desktop, only apps.

Their default config
([`hopos-headless.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-headless.cfg))
ships one such job, `welcome`: a page on port 80 that says the node is up and
what it runs on — cores, RAM partition, architecture, uptime. Open
`http://<node-ip>/` and that is your install check; delete the line and put
your own workload there. It takes no key and no address, because every app is
handed the node address itself. The RISC-V image is headless too and carries
the same job.

That one line covers both architectures. A job may list several artifacts, each
with a `match` on node attributes, and the agent downloads the first one that
fits — so the arm64 build goes to the Pi and UEFI boards and the riscv64 build
to a RISC-V node, from the same config:

```json
"artifacts":[
  {"url":"…/welcome-arm64-tamago.elf",  "match":{"node.arch":"arm64"}},
  {"url":"…/welcome-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}]
```

An artifact without `match` is the catch-all, so put it last if you add one.

## Trust model

The config (including the API key) is plaintext on the boot medium — the
same trust model as the Pi's own `cmdline.txt`: whoever holds the stick
holds the node. Keep the stick as safe as you'd keep a root password.
