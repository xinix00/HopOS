//! De minimale 16550-UART: de tegenhanger van `driver-pl011` voor de
//! UEFI-console.
//!
//! De ACPI SPCR zegt welk van de twee een platform heeft (Interface Type)
//! én met welke registerstap (GAS access size): de DesignWare 8250 van de
//! Orion O6N (Cix P1) ligt op 32-bit-stride, een klassieke 16550 op
//! byte-stride. Alleen THR en LSR worden aangeraakt: de firmware heeft
//! baudrate en lijn al gezet, en wie ze opnieuw zet zonder de klok van het
//! blok te kennen, maakt er ruis van.
//!
//! Een UART die nooit THRE meldt (dood, ongeklokt, alle enen) mag de
//! console niet eeuwig gijzelen: na een begrensde poll (~1M lezingen, veel
//! meer dan zestien tekens op 115200) is hij dood verklaard en slikt hij
//! alles. Dezelfde vangrail als de PL011 en als Go's `ns16550.Putc`.

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

use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::Pa;

/// Transmit holding register (registerindex).
const THR: u64 = 0;
/// Line status register (registerindex).
const LSR: u64 = 5;
/// LSR: de THR is leeg.
const LSR_THRE: u32 = 1 << 5;
/// Hoe vaak we op THRE pollen voor de UART dood heet.
const POLL_MAX: u32 = 1 << 20;

/// Een 16550 op een vast adres met registerstap `1 << shift`.
pub struct Ns16550 {
    base: Pa,
    shift: u8,
    dead: AtomicBool,
}

impl Ns16550 {
    /// De UART op `base`; `shift` is de registerstap als macht van twee (0
    /// = byte-stride, 2 = 32-bit-stride zoals DesignWare).
    ///
    /// # Safety
    ///
    /// `base` is een 16550-registerblok met die stap, gemapt als Device, en
    /// blijft dat zolang het programma draait.
    #[must_use]
    pub const unsafe fn new(base: Pa, shift: u8) -> Self {
        Self {
            base,
            shift,
            dead: AtomicBool::new(false),
        }
    }

    fn reg(&self, idx: u64) -> Pa {
        self.base.add(idx << self.shift)
    }

    fn read(&self, idx: u64) -> u32 {
        // Byte-stride: een 32-bit-lees op LSR (offset 5) is ongealigneerd en
        // faultt op device-geheugen, dus dan per byte.
        if self.shift == 0 {
            u32::from(dev::read8(self.reg(idx)))
        } else {
            dev::read32(self.reg(idx))
        }
    }

    fn write(&self, idx: u64, v: u8) {
        if self.shift == 0 {
            dev::write8(self.reg(idx), v);
        } else {
            dev::write32(self.reg(idx), u32::from(v));
        }
    }

    /// Is de UART dood verklaard?
    #[must_use]
    pub fn is_dead(&self) -> bool {
        self.dead.load(Relaxed)
    }

    /// Eén byte, na een begrensde wacht op een lege THR.
    pub fn putc(&self, c: u8) {
        if self.is_dead() {
            return;
        }
        let mut i = 0;
        while self.read(LSR) & LSR_THRE == 0 {
            i += 1;
            if i > POLL_MAX {
                self.dead.store(true, Relaxed);
                return;
            }
        }
        self.write(THR, c);
    }

    /// Schrijft `b`, met `\r` vóór elke `\n` (een terminal wil beide).
    pub fn write_bytes(&self, b: &[u8]) {
        for &c in b {
            if c == b'\n' {
                self.putc(b'\r');
            }
            self.putc(c);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn writes_on_the_register_stride() {
        // Een nep-blok op 32-bit-stride: LSR (index 5) op offset 20 met
        // THRE gezet, THR op offset 0.
        let mut regs = [0u32; 8];
        regs[5] = LSR_THRE;
        let base = Pa(regs.as_mut_ptr() as usize as u64);
        // SAFETY: `regs` leeft de hele test en heeft de indeling.
        let u = unsafe { Ns16550::new(base, 2) };
        u.putc(b'x');
        assert_eq!(regs[0], u32::from(b'x'));
        assert!(!u.is_dead());
    }

    #[test]
    fn a_uart_that_never_drains_is_declared_dead() {
        let mut regs = [0u8; 8];
        let base = Pa(regs.as_mut_ptr() as usize as u64);
        // SAFETY: zie hierboven, nu op byte-stride.
        let u = unsafe { Ns16550::new(base, 0) };
        u.putc(b'x');
        assert!(u.is_dead());
        assert_eq!(regs[0], 0);
    }
}
