//go:build riscv64 && linkcpuinit

package dev

// De app-kant van een slot (linkcpuinit = dit ís een app-image): Push en Pull
// zijn no-ops, precies zoals op ARM.
//
// Waarom dat mag: alles wat een app met HOP deelt — control page, mailbox-ringen,
// frame-ringen — mapt de kooi als DEVICE (kern/slots slotMap). Ongecachet en
// niet-herordend, dus er valt niets te onderhouden: wat de app schrijft staat er,
// en wat HOP schrijft ziet hij.
//
// En waarom het móet: de cache-ops van dit silicium werken op FYSIEKE adressen
// (dev_riscv64.s), terwijl een app linkadressen heeft — de kooi verplaatst hem.
// Zou hij hier toch een op afvuren, dan raakte die de cacheline van een ánder
// slot. Met één bewoner is dat een bug, met meerdere is het het weggooien van
// iemand anders zijn data. Dus doet de app hier niets, en is dat afgedwongen door
// het bouwpad in plaats van door discipline.
func Push(addr, size uintptr) {}
func Pull(addr, size uintptr) {}
