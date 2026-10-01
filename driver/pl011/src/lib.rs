//! De PL011-UART: de console van QEMU virt en de Pi's.
//!
//! Hetzelfde PrimeCell-registerblok zit op QEMU virt (0x0900_0000), de Pi 4
//! (0xFE20_1000) en de Pi 5 (0x10_7d00_1000). Alleen de basis verschilt per
//! board; de offsets en de TX-FIFO-vol-bit zijn PrimeCell-standaard en leven
//! daarom één keer, hier, in plaats van per board hergedefinieerd (dan
//! corrigeer je een verkeerde offset maar in één board).
//!
//! Deze crate bezit het registerblok en niets anders: geen slot, geen
//! buffer. Wie meerdere schrijvers heeft (de console), zet het ene
//! toegestane slot erboven (handboek §1.3).

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

use core::fmt;
use core::mem::offset_of;
use core::sync::atomic::{AtomicBool, Ordering::Relaxed};
use dev::{Pa, Reg};

/// Het PL011-registerblok (ARM DDI 0183, tabel 3-1).
#[repr(C)]
struct Regs {
    /// Data register: schrijven zet een byte in de TX-FIFO.
    dr: Reg<u32>,
    /// Receive status / error clear.
    rsr: Reg<u32>,
    _r0: [u32; 4],
    /// Flag register: FIFO-standen en busy.
    fr: Reg<u32>,
    _r1: u32,
    /// IrDA low-power counter.
    ilpr: Reg<u32>,
    /// Integer baud rate divisor.
    ibrd: Reg<u32>,
    /// Fractional baud rate divisor.
    fbrd: Reg<u32>,
    /// Line control: woordlengte, FIFO aan.
    lcr_h: Reg<u32>,
    /// Control: UART, TX en RX aan.
    cr: Reg<u32>,
    /// Interrupt FIFO level select.
    ifls: Reg<u32>,
    /// Interrupt mask set/clear.
    imsc: Reg<u32>,
    /// Raw interrupt status.
    ris: Reg<u32>,
    /// Masked interrupt status.
    mis: Reg<u32>,
    /// Interrupt clear.
    icr: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Regs, dr) == 0x00);
    assert!(offset_of!(Regs, rsr) == 0x04);
    assert!(offset_of!(Regs, fr) == 0x18);
    assert!(offset_of!(Regs, ilpr) == 0x20);
    assert!(offset_of!(Regs, ibrd) == 0x24);
    assert!(offset_of!(Regs, fbrd) == 0x28);
    assert!(offset_of!(Regs, lcr_h) == 0x2c);
    assert!(offset_of!(Regs, cr) == 0x30);
    assert!(offset_of!(Regs, ifls) == 0x34);
    assert!(offset_of!(Regs, imsc) == 0x38);
    assert!(offset_of!(Regs, ris) == 0x3c);
    assert!(offset_of!(Regs, mis) == 0x40);
    assert!(offset_of!(Regs, icr) == 0x44);
};

/// FR: TX-FIFO vol.
const FR_TXFF: u32 = 1 << 5;
/// LCR_H: FIFO's aan.
const LCR_FEN: u32 = 1 << 4;
/// LCR_H: 8 databits.
const LCR_WLEN8: u32 = 0b11 << 5;
/// CR: UART aan.
const CR_UARTEN: u32 = 1 << 0;
/// CR: zender aan.
const CR_TXE: u32 = 1 << 8;
/// CR: ontvanger aan.
const CR_RXE: u32 = 1 << 9;

/// De poll-grens op een volle TX-FIFO: ~1M leesacties is veel meer dan 16
/// tekens op 115200 baud. Een ongeklokte of dode PL011 (de Pi 5-debug-UART
/// zonder sessie) leest all-ones en zou de console anders eeuwig gijzelen;
/// na deze grens valt de UART uit het printpad in plaats van de boot te
/// laten hangen (Go-driver, `metal/driver/pl011`).
const POLL_LIMIT: u32 = 1 << 20;

/// Eén PL011 op een vaste basis.
pub struct Pl011 {
    base: Pa,
    /// Bleef de TX-FIFO voorbij [`POLL_LIMIT`] vol, dan is de UART dood en
    /// schrijven we er niet meer naar.
    dead: AtomicBool,
}

impl Pl011 {
    /// Een PL011 op `base`.
    ///
    /// # Safety
    ///
    /// `base` is de basis van een gemapt PL011-registerblok dat zolang het
    /// programma draait blijft bestaan (een layout-adres van het board).
    #[must_use]
    pub const unsafe fn new(base: Pa) -> Self {
        Self {
            base,
            dead: AtomicBool::new(false),
        }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new` zegt dat `base` een gemapt
        // PL011-blok is dat voor altijd blijft; `Regs` is precies dat blok.
        unsafe { dev::regs(self.base) }
    }

    /// Zet de UART aan: 8N1 met FIFO's, TX en RX aan. De baudrate blijft
    /// staan zoals de firmware hem zette (QEMU negeert hem; de Pi-firmware
    /// zet hem met `uart_2ndstage=1`).
    pub fn init(&self) {
        let r = self.regs();
        r.cr.write(0);
        r.lcr_h.write(LCR_FEN | LCR_WLEN8);
        r.imsc.write(0);
        r.icr.write(0x7ff);
        r.cr.write(CR_UARTEN | CR_TXE | CR_RXE);
    }

    /// Is de UART uit het printpad gevallen?
    #[must_use]
    pub fn is_dead(&self) -> bool {
        self.dead.load(Relaxed)
    }

    /// Stuurt één byte, begrensd wachtend op ruimte in de TX-FIFO.
    pub fn putc(&self, c: u8) {
        if self.dead.load(Relaxed) {
            return;
        }
        let r = self.regs();
        let mut spins = 0u32;
        while r.fr.read() & FR_TXFF != 0 {
            spins += 1;
            if spins > POLL_LIMIT {
                self.dead.store(true, Relaxed);
                return;
            }
        }
        r.dr.write(u32::from(c));
    }

    /// Schrijft `b`, met `\n` als `\r\n` (een terminal wil beide).
    pub fn write(&self, b: &[u8]) {
        for &c in b {
            if c == b'\n' {
                self.putc(b'\r');
            }
            self.putc(c);
        }
    }
}

impl fmt::Write for &Pl011 {
    fn write_str(&mut self, s: &str) -> fmt::Result {
        self.write(s.as_bytes());
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use core::fmt::Write as _;

    /// Een nep-registerblok in gewoon geheugen: `dev` doet op de host
    /// normale geheugentoegang.
    fn fake() -> (Box<[u32; 32]>, Pl011) {
        let mut mem = Box::new([0u32; 32]);
        let pa = Pa(mem.as_mut_ptr() as usize as u64);
        // SAFETY: de box leeft zolang de test loopt en is groot genoeg voor
        // het blok.
        (mem, unsafe { Pl011::new(pa) })
    }

    #[test]
    fn writes_bytes_to_dr_and_expands_newline() {
        let (mem, u) = fake();
        u.putc(b'x');
        assert_eq!(mem[0], u32::from(b'x'));
        let mut w = &u;
        writeln!(w, "a").unwrap();
        // De laatste byte in DR is de '\n', na een '\r'.
        assert_eq!(mem[0], u32::from(b'\n'));
    }

    #[test]
    fn a_stuck_fifo_marks_the_uart_dead() {
        let (mut mem, u) = fake();
        mem[0x18 / 4] = FR_TXFF; // TX-FIFO blijft vol
        u.putc(b'x');
        assert!(u.is_dead());
        mem[0x18 / 4] = 0;
        mem[0] = 0;
        u.putc(b'y'); // dood blijft dood: DR onaangeroerd
        assert_eq!(mem[0], 0);
    }

    #[test]
    fn init_enables_tx_and_rx() {
        let (mem, u) = fake();
        u.init();
        assert_eq!(mem[0x30 / 4], CR_UARTEN | CR_TXE | CR_RXE);
        assert_eq!(mem[0x2c / 4], LCR_FEN | LCR_WLEN8);
    }
}
