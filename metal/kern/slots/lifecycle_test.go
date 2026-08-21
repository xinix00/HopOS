package slots

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// withFakeGrant hangt een provider op die alleen telt, en zet de echte terug.
func withFakeGrant(t *testing.T) (envCalls, releases *int) {
	t.Helper()
	old := grant
	t.Cleanup(func() { grant = old })
	var env, rel int
	grant = GrantHooks{
		Env: func(_ int, e map[string]string) map[string]string {
			env++
			e["FB_WINDOW"] = "1"
			return e
		},
		Release: func(int) { rel++ },
	}
	return &env, &rel
}

// De pure prep vóór het lifecycle-venster mag de grant-provider NIET aanraken.
// Tot 20-08 riep prepStart grantEnv vóór claimSlot: een tweede Start op een
// levend slot pakte zo de framebuffer van de dráaiende app af (HOP-console
// zwart) en gaf hem op zijn eigen faalpad ook nog vrij.
func TestPureStartPrepDoesNotTouchTheGrant(t *testing.T) {
	envCalls, releases := withFakeGrant(t)
	if _, err := prepareBaseEnv(map[string]string{"FB": "1"}, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if *envCalls != 0 || *releases != 0 {
		t.Fatalf("pure prep raakte de provider: Env=%d Release=%d", *envCalls, *releases)
	}
}

// prepareGrantedEnv is eigenaar van de grant tot hij slaagt: blaast de
// toevoeging van de provider de env-limiet op, dan gaat het glas terug —
// anders blijft de console weg voor een task die nooit draaide.
func TestGrantedEnvTooLargeGivesTheGrantBack(t *testing.T) {
	old := grant
	t.Cleanup(func() { grant = old })
	releases := 0
	grant = GrantHooks{
		Env: func(_ int, e map[string]string) map[string]string {
			e["FB_WINDOW"] = strings.Repeat("x", layout.CtrlEnvMax)
			return e
		},
		Release: func(int) { releases++ },
	}
	var attempt startGrant
	_, err := attempt.prepare(3, map[string]string{"FB": "1"})
	if err == nil {
		t.Fatal("de provider-toevoeging had de env-limiet moeten overschrijden")
	}
	// Dit is dezelfde buitenste rollback die startImage/startStream deferen.
	// prepareGrantedEnv gaf het token op zijn eigen validatiefout al terug;
	// rollback mag daar geen tweede Release van maken.
	attempt.rollback(3, err)
	if releases != 1 {
		t.Fatalf("grant-release na mislukte env-validatie = %d, wil exact 1", releases)
	}
}

// Na een geslaagde grant-prepare kan imageplaatsing of streaming nog falen.
// Het token geeft die grant dan één keer terug, ook als cleanup per ongeluk
// opnieuw wordt aangeroepen.
func TestStartGrantRollbackReleasesExactlyOnce(t *testing.T) {
	old := grant
	t.Cleanup(func() { grant = old })
	releases := 0
	grant = GrantHooks{
		Env:     func(_ int, env map[string]string) map[string]string { return env },
		Release: func(int) { releases++ },
	}
	var attempt startGrant
	if _, err := attempt.prepare(3, map[string]string{"FB": "1"}); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("plaatsing mislukt")
	attempt.rollback(3, fail)
	attempt.rollback(3, fail)
	if releases != 1 || attempt.acquired {
		t.Fatalf("grant-rollback: releases=%d acquired=%v", releases, attempt.acquired)
	}
}

// Een duplicate Start bereikt de muterende grant-provider pas ná zijn claim
// (claimStart); op de claim-fout is zijn ownership-token dus nog leeg. De
// algemene rollback van die mislukte poging mag de bestaande FB-houder niet
// vrijgeven.
func TestDuplicateStartClaimFailureKeepsLiveGrant(t *testing.T) {
	const slot = 3
	old := grant
	t.Cleanup(func() { grant = old })
	holder := 0
	envCalls := 0
	releases := 0
	grant = GrantHooks{
		Env: func(i int, env map[string]string) map[string]string {
			envCalls++
			if env["FB"] == "1" {
				holder = i
			}
			return env
		},
		Release: func(i int) {
			if holder == i {
				holder = 0
				releases++
			}
		},
	}

	// De eerste poging stelt de reeds levende FB-eigenaar voor.
	if _, err := prepareGrantedEnv(slot, map[string]string{"FB": "1"}); err != nil {
		t.Fatal(err)
	}
	claimBusy := errors.New("slot leeft nog")
	var duplicateGrant startGrant
	duplicateGrant.rollback(slot, claimBusy)

	if envCalls != 1 {
		t.Fatalf("duplicate bereikte grant.Env: totaal Env-calls = %d, wil 1", envCalls)
	}
	if holder != slot || releases != 0 {
		t.Fatalf("levende grant gewijzigd: holder=%d releases=%d", holder, releases)
	}
}

// ErrDispatch is een onbekende startuitkomst, ook wanneer hij gewrapt is.
// Alle ownership van de poging blijft dan bij dezelfde kooi: in het bijzonder
// zowel de hostCore-plaatsing als de succesvol verkregen grant.
func TestErrDispatchKeepsHostCoreAndGrant(t *testing.T) {
	const (
		slot         = 3
		previousCore = 5
		newCore      = 7
	)
	oldGrant := grant
	t.Cleanup(func() { grant = oldGrant })
	envCalls := 0
	releases := 0
	grant = GrantHooks{
		Env: func(_ int, env map[string]string) map[string]string {
			envCalls++
			return env
		},
		Release: func(int) { releases++ },
	}

	oldHostCore := hostCore
	if len(hostCore) <= slot {
		hostCore = make([]int, layout.SlotCap+1)
		copy(hostCore, oldHostCore)
	}
	oldCore := hostCore[slot]
	hostCore[slot] = previousCore
	t.Cleanup(func() {
		hostCore[slot] = oldCore
		hostCore = oldHostCore
	})

	var attempt startGrant
	if _, err := attempt.prepare(slot, map[string]string{"FB": "1"}); err != nil {
		t.Fatal(err)
	}
	err := fmt.Errorf("armSlot: %w", ErrDispatch)
	hostCore[slot] = newCore // StartShared/StartStreamOn publiceren vóór claim.
	attempt.rollback(slot, err)
	rollbackHostCore(slot, previousCore, err)
	if hostCore[slot] != newCore {
		t.Fatalf("hostCore na ErrDispatch = %d, wil %d", hostCore[slot], newCore)
	}
	if envCalls != 1 || releases != 0 || !attempt.acquired {
		t.Fatalf("grant na ErrDispatch: Env=%d releases=%d acquired=%v", envCalls, releases, attempt.acquired)
	}

	// Controlepad: een eenduidige claim-/plaatsingsfout herstelt juist de
	// voorafgaande levende mapping, niet de default core (0).
	hostCore[slot] = newCore
	rollbackHostCore(slot, previousCore, errors.New("claim mislukt"))
	if hostCore[slot] != previousCore {
		t.Fatalf("hostCore na eenduidige fout = %d, wil vorige %d", hostCore[slot], previousCore)
	}
}

// De SMP-toets leest hostCore, dus hij hoort ná de plaatsing te lopen. In
// prepStart las hij de VORIGE plaatsing en liet een pool-geplaatste SMP-kooi
// stilletjes door (gevonden 20-08).
func TestValidateSMPPlacementUsesPlacedCore(t *testing.T) {
	const slot = 1
	oldHostCore := hostCore
	if len(hostCore) <= slot {
		hostCore = make([]int, layout.SlotCap+1)
		copy(hostCore, oldHostCore)
	}
	old := hostCore[slot]
	hostCore[slot] = 7
	t.Cleanup(func() {
		hostCore[slot] = old
		hostCore = oldHostCore
	})
	if err := validateSMPPlacement(slot, 2); err == nil {
		t.Fatal("pool-geplaatste SMP-kooi moet geweigerd worden")
	}
	if err := validateSMPPlacement(slot, 1); err != nil {
		t.Fatalf("single-core placement: %v", err)
	}
}
