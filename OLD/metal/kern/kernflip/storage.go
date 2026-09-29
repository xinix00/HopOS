package kernflip

import "fmt"

// De opslag-haak, naar het model van de agent-state: de kern kent hopfs niet
// als eigenaar, de main geeft hier een functie af die de boom vastlegt en het
// bestandssysteem dichthoudt tot de sprong (hopfs.FS.Freeze). Zonder zou een
// flip de volumes kwijtraken: de nieuwe kern mount dezelfde schijf, en vindt
// alleen wat vastgelegd is.
var storageFreeze func() (thaw func(), err error)

// UseStorage registreert de vastleg-functie (cmd/hopos, na het mounten).
func UseStorage(freeze func() (thaw func(), err error)) { storageFreeze = freeze }

// freezeStorage weigert een flip die de volumes zou verliezen. thaw is nooit
// nil, zodat een latere fout in de flip altijd kan ontdooien.
func freezeStorage() (func(), error) {
	if storageFreeze == nil {
		return func() {}, nil
	}
	thaw, err := storageFreeze()
	if err != nil {
		return nil, fmt.Errorf("kernflip: saving the storage tree failed, flip refused (the volumes would be lost): %w", err)
	}
	return thaw, nil
}
