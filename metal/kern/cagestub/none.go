//go:build !embedcagestub

package cagestub

// Geen ingebakken stub. Op ARM is dat de normale toestand (de EL2-trampoline
// zit in HOP's image); op een architectuur die de stub wél nodig heeft weigert
// kern/slots luid bij de eerste start, in plaats van een hart op nullen te
// starten.
var stub []byte
