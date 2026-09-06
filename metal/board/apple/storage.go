// storage.go — de DMA-regio van de opslagketen, uit het PA-plan gesneden.
//
// De ANS-coprocessor vraagt tijdens zijn opstart om geheugen (syslog, crashlog,
// io-rapportage) en de NVMe-driver heeft zijn queues nodig. Alles komt uit één
// aaneengesloten stuk buiten élke RAM-declaratie: device-gemapt, ongecachet, en
// met één venster in de SART in plaats van tien.
package apple

// storageNext begint na het hele gecachete NVMe-datablok. RTKit-buffers
// blijven daarbuiten ongecached; NVMe-initialisatie mag ze niet wissen.
const StorageDriverReserved = 0x400000

var storageNext = uintptr(StorageDMAPA) + StorageDriverReserved

// StorageBuf snijdt een 16KB-uitgelijnd stuk uit de opslag-regio. 0 = op. Geen
// vrijgave: de coprocessor houdt deze buffers vast zolang de node leeft, en een
// allocator die dat niet weerspiegelt zou alleen maar doen alsof.
func StorageBuf(size uint64) uintptr {
	if size == 0 || size > StorageDMASize {
		return 0
	}
	size = (size + 0x3fff) &^ 0x3fff
	if storageNext+uintptr(size) > uintptr(StorageDMAPA)+StorageDMASize {
		return 0
	}
	p := storageNext
	storageNext += uintptr(size)
	return p
}
