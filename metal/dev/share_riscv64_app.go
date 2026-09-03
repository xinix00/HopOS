//go:build riscv64 && linkramsize

package dev

// De app-kant van een slot (linkramsize = dit ís een app-image): Push en Pull
// zijn no-ops, precies zoals op ARM.
//
// LET OP de tag: dit was `linkcpuinit`, en dat werd stil fataal toen de KERN
// die tag óók kreeg (de boot-hart-loterij, 17-08): vanaf dat moment kreeg de
// kern deze no-ops, verdween dev.CleanInv door deadcode-eliminatie compleet
// uit het image (nm: 0 treffers), en draaide élke non-coherente naad — ringen,
// ctx-blokken, mailboxen — op evictie-geluk van de cache. Gemeten als boot
// 7-11 (17-08): slot-TX-ringen die na ~30s "corrupt" of eeuwig leeg lazen
// terwijl de app schreef. linkramsize dragen alle riscv64-app-builds
// (tools/apps-release.sh, de slot-apps in de gate) en nooit een kern-build —
// dát is de eigenschap die hier scheidt.
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
// RealCacheOps: 0 — zie share_riscv64.go. Linkt dit bestand ooit toch in de
// kern, dan breekt kern/slots' compile-time check de build.
const RealCacheOps = 0

func Push(addr, size uintptr) {}
func Pull(addr, size uintptr) {}
