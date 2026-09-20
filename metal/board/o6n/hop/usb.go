package hop

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// firmwareUSBMemory approves firmware-owned ACPI RAM, never a controller
// register window. This protects both FADT pointers and the CIX variable RAM.
func firmwareUSBMemory(base, size uint64) bool {
	if base == 0 || size == 0 || base+size < base {
		return false
	}
	covered := false
	for _, d := range uefi.MemoryMap() {
		if d.Pages > ^uint64(0)/4096 {
			continue
		}
		end := d.Start + d.Pages*4096
		if end < d.Start {
			continue
		}
		if (d.Type == 11 || d.Type == 12) && base < end && d.Start < base+size {
			return false
		}
		if (d.Type == 9 || d.Type == 10) && base >= d.Start && base+size <= end {
			covered = true
		}
	}
	return covered && uefi.MapHigh(base, size)
}

// USBHosts discovers firmware-configured native xHCI hosts, in stable DMA
// slot order. It neither probes nor reconfigures disabled USB controllers.
func USBHosts() ([usbHostCount]USBHost, error) {
	if !IsCix() {
		return [usbHostCount]USBHost{}, fmt.Errorf("not CIX firmware")
	}
	b := uefi.Tables().DSDT(firmwareUSBMemory)
	if b == nil {
		return [usbHostCount]USBHost{}, fmt.Errorf("no valid DSDT in ACPI RAM")
	}
	f, err := parseUSBFirmware(b)
	if err != nil {
		return [usbHostCount]USBHost{}, err
	}
	if !firmwareUSBMemory(f.base, f.size) {
		return [usbHostCount]USBHost{}, fmt.Errorf("USB variables outside ACPI RAM")
	}
	values := make([]byte, int(f.size))
	for i := range values {
		values[i] = dev.Read8(uintptr(f.base + uint64(i)))
	}
	return f.enabledHosts(values), nil
}
