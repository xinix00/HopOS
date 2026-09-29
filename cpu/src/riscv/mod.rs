//! De riscv64-helft van de architectuurlaag: machine mode op QEMU virt en de
//! XuanTie C906 (SG2002, de LicheeRV Nano).
//!
//! Dezelfde machine als de arm64-helft, andere letters (Go, `cpu/mmode`):
//!
//! ```text
//! ARM                     RISC-V
//! EL2                     machine mode         (de kern)
//! EL1-app                 supervisor mode      (een slot)
//! HVC-yield               ecall-yield          (a0 = wektijd, a7 = 0/1)
//! stage-2 (VTTBR)         Sv39 (satp) + PMP    (verplaatsen + begrenzen)
//! ERET                    mret
//! PSCI CPU_ON             msip op een geparkeerd hart
//! GIC                     PLIC + CLINT
//! ```
//!
//! Wat hier staat, in de volgorde van de boot: [`csr`] (de losse
//! instructies), [`boot`] (`_start`, BSS, de parkeerlus van de andere
//! harts), [`trap`] (de trap-ingang van de kern), [`clint`] (wekker en
//! kick), [`idle`] (de [`executor::Sleeper`] en de klok), [`plic`] (de
//! [`crate::irq::Controller`]), [`trng`] (er is er geen: luid). Het
//! kooi-spoor: [`pmp`] (de whitelist, TOR), [`sv39`] (de relocatie), en
//! [`switch`] (de M-mode-switcher op een app-hart), [`oscore`] (de
//! bewoners van het hart van de kern, in zijn idle).
//!
//! Wat NIET hier staat: het cache-onderhoud (`th.dcache.*` van de C906). Dat
//! is `dev::push`/`dev::pull` achter de feature `thead` van `dev`, want
//! alleen `dev` raakt rauw geheugen aan (handboek §5).
//!
//! Op de host bouwt deze module met stubs: de rekenkunde (PMP,
//! Sv39, de PLIC-registers, de slaap) is wat de host bewijst.

pub mod boot;
pub mod clint;
pub mod csr;
pub mod idle;
pub mod oscore;
pub mod plic;
pub mod pmp;
pub mod sv39;
pub mod switch;
pub mod trap;
pub mod trng;
