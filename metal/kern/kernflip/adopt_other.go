//go:build !tamago

package kernflip

// Host-kant: de flip draait alleen op het target. Het blob-formaat en de
// bundel-parser zijn wél host-testbaar (dat is precies waar de tests op zitten),
// dus deze stubs houden het pakket compileerbaar zonder iets te beloven.
func setAdopting(v bool) {}
func ownRamEnd() uint64  { return 0 }
