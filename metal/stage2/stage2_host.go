//go:build !tamago

package stage2

// Host-kant (unit-tests): er is geen EL2 en geen TLB — de HVC is een no-op.
// Tests bewijzen de tabel-inhoud die Build schrijft, niet de intrekking zelf.
func hvcRevoke() {}
