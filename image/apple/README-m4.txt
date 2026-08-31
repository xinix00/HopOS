HopOS for the Mac mini (Apple silicon)
======================================

This turns a Mac mini into a HopOS node. macOS stays on the machine, smaller;
HopOS becomes what the Mac starts when you power it on.

What you need
-------------
* A Mac mini with Apple silicon, a keyboard, a mouse and a screen (Recovery is
  a graphical environment — this is the only step that needs one).
* This image, written to a USB drive: use hop-imager, or
  `gunzip -c hopos-m4-headless.img.gz | sudo dd of=/dev/rdiskN bs=4m`.
  Do not format the drive yourself — the image brings its own FAT filesystem,
  which is what Recovery can read.
* Ten minutes, and the willingness to shrink macOS.

Step 0 — allow it, once
-----------------------
From Recovery: Startup Security Utility -> Security Policy -> Reduced Security,
and tick both boxes (kernel extensions, and remote management). Apple calls the
result Permissive Security; without it the Mac will not start anything but
Apple's own kernel.

Step 1 — get into Recovery
--------------------------
Shut the Mac down. Then press and HOLD the power button — do not tap it — until
"Loading startup options" appears. Choose Options -> Continue, then open
Utilities -> Terminal from the menu bar.

Step 2 — run the installer
--------------------------
You are reading this from the USB drive you wrote this image to, so it is
already plugged in and it mounts as HOPOS. In the Terminal:

    sh /Volumes/HOPOS/install.sh          # shows the plan, changes nothing
    sh /Volumes/HOPOS/install.sh go       # does it

The first command is a dry run: it shows the disk, how much macOS is using, how
much it will keep and how much HopOS gets. The second one asks for confirmation
before it shrinks anything.

How much does macOS keep? Whatever it is using plus 20 GB, and never less than
60. Override it if you want a different split:

    KEEP=120 sh /Volumes/HOPOS/install.sh go

Step 3 — restart
----------------
That is it. The Mac now comes up as a HopOS node.

What to expect
--------------
No screen output. The display firmware on this hardware does not come up for
us, so a Mac mini node is headless — that is a property of the machine, not a
fault. The node tells you it is alive over the network: it takes a DHCP lease
and serves its welcome page on port 80. Its console is on TCP port 5555
(`nc <node> 5555`).

Going back to macOS
-------------------
    sh /Volumes/HOPOS/install.sh revert

Recovery boots from its own partition and is never affected by what we install,
so this way back always exists — hold the power button and you are there.

Updating HopOS
--------------
Same as installing: write a newer image to the drive, run install.sh go again. A full image
every time, on purpose. Nodes do not update themselves from the network: one
wrong image should never be able to take out more than the machine in front of
you.

What is on this drive
---------------------
install.sh      the installer (dry run by default)
hopos-m4.img    HopOS itself — the node
README.txt      this file
