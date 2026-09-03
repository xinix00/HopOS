package main

// De object-store-demo (STOREDEMO=roundtrip): bewijst de hele keten van
// app tot bucket — Pull-miss, Push, lokaal vervuilen, Pull herstelt, List
// relatief aan de eigen map, Drop. De exitcode draagt het resultaat naar
// HOP (0 = de keten klopt); de fake-S3 op de Mac verifieert van buitenaf
// dat élke key onder apps/<cluster>/<job>/ viel en de push-hash de payload
// dekte. Zelfde patroon als de FSDEMO-rollen.

import (
	"bytes"

	"github.com/xinix00/HopOS/metal/v2/app/applib"
)

func storeDemo(app *applib.App) {
	const f = "state.json"
	leven1 := []byte("leven 1: geboren om te pushen")

	// 1. Verse map: een pull van iets dat er nooit was is een nette fout.
	if _, err := app.Pull(f); err == nil {
		exitf(app, 1, "STOREDEMO: pull of a never-pushed object succeeded?!")
	}
	// 2. Schrijven en pushen — vanaf nu is het persistent.
	if err := app.WriteFile(f, leven1); err != nil {
		exitf(app, 2, "STOREDEMO: write: %v", err)
	}
	n, err := app.Push(f)
	if err != nil || n != uint64(len(leven1)) {
		exitf(app, 3, "STOREDEMO: push: n=%d err=%v", n, err)
	}
	// 3. Lokaal vervuilen (lánger dan het origineel, zodat een niet-
	//    vervangende pull door de staart zou vallen) en terughalen: de
	//    bucket wint.
	if err := app.WriteFile(f, []byte("vervuild — en met opzet veel langer dan het origineel")); err != nil {
		exitf(app, 4, "STOREDEMO: dirty local write: %v", err)
	}
	if _, err := app.Pull(f); err != nil {
		exitf(app, 5, "STOREDEMO: pull back: %v", err)
	}
	b, err := app.ReadFile(f)
	if err != nil || !bytes.Equal(b, leven1) {
		exitf(app, 6, "STOREDEMO: after pull %d bytes (%q), want %q", len(b), b, leven1)
	}
	// 4. List is relatief aan de eigen map: direct voer voor Pull.
	names, err := app.StoreList("")
	if err != nil || len(names) != 1 || names[0] != f {
		exitf(app, 7, "STOREDEMO: list = %v (err %v), want [%s]", names, err, f)
	}
	// 5. Drop, en de map is weer leeg.
	if err := app.StoreDrop(f); err != nil {
		exitf(app, 8, "STOREDEMO: drop: %v", err)
	}
	if _, err := app.Pull(f); err == nil {
		exitf(app, 9, "STOREDEMO: object still exists after drop")
	}
	exitf(app, 0, "STOREDEMO OK: push/pull/list/drop round-trip verified (%d bytes)", n)
}
