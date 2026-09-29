//! Blok 0 van een EDID (VESA E-EDID 1.3/1.4): de header, de checksum, de
//! fabrikant en de voorkeursmodus (de eerste detailed timing descriptor).
//!
//! Klein met opzet: de keten drijft 1080p60 vast, want de framebuffer in
//! het plan heeft die maat. De EDID zegt alleen of de sink dat ook wil;
//! een monitor die iets anders prefereert, krijgt een regel in de log en
//! toch 1080p60 (in Go bestond deze lezer niet, en daar was een monitor
//! die 1080p60 niet kon "buiten bereik van deze eerste versie").

use core::fmt;

/// De maat van één EDID-blok.
pub const BLOCK: usize = 128;
/// De vaste header van blok 0.
const HEADER: [u8; 8] = [0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00];
/// De eerste detailed timing descriptor: de voorkeursmodus (EDID 1.3 en
/// later: "preferred timing mode" staat altijd aan in blok 0).
const DTD0: usize = 54;
/// De maat van een descriptor.
const DTD_LEN: usize = 18;

/// Waarom er geen bruikbare EDID is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// De sink antwoordde niet op de DDC (geen sink, of de bus is dood).
    Nack {
        /// Het byte waar het misging.
        byte: u8,
    },
    /// De DDC-master meldde geen done en geen error binnen de grens.
    Timeout {
        /// Het byte waar het misging.
        byte: u8,
    },
    /// De eerste acht bytes zijn niet `00 FF FF FF FF FF FF 00`.
    Header,
    /// De 128 bytes tellen niet op tot nul (mod 256).
    Checksum {
        /// De som.
        sum: u8,
    },
    /// De eerste descriptor is geen timing (pixelklok nul) of is leeg.
    NoTiming,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Nack { byte } => write!(f, "edid: no answer on the DDC at byte {byte}"),
            Self::Timeout { byte } => write!(f, "edid: DDC timeout at byte {byte}"),
            Self::Header => f.write_str("edid: bad header"),
            Self::Checksum { sum } => write!(f, "edid: checksum {sum:#04x}, want 0"),
            Self::NoTiming => f.write_str("edid: no preferred detailed timing"),
        }
    }
}

/// Een videomodus zoals een detailed timing descriptor hem geeft.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Mode {
    /// De pixelklok in kHz.
    pub pixel_khz: u32,
    /// Actieve pixels per regel.
    pub hdisplay: u16,
    /// Het begin van de hsync.
    pub hsync_start: u16,
    /// Het einde van de hsync.
    pub hsync_end: u16,
    /// De hele regel.
    pub htotal: u16,
    /// Actieve regels.
    pub vdisplay: u16,
    /// Het begin van de vsync.
    pub vsync_start: u16,
    /// Het einde van de vsync.
    pub vsync_end: u16,
    /// Het hele beeld.
    pub vtotal: u16,
    /// Geïnterlinieerd.
    pub interlaced: bool,
    /// Positieve hsync.
    pub hsync_pos: bool,
    /// Positieve vsync.
    pub vsync_pos: bool,
}

impl Mode {
    /// CEA VIC 16, de modus die de keten drijft.
    pub const CEA_1080P60: Mode = Mode {
        pixel_khz: crate::PIXEL_KHZ,
        hdisplay: crate::H_DISPLAY as u16,
        hsync_start: crate::H_SYNC_START as u16,
        hsync_end: crate::H_SYNC_END as u16,
        htotal: crate::H_TOTAL as u16,
        vdisplay: crate::V_DISPLAY as u16,
        vsync_start: crate::V_SYNC_START as u16,
        vsync_end: crate::V_SYNC_END as u16,
        vtotal: crate::V_TOTAL as u16,
        interlaced: false,
        hsync_pos: true,
        vsync_pos: true,
    };

    /// De beeldfrequentie in Hz, afgerond.
    #[must_use]
    pub fn refresh_hz(&self) -> u32 {
        let dots = u64::from(self.htotal) * u64::from(self.vtotal);
        if dots == 0 {
            return 0;
        }
        let mhz = u64::from(self.pixel_khz) * 1_000_000 / dots;
        (mhz.saturating_add(500) / 1000) as u32
    }

    /// Is dit de modus die de keten drijft?
    #[must_use]
    pub fn is_driven(&self) -> bool {
        *self == Self::CEA_1080P60
    }
}

impl fmt::Display for Mode {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{}x{}{}{}",
            self.hdisplay,
            self.vdisplay,
            if self.interlaced { 'i' } else { 'p' },
            self.refresh_hz()
        )
    }
}

/// Wat blok 0 zegt.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Info {
    /// De PNP-fabrikantcode, drie letters (bijvoorbeeld `DEL`).
    pub vendor: [u8; 3],
    /// De productcode.
    pub product: u16,
    /// De voorkeursmodus.
    pub preferred: Mode,
}

impl Info {
    /// De fabrikantcode als tekst (`???` als hij geen letters is).
    #[must_use]
    pub fn vendor_str(&self) -> &str {
        core::str::from_utf8(&self.vendor).unwrap_or("???")
    }
}

/// Toetst blok 0 en haalt fabrikant en voorkeursmodus eruit.
pub fn parse(b: &[u8; BLOCK]) -> Result<Info, Error> {
    if b[..8] != HEADER {
        return Err(Error::Header);
    }
    let sum = b.iter().fold(0u8, |s, &x| s.wrapping_add(x));
    if sum != 0 {
        return Err(Error::Checksum { sum });
    }
    // Fabrikant: drie letters van vijf bits, big-endian, 'A' = 1.
    let id = u16::from_be_bytes([b[8], b[9]]);
    let letter = |shift: u16| {
        let v = ((id >> shift) & 0x1F) as u8;
        if (1..=26).contains(&v) {
            b'A' + v - 1
        } else {
            b'?'
        }
    };
    let preferred = timing(&b[DTD0..DTD0 + DTD_LEN]).ok_or(Error::NoTiming)?;
    Ok(Info {
        vendor: [letter(10), letter(5), letter(0)],
        product: u16::from_le_bytes([b[10], b[11]]),
        preferred,
    })
}

/// Ontleedt één detailed timing descriptor; `None` als het geen timing is.
fn timing(d: &[u8]) -> Option<Mode> {
    let d: &[u8; DTD_LEN] = d.try_into().ok()?;
    let clk = u16::from_le_bytes([d[0], d[1]]);
    if clk == 0 {
        return None;
    }
    let lo_hi = |lo: u8, hi: u8| u16::from(lo) | (u16::from(hi) << 8);
    let hact = lo_hi(d[2], d[4] >> 4);
    let hblank = lo_hi(d[3], d[4] & 0xF);
    let vact = lo_hi(d[5], d[7] >> 4);
    let vblank = lo_hi(d[6], d[7] & 0xF);
    let hso = lo_hi(d[8], (d[11] >> 6) & 0x3);
    let hsw = lo_hi(d[9], (d[11] >> 4) & 0x3);
    let vso = u16::from(d[10] >> 4) | (u16::from((d[11] >> 2) & 0x3) << 4);
    let vsw = u16::from(d[10] & 0xF) | (u16::from(d[11] & 0x3) << 4);
    if hact == 0 || vact == 0 {
        return None;
    }
    let flags = d[17];
    // Bits 4..3 = 0b11: digital separate sync; alleen daar betekenen bit 2
    // en 1 de polariteit van vsync en hsync.
    let separate = flags & 0x18 == 0x18;
    Some(Mode {
        pixel_khz: u32::from(clk) * 10,
        hdisplay: hact,
        hsync_start: hact + hso,
        hsync_end: hact + hso + hsw,
        htotal: hact + hblank,
        vdisplay: vact,
        vsync_start: vact + vso,
        vsync_end: vact + vso + vsw,
        vtotal: vact + vblank,
        interlaced: flags & 0x80 != 0,
        hsync_pos: separate && flags & 0x02 != 0,
        vsync_pos: separate && flags & 0x04 != 0,
    })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;

    /// Een EDID van een 1080p60-monitor ("DEL", product 0xA0B1), met de
    /// CEA-861-descriptor van VIC 16 zoals elke 1080p-monitor hem draagt.
    pub(crate) fn monitor_1080p60() -> [u8; BLOCK] {
        let mut b = [0u8; BLOCK];
        b[..8].copy_from_slice(&HEADER);
        // D = 4, E = 5, L = 12: 00100 00101 01100.
        let id: u16 = (4 << 10) | (5 << 5) | 12;
        b[8..10].copy_from_slice(&id.to_be_bytes());
        b[10..12].copy_from_slice(&0xA0B1u16.to_le_bytes());
        b[18] = 1; // versie 1.4
        b[19] = 4;
        b[DTD0..DTD0 + DTD_LEN].copy_from_slice(&[
            0x02, 0x3A, 0x80, 0x18, 0x71, 0x38, 0x2D, 0x40, 0x58, 0x2C, 0x45, 0x00, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x1E,
        ]);
        fix_checksum(&mut b);
        b
    }

    pub(crate) fn fix_checksum(b: &mut [u8; BLOCK]) {
        b[127] = 0;
        let sum = b.iter().fold(0u8, |s, &x| s.wrapping_add(x));
        b[127] = sum.wrapping_neg();
    }

    #[test]
    fn a_1080p60_monitor_parses_to_the_driven_mode() {
        let info = parse(&monitor_1080p60()).unwrap();
        assert_eq!(info.vendor_str(), "DEL");
        assert_eq!(info.product, 0xA0B1);
        assert_eq!(info.preferred, Mode::CEA_1080P60);
        assert!(info.preferred.is_driven());
        assert_eq!(info.preferred.refresh_hz(), 60);
        assert_eq!(std::format!("{}", info.preferred), "1920x1080p60");
    }

    #[test]
    fn header_and_checksum_are_checked() {
        let mut b = monitor_1080p60();
        b[3] = 0;
        assert_eq!(parse(&b), Err(Error::Header));
        let mut b = monitor_1080p60();
        b[100] ^= 1;
        assert!(matches!(parse(&b), Err(Error::Checksum { .. })));
    }

    #[test]
    fn a_display_descriptor_first_is_no_timing() {
        let mut b = monitor_1080p60();
        b[DTD0] = 0;
        b[DTD0 + 1] = 0;
        fix_checksum(&mut b);
        assert_eq!(parse(&b), Err(Error::NoTiming));
    }

    #[test]
    fn a_1440p_preference_is_not_the_driven_mode() {
        // 2560x1440@59.95 CVT-RB: 241,5 MHz, hblank 160, vblank 41.
        let mut b = monitor_1080p60();
        b[DTD0..DTD0 + DTD_LEN].copy_from_slice(&[
            0x56, 0x5E, 0x00, 0xA0, 0xA0, 0xA0, 0x29, 0x50, 0x30, 0x20, 0x35, 0x00, 0x00, 0x00,
            0x00, 0x00, 0x00, 0x1A,
        ]);
        fix_checksum(&mut b);
        let m = parse(&b).unwrap().preferred;
        assert_eq!(
            (m.hdisplay, m.vdisplay, m.htotal, m.vtotal),
            (2560, 1440, 2720, 1481)
        );
        assert_eq!(m.pixel_khz, 241_500);
        assert_eq!(m.refresh_hz(), 60);
        assert!(m.hsync_pos && !m.vsync_pos);
        assert!(!m.is_driven());
    }
}
