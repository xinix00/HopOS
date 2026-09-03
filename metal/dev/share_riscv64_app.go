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
// Waarom dat mag: de gedeelde control page, mailboxringen en framequeue-
// descriptorpagina's mapt de kooi als DEVICE (kern/slots slotMap). Ongecachet
// en niet-herordend, dus Push/Pull hebben voor díé metadata niets te doen.
// Framepayload is anders: die blijft in gewoon gecachet app-RAM. frameq gebruikt
// daarvoor zijn expliciete PublishPayload/AcquirePayload-naad met de door HOP
// verstrekte fysieke partitie-basis; die roept CleanInv rechtstreeks aan.
//
// En waarom het móet: de cache-ops van dit silicium werken op FYSIEKE adressen
// (dev_riscv64.s), terwijl een app linkadressen heeft — de kooi verplaatst hem.
// Zou generieke Push/Pull op een linkadres toch een cache-op afvuren, dan raakte
// die de cacheline van een ánder slot. Daarom blijven deze functies no-op en
// vertaalt alleen de frameq-payloadnaad eerst naar het juiste fysieke adres.
// RealCacheOps: 0 — zie share_riscv64.go. Linkt dit bestand ooit toch in de
// kern, dan breekt kern/slots' compile-time check de build.
const RealCacheOps = 0

func Push(addr, size uintptr) {}
func Pull(addr, size uintptr) {}
