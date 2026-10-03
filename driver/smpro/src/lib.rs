//! De SoC-sensoren van Ampere's systeembeheer-processor (SMpro) via een
//! ACPI PCC-kanaal: het Altra-equivalent van de VideoCore-mailbox van de Pi.
//!
//! Linux-referenties: `drivers/hwmon/xgene-hwmon.c` (het berichtformaat: de
//! Altra-SMpro spreekt de SLIMpro-taal van zijn APM X-Gene-voorouder) en
//! `drivers/mailbox/pcc.c` (de doorbell-handdruk). De Go-voorganger is
//! `OLD/metal/driver/smpro`. Eén bewuste afwijking van Linux: wij pollen op
//! CMD_COMPLETE in plaats van de platform-interrupt af te wachten; één
//! temperatuur per minuut rechtvaardigt geen GIC-bedrading.
//!
//! Waar het kanaal woont, zegt de PCCT (fw/acpi, het board zet hem om in
//! [`Pcc`]); dit crate kent het board niet. Eén aanroeper (de
//! telemetrie-taak), dus `&mut self` en geen slot.

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

use core::mem::offset_of;
use dev::{Pa, Reg};

/// Het PCC-kanaal van de hardware-monitor op socket 0: vast in alle
/// Altra-firmware (edk2-platforms `Dsdt.asl`: device APMC0D29 met `_DSD`
/// "pcc-channel" = 14; identiek bij ADLINK, Jade en ASRock; kanaal 29 is
/// socket 1). Wij lezen geen AML, dus dit is de ene firmware-constante; een
/// platform waar hij niet klopt, is onschuldig: de proeflees faalt en de
/// telemetrie blijft uit.
pub const HWMON_CHANNEL: u32 = 14;

/// Het gedeelde geheugen van een generiek PCC-kanaal (ACPI 6.4 §14.2):
/// signatuur, command (u16) en status (u16) samen als één gealigneerd
/// woord, dan de payload.
#[repr(C)]
struct Shmem {
    signature: Reg<u32>,
    /// command [15:0], status [31:16].
    cmd_status: Reg<u32>,
    msg: [Reg<u32>; 3],
}

const _: () = {
    assert!(offset_of!(Shmem, signature) == 0);
    assert!(offset_of!(Shmem, cmd_status) == 4);
    assert!(offset_of!(Shmem, msg) == 8);
};

/// Hoeveel shmem een kanaal minstens moet hebben.
pub const SHMEM_MIN: u64 = core::mem::size_of::<Shmem>() as u64;

/// "PCC" plus het kanaalnummer (`include/acpi/pcc.h`).
const PCC_SIGNATURE: u32 = 0x5043_4300;
/// Command: "interrupt het OS bij antwoord". Het door SMpro geteste
/// Linux-pad; de SPI verzuipt in onze uitstaande GIC.
const CMD_GEN_DB_INT: u32 = 1 << 15;
/// Status: het platform is klaar met dit commando.
const ST_CMD_COMPLETE: u32 = 1 << 0;
/// DBG | SENSOR_READ | handle (xgene-hwmon).
const SENSOR_RD_MSG: u32 = 0x04ff_e902;
/// De SoC-temperatuursensor.
const SOC_TEMP_REG: u32 = 0x10;
/// Antwoorddata: sensor (nog) niet geldig.
const INVALID_BIT: u32 = 1 << 15;
/// Antwoordtype in [31:28]: SMpro-fout.
const MSG_TYPE_ERR: u32 = 7;
/// De ondergrens van het wachtbudget: de SMpro antwoordt in de praktijk in
/// microseconden.
const BUDGET_FLOOR_NS: u64 = 50_000_000;

/// Een PCC-subkanaal zoals de PCCT het beschrijft (type 0/1/2).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct Pcc {
    /// Het gedeelde geheugen.
    pub shmem: Pa,
    /// Zijn lengte.
    pub shmem_len: u64,
    /// Het doorbell-register.
    pub doorbell: Pa,
    /// 32 of 64 bits.
    pub doorbell_width: u8,
    /// Doorbell: welke bits bewaard blijven.
    pub preserve: u64,
    /// Doorbell: welke bits gezet worden.
    pub write: u64,
    /// De nominale latentie in microseconden.
    pub latency_us: u32,
}

fn le(b: &[u8], off: usize, n: usize) -> u64 {
    b.get(off..off + n)
        .map(|s| s.iter().rev().fold(0u64, |a, &x| (a << 8) | u64::from(x)))
        .unwrap_or(0)
}

/// Subkanaal `idx` uit de PCCT-tabel `pcct` (de bytes zoals het board ze
/// laadde), of `None` als de index niet bestaat, de entry kapot is, of het
/// type buiten 0..2 valt. De subkanalen staan vanaf offset 48 (SDT-kop,
/// flags, reserved) en tellen ordinaal: de positie ís het kanaalnummer
/// waar de DSDT-property "pcc-channel" naar wijst. Types 0/1/2 delen de
/// veld-offsets die wij nodig hebben; de extended types (3+) zijn
/// CPPC-constructies die we overslaan. QEMU virt heeft geen PCCT.
#[must_use]
pub fn pcc_from(pcct: &[u8], idx: u32) -> Option<Pcc> {
    let mut off = 48usize;
    let mut n = 0;
    while off + 2 <= pcct.len() {
        let (typ, l) = (*pcct.get(off)?, usize::from(*pcct.get(off + 1)?));
        if l < 2 || off + l > pcct.len() {
            return None; // kapotte entry: niet verder gissen
        }
        if n == idx {
            if typ > 2 || l < 62 {
                return None;
            }
            let e = pcct.get(off..off + l)?;
            // De GAS op +24: space, breedte, offset, access, adres (8).
            return Some(Pcc {
                shmem: Pa(le(e, 8, 8)),
                shmem_len: le(e, 16, 8),
                doorbell_width: *e.get(25)?,
                doorbell: Pa(le(e, 28, 8)),
                preserve: le(e, 36, 8),
                write: le(e, 44, 8),
                latency_us: le(e, 52, 4) as u32,
            });
        }
        n += 1;
        off += l;
    }
    None
}

/// Eén open PCC-kanaal naar de SMpro.
pub struct Smpro {
    ch: u32,
    pcc: Pcc,
    clock: fn() -> u64,
    /// Het gedeelde geheugen is nog van de firmware: een vorig commando
    /// liep af zonder CMD_COMPLETE. Tot dat bit komt, schrijven wij er niet
    /// in.
    pending: bool,
}

impl Smpro {
    /// Een kanaal op subruimte `ch` van de PCCT.
    ///
    /// # Safety
    ///
    /// `pcc.shmem` (voor `pcc.shmem_len` bytes) en `pcc.doorbell` zijn
    /// gemapt als Device en blijven bestaan; niemand anders dan deze driver
    /// en de SMpro gebruikt het kanaal.
    #[must_use]
    pub unsafe fn new(ch: u32, pcc: Pcc, clock: fn() -> u64) -> Self {
        Self {
            ch,
            pcc,
            clock,
            pending: false,
        }
    }

    fn shm(&self) -> &'static Shmem {
        // SAFETY: de voorwaarde van `new`; `call` toetst de lengte eerst.
        unsafe { dev::regs(self.pcc.shmem) }
    }

    /// De SoC-temperatuur in milligraden Celsius; `None` als de SMpro niet
    /// antwoordt of de sensor ongeldig meldt. De sensor meldt hele graden
    /// als 9-bit two's-complement.
    pub fn soc_temp_milli_c(&mut self) -> Option<i32> {
        let (r0, r1) = self.call(SENSOR_RD_MSG, SOC_TEMP_REG, 0)?;
        if r0 >> 28 == MSG_TYPE_ERR || r1 & INVALID_BIT != 0 {
            return None;
        }
        // Het tekenbit is bit 8 (xgene TEMP_NEGATIVE_BIT).
        let t = ((r1 << 23) as i32) >> 23;
        Some(t * 1000)
    }

    /// Eén synchroon bericht: kop en payload in het gedeelde geheugen,
    /// doorbell, pollen op CMD_COMPLETE.
    fn call(&mut self, m0: u32, m1: u32, m2: u32) -> Option<(u32, u32)> {
        if self.pcc.shmem.0 == 0 || self.pcc.shmem_len < SHMEM_MIN {
            return None;
        }
        let s = self.shm();
        let complete = ST_CMD_COMPLETE << 16;
        if self.pending && s.cmd_status.read() & complete == 0 {
            return None;
        }
        s.signature.write(PCC_SIGNATURE | self.ch);
        // Command en status in één gealigneerde schrijf: het commandtype
        // (bits 31:28 van het bericht), CMD_COMPLETE gewist, de overige
        // statusbits bewaard.
        let keep = s.cmd_status.read() & !(0xffff | complete);
        s.cmd_status.write(keep | (m0 >> 28) | CMD_GEN_DB_INT);
        for (reg, w) in s.msg.iter().zip([m0, m1, m2]) {
            reg.write(w);
        }
        self.pending = true;
        dev::mb();
        self.ring();

        // Linux budgetteert 500 maal de PCCT-latentie; hetzelfde, met een
        // vloer.
        let budget = (u64::from(self.pcc.latency_us) * 500_000).max(BUDGET_FLOOR_NS);
        if !dev::poll_until(self.clock, budget, || s.cmd_status.read() & complete != 0) {
            return None;
        }
        self.pending = false;
        dev::mb();
        let [a, b, _] = &s.msg;
        Some((a.read(), b.read()))
    }

    /// De doorbell: read-modify-write met de PCCT-maskers (de pcc.c-
    /// semantiek). De Altra meldt een 32-bit register; 64 voor de
    /// volledigheid van de GAS.
    fn ring(&self) {
        let db = self.pcc.doorbell;
        if self.pcc.doorbell_width == 64 {
            dev::write64(db, (dev::read64(db) & self.pcc.preserve) | self.pcc.write);
        } else {
            let v = (u64::from(dev::read32(db)) & self.pcc.preserve) | self.pcc.write;
            dev::write32(db, v as u32);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::cell::RefCell;
    use std::vec;
    use std::vec::Vec;

    thread_local! {
        /// De nep-SMpro: shmem en doorbell, en de temperatuur die hij meldt.
        static HW: RefCell<(Pa, Pa, u32, bool)> = const { RefCell::new((Pa(0), Pa(0), 0, true)) };
    }

    /// De klok is de SMpro: staat de bel, dan antwoordt hij.
    fn clock() -> u64 {
        HW.with(|h| {
            let (shm, db, temp, alive) = *h.borrow();
            if alive && shm.0 != 0 && dev::read32(db) & 1 != 0 {
                dev::write32(db, 0);
                assert_eq!(dev::read32(shm), PCC_SIGNATURE | HWMON_CHANNEL);
                assert_eq!(dev::read32(shm.add(8)), SENSOR_RD_MSG);
                dev::write32(shm.add(12), temp);
                let st = dev::read32(shm.add(4));
                dev::write32(shm.add(4), st | (ST_CMD_COMPLETE << 16));
            }
            1_000_000
        }) + CLOCK.with(|c| {
            *c.borrow_mut() += 1_000_000;
            *c.borrow()
        })
    }

    thread_local! {
        static CLOCK: RefCell<u64> = const { RefCell::new(0) };
    }

    fn dev_with(temp: u32, alive: bool) -> (Smpro, std::vec::Vec<u64>) {
        let mut mem = vec![0u64; 8];
        let shm = Pa(mem.as_mut_ptr() as usize as u64);
        let db = shm.add(32);
        HW.with(|h| *h.borrow_mut() = (shm, db, temp, alive));
        let pcc = Pcc {
            shmem: shm,
            shmem_len: 20,
            doorbell: db,
            doorbell_width: 32,
            preserve: 0,
            write: 1,
            latency_us: 10,
        };
        // SAFETY: shmem en doorbell liggen in `mem`, dat de test overleeft.
        (unsafe { Smpro::new(HWMON_CHANNEL, pcc, clock) }, mem)
    }

    #[test]
    fn reads_the_soc_temperature() {
        let (mut d, _m) = dev_with(47, true);
        assert_eq!(d.soc_temp_milli_c(), Some(47_000));
        // Negatief: 9 bits two's-complement.
        let (mut d, _m) = dev_with(0x1ff, true);
        assert_eq!(d.soc_temp_milli_c(), Some(-1000));
        let (mut d, _m) = dev_with(INVALID_BIT | 40, true);
        assert_eq!(d.soc_temp_milli_c(), None);
    }

    /// `smpro_test.go`: een onafgemaakt commando laat de buffer van de
    /// firmware.
    #[test]
    fn a_pending_command_leaves_the_buffer_to_the_firmware() {
        let (mut d, mem) = dev_with(47, false);
        assert_eq!(d.soc_temp_milli_c(), None, "no answer");
        assert!(d.pending);
        let shm = Pa(mem.as_ptr() as usize as u64);
        dev::write32(shm, 0x1234);
        dev::write32(shm.add(8), 0x5678);
        assert_eq!(d.soc_temp_milli_c(), None);
        assert_eq!(
            dev::read32(shm),
            0x1234,
            "firmware-owned buffer overwritten"
        );
        assert_eq!(dev::read32(shm.add(8)), 0x5678);
        // Het platform maakt het af: dan mag het volgende bericht erin.
        HW.with(|h| h.borrow_mut().3 = true);
        let st = dev::read32(shm.add(4));
        dev::write32(shm.add(4), st | (ST_CMD_COMPLETE << 16));
        assert_eq!(d.soc_temp_milli_c(), Some(47_000));
    }

    /// Eén type-`typ`-subkanaal van 62 bytes met herkenbare velden.
    fn subspace(typ: u8, shmem: u64, db: u64, preserve: u64, write: u64, lat: u32) -> Vec<u8> {
        let mut e = vec![0u8; 62];
        e[0] = typ;
        e[1] = 62;
        e[8..16].copy_from_slice(&shmem.to_le_bytes());
        e[16..24].copy_from_slice(&0x100u64.to_le_bytes());
        e[25] = 32;
        e[28..36].copy_from_slice(&db.to_le_bytes());
        e[36..44].copy_from_slice(&preserve.to_le_bytes());
        e[44..52].copy_from_slice(&write.to_le_bytes());
        e[52..56].copy_from_slice(&lat.to_le_bytes());
        e
    }

    /// `pcct_test.go`: de ordinale nummering, de offsets uit de spec, en
    /// nette afwijzing van ontbrekende indexen en extended types.
    #[test]
    fn pcct_subspaces_are_counted_in_order() {
        let mut t = vec![0u8; 48];
        t.extend(subspace(2, 0x8860_0000, 0x1000_0054_0010, !1, 1, 500));
        t.extend(subspace(1, 0x8860_1000, 0x1000_0054_0020, 0, 0x53, 100));
        t.extend(subspace(3, 0xdead, 0xbeef, 0, 0, 0));
        let p = pcc_from(&t, 0).unwrap();
        assert_eq!(
            p,
            Pcc {
                shmem: Pa(0x8860_0000),
                shmem_len: 0x100,
                doorbell: Pa(0x1000_0054_0010),
                doorbell_width: 32,
                preserve: !1,
                write: 1,
                latency_us: 500,
            }
        );
        let p = pcc_from(&t, 1).unwrap();
        assert_eq!((p.shmem, p.write), (Pa(0x8860_1000), 0x53));
        assert_eq!(pcc_from(&t, 2), None, "extended type");
        assert_eq!(pcc_from(&t, 9), None);
        assert_eq!(pcc_from(&[], 0), None);
        let mut broken = vec![0u8; 48];
        broken.extend([2, 200]);
        assert_eq!(pcc_from(&broken, 0), None);
    }

    #[test]
    fn a_short_channel_is_refused() {
        let (mut d, _m) = dev_with(47, true);
        d.pcc.shmem_len = 8;
        assert_eq!(d.soc_temp_milli_c(), None);
    }
}
