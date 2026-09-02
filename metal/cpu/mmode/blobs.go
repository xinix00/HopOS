package mmode

// De M-mode-blob: het stuk van dit pakket dat een APP-HART uitvoert, en dat
// daarom niet in het kern-image mag blijven wonen (docs/kern-flip.md). Het is
// er één en niet drie zoals op ARM, want mentry, mrotate, park en parkenter
// zijn hier één aaneengesloten stuk assembly.
//
// Deze lijst is de ÉNE beschrijving ervan: kern/slots kopieert hem en rekent er
// de som over, kern/kernflip rekent dezelfde som over de blob ín een nieuwe
// kern-bundel. Zelfde rol als el2.BlobSymbols aan de ARM-kant.
var BlobSymbols = [][2]string{
	{"mentry", "mmodeEnd"},
}

// MaxBlobSize is de bovengrens. Een groter bereik tussen mentry en zijn
// eindmarker betekent dat de linker ze uit elkaar heeft getrokken, en dat hoort
// hard te vallen in plaats van stilzwijgend megabytes te kopiëren.
const MaxBlobSize = 0x4000
