#!/bin/sh
# install.sh — put HopOS on a Mac with Apple silicon.
#
# Run this from Recovery (1TR), in Terminal:
#
#     sh /Volumes/HOPOS/install.sh              # look only, change nothing
#     sh /Volumes/HOPOS/install.sh go           # do it
#
# What it does, and why each step exists:
#
#   1. Shrinks macOS. This Mac keeps macOS — it has to. On Apple silicon the
#      boot object we install is registered against an installed macOS, so
#      removing macOS removes our own way in. The freed space becomes a plain
#      gap in the partition table, which is where HopOS will store data once
#      its NVMe driver can write.
#   2. Installs HopOS as the boot object. From then on this Mac powers on
#      straight into HopOS: no bootloader, no macOS, no login window.
#
# Going back is one command, and Recovery always boots no matter what we
# install, because it lives on its own partition:
#
#     sh /Volumes/HOPOS/install.sh revert     # back to macOS
#     sh /Volumes/HOPOS/install.sh m1n1       # back to the bring-up loader
#
# Requirements: the security policy must be Permissive. The installer checks
# this and stops with the exact command if it is not -- installing under Full
# Security leaves a Mac that boots to Recovery and tells you nothing useful,
# which cost us an afternoon on 31-08.
set -e

HERE="$(cd "$(dirname "$0")" && pwd)"
VOLUME="${VOLUME:-/Volumes/Macintosh HD}"

# The image lives next to this script — that is the whole point of the zip you
# unpacked. Named ones first, then any single .img in the same folder, so a
# rename does not break the installer. IMAGE=... still wins, for the bring-up
# case where you want to install a probe instead of the node.
if [ -z "${IMAGE:-}" ]; then
	for candidate in "$HERE/hopos-m4.img" "$HERE/hopos-apple.img"; do
		[ -f "$candidate" ] && IMAGE="$candidate" && break
	done
fi
if [ -z "${IMAGE:-}" ]; then
	set -- "$HERE"/*.img
	[ -f "$1" ] && [ "$#" -eq 1 ] && IMAGE="$1"
fi
if [ -z "${IMAGE:-}" ]; then
	echo "No image found next to this script ($HERE)."
	echo "Unpack the whole zip and run install.sh from inside it, or pass one:"
	echo "    IMAGE=/path/to/hopos-m4.img sh $0 $1"
	exit 1
fi

# How much macOS keeps. Not a fixed number: it is what macOS is using now plus
# room to keep working. The margin covers a macOS update (it wants ~20 GB free
# while it runs) and the Preboot volume, which grows by a copy of the boot
# object every time this installer runs.
MARGIN_GB="${MARGIN_GB:-20}"
FLOOR_GB="${FLOOR_GB:-60}"

say() { printf '%s\n' "$*"; }
rule() { say ""; say "-- $* ------------------------------------------------------"; }

# ---------------------------------------------------------------- the machine
rule "This Mac"
if [ ! -d "$VOLUME" ]; then
	say "Cannot find $VOLUME."
	say "Pass the right one:  VOLUME=/Volumes/<name> sh $0 $1"
	exit 1
fi
say "macOS volume : $VOLUME"
say "HopOS image  : $IMAGE"

# The security policy. Only Permissive Security lets iBoot boot an object that
# Apple did not sign, and nothing downstream says so out loud: kmutil accepts
# the install, and the Mac then boots to Recovery with "the custom kernel could
# not be started". So ask first.
#
# NOTE: `bputil -f` is --full-security, NOT "clear the boot object". It is the
# way to hand the Mac back to Apple's own boot chain, and it switches custom
# kernels off entirely. Clearing just our object is the kmutil call in `revert`.
MODE=$(bputil -d 2>/dev/null | sed -n 's/^Security Mode: *\([A-Za-z]*\).*/\1/p' | head -1)
say "security     : ${MODE:-unknown}"
if [ "$MODE" != "Permissive" ]; then
	say ""
	say "This Mac is not in Permissive Security, so iBoot will refuse HopOS."
	say "Set it from THIS Recovery session (1TR only) and run the installer again:"
	say ""
	say "    bputil -n        # --permissive-security, asks for your password"
	say ""
	say "Or: Startup Security Utility -> Security Policy -> Reduced Security,"
	say "with both boxes ticked. Override this check with FORCE=1 if you know"
	say "better than the policy readout."
	[ -n "${FORCE:-}" ] || exit 1
fi
[ -f "$IMAGE" ] || { say "That image is not here. Is the USB drive still plugged in?"; exit 1; }

SIZE=$(stat -f%z "$IMAGE" 2>/dev/null || wc -c < "$IMAGE")
if [ $((SIZE % 16384)) -ne 0 ]; then
	say ""
	say "This image is $SIZE bytes, which is not a whole number of 16K pages."
	say "The firmware maps a raw boot object and refuses anything else, so it"
	say "would panic before HopOS ever starts. Rebuild it with image/apple-m4.sh."
	exit 1
fi
say "image size   : $SIZE bytes ($((SIZE / 16384)) pages) — good"

# ------------------------------------------------------------------- the disk
rule "Disk"
diskutil list disk0 || true

# What we shrink is the physical store of the container that holds the macOS
# volume named above — asked of that volume itself, not "the first container on
# the disk", because this Mac has three (firmware, macOS, Recovery) and only one
# of them is ours to touch. Everything else stays exactly as it is.
CONTAINER=$(diskutil info "$VOLUME" 2>/dev/null | awk '/APFS Container:/ {print $NF}')
STORE=$(diskutil info "$VOLUME" 2>/dev/null | awk '/APFS Physical Store:/ {print $NF}')
[ -n "$STORE" ] || { say "$VOLUME is not on an APFS container — nothing to resize."; exit 1; }

# What macOS actually occupies, straight from that container.
USED_B=$(diskutil apfs list "$CONTAINER" 2>/dev/null | awk '/Capacity In Use By Volumes:/ {print $6; exit}')
[ -n "$USED_B" ] || { say "Could not read how much $CONTAINER is using — stopping rather than guessing."; exit 1; }
USED_GB=$(( USED_B / 1000000000 ))
KEEP_GB=$(( USED_GB + MARGIN_GB ))
[ "$KEEP_GB" -lt "$FLOOR_GB" ] && KEEP_GB=$FLOOR_GB
[ -n "${KEEP:-}" ] && KEEP_GB=$KEEP

TOTAL_GB=$(diskutil info disk0 2>/dev/null | awk '/Disk Size/ {gsub(/[(]/, "", $5); print int($5 / 1000000000); exit}')
FREE_GB=$(( TOTAL_GB - KEEP_GB - 6 ))

# How big the container is right now. A second run on an already shrunk Mac must
# not try to grow it back: the space behind it is the space we came for.
CUR_GB=$(diskutil info "$CONTAINER" 2>/dev/null | awk '/Disk Size/ {gsub(/[(]/, "", $5); print int($5 / 1000000000); exit}')
RESIZE=yes
if [ -n "$CUR_GB" ] && [ "$CUR_GB" -le "$((KEEP_GB + 2))" ]; then
	RESIZE=no
	FREE_GB=$(( TOTAL_GB - CUR_GB - 6 ))
fi

say "container    : $CONTAINER on $STORE (${CUR_GB:-?} GB now)"
say "macOS in use : ${USED_GB} GB"
if [ "$RESIZE" = no ]; then
	say "keep for it  : ${CUR_GB} GB   (already small enough — no resize needed)"
else
	say "keep for it  : ${KEEP_GB} GB   (in use + ${MARGIN_GB} GB, never below ${FLOOR_GB})"
fi
say "left for HopOS: about ${FREE_GB} GB"
say ""
say "Override with:  KEEP=120 sh $0 go"

case "${1:-look}" in
look)
	rule "Nothing was changed"
	say "This was the dry run. When the numbers above look right:"
	say ""
	say "    sh $0 go"
	say ""
	exit 0
	;;
m1n1)
	rule "Back to m1n1"
	# The bring-up loader. Not a step backwards but a step sideways: with m1n1
	# as the boot object there is a proxy again, and an image can be loaded over
	# USB in a fraction of a second instead of costing a trip to Recovery. That
	# is the difference between debugging and guessing.
	M1N1="${M1N1:-$HERE/m1n1.bin}"
	[ -f "$M1N1" ] || { say "No m1n1.bin next to this script ($HERE)."; exit 1; }
	kmutil configure-boot -c "$M1N1" --raw --entry-point 2048 \
		--lowest-virtual-address 0 -v "$VOLUME"
	say ""
	say "Done. After a restart this Mac comes up as m1n1 with its proxy."
	exit 0
	;;
revert)
	rule "Back to macOS"
	# -c is optional, and leaving it out is what CLEARS the custom boot object.
	# That is the small hammer: the security policy stays as it is, so the next
	# install needs no 1TR trip to Startup Security Utility. `bputil -f` is the
	# big hammer (Full Security) and switches custom kernels off altogether.
	say "Clearing the custom boot object; the security policy stays Permissive."
	kmutil configure-boot -v "$VOLUME"
	say ""
	say "Done. This Mac boots macOS again after a restart, and Recovery is"
	say "always reachable regardless (it lives on its own partition)."
	say "To hand security back to Apple's defaults as well:  bputil -f"
	exit 0
	;;
go) ;;
*)
	say "Unknown argument '$1'. Use: look (default), go, m1n1, or revert."
	exit 1
	;;
esac

# ------------------------------------------------------------------ 1. shrink
if [ "$RESIZE" = no ]; then
	rule "1. Shrinking macOS — skipped"
	say "macOS already fits in ${CUR_GB} GB and there is room behind it."
	say "Nothing to do here; on to the boot object."
else
rule "1. Shrinking macOS to ${KEEP_GB} GB"
say "This keeps every file macOS has; it only gives back space it is not using."
printf "Type yes to continue: "
read answer
[ "$answer" = "yes" ] || { say "Stopped. Nothing was changed."; exit 1; }

if ! diskutil apfs resizeContainer "$STORE" "${KEEP_GB}g"; then
	say ""
	say "The resize was refused. The usual reason is Time Machine's local"
	say "snapshots: they pin blocks in place and cannot move. Clear them and"
	say "try again:"
	say ""
	say "    tmutil deletelocalsnapshots /"
	say ""
	say "If it still refuses, the message above names the smallest size this"
	say "container will accept — run again with KEEP=<that number + 5>."
	exit 1
fi
say ""
diskutil list disk0 || true
fi

# ----------------------------------------------------------------- 2. install
rule "2. Installing HopOS as the boot object"
say "After this, powering on this Mac starts HopOS instead of macOS."
say "Recovery is untouched and always reachable: hold the power button."
printf "Type yes to continue: "
read answer
[ "$answer" = "yes" ] || { say "Stopped. The disk was resized, nothing was installed."; exit 1; }

# Clear whatever is registered now, then install fresh. Two reasons. Repeated
# installs each leave a copy of the boot object on the Preboot volume (it stood
# at 9.1 GB after a day of experiments), and a stale registration is one more
# thing that can be inconsistent when a boot fails -- and a failed custom-kernel
# boot tells you nothing about which of the two it was.
say "Clearing any previously registered boot object first."
kmutil configure-boot -v "$VOLUME" || say "(nothing to clear)"

# REBUILD THE PREBOOT VOLUME. Not optional, not a repair step -- part of every
# install, and the single most expensive lesson of 31-08.
#
# The custom boot object does not live in the LocalPolicy alone; the volume
# group's Preboot volume carries what iBoot actually reads. Repeated installs
# leave copies there (it stood at 9.1 GB after one day), and once that volume is
# stale iBoot refuses the object with "starting up with the custom kernel
# failed" -- while `bputil -d` still shows a healthy Permissive policy with a
# registered hash, and the image on the USB drive verifies byte-perfect. So
# every signal says "fine" and the Mac still boots to Recovery.
#
# That cost an afternoon of blaming a kernel that was never even reached. One
# command fixes it, and it is cheap, so it runs every time.
say ""
say "Rebuilding the Preboot volume (this is what iBoot actually reads)."
diskutil apfs updatePreboot "$VOLUME" || {
	say ""
	say "updatePreboot failed. Do not install on top of this: a stale Preboot"
	say "is exactly what makes iBoot refuse a perfectly good boot object."
	exit 1
}

# COMPRESS=1 adds kmutil's --compress: the same install, but the payload goes
# into the LocalPolicy packed. Off by default, which is kmutil's own default
# too — the firmware then unpacks it, and during bring-up that is one step in
# the chain you would rather not have.
if [ -n "${COMPRESS:-}" ]; then
	kmutil configure-boot -c "$IMAGE" --raw --compress --entry-point 2048 \
		--lowest-virtual-address 0 -v "$VOLUME"
else
	kmutil configure-boot -c "$IMAGE" --raw --entry-point 2048 \
		--lowest-virtual-address 0 -v "$VOLUME"
fi

rule "Done"
say "Restart this Mac and it comes up as a HopOS node."
say ""
say "There is no screen output yet on this hardware — the display firmware"
say "does not come up — so the node tells you it is alive over the network:"
say "it asks for a DHCP lease and serves its welcome page on port 80."
say ""
say "To hand the Mac back to macOS:  sh $0 revert"
