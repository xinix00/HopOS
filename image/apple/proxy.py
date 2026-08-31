#!/usr/bin/env python3
# proxy.py — load a new image into a running HopOS probe over the dockchannel
# and boot it, without a trip to 1TR.
#
#   image/apple/proxy.py metal/out/probeapple.img          # load + boot
#   image/apple/proxy.py metal/out/hopos-apple.img --watch 60
#   image/apple/proxy.py --ping                            # is anyone there?
#
# The counterpart is board/apple/proxy.go. Installing a boot object needs 1TR,
# so the installed image is the anchor and this is the workbench: whatever we
# send here is gone after a power cycle, and the machine comes back up on the
# image that was installed.
#
# The wire format is deliberately tiny (see proxy.go): a 32-byte header, and for
# a load the bytes right behind it.
import argparse, os, re, struct, sys, time

try:
    import serial
except ImportError:
    sys.exit("pyserial missing — use ~/Git/m1n1/venv/bin/python3")

MAGIC = b"HOPPRX01"
SCRATCH = 0  # 0 = let the far side pick its own scratch window
DEFAULT_PORT = "/dev/cu.kis-100000-ch-0"


def header(cmd, addr=0, length=0):
    return MAGIC + cmd + b"\0" * 7 + struct.pack("<QQ", addr, length)


def checksum(data):
    # Same walk as proxy.go, so the two numbers are comparable by eye.
    s = 0
    for b in data:
        s = (s * 31 + b) & 0xFFFFFFFF
    return s


def open_port(dev, baud, tries=3):
    """Open the port, re-establishing it if the debug USB device dropped.

    The kis ports come and go on the host side — a long write can make the
    device disappear mid-transfer (measured 30-08, at 2.4 MB). Switching the
    target back into debug USB mode brings them back without a reboot, which
    matters: a reboot would throw away everything already loaded.
    """
    import subprocess
    for attempt in range(tries):
        if os.path.exists(dev):
            try:
                return serial.Serial(dev, baud, timeout=0.2)
            except serial.SerialException:
                pass
        subprocess.run(["sudo", "-n", "/usr/local/sbin/macvdmtool", "debugusb"],
                       capture_output=True)
        for _ in range(15):
            if os.path.exists(dev):
                break
            time.sleep(1)
    sys.exit(f"{dev} will not come back — try: sudo macvdmtool reboot debugusb")


def drain(port, seconds, echo=True):
    end = time.time() + seconds
    out = b""
    while time.time() < end:
        d = port.read(4096)
        if d:
            out += d
            if echo:
                sys.stdout.write(d.decode("utf-8", "replace"))
                sys.stdout.flush()
    return out


def main():
    ap = argparse.ArgumentParser(description="Send an image to a running HopOS probe.")
    ap.add_argument("image", nargs="?", help="the .img to load (mkkernel -apple output)")
    ap.add_argument("--port", default=DEFAULT_PORT)
    ap.add_argument("--baud", type=int, default=1500000)
    ap.add_argument("--addr", type=lambda v: int(v, 0), default=SCRATCH,
                    help="load address; 0 (default) lets the running image pick its scratch")
    ap.add_argument("--ping", action="store_true", help="only ask whether the proxy is listening")
    ap.add_argument("--no-boot", action="store_true", help="load the image but do not jump into it")
    ap.add_argument("--watch", type=float, default=20, help="seconds to keep reading after the jump")
    ap.add_argument("--piece", type=int, default=512,
                    help="bytes per write (default 256: what the far side drains per poll)")
    ap.add_argument("--gap", type=float, default=0.005,
                    help="seconds between writes (default 0.005: the far side's poll interval)")
    ap.add_argument("--chunk", type=int, default=0,
                    help="send the image as separate loads of N bytes each. Slower, but every "
                         "load is short enough that the far side never waits long for the next "
                         "byte — the way in when the running image cannot take a long transfer.")
    args = ap.parse_args()

    if not os.path.exists(args.port):
        sys.exit(f"{args.port} is not there — put the target in debug USB mode:\n"
                 f"  sudo macvdmtool debugusb        (switch without rebooting)\n"
                 f"  sudo macvdmtool reboot debugusb (restart into it)")

    port = open_port(args.port, args.baud)

    if args.ping or not args.image:
        port.write(header(b"P"))
        port.flush()
        got = drain(port, 3)
        if b"proxy: ready" not in got:
            sys.exit("\nno answer — is a probe with the proxy running? (a plain agent has none)")
        print("\nproxy is listening")
        return

    # Waar mag dit heen? Bij --addr 0 vragen we het aan de overkant in plaats
    # van het te weten: die kent zijn eigen scratch-venster, en een offset
    # optellen bij nul geeft een adres dat nergens op slaat — precies waar de
    # blok-modus op strandde (30-08: blok 1 kwam aan, blok 2 werd geweigerd).
    if args.addr == 0:
        port.write(header(b"P"))
        port.flush()
        reply = drain(port, 3, echo=False).decode("utf-8", "replace")
        m = re.search(r"proxy: ready — scratch (0x[0-9a-f]+)", reply)
        if not m:
            sys.exit("no answer to ping — is a probe with the proxy running?")
        args.addr = int(m.group(1), 16)
        print(f"far side offers scratch at {args.addr:#x}")

    img = open(args.image, "rb").read()
    if len(img) % 0x4000:
        sys.exit(f"{args.image} is {len(img)} bytes, which is not a whole number of 16K pages — "
                 "iBoot maps a raw boot object and rejects the rest; rebuild with mkkernel -apple")
    print(f"sending {args.image} — {len(img)} bytes to {args.addr:#x}, checksum {checksum(img):#x}")

    t0 = time.time()
    if args.chunk:
        # Blok voor blok, elk met een eigen LOAD op zijn eigen adres en een
        # eigen bevestiging. Duurder in rondjes, maar een wegvallende poort
        # kost dan één blok in plaats van de hele overdracht — en dat gebeurt
        # op deze kabel gewoon.
        step = args.chunk
        off = 0
        while off < len(img):
            piece = img[off:off + step]
            try:
                port.write(header(b"L", args.addr + off, len(piece)))
                for i in range(0, len(piece), args.piece):
                    port.write(piece[i:i + args.piece])
                    time.sleep(args.gap)
                port.flush()
                ack, t1 = b"", time.time()
                while time.time() - t1 < 15:
                    ack += drain(port, 0.3, echo=False)
                    if b"proxy: loaded" in ack or b"refusing" in ack:
                        break
            except serial.SerialException:
                ack = b""
            if b"proxy: loaded" not in ack:
                print(f"  block at +{off} lost the link, re-attaching and retrying")
                try:
                    port.close()
                except Exception:
                    pass
                port = open_port(args.port, args.baud)
                continue
            off += len(piece)
            print(f"  {off // 1024} KB of {len(img) // 1024}", end="\r", flush=True)
            # Even laten bezinken: de overkant moet zijn bevestiging kwijt
            # kunnen en terug zijn bij het zoeken naar de volgende kop voordat
            # wij er weer bytes in duwen.
            drain(port, 0.15, echo=False)
        print(f"  sent in {time.time() - t0:.1f}s" + " " * 20)
    else:
        port.write(header(b"L", args.addr, len(img)))
        port.flush()
        sent = 0
        while sent < len(img):
            chunk = img[sent:sent + args.piece]
            port.write(chunk)
            sent += len(chunk)
            # Pace to the far side, not to the line. The dockchannel FIFO is
            # small and the reader empties it on its own clock (5 ms, its own
            # goroutine); writing a 4 KB burst simply drops whatever does not
            # fit, and then the load never completes while everything looks
            # healthy. Measured 30-08: 64 bytes went through first try, 16 KB
            # in bursts did not.
            time.sleep(args.gap)
            if sent % (128 * 1024) < args.piece:
                print(f"  {sent // 1024} KB of {len(img) // 1024}", end="\r", flush=True)
        port.flush()
        print(f"  sent in {time.time() - t0:.1f}s, waiting for the far side" + " " * 12)
        # Wachten tot de bevestiging er is, niet tot een vaste tijd om is: de
        # overkant leest op zijn eigen klok, en een vast venster meet vooral
        # zichzelf (30-08: "8,3s" was 8 seconden wachten en 0,3 seconde sturen).
        got, t1 = b"", time.time()
        while time.time() - t1 < 20:
            got += drain(port, 0.4, echo=False)
            if b"proxy: loaded" in got or b"refusing" in got:
                break
        if b"proxy: loaded" not in got:
            sys.exit("\nthe far side never confirmed the load — check the checksum line above")
        for line in got.decode("utf-8", "replace").splitlines():
            if "proxy:" in line:
                print("  " + line.strip())
    print(f"  done in {time.time() - t0:.1f}s")

    if args.no_boot:
        print("\nloaded; not jumping (--no-boot)")
        return
    print("\njumping...")
    port.write(header(b"J", args.addr))
    port.flush()
    drain(port, args.watch)


if __name__ == "__main__":
    main()
