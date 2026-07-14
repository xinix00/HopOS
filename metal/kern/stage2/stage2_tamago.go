//go:build tamago

package stage2

// hvcRevoke doet HVC #0 vanuit EL1 → de revoke-vector op EL2 (TLBI ALLE1IS).
// De handler raakt geen GP-registers, dus niets te bewaren. Zie revoke_arm64.s.
func hvcRevoke()
