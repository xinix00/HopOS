//! De architectuurlaag van HopOS v3 (arm64 eerst, riscv64 daarna).
//!
//! Twee sporen delen deze crate en elk heeft zijn eigen bestanden:
//!
//! - het boot-spoor: [`boot`] (de `_start`-stub, stack, BSS, de sprong naar
//!   Rust), [`vectors`] (de vectortabel: IRQ zet een vlag, een synchrone
//!   exception meldt zich en parkeert), [`console`] (de vroege UART-haak);
//! - het kooi-spoor: [`el2`] (stage-2, de switcher, de HVC-handler),
//!   [`psci`], [`smp`], [`irq`] (het contract: een lijn, een controller, één
//!   werkwoord), [`idle`] (de `Sleeper` van de executor: WFE met event-stream,
//!   WFI op de timer, yield), [`trng`], [`drbg`], [`memattr`], [`memlimit`].
//!
//! De specificatie is `OLD/metal/cpu` en de assembly ernaast; elk bestand
//! draagt de gedateerde metingen van zijn Go-voorganger mee.

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

pub mod boot;
pub mod console;
pub mod drbg;
pub mod el2;
pub mod idle;
pub mod irq;
pub mod memattr;
pub mod memlimit;
pub mod psci;
pub mod smp;
pub mod trng;
pub mod vectors;
