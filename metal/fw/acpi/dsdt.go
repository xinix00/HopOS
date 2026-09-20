package acpi

// DSDT returns the checksum-validated Differentiated System Description
// Table. ACPI links it through the FADT, not through the XSDT entries.
// readable must approve the firmware RAM range before it is accessed.
func (t *Tables) DSDT(readable func(uint64, uint64) bool) []byte {
	f := t.table("FACP")
	if len(f) < 44 || readable == nil {
		return nil
	}
	pa := uint64(u32(f[40:]))
	if len(f) >= 148 && u64(f[140:]) != 0 {
		pa = u64(f[140:])
	}
	if !plausiblePA(uintptr(pa), 36) || !readable(pa, 36) {
		return nil
	}
	h := mem(uintptr(pa), 36)
	n := uint64(u32(h[4:]))
	if string(h[:4]) != "DSDT" || n < 36 || n > 1<<22 || !readable(pa, n) {
		return nil
	}
	return decodeAt(uintptr(pa))
}
