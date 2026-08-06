//go:build !gui

// surfgrant_stub.go — de kale helft van de surface-grant-naad. De echte
// administratie (surfgrant.go) is gui-werk: zonder display-houder valt er
// niets te verlenen, dus kaal gebouwd linkt hij niet mee — zelfde knop als
// gui/driver/rkscan en gui/fbgrant. Wat blijft zijn de drie namen die de rest van
// kern/slots onvoorwaardelijk aanroept (rpc.go voor OpSurfGrant/OpSurfRevoke,
// releaseSlot bij het vrijgeven): de grant-vraag krijgt een fout — de app valt
// dan terug op de pixels-over-de-socket-weg, die óók het pad voor apps op een
// andere node is — en de opruimpaden zijn no-ops omdat er niets te ruimen valt.
package slots

import "fmt"

func SurfaceGrant(i int, off, n uint64) (uint64, error) {
	return 0, fmt.Errorf("surface grant: headless build — no display subsystem on this node (build with -tags gui)")
}

func SurfaceRevoke(i int)     {}
func SurfaceHolderGone(i int) {}
