//! Firmware-lezers: FDT, ACPI, bootcfg, en voor Apple silicon de ADT,
//! boot_args (xnuboot) en de GPT. Lezen, geen registers.
//!
//! Alles hier werkt op slices: de firmware-input is onvertrouwd, dus
//! indexeren gaat met `get` en een kromme blob is een `None` of een fout,
//! nooit een panic. Wie een adres heeft (het board), maakt er een slice
//! van; deze crate kent geen adressen en heeft geen `unsafe`.
//!
//! `adt`, `xnuboot` en `gpt` zijn van de Mac mini M4 (`board-apple`):
//! host-getest met de getallen die de Go-lezers op ijzer maten. `acpi` is
//! geport voor de O6N en QEMU onder EDK2; [`aml`] leest precies één AML-ding, de `_PRT`
//! (INTx als terugval naast MSI-X). De `_CPC`-scan staat bij de O6N.

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
pub mod adt;
pub mod aml;
pub mod bootcfg;
mod bytes;
pub mod fdt;
pub mod gpt;
pub mod xnuboot;
