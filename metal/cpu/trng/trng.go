// Package trng levert hardware-entropie op AArch64 via de twee
// standaardbronnen, in aflopende voorkeur:
//
//  1. FEAT_RNG — de RNDR-systeemregisterinstructie (Armv8.5+; o.a. de
//     Orion O6N). Puur een CPU-instructie, dus overal veilig te proberen:
//     ontbreekt de feature, dan zegt rndrSupported nee en raken we RNDR nooit.
//  2. SMCCC TRNG (Arm DEN 0098) — een firmware-SMC. TF-A op de Altra levert
//     dit; QEMU's kale PSCI niet. Een niet-herkende SMC op een machine zónder
//     EL3-monitor is architecturaal UNDEF (→ crash), dus we proberen dit
//     alleen als ID_AA64PFR0_EL1.EL3 != 0: dán zit er een monitor onder ons
//     die onbekende functie-ID's netjes met NOT_SUPPORTED (-1) beantwoordt.
//
// Ontbreken beide, dan geeft Fill ok=false en seedt de aanroeper zelf
// (jitter-DRBG in het board). Board-onafhankelijk: één bron van waarheid voor
// de entropie-hardware, de boards kiezen alleen de terugvaller.
//
// Fill (met SMCCC-terugval) is voor de HOP-kern; gekooide apps gebruiken
// FillCPU: een app praat nooit met de firmware — HCR_EL2.TSC trapt elke SMC
// uit de kooi als isolatie-overtreding, dus het SMCCC-pad is daar per
// definitie verboden terrein.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package trng

import (
	"encoding/binary"

	"hop-os/metal/cpu/psci"
)

// SMCCC TRNG (Arm DEN 0098), 64-bit conventie.
const (
	trngVersion uint64 = 0x8400_0050
	trngRND64   uint64 = 0xC400_0053

	trngNoEntropy = -3  // SMCCC-foutcode: entropie tijdelijk op → opnieuw
	trngRNDBits   = 192 // maximum per TRNG_RND64-call (24 bytes: R1:R2:R3)
)

// rndr64 leest één 64-bit waarde uit RNDR (FEAT_RNG). ok=false als de kern
// geen entropie kon leveren (PSTATE.Z gezet na de MRS). Zie trng_arm64.s.
func rndr64() (v uint64, ok bool)

// rndrSupported leest ID_AA64ISAR0_EL1.RNDR ([63:60]) — !=0 ⇒ FEAT_RNG.
func rndrSupported() bool

// el3Present leest ID_AA64PFR0_EL1.EL3 ([15:12]) — !=0 ⇒ er zit een
// EL3-monitor onder ons (SMC's naar onbekende functies zijn dan veilig).
func el3Present() bool

// Fill vult dst volledig met hardware-entropie en geeft de gebruikte bron
// terug ("rndr" of "smccc-trng"). ok=false ⇒ geen hardwarebron beschikbaar;
// de aanroeper seedt dan zelf. Alleen voor de HOP-kern: het SMCCC-pad is een
// firmware-call — gekooide apps gebruiken FillCPU.
func Fill(dst []byte) (source string, ok bool) {
	if src, ok := FillCPU(dst); ok {
		return src, true
	}
	if len(dst) != 0 && el3Present() && fillSMCCC(dst) {
		return "smccc-trng", true
	}
	return "", false
}

// FillCPU vult dst uitsluitend uit de CPU-instructiebron (RNDR, FEAT_RNG) —
// géén firmware-SMC. De bron voor gekooide apps: HCR_EL2.TSC trapt elke SMC
// uit de kooi (isolatie-invariant "een app praat nooit met EL3"), dus zelfs
// een póging via SMCCC zou de app vellen. ok=false ⇒ geen FEAT_RNG (o.a. de
// Altra's Neoverse N1, Armv8.2); de aanroeper seedt dan zelf (jitter).
func FillCPU(dst []byte) (source string, ok bool) {
	if len(dst) == 0 {
		return "", false
	}
	if rndrSupported() && fillRNDR(dst) {
		return "rndr", true
	}
	return "", false
}

// fillRNDR vult dst 8 bytes per keer uit RNDR, met een korte retry als de
// kern net geen woord klaar had.
func fillRNDR(dst []byte) bool {
	var word [8]byte
	for len(dst) > 0 {
		var v uint64
		ok := false
		for try := 0; try < 16 && !ok; try++ {
			v, ok = rndr64()
		}
		if !ok {
			return false
		}
		binary.BigEndian.PutUint64(word[:], v)
		dst = dst[copy(dst, word[:]):]
	}
	return true
}

// fillSMCCC vult dst via TRNG_RND64 (192 bits per call). Vereist een
// EL3-monitor (zie Fill) én firmware die DEN 0098 implementeert (TRNG_VERSION
// >= 0). NO_ENTROPY wordt begrensd herprobeerd; elke andere fout stopt.
func fillSMCCC(dst []byte) bool {
	if int64(psci.SMC(trngVersion, 0, 0, 0)) < 0 {
		return false // firmware kent de TRNG-interface niet
	}
	var block [24]byte
	for len(dst) > 0 {
		var r0, r1, r2, r3 uint64
		got := false
		for try := 0; try < 128; try++ {
			r0, r1, r2, r3 = psci.SMC4(trngRND64, trngRNDBits, 0, 0)
			if int64(r0) >= 0 {
				got = true
				break
			}
			if int64(r0) != trngNoEntropy {
				return false // NOT_SUPPORTED / INVALID_PARAMETER
			}
		}
		if !got {
			return false // entropie bleef op
		}
		binary.BigEndian.PutUint64(block[0:], r1)  // bits [191:128]
		binary.BigEndian.PutUint64(block[8:], r2)  // bits [127:64]
		binary.BigEndian.PutUint64(block[16:], r3) // bits [63:0]
		dst = dst[copy(dst, block[:]):]
	}
	return true
}
