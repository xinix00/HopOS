#!/usr/bin/env python3
# console.py — de console van de mini lezen zonder m1n1.
#
#   sudo macvdmtool reboot debugusb   (of gewoon opnieuw opstarten)
#   image/apple/console.py /dev/cu.kis-100000-ch-0 60
#
# Ná de installatie is dit het enige oor: load-probe.py leest de console door
# m1n1's proxy heen, en die bestaat dan niet meer. HopOS schrijft altijd naar
# de dockchannel én uart0 (board/apple/console.go), dus dit leest wat er is.
# Eén lezer per poort (twee lezers op één tty was les 1 van de meetbank), en
# de baudrate op de open fd, niet via stty op het pad (les 2).
import sys, time, serial

dev = sys.argv[1]
secs = float(sys.argv[2]) if len(sys.argv) > 2 else 60
baud = int(sys.argv[3]) if len(sys.argv) > 3 else 1500000

s = serial.Serial(dev, baud, timeout=1)
t0 = time.time()
try:
    while time.time() - t0 < secs:
        d = s.read(4096)
        if d:
            sys.stdout.write(d.decode("utf-8", "replace"))
            sys.stdout.flush()
except KeyboardInterrupt:
    pass
