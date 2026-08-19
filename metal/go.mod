module github.com/xinix00/HopOS/metal

go 1.26.4

require github.com/usbarmory/tamago v1.26.4

require github.com/xinix00/lean v0.10.0

require github.com/xinix00/hoplockserver v0.2.1 // indirect

// Directe dep sinds de app-object-store (cmd/hopos/store.go gebruikt de
// streaming object-API + ListObjects van v0.3.0).
require github.com/xinix00/hoplock v0.4.1

// De gepubliceerde HOP-tag waar deze boom tegen bouwt (o.a.
// hopos.ErrNoCapacity en StartStaged mét jobnaam). GEEN lokale replace meer
// (Derek, 04-08): een hop-wijziging bereikt hop-os alleen via commit+tag, dus
// wat hier bouwt bouwt overal — ook downstream (de -hopos-satellieten), die een
// replace nooit zouden zien.
//
// LOSSE comment-blokken, NIET in het require-blok: een comment binnen (of
// vastgeplakt aan) een require-blok maakt dat blok niet-canoniek voor cmd/go's
// go.mod-herschrijver, en die verbouwt dan élke build de structuur — een pad
// waarin de tamago-toolchain panict op x/mod modfile ("index out of range").
// Gemeten 19-08 in deze boom; surf's go.mod draagt dezelfde waarschuwing.

require (
	github.com/xinix00/hop v0.20.23
	golang.org/x/crypto/x509roots/fallback v0.0.0-20260709184058-243e02a382f8
)

// De netstack woont sinds 12-08 in xinix00/lean (leannet) en reist dus mee als
// gewone require — géén replace, géén fork-onderhoud. Dat is de reden dat we
// hem zelf gebouwd hebben: een replace geldt ALLEEN in de main module, dus
// alles wat metal importeert (hop-os-surf, de vitals/welcome-apps) bouwde
// eerder tegen ongepatchte upstream-lneto terwijl metal zelf de fixes had.
// Onze eigen code in onze eigen module heeft dat probleem niet.

// Tot hop's volgende release: hopos.PoolReporter (de optionele
// grootste-gat-vraag) staat daar nog niet in.
