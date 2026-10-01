//! De staging: het image dat een lader vóór de boot neerlegde (QEMU's
//! `-device loader`, de UEFI-stub, m1n1), en de rol ervan. Waar het ligt en
//! waar de rol vandaan komt (een woord, de cmdline, een gebakken image) is
//! per board; de vorm is hier één keer.

/// Wat het gestagede image is: welke weg de kern ermee gaat.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum StagedRole {
    /// Een gewone app: de kern plaatst hem zelf, twee keer (het ABI-bewijs).
    App,
    /// Hop: de kern plaatst hem één keer, in slot 1, met de bevoegdheid.
    Hop,
}

/// De rol bij een rolwoord: 0 = app, 1 = Hop. Een ander woord komt rauw
/// terug, zodat de kern het luid kan noemen (en niets plaatst).
pub const fn role(word: u64) -> Result<StagedRole, u64> {
    match word {
        0 => Ok(StagedRole::App),
        1 => Ok(StagedRole::Hop),
        w => Err(w),
    }
}

/// Het rolwoord van een `hopos.stage`-waarde uit een config of cmdline:
/// leeg of `hop` is Hop (de standaard), `app` het ABI-bewijs van appspike,
/// de rest onbekend (2).
#[must_use]
pub fn role_code(v: &str) -> u64 {
    match v {
        "" | "hop" => 1,
        "app" => 0,
        _ => 2,
    }
}

/// De rol uit het woord op `pa` (zie [`role`]). Op de host app.
pub fn role_at(pa: u64) -> Result<StagedRole, u64> {
    role(imp::word(pa))
}

/// Het image op `at` met zijn maat in het woord op `hdr`, of `None` als die
/// maat nul is of boven `max` ligt. Alleen de maat wordt hier getoetst; de
/// inhoud is onvertrouwd en gaat door de ELF-lezer en `abi::place`.
///
/// De aanroeper geeft een staging die de identity map als Normal-RAM mapt,
/// buiten de kern-RAM en de pool, waar na de lader niemand meer schrijft.
/// Op de host altijd `None`.
#[must_use]
pub fn staged_at(hdr: u64, at: u64, max: u64) -> Option<&'static [u8]> {
    let size = imp::word(hdr);
    if size == 0 || size > max {
        return None;
    }
    imp::slice(at, usize::try_from(size).ok()?)
}

#[cfg(target_os = "none")]
mod imp {
    pub(super) fn word(pa: u64) -> u64 {
        dev::read64(dev::Pa(pa))
    }

    pub(super) fn slice(at: u64, len: usize) -> Option<&'static [u8]> {
        // SAFETY: `[at, at + len)` ligt binnen de staging van het board
        // (`len <= max`, net getoetst in `staged_at`): RAM dat de identity
        // map als Normal mapt, buiten de pool en de kern-RAM, dus niemand
        // schrijft erin nadat de lader het vulde. Alleen lezen.
        Some(unsafe { core::slice::from_raw_parts(at as usize as *const u8, len) })
    }
}

#[cfg(not(target_os = "none"))]
mod imp {
    //! Host-stub: er is geen lader die iets neerlegde.
    pub(super) fn word(_pa: u64) -> u64 {
        0
    }

    pub(super) fn slice(_at: u64, _len: usize) -> Option<&'static [u8]> {
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_role_word() {
        assert_eq!(role(0), Ok(StagedRole::App));
        assert_eq!(role(1), Ok(StagedRole::Hop));
        assert_eq!(role(7), Err(7));
        assert_eq!(role_code(""), 1);
        assert_eq!(role_code("hop"), 1);
        assert_eq!(role_code("app"), 0);
        assert_eq!(role(role_code("hopp")), Err(2));
    }
}
