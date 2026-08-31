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
#     sh /Volumes/HOPOS/install.sh revert
#
# Requirements: the security policy must be Permissive (Startup Security
# Utility -> Security Policy -> Reduced Security, with both boxes ticked).
# The installer checks and tells you if it is not.
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
revert)
	rule "Back to macOS"
	M1N1="${M1N1:-$HERE/m1n1.bin}"
	if [ -f "$M1N1" ]; then
		say "Restoring the previous boot object ($M1N1)."
		kmutil configure-boot -c "$M1N1" --raw --entry-point 2048 \
			--lowest-virtual-address 0 -v "$VOLUME"
		say "Done. This Mac boots that image again after a restart."
	else
		say "To hand this Mac back to macOS, clear the custom boot object:"
		say ""
		say "    bputil -f"
		say ""
		say "Then restart. Recovery boots from its own partition, so this"
		say "always works, whatever we installed."
	fi
	exit 0
	;;
go) ;;
*)
	say "Unknown argument '$1'. Use: look (default), go, or revert."
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
