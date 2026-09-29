//! Host-kant van [`switch`](self): er is geen EL2-assembly, dus ook geen
//! blobs om te kopiëren (zoals `pc_host.go` en `stage2_host.go`).
//!
//! De tests bewijzen de indeling van de descriptor, de som, de thunks en de
//! ctx-lezers over een buffer; de assembly zelf bewijst het board.

use super::Error;
use super::dispatch::Flavor;

/// Geen blobs op de host.
pub(super) fn blobs(_flavor: Flavor) -> Result<[&'static [u8]; 3], Error> {
    Err(Error::NoSwitchCode)
}

/// Geen parkeerlus op de host.
pub(super) fn park_code() -> Result<&'static [u8], Error> {
    Err(Error::NoSwitchCode)
}

/// Geen I-cache op de host.
pub(super) fn publish_code() {}

/// Geen IPI op de host.
pub(super) fn apple_ipi(_v: u64) {}
