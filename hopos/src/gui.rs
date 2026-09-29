//! Het gui-vlak van de kern-binary: de console op het glas, de meetregels
//! naast de bunny, de USB-invoer na het netwerk, en de framebuffer-grant
//! aan de display-app (Go: `cmd/hopos/gui.go`, `usbinput.go` en de
//! `fb.Init`/`screenStatus` uit de main).
//!
//! Alleen met de feature `gui` staat hier iets; kaal is elke functie een
//! no-op met dezelfde signatuur (handboek §7: `cfg` op module-niveau), en
//! linkt de binary geen regel display-code. Go zei op 06-08 "geen dood
//! gewicht": 183 KB voor de hele gui-smaak op de Radxa, en een headless
//! node draagt er niets van.
//!
//! Eigendom:
//!
//! - de [`driver_fb::Console`] staat in een `LocalCell` op de OS-core. De
//!   console-haak leent hem per stuk tekst (onder het console-slot van
//!   `cpu`), de meettaak per seconde; een lening die al loopt (een
//!   noodregel uit exception-context midden in een regel) slaat het glas
//!   over, de UART krijgt alles. Een andere core tekent nooit: hij zou een
//!   `Local` van deze core aanraken.
//! - de framebuffer-grant ([`gui_fbgrant::FbGrant`]) is van de
//!   lifecycle-actor, via [`GuiGrants`] (`kern::grants::Grants`).

#[cfg(feature = "gui")]
pub(crate) use on::*;

#[cfg(not(feature = "gui"))]
pub(crate) use off::*;

#[cfg(not(feature = "gui"))]
mod off {
    //! Kaal gebouwd: headless, en het glas gaat nooit weg.

    use executor::Executor;

    /// Niets te onthouden.
    pub(crate) fn keep_uart(_uart: fn(&[u8])) {}

    /// Geen framebuffer-console.
    pub(crate) fn init_framebuffer_console(_board: &'static crate::Machine) {}

    /// Geen meetregels.
    pub(crate) fn start_screen_status(_exec: &'static Executor) {}

    /// Geen USB-invoer.
    pub(crate) fn start_usb_input(_exec: &'static Executor) {}

    /// De grant-aanbieder van de lifecycle: geen. Een job die om het glas
    /// vraagt, draait headless.
    pub(crate) fn slot_grants() -> kern::grants::NoGrants {
        kern::grants::NoGrants
    }
}

#[cfg(feature = "gui")]
mod on {
    use board::Board;
    use core::fmt::{self, Write as _};
    use core::sync::atomic::{AtomicPtr, AtomicUsize, Ordering::Acquire, Ordering::Release};
    use cpu::println;
    use driver_fb::{Console, Desc};
    use executor::Executor;
    use gui_fbgrant::FbGrant;
    use sync::LocalCell;

    /// De console op het glas. Alleen de OS-core raakt hem aan.
    static GLASS: LocalCell<Console> = LocalCell::cell(Console::new());
    /// De core die het glas bezit (de OS-core bij de init); `usize::MAX` =
    /// geen glas.
    static GLASS_CORE: AtomicUsize = AtomicUsize::new(usize::MAX);
    /// De UART-haak van het board, zoals `main` hem zette: de tee schrijft
    /// eerst daarheen. Null = nog geen tee.
    static UART: AtomicPtr<()> = AtomicPtr::new(core::ptr::null_mut());

    /// De framebuffer-grant van deze node.
    static GRANT: LocalCell<FbGrant> = LocalCell::cell(FbGrant::new());

    /// De kopregels: de bunny van `main`, zonder de lege scheidingsregel.
    /// De meetregels komen rechts op de eerste drie.
    const HEADER_ROWS: usize = 4;

    /// Zet de console op het glas als het board een framebuffer geeft:
    /// schone lei, de bunny als vaste kop, en vanaf nu gaat elke logregel
    /// naar de UART én het scherm. Geen framebuffer is geen fout: één regel.
    pub(crate) fn init_framebuffer_console(board: &'static crate::Machine) {
        if UART.load(Acquire).is_null() {
            return; // Zonder UART-haak geen tee: dan blijft het bij de UART.
        }
        let Some(d) = board.framebuffer() else {
            println!("fb: no framebuffer on this board, console on the UART only HOPOS_FB_NONE");
            return;
        };
        normal_nc(&d);
        let r = GLASS.try_borrow_mut().map(|mut c| {
            c.init(d).map(|()| {
                let mut lines = [""; HEADER_ROWS];
                for (l, b) in lines.iter_mut().zip(crate::BUNNY.iter()) {
                    *l = b;
                }
                c.header(&lines);
                c.cells()
            })
        });
        match r {
            Ok(Ok((cols, rows))) => {
                GLASS_CORE.store(board.this_core(), Release);
                cpu::console::set_sink(tee);
                if let Ok(mut g) = GRANT.try_borrow_mut()
                    && let Err(e) = g.offer(d)
                {
                    println!("fb: not offered to a display app: {e} HOPOS_FB_ENV");
                }
                println!(
                    "fb: console on {}x{} stride {} @ {:#x} ({cols}x{rows} cells) HOPOS_FB_CONSOLE",
                    d.width, d.height, d.stride, d.base.0
                );
            }
            Ok(Err(e)) => println!("fb: framebuffer refused: {e} HOPOS_FB_NONE"),
            Err(_) => println!("fb: console busy at init HOPOS_FB_NONE"),
        }
    }

    /// Het vlak is DRAM, geen registerblok: laat het fabric de pixelstores
    /// gatheren in plaats van ze als losse Device-transacties te sturen
    /// (`cpu::memattr`). Weigert de map (een venster dat niet op 2 MB
    /// valt), dan tekenen we zoals het board hem mapte; stil in Go, hier
    /// één regel, want het is een optimalisatie en geen voorwaarde.
    fn normal_nc(d: &Desc) {
        let Some(size) = d.size() else { return };
        if let Err(e) = cpu::memattr::normal_nc(d.base.0, size) {
            println!("fb: left as the board mapped it ({e})");
        }
    }

    /// Onthoudt de UART-haak die `main` van het board kreeg (in `kmain`,
    /// vóór een verhuizing naar de OS-core): de tee schrijft eerst
    /// daarheen. `console()` een tweede keer vragen zou de UART midden in
    /// de boot opnieuw opzetten.
    pub(crate) fn keep_uart(uart: fn(&[u8])) {
        UART.store(uart as *mut (), Release);
    }

    /// De console-haak in de gui-smaak: alles naar de UART, en op de
    /// OS-core ook naar het glas.
    fn tee(b: &[u8]) {
        let p = UART.load(Acquire);
        if !p.is_null() {
            // SAFETY: `UART` wordt alleen door `tee_on` geschreven, met een
            // geldige `fn(&[u8])`; een functiepointer en een datapointer
            // zijn op onze targets even groot, en null is uitgesloten
            // (zelfde vorm als `cpu::console::sink`).
            let uart = unsafe { core::mem::transmute::<*mut (), fn(&[u8])>(p) };
            uart(b);
        }
        if GLASS_CORE.load(Acquire) != crate::BOARD.this_core() {
            return;
        }
        if let Ok(mut c) = GLASS.try_borrow_mut() {
            c.write(b);
        }
    }

    /// Start de meetregels rechts naast de bunny: kern-heap als percentage,
    /// datum en tijd met seconden, elke seconde. Een bevroren klok verraadt
    /// zo een hangende kern meteen (Derek, 15-07).
    pub(crate) fn start_screen_status(exec: &'static Executor) {
        if GLASS_CORE.load(Acquire) == usize::MAX {
            return;
        }
        if exec.spawn(screen_status(exec)).is_err() {
            println!("fb: screen status task not spawned HOPOS_FB_STATUS_FAIL");
        }
    }

    async fn screen_status(exec: &'static Executor) {
        let start = exec.now();
        let mut n: u64 = 0;
        loop {
            let h = crate::HEAP.stats();
            let total = h.used.saturating_add(h.free).max(1);
            let wall = crate::clock::offset().saturating_add(exec.now()) / 1_000_000_000;
            let (date, time) = civil(wall);
            let mut mem = Line::new();
            let _ = write!(
                mem,
                "mem {}% ({}/{}MB)",
                h.used.saturating_mul(100) / total,
                h.used >> 20,
                total >> 20
            );
            if let Ok(mut c) = GLASS.try_borrow_mut() {
                c.header_status(0, mem.as_str());
                c.header_status(1, date.as_str());
                c.header_status(2, time.as_str());
            }
            n += 1;
            exec.until(start.saturating_add(n.saturating_mul(1_000_000_000)))
                .await;
        }
    }

    /// De USB-invoer na het netwerk: HOP serveert de HID-stroom op het
    /// interne gateway-adres, en dat bestaat pas na de switch. Vandaag
    /// meldt nog geen board zijn xHCI's aan (docs/gui.md: de bedrading per
    /// board), dus dit is één regel.
    pub(crate) fn start_usb_input(_exec: &'static Executor) {
        println!("usb: no host controllers registered on this board HOPOS_USB_NONE");
    }

    /// Een regel van hoogstens 32 bytes op de stack: de meetregels
    /// alloceren niets.
    struct Line {
        buf: [u8; 32],
        len: usize,
    }

    impl Line {
        const fn new() -> Line {
            Line {
                buf: [0; 32],
                len: 0,
            }
        }
        fn as_str(&self) -> &str {
            self.buf
                .get(..self.len)
                .and_then(|b| core::str::from_utf8(b).ok())
                .unwrap_or("")
        }
    }

    impl fmt::Write for Line {
        fn write_str(&mut self, s: &str) -> fmt::Result {
            let end = self.len.checked_add(s.len()).ok_or(fmt::Error)?;
            let dst = self.buf.get_mut(self.len..end).ok_or(fmt::Error)?;
            dst.copy_from_slice(s.as_bytes());
            self.len = end;
            Ok(())
        }
    }

    /// Datum (`dd-mm-jjjj`) en tijd (`uu:mm:ss`, UTC) uit Unix-seconden:
    /// het algoritme `civil_from_days` van Howard Hinnant, zonder tabel.
    fn civil(secs: u64) -> (Line, Line) {
        let days = secs / 86_400;
        let rem = secs % 86_400;
        let z = days as i64 + 719_468;
        let era = z.div_euclid(146_097);
        let doe = z.rem_euclid(146_097);
        let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
        let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
        let mp = (5 * doy + 2) / 153;
        let d = doy - (153 * mp + 2) / 5 + 1;
        let m = if mp < 10 { mp + 3 } else { mp - 9 };
        let y = yoe + era * 400 + i64::from(m <= 2);
        let (mut date, mut time) = (Line::new(), Line::new());
        let _ = write!(date, "{d:02}-{m:02}-{y:04}");
        let _ = write!(
            time,
            "{:02}:{:02}:{:02}",
            rem / 3600,
            rem % 3600 / 60,
            rem % 60
        );
        (date, time)
    }

    /// De grant-aanbieder van de lifecycle in de gui-smaak: de
    /// framebuffer-grant, via [`hooks::GuiGrants`].
    pub(crate) fn slot_grants() -> hooks::GuiGrants {
        hooks::GuiGrants
    }

    /// De grant-haakjes voor de slot-lifecycle (`kern::grants::Grants`); de
    /// lifecycle-actor (slots.rs) is hun eigenaar en roept ze.
    pub(crate) mod hooks {
        use super::{GLASS, GRANT, HEADER_ROWS};
        use driver_fb::Desc;
        use gui_fbgrant::Glass;
        use kern::Slot;
        use kern::cage::Console as _;
        use kern::grants::{Grants, Window, WindowMap};

        /// De console als [`Glass`] voor de grant: eraf bij de toekenning,
        /// terug (schone lei, bunny) bij het vrijkomen.
        struct KernGlass;

        impl Glass for KernGlass {
            fn hand_over(&mut self) {
                if let Ok(mut c) = GLASS.try_borrow_mut() {
                    c.disable();
                }
            }
            fn take_back(&mut self, d: Desc) {
                if let Ok(mut c) = GLASS.try_borrow_mut()
                    && c.init(d).is_ok()
                {
                    let mut lines = [""; HEADER_ROWS];
                    for (l, b) in lines.iter_mut().zip(crate::BUNNY.iter()) {
                        *l = b;
                    }
                    c.header(&lines);
                }
            }
        }

        /// Het venster in de kooi, op ijzer: het tabelblok van het slot uit het
        /// plan van deze node, en `cpu::el2::stage2::grant_window` (dezelfde
        /// bouwer als de kooi zelf, cage.rs). De toets na een flip leest met de
        /// pure rekenkunde van `kern::stage2` over `dev`.
        struct CageWindows;

        impl CageWindows {
            fn block(slot: Slot) -> kern::Result<dev::Pa> {
                let bad = kern::Error::SlotRange {
                    slot: slot.get(),
                    max: abi::layout::SLOT_CAP,
                };
                let plan = crate::slots::os_plan().map_err(|_| bad)?;
                let s = abi::layout::Slot::new(slot.get()).ok_or(bad)?;
                plan.cage_table_pa(s).map_err(|_| bad)
            }
        }

        impl WindowMap for CageWindows {
            fn map(&mut self, slot: Slot, w: Window) -> kern::Result {
                let block = Self::block(slot)?;
                cpu::el2::stage2::grant_window(block, w.pa, w.size).map_err(|_| {
                    kern::Error::Range {
                        base: w.pa,
                        size: w.size,
                    }
                })
            }

            fn is_mapped(&self, slot: Slot, w: Window) -> kern::Result<bool> {
                let block = Self::block(slot)?;
                let s2 = kern::stage2::Stage2 {
                    cage_pa: block
                        .0
                        .wrapping_sub(slot.get() as u64 * kern::stage2::CAGE_STRIDE),
                    max_slots: abi::layout::SLOT_CAP,
                };
                s2.has_grant_window(&crate::DevMem, slot.get(), w.pa, w.size)
            }
        }

        /// De grant-haakjes van de gui-smaak voor de slot-lifecycle
        /// (`kern::grants::Grants`). De actor roept `env` na de claim en vóór
        /// de env op de control-page gaat (`Request::Env`, kern/src/slots.rs),
        /// `arm` na de kooibouw en vóór de dispatch, `adopt` bij de adoptie,
        /// en `release` na een bevestigde stop en bij een abort.
        ///
        /// De grant zelf staat in [`GRANT`], een `LocalCell` op de OS-core:
        /// de console-init biedt het venster aan vóór de actor bestaat, en
        /// daarna raakt alleen de actor hem aan, via deze haakjes.
        pub(crate) struct GuiGrants;

        impl Grants for GuiGrants {
            fn env(&mut self, slot: Slot, env: &[u8], out: &mut alloc::vec::Vec<u8>) {
                if let Ok(mut g) = GRANT.try_borrow_mut() {
                    g.env(slot, env, out, &mut KernGlass, &crate::KernConsole);
                }
            }

            fn arm(&mut self, slot: Slot) -> kern::Result {
                let Ok(g) = GRANT.try_borrow() else {
                    return Err(kern::Error::Busy);
                };
                if g.arm(slot, &mut CageWindows)? {
                    crate::KernConsole.log(format_args!(
                        "slot {slot}: fb window mapped at ipa {:#x} HOPOS_FB_ARM",
                        kern::stage2::FB_IPA
                    ));
                }
                Ok(())
            }

            fn adopt(&mut self, slot: Slot) -> kern::Result {
                let Ok(mut g) = GRANT.try_borrow_mut() else {
                    return Err(kern::Error::Busy);
                };
                g.adopt(slot, &CageWindows, &mut KernGlass)
            }

            fn release(&mut self, slot: Slot) {
                if let Ok(mut g) = GRANT.try_borrow_mut() {
                    g.release(slot, &mut KernGlass, &crate::KernConsole);
                }
            }
        }
    }
}
