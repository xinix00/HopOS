//! De I2C-controller van de RK3566 (`i2c@fdd40000`, `rockchip,rk3568-i2c`)
//! gepold, met precies twee handelingen: één register lezen en één register
//! schrijven. Meer vraagt de spanning van de cores niet (`clock`).
//!
//! Dit bezit alleen de registers van het blok. Klok, pinnen en de deler
//! (`CLKDIV`) zijn van U-Boot: die praat over deze bus met de PMIC en met
//! vdd_cpu (de DTB van het board: `regulator-init-microvolt` op beide), dus
//! na `booti` staat alles open en op 100 kHz. Een flip raakt het blok niet.
//!
//! Afgekeken van Linux `drivers/i2c/busses/i2c-rk3x.c` (`rk3x_i2c_setup`:
//! de gecombineerde schrijf-lees in `REG_CON_MOD_REGISTER_TX`,
//! `rk3x_i2c_start`, `rk3x_i2c_prepare_read`, `rk3x_i2c_fill_transmit_buf`,
//! `rk3x_i2c_stop`), het pollen van U-Boot `drivers/i2c/rk_i2c.c`
//! (`rk_i2c_send_start_bit`, de wachtlussen op `IPD`).

use dev::Pa;

/// I2C0: de PMIC (RK817 op 0x20) en vdd_cpu (0x40).
pub const I2C0: Pa = Pa(0xFDD4_0000);

const CON: u64 = 0x00;
const CLKDIV: u64 = 0x04;
/// Het doeladres voor de lees na het registeradres.
const MRXADDR: u64 = 0x08;
/// Het registeradres dat de controller eerst schrijft.
const MRXRADDR: u64 = 0x0c;
/// Schrijven: zoveel bytes uit TXDATA; de schrijf start de overdracht.
const MTXCNT: u64 = 0x10;
/// Lezen: zoveel bytes naar RXDATA; de schrijf start de overdracht.
const MRXCNT: u64 = 0x14;
const IEN: u64 = 0x18;
/// Wat er gebeurd is; schrijf een bit om hem te wissen.
const IPD: u64 = 0x1c;
const TXDATA0: u64 = 0x100;
const RXDATA0: u64 = 0x200;

const CON_EN: u32 = 1 << 0;
/// `REG_CON_MOD_TX`: alleen schrijven.
const MOD_TX: u32 = 0 << 1;
/// `REG_CON_MOD_REGISTER_TX`: adres en register schrijven, herstart, lezen.
const MOD_REGISTER_TX: u32 = 1 << 1;
const CON_START: u32 = 1 << 3;
const CON_STOP: u32 = 1 << 4;
/// Een NACK na de laatste gelezen byte.
const CON_LASTACK: u32 = 1 << 5;
/// Stop bij een NACK van het apparaat.
const CON_ACTACK: u32 = 1 << 6;
/// De timing-bits die Linux bij elke schrijf van `CON` bewaart.
const CON_TUNING: u32 = 0xff00;
/// `REG_MRXADDR_VALID(0)`: byte 0 van MRX(R)ADDR telt.
const VALID0: u32 = 1 << 24;

const INT_MBTF: u32 = 1 << 2;
const INT_MBRF: u32 = 1 << 3;
const INT_START: u32 = 1 << 4;
const INT_STOP: u32 = 1 << 5;
const INT_NAK: u32 = 1 << 6;
const INT_ALL: u32 = 0x7f;

/// Hoe lang één stap mag duren. Drie bytes op 100 kHz zijn 0,3 ms; U-Boot
/// geeft 100 ms, maar dit draait op de OS-core en een dode bus moet snel
/// een fout zijn.
const STEP_NS: u64 = 10_000_000;

/// Waarom een overdracht mislukte.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    /// Een stap kwam niet binnen [`STEP_NS`]: geen klok, geen pinnen, of
    /// een bus die laag hangt.
    Timeout {
        /// Welke stap (`start`, `read`, `write`, `stop`).
        step: &'static str,
        /// `IPD` op dat moment.
        ipd: u32,
        /// `CON`.
        con: u32,
    },
    /// Het apparaat antwoordde met een NACK: niets op dat adres.
    Nak {
        /// Het 7-bit adres.
        addr: u8,
        /// Het register.
        reg: u8,
    },
}

impl core::fmt::Display for Error {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match *self {
            Self::Timeout { step, ipd, con } => {
                write!(f, "i2c0 timeout in {step} (ipd {ipd:#x}, con {con:#x})")
            }
            Self::Nak { addr, reg } => {
                write!(f, "i2c0 nack from {addr:#04x} at register {reg:#04x}")
            }
        }
    }
}

/// De `Result` van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Het woord in `TXDATA0` voor één registerschrijf: adres met schrijfbit,
/// register, waarde (`rk3x_i2c_fill_transmit_buf`: het adres gaat als
/// eerste byte mee).
#[must_use]
pub const fn write_word(addr: u8, reg: u8, val: u8) -> u32 {
    ((addr as u32 & 0x7f) << 1) | ((reg as u32) << 8) | ((val as u32) << 16)
}

/// `MRXADDR` en `MRXRADDR` voor de gecombineerde schrijf-lees van één
/// register (`rk3x_i2c_setup`, het pad `msgs[0].len < 4`): het adres zonder
/// leesbit, de controller zet het zelf na de herstart.
#[must_use]
pub const fn read_words(addr: u8, reg: u8) -> (u32, u32) {
    (((addr as u32 & 0x7f) << 1) | VALID0, reg as u32 | VALID0)
}

/// De controller. Eén eigenaar: de klokknop op de OS-core.
pub struct I2c {
    base: Pa,
}

impl I2c {
    /// De controller op `base`.
    ///
    /// # Safety
    ///
    /// `base` is het gemapte registerblok van een rk3x-controller met klok
    /// en pinnen aan, en niemand anders gebruikt de bus.
    #[must_use]
    pub unsafe fn new(base: Pa) -> Self {
        Self { base }
    }

    fn r(&self, off: u64) -> u32 {
        dev::read32(self.base.add(off))
    }

    fn w(&self, off: u64, v: u32) {
        dev::write32(self.base.add(off), v);
    }

    /// De deler die U-Boot zette, voor de bootregel.
    #[must_use]
    pub fn clkdiv(&self) -> u32 {
        self.r(CLKDIV)
    }

    /// Leest register `reg` van apparaat `addr`.
    pub fn read(&mut self, addr: u8, reg: u8) -> Result<u8> {
        let (a, r) = read_words(addr, reg);
        self.w(MRXADDR, a);
        self.w(MRXRADDR, r);
        let con = self.start(MOD_REGISTER_TX)?;
        self.w(IEN, INT_MBRF | INT_NAK);
        self.w(CON, con | CON_LASTACK);
        self.w(MRXCNT, 1);
        let done = self.wait(INT_MBRF, "read", addr, reg);
        let v = self.r(RXDATA0) as u8;
        self.stop(con);
        done.map(|()| v)
    }

    /// Schrijft `val` in register `reg` van apparaat `addr`.
    pub fn write(&mut self, addr: u8, reg: u8, val: u8) -> Result {
        let con = self.start(MOD_TX)?;
        self.w(IEN, INT_MBTF | INT_NAK);
        self.w(CON, con);
        self.w(TXDATA0, write_word(addr, reg, val));
        self.w(MTXCNT, 3);
        let done = self.wait(INT_MBTF, "write", addr, reg);
        self.stop(con);
        done
    }

    /// De START, en het `CON`-woord voor de rest van de overdracht.
    fn start(&mut self, mode: u32) -> Result<u32> {
        let con = (self.r(CON) & CON_TUNING) | CON_EN | mode | CON_ACTACK;
        self.w(IPD, INT_ALL);
        self.w(IEN, INT_START);
        self.w(CON, con | CON_START);
        if let Err(e) = self.wait(INT_START, "start", 0, 0) {
            self.stop(con);
            return Err(e);
        }
        Ok(con)
    }

    /// De STOP, daarna de controller uit (de tuning-bits blijven).
    fn stop(&mut self, con: u32) {
        self.w(IPD, INT_ALL);
        self.w(IEN, INT_STOP);
        self.w(CON, con | CON_STOP);
        let _ = self.wait(INT_STOP, "stop", 0, 0);
        self.w(IPD, INT_ALL);
        self.w(IEN, 0);
        self.w(CON, con & CON_TUNING);
    }

    /// Wacht op `bit` in `IPD` en wist hem; een NACK eerst.
    fn wait(&self, bit: u32, step: &'static str, addr: u8, reg: u8) -> Result {
        let until = cpu::idle::now().saturating_add(STEP_NS);
        loop {
            let ipd = self.r(IPD);
            if ipd & INT_NAK != 0 {
                self.w(IPD, INT_NAK);
                return Err(Error::Nak { addr, reg });
            }
            if ipd & bit != 0 {
                self.w(IPD, bit);
                return Ok(());
            }
            if cpu::idle::now() >= until {
                return Err(Error::Timeout {
                    step,
                    ipd,
                    con: self.r(CON),
                });
            }
            core::hint::spin_loop();
        }
    }
}
