package hopslot

// slotHint wordt door slots.Start in een app-image gepatcht (symbool
// "hop-os/metal/board/hopslot.slotHint"): het slotnummer van deze start.
// 0 = niet gepatcht (een image buiten slots om). Moet in dít pakket blijven
// wonen — de symboolnaam is deel van het laad-contract.
var slotHint uint64
