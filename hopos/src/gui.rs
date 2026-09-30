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
//! - de USB-controllers zijn van één taak, de USB-taak ([`usb`]): hij
//!   bezit de `gui_usbin::Manager` als `&mut` en is de enige die een xHCI
//!   aanraakt. Wat hij leest, gaat als waarde door een SPSC-rij naar de
//!   input-listener in `net.rs` (`net::input`), die de rij bezit en de
//!   regels aan de display-app schrijft.

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

    /// Geen USB: niets stil te leggen.
    pub(crate) async fn quiesce_usb(_exec: &'static Executor) {}

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
    /// interne gateway-adres, en dat bestaat pas na de switch. Het board
    /// noemt zijn controllers (`Board::usb_hosts`: PCIe en firmware zijn
    /// dan al gedaan); de USB-taak brengt ze op en pompt de rapporten.
    pub(crate) fn start_usb_input(exec: &'static Executor) {
        usb::start(exec);
    }

    /// Vóór de sprong van een flip: de USB-controllers halteren (ze zijn
    /// DMA-masters op de bus; de Pi 5, 30-09) en hoogstens een seconde
    /// wachten tot de USB-taak dat meldt. Zonder levende USB-taak niets.
    pub(crate) async fn quiesce_usb(exec: &'static Executor) {
        if !usb::USB_LIVE.load(core::sync::atomic::Ordering::Acquire) {
            return;
        }
        usb::FLIP_STOP.set();
        let _ = sync::select(
            usb::USB_QUIET.wait(),
            exec.after(core::time::Duration::from_secs(1)),
        )
        .await;
    }

    /// De houder van het glas als slot van de ABI (het bron-IP van de
    /// display-app volgt eruit), voor de input-listener. `None` zonder
    /// houder, of als de grant net geleend is (dan komt er niemand binnen,
    /// en de volgende regel toetst opnieuw).
    pub(crate) fn glass_holder() -> Option<abi::layout::Slot> {
        let s = GRANT.try_borrow().ok()?.holder()?;
        abi::layout::Slot::new(s.get())
    }

    /// De USB-taak (Go: `usbin.Start` en `Manager.Run`).
    ///
    /// Eigendom: de taak bezit de `Manager` (alle controllers) en de
    /// zendkant van [`INPUT`]; de input-listener (`net::input`) bezit de
    /// ontvangkant. Een rapport gaat als waarde van de een naar de ander;
    /// vol is weggooien en tellen, want de pollus mag nooit wachten op een
    /// display die niet leest.
    mod usb {
        /// De flip vraagt de USB-taak te stoppen (`quiesce_usb`).
        pub(super) static FLIP_STOP: sync::Signal = sync::Signal::new();
        /// De USB-taak meldt dat de controllers stilstaan.
        pub(super) static USB_QUIET: sync::Signal = sync::Signal::new();
        /// Draait de USB-taak met levende controllers?
        pub(super) static USB_LIVE: core::sync::atomic::AtomicBool =
            core::sync::atomic::AtomicBool::new(false);

        use super::GRANT;
        use board::{Board, UsbHost, UsbHosts, UsbKind};
        use core::fmt;
        use core::future::Future;
        use core::net::Ipv4Addr;
        use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
        use core::time::Duration;
        use cpu::println;
        use driver_hid::Event;
        use driver_xhci::Hc;
        use executor::Executor;
        use gui_usbin::deliver::{self, INPUT_PORT, InputQueue, InputTx};
        use gui_usbin::register::{HostSpec, PrepareError, Registry};
        use gui_usbin::{Manager, Sink, Timer};

        /// De rij van de USB-taak naar de input-listener.
        static INPUT: InputQueue = InputQueue::new();

        /// Gebeurtenissen die de rij niet meer in pasten (de listener liep
        /// achter). Een meting, geen fout: invoer is lossy by design.
        pub(crate) static QUEUE_DROPS: AtomicU64 = AtomicU64::new(0);

        /// Gebeurtenissen die de USB-taak las, totaal.
        pub(crate) static EVENTS: AtomicU64 = AtomicU64::new(0);

        /// Het timerwiel van de executor als klok en slaap van de driver:
        /// een poortreset of een commando slaapt hierop in plaats van de
        /// core vast te houden.
        #[derive(Clone, Copy)]
        struct UsbTimer(&'static Executor);

        impl Timer for UsbTimer {
            fn now(&self) -> u64 {
                self.0.now()
            }
            fn sleep(&self, ns: u64) -> impl Future<Output = ()> {
                self.0.after(Duration::from_nanos(ns))
            }
        }

        /// De sink van de manager: invoer de rij in, logregels naar de
        /// console.
        struct UsbSink {
            tx: InputTx<'static>,
            #[cfg(feature = "media")]
            optical: crate::optical::Bridge,
        }

        impl Sink for UsbSink {
            fn input(&mut self, e: Event) {
                EVENTS.fetch_add(1, Relaxed);
                if !deliver::offer(&mut self.tx, e) {
                    // Eén regel bij de eerste, daarna tellen.
                    if QUEUE_DROPS.fetch_add(1, Relaxed) == 0 {
                        println!(
                            "usb: input queue full ({} events), dropping (counted) HOPOS_USB_QUEUE_FULL",
                            deliver::QUEUE_DEPTH
                        );
                    }
                }
            }

            fn log(&mut self, args: fmt::Arguments<'_>) {
                println!("{args}");
            }
            #[cfg(feature = "media")]
            fn storage_attached(&mut self, info: &gui_usbin::storage::BulkInfo) {
                self.optical.attached(info);
            }
            #[cfg(feature = "media")]
            fn storage_gone(&mut self, id: gui_usbin::storage::BulkId) {
                self.optical.gone(id);
            }
            #[cfg(feature = "media")]
            fn bulk_out(&mut self, req: &gui_usbin::storage::BulkReq) -> &[u8] {
                self.optical.output(req)
            }
            #[cfg(feature = "media")]
            fn bulk_in(&mut self, req: &gui_usbin::storage::BulkReq) -> &mut [u8] {
                self.optical.input(req)
            }
            #[cfg(feature = "media")]
            fn bulk_done(
                &mut self,
                req: &gui_usbin::storage::BulkReq,
                r: core::result::Result<usize, gui_usbin::storage::BulkError>,
            ) {
                self.optical.done(req, r);
            }
        }

        /// Vraagt het board zijn controllers, belooft het invoeradres aan de
        /// grant en spawnt de USB-taak. De belofte gaat vóór de taak (en
        /// dus vóór elke plaatsing, die `main` pas hierna start): de
        /// bring-up zelf slaapt op de executor, en een display-app die
        /// intussen het glas kreeg, moet het adres al in zijn env hebben.
        /// Komt geen controller op, dan trekt de taak het in.
        pub(super) fn start(exec: &'static Executor) {
            let hosts = crate::BOARD.usb_hosts();
            if hosts.is_empty() {
                println!("usb: no host controllers on this board HOPOS_USB_NONE");
                return;
            }
            let Some((tx, rx)) = INPUT.split() else {
                println!("usb: input queue already taken HOPOS_USB_FAIL");
                return;
            };
            if let Ok(mut g) = GRANT.try_borrow_mut() {
                g.use_input(Ipv4Addr::from(abi::layout::HOST_IP4), INPUT_PORT);
            }
            let fb = GRANT
                .try_borrow()
                .ok()
                .and_then(|g| g.desc())
                .map(|d| (d.width, d.height));
            if exec
                .spawn(run(
                    exec,
                    hosts,
                    UsbSink {
                        tx,
                        #[cfg(feature = "media")]
                        optical: crate::optical::Bridge::default(),
                    },
                ))
                .is_err()
            {
                println!("usb: task not spawned HOPOS_USB_FAIL");
                stop_input();
                return;
            }
            if exec
                .spawn(crate::net::input::serve(
                    exec,
                    deliver::Deliverer::new(rx, fb),
                ))
                .is_err()
            {
                println!("usb: input listener not spawned HOPOS_USB_FAIL");
            }
        }

        /// Trekt het invoeradres in bij de grant.
        fn stop_input() {
            if let Ok(mut g) = GRANT.try_borrow_mut() {
                g.stop_input();
            }
        }

        /// De taak: controllers op, dan de ronde van de manager tot het
        /// einde van de node (Go's `Run`; er zijn nog geen bulk-verzoeken
        /// van buiten, dus alleen de slaap tot de volgende ronde).
        async fn run(exec: &'static Executor, hosts: UsbHosts, mut sink: UsbSink) {
            let mut reg = Registry::new();
            for h in hosts.iter() {
                let spec = HostSpec {
                    name: h.name,
                    base: h.regs.base,
                    bus_off: h.bus_off,
                    dma: h.dma.base,
                    dma_size: h.dma.size,
                };
                if reg.register(spec).is_err() {
                    println!(
                        "usb: {}: more controllers than the manager takes, skipped",
                        h.name
                    );
                }
            }
            let t = UsbTimer(exec);
            let mut mgr = Manager::new(t);
            let live = reg
                .bring_up(&mut mgr, &mut sink, async |spec: &HostSpec| {
                    make(&hosts, spec, &t).await
                })
                .await;
            if live == 0 {
                stop_input();
                println!("usb: no controller came up, INPUT_ADDR withdrawn HOPOS_USB_NONE");
                return;
            }
            println!(
                "usb: {live} controller(s) up, polling every {} ms HOPOS_USB_UP",
                gui_usbin::POLL_INTERVAL_NS / 1_000_000
            );
            USB_LIVE.store(true, core::sync::atomic::Ordering::Release);
            loop {
                #[cfg(feature = "media")]
                if let Some(req) = sink.optical.next() {
                    mgr.enqueue(req, &mut sink);
                }
                let wait = match sync::select(mgr.step(&mut sink), FLIP_STOP.wait()).await {
                    sync::Either::Left(w) => w,
                    sync::Either::Right(()) => {
                        // Een flip komt: alle controllers stil, en zeggen dat
                        // het zo is. De taak eindigt; komt de sprong er niet,
                        // dan is de invoer weg tot de volgende boot.
                        mgr.stop_all().await;
                        println!("usb: {live} controller(s) halted for the flip HOPOS_USB_HALTED");
                        USB_QUIET.set();
                        return;
                    }
                };
                #[cfg(feature = "media")]
                let _ = sync::select(
                    exec.after(Duration::from_nanos(wait)),
                    crate::optical::WAKE.wait(),
                )
                .await;
                #[cfg(not(feature = "media"))]
                exec.after(Duration::from_nanos(wait)).await;
            }
        }

        /// Maakt de driver voor één aangeboden controller: een DWC3-core
        /// eerst in hostmodus, dan de xHCI op hetzelfde venster.
        async fn make(hosts: &UsbHosts, spec: &HostSpec, t: &UsbTimer) -> Result<Hc, PrepareError> {
            let Some(h) = hosts.iter().find(|h| h.name == spec.name) else {
                return Err(PrepareError {
                    what: "controller not offered by the board",
                    value: spec.base.0,
                });
            };
            if h.kind == UsbKind::Dwc3 {
                dwc3_host_mode(h, t).await?;
            }
            // SAFETY: het venster komt van `Board::usb_hosts`: het
            // capability-blok van een xHCI (of een DWC3 die nu in hostmodus
            // staat), Device gemapt voor altijd door het board, en `h.dma` is
            // DMA-geheugen dat het board voor deze controller alleen plande.
            Ok(unsafe { Hc::new(spec.base, spec.name, spec.bus_off) })
        }

        /// De DWC3-core van `h` in hostmodus (de RK3566), met de globale
        /// registers in één regel: op dat silicium is de vraag niet "werkt
        /// de driver" maar "staat de klok en de PHY aan" (Go, 06-08).
        async fn dwc3_host_mode(h: &UsbHost, t: &UsbTimer) -> Result<(), PrepareError> {
            // SAFETY: het board noemt dit venster als DWC3-core: de
            // globale registers liggen op +0xC100 binnen `h.regs`, Device
            // gemapt voor altijd.
            let core = unsafe { driver_dwc3::Core::new(h.regs.base) };
            let r = core.host_mode(t).await;
            let g = core.regs();
            println!(
                "usb: {} dwc3 id={:08x} gctl={:08x} usb2={:08x} usb3={:08x} gsts={:08x}",
                h.name, g.id, g.ctl, g.usb2_phy, g.usb3_pipe, g.sts
            );
            r.map_err(|e| PrepareError {
                what: e.what(),
                value: e.value(),
            })
        }
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
