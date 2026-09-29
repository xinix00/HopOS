//! Firmware-lezers: FDT, ACPI en bootcfg. Lezen, geen registers.
//!
//! Alles hier werkt op slices: de firmware-input is onvertrouwd, dus
//! indexeren gaat met `get` en een kromme blob is een `None` of een fout,
//! nooit een panic. Wie een adres heeft (het board), maakt er een slice
//! van; deze crate kent geen adressen en heeft geen `unsafe`.
//!
//! Nog niet geport uit `OLD/metal/fw`: `adt`, `xnuboot`; ze komen met de
//! boards die ze nodig hebben (de Apple-machines). `acpi` is geport voor de
//! O6N en QEMU onder EDK2; de AML-scan (`cpc.go`, `dsdt.go`'s aanroepers)
//! en de PCCT nog niet.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]
#![forbid(unsafe_code)]

pub mod acpi;
pub mod bootcfg;
pub mod fdt;
