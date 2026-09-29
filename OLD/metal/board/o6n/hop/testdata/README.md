`o6n-dsdt.aml` is the checksum-validated DSDT captured from the O6N test node on 2026-09-18. SHA256: `32366cf689b027494a1cd97df9f77d717c9d114080edb25b7d40c70300d4eff2`.

The fixture contains six `XHC0..5` and four `USB0..3` devices with HID `PNP0D10`, their static register resources, and firmware enable/role predicates. Tests use these actual bytes, then mutate unsupported descriptions and exercise all 1024 enable masks. Run from the parent directory: `GOWORK=off go test usb_firmware.go usb_firmware_test.go`.

Primary implementation sources:

- [CIX USB ACPI descriptions](https://github.com/cixtech/cix_opensource__release__edk2-platforms/blob/e5cb70f632756654672c66e0c2628d501feffa2c/Platform/CIX/Sky1/Drivers/AcpiSocTables/Dsdt-USB.asl)
- [CIX firmware variable byte reader](https://github.com/cixtech/cix_opensource__release__edk2-platforms/blob/e5cb70f632756654672c66e0c2628d501feffa2c/Platform/CIX/Sky1/Drivers/AcpiSocTables/Dsdt-AcpiRam.asl)
- [Sky1 device-tree implementation](https://github.com/Sky1-Linux/linux-sky1/blob/57e018a398248d7e5e4d798610df79a557c0629f/patches-latest/0001-arm64-dts-cix-Add-Sky1-SoC-and-Radxa-Orion-O6-device.patch): `usbhs_0..3` use `cdns,usbssp` and name the windows at `0x09268000`, `0x09298000`, `0x092c8000`, `0x092f8000` **xhci**. CIX's `USB_EHCI_HOST*` macro names are misleading; these USB2-only ports use the existing xHCI path too.

No USB controller register was probed to obtain this fixture. The diagnostic followed FADT X_DSDT, checked ACPI RAM ownership and the table checksum, then captured GNVA enable bytes separately.
