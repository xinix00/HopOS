//go:build !tamago

package slots

// Host-kant: er is geen RAM-declaratie om uit de pool te knippen — de tests
// zien exact de pool die het (test-)plan opgeeft.
func ownRegion() (start, end uint64) { return 0, 0 }
