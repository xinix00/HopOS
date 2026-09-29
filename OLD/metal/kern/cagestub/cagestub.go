// Package cagestub draagt de kooi-stub ín de node-binary: het stukje code dat
// HOP vóór élke app op de partitie zet, op architecturen waar de kooi niet in een
// tabel maar in registers zit.
//
// Op ARM is de EL2-trampoline dat stukje: die staat in HOP's eigen image, activeert
// de stage-2-tabel en dropt naar de app. Op RISC-V bestaat zo'n trampoline niet —
// er is geen tweede privilege-laag onder ons — dus draait de stub op het app-hart
// zelf, ín de partitie: T-Head-CSR-init, BSS nullen, de PMP-kooi programmeren,
// hem terúglezen (CageVerify: mismatch = parken, nooit dispatchen) en dan pas de
// app inspringen. Zie image/licheerv/stub-slot/stub-slot.S voor het recept en de
// tabel-offsets; kern/slots/cage_riscv64.go patcht die tabel per start.
//
// Ingebakken en niet van een URL gehaald, om dezelfde reden als kern/apploaderblob:
// een node is self-contained. 400 bytes, dus de kosten zijn nul.
package cagestub

// Stub is de kooi-stub, of leeg als deze build hem niet meekreeg.
func Stub() []byte { return stub }
