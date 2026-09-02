//go:build !tamago

package stage2

// Host-kant (unit-tests): er is geen EL2 en geen TLB — de HVC is een no-op.
// Tests bewijzen de tabel-inhoud die Build schrijft, niet de intrekking zelf.
func hvcRevoke() {}

// De switch-code-kopie bestaat alleen op het target: op de host geven de
// el2-accessors 0 en is er niets te kopiëren.
func installSwitchCode() {}

func adoptingNow() bool { return false }

// SetAdopting/SwitchCodeHash/BlobBytes: de kern-flip draait niet op de host.
func SetAdopting(v bool)     {}
func SetFlipCapable(v bool)  {}
func SwitchCodeHash() uint64 { return 0 }
func BlobBytes() []byte      { return nil }

// (ChainloadEL2 is verhuisd naar cpu/el2.Chainload — zie chain.go daar.)
// De host heeft niets om in te
// springen. Bewust een no-op die terugkeert — een host-test die hier komt is
// zelf fout, en dat toont hij dan.

func Adopting() bool { return false }
