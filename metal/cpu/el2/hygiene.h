// hygiene.h — het ENE blok voor "deze core gaat zo vers geschreven code
// uitvoeren". Drie ingangen springen in code die net met gewone stores is
// neergelegd — de app-drop (el2.s), de SMP-secundaire (smp.s) en de kern-flip
// (chain_arm64.s) — en ze doen daarbij exact hetzelfde, dus het STAAT er ook
// maar één keer (Derek, 01-09: "we doen freaking hetzelfde — core klaarmaken,
// springen, cleanen, vullen").
//
// Waarom dit blok bestaat: de datacache-veeg van de plaatser (slots.Scrub,
// óók één implementatie) raakt de instructiecache principieel niet, en een
// PIPT-I$ houdt de instructies van de vórige huurder van deze fysieke
// adressen vast. De les is twee keer op ijzer betaald: Altra 15-07 (elke
// warme herdispatch dood bij boot) en de M4-kern-flip 01-09 (elke sprong
// dood vóór de eerste instructie, vier flashes lang). QEMU-TCG modelleert
// geen caches en verhult dit dus volledig.
//
// Lokaal (IALLU, niet IALLUIS): elke gebruiker is de core die er zélf zo
// meteen in springt.
#define I_HYGIENE \
	WORD $0xd508751f; \
	WORD $0xd5033f9f; \
	WORD $0xd5033fdf
