package main

// De object-store-demo (STOREDEMO=roundtrip): bewijst de hele keten van
// app tot bucket — Pull-miss, Push, lokaal vervuilen, Pull herstelt, List
// relatief aan de eigen map, Drop. De exitcode draagt het resultaat naar
// HOP (0 = de keten klopt); de fake-S3 op de Mac verifieert van buitenaf
// dat élke key onder apps/<cluster>/<job>/ viel en de push-hash de payload
// dekte. Zelfde patroon als de FSDEMO-rollen.

import (
	"bytes"

	"hop-os/metal/app/applib"
)

func storeDemo(app *applib.App) {
	const f = "state.json"
	leven1 := []byte("leven 1: geboren om te pushen")

	// 1. Verse map: een pull van iets dat er nooit was is een nette fout.
	if _, err := app.Pull(f); err == nil {
		app.Logf("STOREDEMO: pull of a never-pushed object succeeded?!")
		exit(app, 1)
	}
	// 2. Schrijven en pushen — vanaf nu is het persistent.
	if err := app.WriteFile(f, leven1); err != nil {
		app.Logf("STOREDEMO: write: %v", err)
		exit(app, 2)
	}
	n, err := app.Push(f)
	if err != nil || n != uint64(len(leven1)) {
		app.Logf("STOREDEMO: push: n=%d err=%v", n, err)
		exit(app, 3)
	}
	// 3. Lokaal vervuilen (lánger dan het origineel, zodat een niet-
	//    vervangende pull door de staart zou vallen) en terughalen: de
	//    bucket wint.
	if err := app.WriteFile(f, []byte("vervuild — en met opzet veel langer dan het origineel")); err != nil {
		app.Logf("STOREDEMO: dirty local write: %v", err)
		exit(app, 4)
	}
	if _, err := app.Pull(f); err != nil {
		app.Logf("STOREDEMO: pull back: %v", err)
		exit(app, 5)
	}
	b, err := app.ReadFile(f)
	if err != nil || !bytes.Equal(b, leven1) {
		app.Logf("STOREDEMO: after pull %d bytes (%q), want %q", len(b), b, leven1)
		exit(app, 6)
	}
	// 4. List is relatief aan de eigen map: direct voer voor Pull.
	names, err := app.StoreList("")
	if err != nil || len(names) != 1 || names[0] != f {
		app.Logf("STOREDEMO: list = %v (err %v), want [%s]", names, err, f)
		exit(app, 7)
	}
	// 5. Drop, en de map is weer leeg.
	if err := app.StoreDrop(f); err != nil {
		app.Logf("STOREDEMO: drop: %v", err)
		exit(app, 8)
	}
	if _, err := app.Pull(f); err == nil {
		app.Logf("STOREDEMO: object still exists after drop")
		exit(app, 9)
	}
	app.Logf("STOREDEMO OK: push/pull/list/drop round-trip verified (%d bytes)", n)
	exit(app, 0)
}
