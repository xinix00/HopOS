# De hulpfuncties van de QEMU-toetsen in tools/, één keer. Een script leest
# ze in met `. "$(dirname "$0")/lib.sh"`, vóór zijn `cd "$DIR"`.

# Een host-poort: de gevraagde als hij vrij is, anders een vrije van het OS
# (met een regel op stderr), zodat een toets naast een andere QEMU of
# server draait. `port 0` is altijd een vrije van het OS.
port() {
	python3 - "$1" "${2:-}" <<'PY'
import socket, sys
want, name = int(sys.argv[1]), sys.argv[2]
s = socket.socket()
try:
    s.bind(("127.0.0.1", want))
    print(s.getsockname()[1])
except OSError:
    s.close()
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    got = s.getsockname()[1]
    print(f"   {name} {want} is taken, using {got}", file=sys.stderr)
    print(got)
s.close()
PY
}

# Staat het patroon (grep -E) op de console in $LOG?
has() { tr -d '\r' <"$LOG" | grep -q -E "$1"; }

# Hoe vaak staat het patroon op de console?
count() { tr -d '\r' <"$LOG" | grep -c -E "$1" || true; }

# Staan alle patronen van een |-lijst op de console?
all() {
	(
		IFS='|'
		for m in $1; do has "$m" || exit 1; done
	)
}
