//! De app-runtime van HopOS: wat een Rust-app nodig heeft om in een slot
//! te draaien.
//!
//! Een app ziet één regio, zijn eigen partitie. Onderin staat zijn RAM
//! (image, heap, stack), bovenin een staart van 2 MB met de control-page, de
//! outbox-ring en de frame-ringen. Uit twee woorden in zijn image, `RamStart`
//! en `RamSize` (de kern patcht ze bij plaatsing), rekent applib elk adres
//! uit ([`Tail`]); een absoluut adres kent de app niet.
//!
//! - [`App`]: het handvat. Bezit de outbox-producer, de control-page-toegang
//!   en de env; meldt READY en draait de heartbeat.
//! - [`log!`]: logregels naar de outbox, zonder allocatie, droppen bij vol.
//! - [`main!`]: de main-schil. `_start`, de stack, de heap, de executor van
//!   de app-core, en dan de `async fn` van de app.
//! - [`AppSleeper`]: de idle van de app-core (WFE, of de yield naar EL2 op
//!   een gedeelde core), met de deurbel van de RX-ring.
//! - [`net`]: frame-niveau netwerk over de frame-ringen: de [`net::Nic`]
//!   (`netdev::Device`), de RX-pomp en de deurbel.
//! - [`sys`]: de system-API-client over een [`sys::Conn`].
//!
//! Wat hier niet staat: de netstack (`leannet`, nog niet gekoppeld), SMP
//! (één app-core, PORT.md beslissing 8), de codec- en device-ops.

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

mod arch;
mod contract;

pub mod app;
pub mod clock;
pub mod ctrl;
pub mod heap;
pub mod log;
pub mod net;
pub mod ring;
pub mod rt;
pub mod sleep;
pub mod sys;
pub mod tail;

pub use app::{App, AppError, Beat};
pub use ctrl::{AppStatus, Ctrl, Env};
pub use rt::{EXEC, Exec, app};
pub use sleep::AppSleeper;
pub use tail::{Tail, TailError, tail_of};
