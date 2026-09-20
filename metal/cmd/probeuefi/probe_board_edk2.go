//go:build !o6n && !altra

package main

// De EDK2-proeftuin (QEMU virt): board.Board-registratie. De Altra heeft
// zijn eigen smaak (probe_board_altra.go); de O6N idem.
import _ "github.com/xinix00/HopOS/metal/v2/board/edk2/hop"
